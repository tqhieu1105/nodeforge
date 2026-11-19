import type { IncomingMessage, ServerResponse } from 'node:http';
import { bytes, HttpError, missing } from '../http.ts';
import type { Repo, Store } from '../store.ts';
import { headRef, isAncestor, OID, readCommit, readTree, tagTarget, ZERO, type GitObject } from './objects.ts';
import { packObjects, unpack } from './pack.ts';
import { band, DELIM, FLUSH, packet, readPackets } from './wire.ts';

const UPLOAD = 'git-upload-pack';
const RECEIVE = 'git-receive-pack';
const V2 = Buffer.concat(['version 2\n', 'agent=forge/1.0\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n'].map(packet).concat(FLUSH));
const shallowCache = new WeakMap<Repo, { tip: string; response: Buffer }>();

function serviceReply(res: ServerResponse, service: string, kind: 'advertisement' | 'result', body: Buffer) {
  res.writeHead(200, {
    'content-type': 'application/x-' + service + '-' + kind,
    'content-length': body.length,
    'cache-control': 'no-cache',
  });
  res.end(body);
}

function peel(repo: Repo, sha: string): string | undefined {
  let tip = sha;
  for (let count = 0; count < 64; count++) {
    const next = tagTarget(repo.objects.get(tip));
    if (!next) return tip === sha ? undefined : tip;
    tip = next;
  }
  return undefined;
}

function advertise(repo: Repo, service: string, v2: boolean) {
  if (v2) return V2;
  const symref = headRef(repo.info.default_branch);
  const head = repo.refs.get(symref);
  const refs = [...repo.refs];
  if (head && service === UPLOAD) refs.unshift(['HEAD', head]);
  if (refs.length === 0) refs.push(['capabilities^{}', ZERO]);
  const capabilities = service === RECEIVE
    ? 'report-status side-band-64k delete-refs ofs-delta'
    : 'side-band-64k shallow ofs-delta no-progress symref=HEAD:' + symref;
  const lines = [packet('# service=' + service + '\n'), FLUSH];
  refs.forEach(([ref, sha], index) => {
    lines.push(packet(sha + ' ' + ref + (index === 0 ? '\0' + capabilities : '') + '\n'));
    const peeled = service === UPLOAD && ref.startsWith('refs/tags/') && peel(repo, sha);
    if (peeled) lines.push(packet(peeled + ' ' + ref + '^{}\n'));
  });
  return Buffer.concat([...lines, FLUSH]);
}

function receive(repo: Repo, body: Buffer) {
  const { lines, offset } = readPackets(body, true);
  if (lines.length === 0) throw new HttpError(400, 'Missing ref commands');
  const [first, features = ''] = lines[0].split('\0');
  lines[0] = first;
  const commands = lines.map(line => {
    const [old, next, ref, extra] = line.split(' ');
    if (!OID.test(old) || !OID.test(next) || !validRef(ref) || extra !== undefined) throw new HttpError(400, 'Invalid ref command');
    return { old, next, ref };
  });
  let unpackError = '';
  try {
    const incoming = offset === body.length && commands.every(command => command.next === ZERO)
      ? new Map<string, GitObject>()
      : unpack(body.subarray(offset), repo.objects);
    for (const [sha, object] of incoming) if (!repo.objects.has(sha)) repo.objects.set(sha, object);
  } catch {
    unpackError = 'invalid pack';
  }

  const report = [packet('unpack ' + (unpackError || 'ok') + '\n')];
  for (const { old, next, ref } of commands) {
    const current = repo.refs.get(ref) ?? ZERO;
    let reason = unpackError;
    if (!reason && next !== ZERO) {
      const object = repo.objects.get(next);
      if (!object || (ref.startsWith('refs/heads/') && object.kind !== 'commit')) reason = 'missing object';
      else if (current !== ZERO && current !== next && !isAncestor(repo.objects, current, next)) reason = 'non-fast-forward';
    }
    if (!reason && old !== current) reason = 'stale ref';
    if (reason) {
      report.push(packet('ng ' + ref + ' ' + reason + '\n'));
    } else {
      if (next === ZERO) repo.refs.delete(ref);
      else repo.refs.set(ref, next);
      report.push(packet('ok ' + ref + '\n'));
    }
  }
  const caps = new Set(features.split(' '));
  const payload = caps.has('report-status') || caps.has('report-status-v2') ? Buffer.concat([...report, FLUSH]) : Buffer.alloc(0);
  return caps.has('side-band-64k') ? Buffer.concat([...band(payload), FLUSH]) : payload;
}

