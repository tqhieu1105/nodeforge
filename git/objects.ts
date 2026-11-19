import { createHash } from 'node:crypto';

export type Kind = 'blob' | 'tree' | 'commit' | 'tag';
export type GitObject = { kind: Kind; data: Buffer };
export type Objects = Map<string, GitObject>;
export type TreeEntry = { name: string; mode: string; sha: string };
export type FileEntry = { mode: string; sha: string };

export const ZERO = '0'.repeat(40);
export const OID = /^[a-f0-9]{40}$/;
export const headRef = (name: string) => 'refs/heads/' + name;

export function objectId(kind: Kind, data: Buffer): string {
  return createHash('sha1').update(kind + ' ' + data.length + '\0').update(data).digest('hex');
}

export function saveObject(objects: Objects, kind: Kind, data: Buffer): string {
  const sha = objectId(kind, data);
  if (!objects.has(sha)) objects.set(sha, { kind, data });
  return sha;
}

export function readTree(object?: GitObject): TreeEntry[] {
  if (object?.kind !== 'tree') return [];
  const entries: TreeEntry[] = [];
  const data = object.data;
  for (let offset = 0; offset < data.length;) {
    const space = data.indexOf(32, offset);
    const nul = data.indexOf(0, space + 1);
    if (space <= offset || nul <= space || nul + 21 > data.length) throw new Error('Invalid tree');
    entries.push({
      name: data.toString('utf8', space + 1, nul),
      mode: data.toString('ascii', offset, space),
      sha: data.toString('hex', nul + 1, nul + 21),
    });
    offset = nul + 21;
  }
  return entries;
}

export function readCommit(object?: GitObject) {
  if (object?.kind !== 'commit') return undefined;
  const text = object.data.toString('utf8');
  const boundary = text.indexOf('\n\n');
  if (boundary === -1) return undefined;
  const headers = text.slice(0, boundary).split('\n');
  const tree = headers[0]?.startsWith('tree ') ? headers[0].slice(5) : '';
  if (!OID.test(tree)) return undefined;
  const parents = headers.filter(line => line.startsWith('parent ')).map(line => line.slice(7));
  return { tree, parents, message: text.slice(boundary + 2).replace(/\n$/, '') };
}

export function tagTarget(object?: GitObject): string | undefined {
  if (object?.kind !== 'tag') return undefined;
  const line = object.data.toString('ascii', 0, 48);
  const target = line.slice(7, 47);
  return line.startsWith('object ') && OID.test(target) ? target : undefined;
}

export function snapshot(objects: Objects, commit?: string): Map<string, FileEntry> {
  const files = new Map<string, FileEntry>();
  function visit(tree: string, prefix: string, depth: number) {
    if (depth > 128) throw new Error('Tree is too deep');
    for (const { name, mode, sha } of readTree(objects.get(tree))) {
      if (mode === '40000') visit(sha, prefix + name + '/', depth + 1);
      else files.set(prefix + name, { mode, sha });
    }
  }
  const root = commit && readCommit(objects.get(commit));
  if (root) visit(root.tree, '', 0);
  return files;
}

export function writeTree(objects: Objects, files: ReadonlyMap<string, FileEntry>): string {
  type Directory = { files: Map<string, FileEntry>; dirs: Map<string, Directory> };
  const directory = (): Directory => ({ files: new Map(), dirs: new Map() });
  const root = directory();
  for (const [path, entry] of files) {
    const parts = path.split('/');
    const name = parts.pop()!;
    let current = root;
    for (const part of parts) {
      if (current.files.has(part)) throw new Error('Conflicting tree paths');
      if (!current.dirs.has(part)) current.dirs.set(part, directory());
      current = current.dirs.get(part)!;
    }
    if (current.dirs.has(name)) throw new Error('Conflicting tree paths');
    current.files.set(name, entry);
  }
  function write(dir: Directory): string {
    const entries: TreeEntry[] = [];
    for (const [name, file] of dir.files) entries.push({ name, ...file });
    for (const [name, child] of dir.dirs) entries.push({ name, mode: '40000', sha: write(child) });
    const sortKey = (entry: TreeEntry) => Buffer.from(entry.name + (entry.mode === '40000' ? '/' : ''));
    entries.sort((a, b) => Buffer.compare(sortKey(a), sortKey(b)));
    return saveObject(objects, 'tree', Buffer.concat(entries.flatMap(entry => [
      Buffer.from(entry.mode + ' ' + entry.name + '\0'), Buffer.from(entry.sha, 'hex'),
    ])));
  }
  return write(root);
}

export function saveCommit(objects: Objects, files: ReadonlyMap<string, FileEntry>, parents: string[], message: string, login: string): string {
  const tree = writeTree(objects, files);
  const identity = login + ' <' + login + '@example.invalid> 1700000000 +0000';
  const headers = ['tree ' + tree, ...parents.map(p => 'parent ' + p), 'author ' + identity, 'committer ' + identity];
  return saveObject(objects, 'commit', Buffer.from(headers.join('\n') + '\n\n' + message + '\n'));
}

export function isAncestor(objects: Objects, ancestor: string, tip: string): boolean {
  const pending = [tip];
  const visited = new Set<string>();
  while (pending.length) {
    const sha = pending.pop()!;
    if (sha === ancestor) return true;
    if (visited.has(sha)) continue;
    visited.add(sha);
    const commit = readCommit(objects.get(sha));
    if (commit) pending.push(...commit.parents);
  }
  return false;
}
