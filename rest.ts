import type { IncomingMessage, ServerResponse } from 'node:http';
import { headRef, OID, readCommit, readTree, saveCommit, saveObject, snapshot } from './git/objects.ts';
import { HttpError, json, missing, readJson, record } from './http.ts';
import { listIssues, type Repo, type Store } from './store.ts';

async function input(req: IncomingMessage) {
  return record(await readJson(req));
}

export async function rest(req: IncomingMessage, res: ServerResponse, url: URL, store: Store) {
  const parts = url.pathname.slice('/api/v3/'.length).split('/').map(decodeURIComponent);
  const method = req.method;
  if (url.pathname === '/api/v3' || url.pathname === '/api/v3/') {
    if (method === 'GET') {
      return json(res, 200, { current_user_url: '/api/v3/user' }, { 'x-oauth-scopes': 'repo, read:org' });
    }
  }
  if (parts.length === 1 && method === 'GET') {
    if (parts[0] === 'meta') return json(res, 200, { installed_version: '1.0.0' });
    if (parts[0] === 'user') return json(res, 200, store.viewer);
  }
  if (parts.length === 2 && parts[0] === 'user' && parts[1] === 'repos') {
    if (method === 'POST') return json(res, 201, store.createRepo(await input(req)).info);
    if (method === 'GET') return listRepos(req, res, url, store);
  }
  if (parts[0] !== 'repos' || parts.length < 3) return missing();
  const repo = store.requireRepo(parts[1], parts[2]);
  if (parts.length === 3) {
    if (method === 'GET') return json(res, 200, repo.info);
    if (method === 'PATCH') {
      const { description } = await input(req);
      if (typeof description === 'string') repo.info.description = description;
      return json(res, 200, repo.info);
    }
    if (method === 'DELETE') {
      store.repos.delete(repo.info.name.toLowerCase());
      return json(res, 204);
    }
  }
  if (parts.length === 5 && parts[3] === 'git' && parts[4] === 'refs' && method === 'POST') {
    const { ref, sha } = await input(req);
    if (typeof ref !== 'string' || !ref.startsWith('refs/heads/') || ref.length <= 11 ||
        typeof sha !== 'string' || !OID.test(sha) || !readCommit(repo.objects.get(sha)) || repo.refs.has(ref)) {
      throw new HttpError(422, 'Invalid branch reference');
    }
    repo.refs.set(ref, sha);
    return json(res, 201, { ref, object: { sha } });
  }
  if (parts.length === 4 && parts[3] === 'branches' && method === 'GET') {
    return json(res, 200, [...repo.refs].filter(([ref]) => ref.startsWith('refs/heads/')).map(([ref, sha]) => ({
      name: ref.slice(11), commit: { sha },
    })));
  }
  if (parts.length === 4 && parts[3] === 'commits' && method === 'GET') {
    const tip = resolveRef(repo, url.searchParams.get('sha'));
    return json(res, 200, listCommits(repo, tip));
  }
  if (parts[3] === 'contents') {
    const path = parts.slice(4).join('/');
    if (method === 'GET') return json(res, 200, getContent(repo, path, resolveRef(repo, url.searchParams.get('ref'))));
    if (method === 'PUT' || method === 'DELETE') return changeContent(res, repo, path, await input(req), method === 'DELETE');
  }
  if (parts.length === 4 && parts[3] === 'labels' && method === 'POST') {
    const { name, color } = await input(req);
    if (typeof name !== 'string' || typeof color !== 'string') throw new HttpError(422, 'Invalid label');
    const label = { name, color };
    repo.labels.set(name, label);
    return json(res, 201, label);
  }
  if (parts[3] === 'issues') {
    if (parts.length === 4) {
      if (method === 'POST') return json(res, 201, store.createIssue(repo, await input(req)));
      if (method === 'GET') {
        const { searchParams } = url;
        const issues = listIssues(repo, searchParams.get('state') || 'open', searchParams.get('direction') === 'asc');
        return json(res, 200, paginate(issues, searchParams));
      }
    } else {
      const issue = repo.issues.get(Number(parts[4])) ?? missing();
      if (parts.length === 5) {
        if (method === 'GET') return json(res, 200, issue);
        if (method === 'PATCH') {
          const { state, title, body } = await input(req);
          if (state === 'open' || state === 'closed') issue.state = state;
          if (typeof title === 'string') issue.title = title;
          if (typeof body === 'string') issue.body = body;
          return json(res, 200, issue);
        }
      }
      if (parts.length === 6 && parts[5] === 'comments') {
        if (method === 'GET') return json(res, 200, repo.comments.get(issue.number) ?? []);
        if (method === 'POST') return json(res, 201, store.addComment(repo, issue, (await input(req)).body));
      }
    }
  }
  return missing();
}