function validRef(ref: string) {
  return typeof ref === 'string' && /^refs\/(heads|tags)\/.+/.test(ref) && !ref.endsWith('.') &&
    !ref.includes('..') && !ref.includes('@{') && !/[\u0000-\u0020\u007f~^:?*\[\\]/.test(ref) &&
    ref.split('/').every(part => part && !part.startsWith('.') && !part.endsWith('.lock'));
}

function listRefs(repo: Repo, lines: string[]) {
  const prefixes = lines.filter(line => line.startsWith('ref-prefix ')).map(line => line.slice(11));
  const matches = (ref: string) => prefixes.length === 0 || prefixes.some(prefix => ref.startsWith(prefix));
  const result: Buffer[] = [];
  const symref = headRef(repo.info.default_branch);
  const head = repo.refs.get(symref);
  const sym = lines.includes('symrefs') ? ' symref-target:' + symref : '';
  if (matches('HEAD')) {
    if (head) result.push(packet(head + ' HEAD' + sym + '\n'));
    else if (lines.includes('unborn')) result.push(packet('unborn HEAD symref-target:' + symref + '\n'));
  }
  for (const [ref, sha] of repo.refs) {
    if (!matches(ref)) continue;
    const peeled = lines.includes('peel') && peel(repo, sha);
    result.push(packet(sha + ' ' + ref + (peeled ? ' peeled:' + peeled : '') + '\n'));
  }
  result.push(FLUSH);
  return Buffer.concat(result);
}

function walk(repo: Repo, roots: string[], depth: number, stops: ReadonlySet<string>) {
  const objects = new Set<string>();
  const commits = new Map<string, { level: number; parents: string[] }>();
  const pending = roots.map(sha => ({ sha, level: 1 }));
  for (let index = 0; index < pending.length; index++) {
    const { sha, level } = pending[index];
    if (objects.has(sha)) continue;
    const object = repo.objects.get(sha);
    if (!object) continue;
    objects.add(sha);
    if (object.kind === 'commit') {
      const commit = readCommit(object);
      if (!commit) continue;
      commits.set(sha, { level, parents: commit.parents });
      pending.push({ sha: commit.tree, level });
      if (level < depth && !stops.has(sha)) {
        for (const parent of commit.parents) pending.push({ sha: parent, level: level + 1 });
      }
    } else if (object.kind === 'tree') {
      for (const entry of readTree(object)) if (entry.mode !== '160000') pending.push({ sha: entry.sha, level });
    } else if (object.kind === 'tag') {
      const target = tagTarget(object);
      if (target) pending.push({ sha: target, level });
    }
  }
  return { objects, commits };
}

function fetchPack(repo: Repo, lines: string[], v2: boolean) {
  const select = (prefix: string) => lines.filter(line => line.startsWith(prefix)).map(line => line.slice(prefix.length).split(' ')[0]);
  const wants = [...new Set(select('want '))];
  const haves = select('have ').filter(sha => repo.objects.has(sha));
  const shallow = new Set(select('shallow '));
  const requestedDepth = Number(select('deepen ')[0]);
  const depth = Number.isSafeInteger(requestedDepth) && requestedDepth > 0 ? requestedDepth : Infinity;
  if (wants.some(sha => !OID.test(sha) || !repo.objects.has(sha))) throw new HttpError(400, 'Unknown wanted object');

  const cacheable = v2 && depth === 1 && wants.length === 1 && haves.length === 0 && shallow.size === 0 && lines.includes('done');
  if (cacheable) {
    const cached = shallowCache.get(repo);
    if (cached?.tip === wants[0]) return cached.response;
  }
  if (v2 && !lines.includes('done')) {
    const reply = [packet('acknowledgments\n')];
    if (haves.length) reply.push(...haves.map(sha => packet('ACK ' + sha + '\n')));
    else reply.push(packet('NAK\n'));
    return Buffer.concat([...reply, FLUSH]);
  }

  const held = walk(repo, haves, Infinity, shallow).objects;
  const stops = depth === Infinity ? shallow : new Set<string>();
  const desired = walk(repo, wants, depth, stops);
  const boundary: Buffer[] = [];
  if (depth < Infinity) {
    for (const [sha, commit] of desired.commits) {
      if (commit.level === depth && commit.parents.some(parent => !desired.commits.has(parent)) && !shallow.has(sha)) {
        boundary.push(packet('shallow ' + sha + '\n'));
      } else if (shallow.has(sha) && commit.parents.every(parent => desired.commits.has(parent))) {
        boundary.push(packet('unshallow ' + sha + '\n'));
      }
    }
  }
  const objects: GitObject[] = [];
  for (const sha of desired.objects) if (!held.has(sha)) objects.push(repo.objects.get(sha)!);
  const pack = packObjects(objects);
  let reply: Buffer[];
  if (v2) {
    reply = boundary.length ? [packet('shallow-info\n'), ...boundary, DELIM] : [];
    reply.push(packet('packfile\n'), ...band(pack), FLUSH);
  } else {
    reply = depth < Infinity ? [...boundary, FLUSH] : [];
    reply.push(haves.length ? packet('ACK ' + haves.at(-1) + '\n') : packet('NAK\n'));
    if (lines.some(line => line.startsWith('want ') && line.split(' ').includes('side-band-64k'))) reply.push(...band(pack), FLUSH);
    else reply.push(pack);
  }
  const response = Buffer.concat(reply);
  if (cacheable && response.length <= 1024 * 1024) shallowCache.set(repo, { tip: wants[0], response });
  return response;
}

export async function smartHttp(req: IncomingMessage, res: ServerResponse, url: URL, store: Store) {
  const match = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(url.pathname);
  if (!match) return missing();
  const repo = store.requireRepo(decodeURIComponent(match[1]), decodeURIComponent(match[2]));
  const endpoint = match[3];
  const service = endpoint === 'info/refs' ? url.searchParams.get('service') : endpoint;
  if (service !== UPLOAD && service !== RECEIVE) return missing();
  const v2 = service === UPLOAD && req.headers['git-protocol']?.split(':').includes('version=2');
  if (req.method === 'GET' && endpoint === 'info/refs') return serviceReply(res, service, 'advertisement', advertise(repo, service, !!v2));
  if (req.method !== 'POST' || endpoint !== service) return missing();
  const body = await bytes(req, 64 * 1024 * 1024);
  if (service === RECEIVE) return serviceReply(res, service, 'result', receive(repo, body));
  const { lines } = readPackets(body, false);
  let reply: Buffer;
  if (v2 && lines[0] === 'command=ls-refs') reply = listRefs(repo, lines);
  else if ((v2 && lines[0] === 'command=fetch') || (!v2 && lines[0]?.startsWith('want '))) reply = fetchPack(repo, lines, !!v2);
  else throw new HttpError(400, 'Unknown upload-pack command');
  return serviceReply(res, service, 'result', reply);
}
