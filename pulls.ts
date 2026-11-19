import { headRef, readCommit, saveCommit, snapshot, type FileEntry, type Objects } from './git/objects.ts';
import { HttpError } from './http.ts';
import type { Pull, Repo, Store } from './store.ts';

export function createPull(store: Store, repo: Repo, input: Record<string, unknown>): Pull {
  const { head, base, title, body } = input;
  if (typeof title !== 'string' || !title.trim() || typeof head !== 'string' || typeof base !== 'string' || head === base) {
    throw new HttpError(422, 'Invalid pull request');
  }
  const headSha = repo.refs.get(headRef(head));
  const baseSha = repo.refs.get(headRef(base));
  if (!headSha || !baseSha) throw new HttpError(422, 'Unknown head or base branch');
  const pull: Pull = {
    id: store.nextId++, number: repo.nextNumber++, title, body: typeof body === 'string' ? body : '',
    state: 'open', head: { ref: head, sha: headSha }, base: { ref: base, sha: baseSha },
    merged: false, merged_at: null, merge_commit_sha: null,
  };
  repo.pulls.set(pull.number, pull);
  return pull;
}

export function listPulls(repo: Repo, state: string) {
  return [...repo.pulls.values()].filter(pull => state === 'all' || pull.state === state).reverse();
}

function ancestorDistances(objects: Objects, tip: string) {
  const distances = new Map([[tip, 0]]);
  for (const [sha, distance] of distances) {
    for (const parent of readCommit(objects.get(sha))?.parents ?? []) {
      if (!distances.has(parent)) distances.set(parent, distance + 1);
    }
  }
  return distances;
}

function mergeBase(objects: Objects, base: string, head: string) {
  const ancestors = ancestorDistances(objects, base);
  let closest: string | undefined;
  let score = Infinity;
  for (const [sha, distance] of ancestorDistances(objects, head)) {
    const fromBase = ancestors.get(sha);
    if (fromBase !== undefined && distance + fromBase < score) {
      closest = sha;
      score = distance + fromBase;
    }
  }
  return closest;
}

const equal = (a?: FileEntry, b?: FileEntry) => a?.sha === b?.sha && a?.mode === b?.mode;

function pullChanges(repo: Repo, base: string, head: string) {
  const ancestor = mergeBase(repo.objects, base, head);
  const before = snapshot(repo.objects, ancestor);
  const after = snapshot(repo.objects, head);
  const changes = [];
  for (const path of [...new Set([...before.keys(), ...after.keys()])].sort()) {
    const old = before.get(path);
    const next = after.get(path);
    if (!equal(old, next)) changes.push({ path, old, next });
  }
  return { ancestor, changes };
}

function lines(objects: Objects, entry?: FileEntry): string[] {
  const data = entry && objects.get(entry.sha)?.data;
  if (!data?.length || data.includes(0)) return [];
  return data.toString('utf8').match(/[^\n]*\n|[^\n]+$/g) ?? [];
}

function lineChanges(before: string[], after: string[]) {
  let start = 0;
  let end = 0;
  const shared = Math.min(before.length, after.length);
  while (start < shared && before[start] === after[start]) start++;
  while (start + end < shared && before[before.length - end - 1] === after[after.length - end - 1]) end++;
  const removed = before.slice(start, before.length - end);
  const added = after.slice(start, after.length - end);
  // Count the longest common subsequence after trimming matching ends.
  let matched = 0;
  if (removed.length * added.length <= 1_000_000) {
    const lengths = new Uint32Array(added.length + 1);
    for (const line of removed) {
      let diagonal = 0;
      for (let j = 1; j <= added.length; j++) {
        const above = lengths[j];
        lengths[j] = line === added[j - 1] ? diagonal + 1 : Math.max(above, lengths[j - 1]);
        diagonal = above;
      }
    }
    matched = lengths[added.length];
  }
  return { additions: added.length - matched, deletions: removed.length - matched };
}

export function pullFiles(repo: Repo, pull: Pull) {
  const head = pull.merged ? pull.head.sha : repo.refs.get(headRef(pull.head.ref)) ?? pull.head.sha;
  const base = pull.merged ? pull.base.sha : repo.refs.get(headRef(pull.base.ref)) ?? pull.base.sha;
  return pullChanges(repo, base, head).changes.map(({ path, old, next }) => {
    const { additions, deletions } = lineChanges(lines(repo.objects, old), lines(repo.objects, next));
    return {
      filename: path, status: old ? next ? 'modified' : 'removed' : 'added',
      additions, deletions, changes: additions + deletions, sha: next?.sha ?? old?.sha,
    };
  });
}

function planMerge(repo: Repo, pull: Pull) {
  const ref = headRef(pull.base.ref);
  const base = repo.refs.get(ref);
  const head = repo.refs.get(headRef(pull.head.ref));
  if (!base || !head) return undefined;
  const { ancestor, changes } = pullChanges(repo, base, head);
  if (!ancestor) return undefined;

  const files = snapshot(repo.objects, base);
  for (const { path, old, next } of changes) {
    const current = files.get(path);
    if (!equal(current, old) && !equal(current, next)) return undefined;
    if (next) files.set(path, next);
    else files.delete(path);
  }
  for (const path of files.keys()) {
    for (let slash = path.indexOf('/'); slash >= 0; slash = path.indexOf('/', slash + 1)) {
      if (files.has(path.slice(0, slash))) return undefined;
    }
  }
  return { files, ref, base, head };
}

export function isMergeable(repo: Repo, pull: Pull) {
  return pull.merged || planMerge(repo, pull) !== undefined;
}

export function mergePull(repo: Repo, pull: Pull, method: unknown = 'squash') {
  if (method !== 'squash' || pull.state !== 'open') throw new HttpError(405, 'Pull request cannot be merged');
  const plan = planMerge(repo, pull);
  if (!plan) throw new HttpError(405, 'Pull request has merge conflicts');
  const { files, ref, base, head } = plan;
  const sha = saveCommit(repo.objects, files, [base], pull.title + ' (#' + pull.number + ')', repo.info.owner.login);
  repo.refs.set(ref, sha);
  pull.head.sha = head;
  pull.base.sha = base;
  pull.state = 'closed';
  pull.merged = true;
  pull.merged_at = '2023-11-14T22:13:20Z';
  pull.merge_commit_sha = sha;
  return { sha, merged: true, message: 'Pull request successfully merged' };
}