function pageBounds(params: URLSearchParams) {
  const integer = (name: string, fallback: number) => {
    const value = Number(params.get(name));
    return Number.isSafeInteger(value) && value > 0 ? value : fallback;
  };
  const limit = Math.min(integer('per_page', 30), 100);
  const page = integer('page', 1);
  const start = (page - 1) * limit;
  return { page, start, end: start + limit };
}

function paginate<T>(items: T[], params: URLSearchParams) {
  const { start, end } = pageBounds(params);
  return items.slice(start, end);
}

function listRepos(req: IncomingMessage, res: ServerResponse, url: URL, store: Store) {
  const { page, start, end } = pageBounds(url.searchParams);
  const repos = [...store.repos.values()].slice(start, end).map(repo => repo.info);
  if (end < store.repos.size) {
    const next = new URL(url.pathname + url.search, 'http://' + req.headers.host);
    next.searchParams.set('page', String(page + 1));
    res.setHeader('link', '<' + next.href + '>; rel="next"');
  }
  return json(res, 200, repos);
}

function resolveRef(repo: Repo, name: string | null) {
  if (name === null) return repo.refs.get(headRef(repo.info.default_branch));
  return repo.refs.get(name) ?? repo.refs.get(headRef(name)) ?? repo.refs.get('refs/tags/' + name) ??
    (repo.objects.has(name) ? name : undefined);
}

function listCommits(repo: Repo, tip?: string) {
  const commits: { sha: string; commit: { message: string } }[] = [];
  const seen = new Set<string>();
  const pending = tip ? [tip] : [];
  while (pending.length) {
    const sha = pending.pop()!;
    if (seen.has(sha)) continue;
    seen.add(sha);
    const commit = readCommit(repo.objects.get(sha));
    if (!commit) continue;
    commits.push({ sha, commit: { message: commit.message } });
    pending.push(...commit.parents.toReversed());
  }
  return commits;
}

function getContent(repo: Repo, path: string, tip?: string) {
  let sha = readCommit(tip ? repo.objects.get(tip) : undefined)?.tree ?? missing();
  let mode = '40000';
  let name = '';
  for (const part of path ? path.split('/') : []) {
    if (mode !== '40000') return missing();
    const entry = readTree(repo.objects.get(sha)).find(entry => entry.name === part) ?? missing();
    ({ name, mode, sha } = entry);
  }
  if (mode === '40000') {
    return readTree(repo.objects.get(sha)).map(entry => ({
      type: entry.mode === '40000' ? 'dir' : 'file',
      name: entry.name,
      path: path ? path + '/' + entry.name : entry.name,
      sha: entry.sha,
    }));
  }
  const object = repo.objects.get(sha);
  if (object?.kind !== 'blob') return missing();
  return { type: 'file', name, path, sha, size: object.data.length, encoding: 'base64', content: object.data.toString('base64') };
}

function changeContent(res: ServerResponse, repo: Repo, path: string, body: Record<string, unknown>, remove: boolean) {
  const { content, sha, message } = body;
  if ((!remove && typeof content !== 'string') || typeof message !== 'string' ||
      path.split('/').some(part => !part || part === '.' || part === '..' || part.includes('\0'))) {
    throw new HttpError(400, 'Invalid file contents');
  }
  const ref = headRef(typeof body.branch === 'string' ? body.branch : repo.info.default_branch);
  const parent = repo.refs.get(ref);
  if (!parent && ref !== headRef(repo.info.default_branch)) return missing();
  const files = snapshot(repo.objects, parent);
  const previous = files.get(path);
  if (remove && !previous) return missing();
  if (previous?.sha !== sha) throw new HttpError(409, 'Content SHA does not match');
  let blob: string | undefined;
  if (remove) {
    files.delete(path);
  } else {
    for (const existing of files.keys()) {
      if (existing.startsWith(path + '/') || path.startsWith(existing + '/')) throw new HttpError(409, 'Conflicting file path');
    }
    blob = saveObject(repo.objects, 'blob', Buffer.from(content as string, 'base64'));
    files.set(path, { sha: blob, mode: previous?.mode ?? '100644' });
  }
  const commit = saveCommit(repo.objects, files, parent ? [parent] : [], message, repo.info.owner.login);
  repo.refs.set(ref, commit);
  if (remove) return json(res, 200, { commit: { sha: commit } });
  return json(res, previous ? 200 : 201, { content: { sha: blob, encoding: 'base64' }, commit: { sha: commit } });
}
