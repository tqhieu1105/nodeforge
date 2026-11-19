import type { IncomingMessage, ServerResponse } from 'node:http';
import { bytes, HttpError, missing } from '../http.ts';
import type { Repo, Store } from '../store.ts';
import { headRef, isAncestor, OID, readCommit, readTree, tagTarget, ZERO, type GitObject } from './objects.ts';
import { packObjects, unpack } from './pack.ts';
import { band, DELIM, FLUSH, packet, readPackets } from './wire.ts';

const UPLOAD = 'git-upload-pack';
const RECEIVE = 'git-receive-pack';
type WireReply = { headers: string[]; body: Buffer | string };
const uploadCache = new WeakMap<Repo, { refs: number; objects: number; replies: Map<string, WireReply> }>();
const gitPaths = new WeakMap<URL, { owner: string; name: string; endpoint: string; service: string | null }>();

function wireReply(service: string, kind: 'advertisement' | 'result', payload: Buffer): WireReply {
  return {
    headers: [
      'content-type', 'application/x-' + service + '-' + kind,
      'content-length', String(payload.length),
      'cache-control', 'no-cache',
    ],
    // Latin-1 preserves every byte and lets Node send a short body with the headers.
    body: payload.length <= 4096 ? payload.toString('latin1') : payload,
  };
}

const V2 = wireReply(UPLOAD, 'advertisement', Buffer.concat([
  ...['version 2\n', 'agent=forge/1.0\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n'].map(packet),
  FLUSH,
]));

function serviceReply(res: ServerResponse, { headers, body }: WireReply) {
  res.writeHead(200, headers);
  if (typeof body === 'string') res.end(body, 'latin1');
  else res.end(body);
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
  return wireReply(service, 'advertisement', Buffer.concat([...lines, FLUSH]));
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
  return Buffer.concat(reply);
}

function upload(repo: Repo, body: Buffer, v2: boolean): WireReply {
  let key: string | undefined;
  let cached = uploadCache.get(repo);
  if (v2 && body.length <= 1024) {
    key = body.toString('latin1');
    if (cached?.refs !== repo.refs.revision || cached.objects !== repo.objects.size) {
      cached = { refs: repo.refs.revision, objects: repo.objects.size, replies: new Map() };
      uploadCache.set(repo, cached);
    }
    const reply = cached.replies.get(key);
    if (reply !== undefined) return reply;
  }

  const { lines } = readPackets(body, false);
  let reply: Buffer;
  let cacheable = false;
  if (v2 && lines[0] === 'command=ls-refs') {
    reply = listRefs(repo, lines);
    cacheable = true;
  } else if ((v2 && lines[0] === 'command=fetch') || (!v2 && lines[0]?.startsWith('want '))) {
    reply = fetchPack(repo, lines, v2);
    cacheable = lines.includes('deepen 1') && lines.includes('done') &&
      !lines.some(line => line.startsWith('have ') || line.startsWith('shallow '));
  } else {
    throw new HttpError(400, 'Unknown upload-pack command');
  }
  const response = wireReply(UPLOAD, 'result', reply);
  if (cacheable && key !== undefined && cached && reply.length <= 128 * 1024) {
    if (cached.replies.size >= 4) cached.replies.delete(cached.replies.keys().next().value!);
    cached.replies.set(key, response);
  }
  return response;
}

export async function smartHttp(req: IncomingMessage, res: ServerResponse, url: URL, store: Store) {
  let path = gitPaths.get(url);
  if (!path) {
    const match = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(url.pathname);
    if (!match) return missing();
    const endpoint = match[3];
    path = { owner: decodeURIComponent(match[1]), name: decodeURIComponent(match[2]), endpoint,
      service: endpoint === 'info/refs' ? url.searchParams.get('service') : endpoint };
    gitPaths.set(url, path);
  }
  const { owner, name, endpoint, service } = path;
  const repo = store.requireRepo(owner, name);
  if (service !== UPLOAD && service !== RECEIVE) return missing();
  const protocol = req.headers['git-protocol'];
  const v2 = service === UPLOAD && (protocol === 'version=2' || !!protocol?.split(':').includes('version=2'));
  if (req.method === 'GET' && endpoint === 'info/refs') return serviceReply(res, advertise(repo, service, v2));
  if (req.method !== 'POST' || endpoint !== service) return missing();
  const body = await bytes(req, 64 * 1024 * 1024);
  const reply = service === RECEIVE ? wireReply(RECEIVE, 'result', receive(repo, body)) : upload(repo, body, v2);
  return serviceReply(res, reply);
}
