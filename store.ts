import type { Objects } from './git/objects.ts';
import { HttpError, missing } from './http.ts';

export type Label = { name: string; color: string };
export type Issue = Readonly<{
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
}>;
export type Comment = { id: number; body: string };
export type Pull = {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  merged: boolean;
  merged_at: string | null;
  merge_commit_sha: string | null;
};

export type Repository = {
  id: number;
  name: string;
  private: boolean;
  description: string;
  default_branch: string;
  owner: { login: string };
  full_name: string;
};

class Refs extends Map<string, string> {
  revision = 0;

  override set(ref: string, sha: string) {
    if (this.get(ref) !== sha) this.revision++;
    return super.set(ref, sha);
  }

  override delete(ref: string) {
    const removed = super.delete(ref);
    if (removed) this.revision++;
    return removed;
  }

  override clear() {
    if (this.size) this.revision++;
    super.clear();
  }
}

export type Repo = {
  info: Repository;
  refs: Refs;
  objects: Objects;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  comments: Map<number, Comment[]>;
  pulls: Map<number, Pull>;
  nextNumber: number;
};

export class Store {
  readonly viewer: { login: string };
  private readonly loginKey: string;
  readonly repos = new Map<string, Repo>();
  nextId = 1;

  constructor(login: string) {
    this.viewer = { login };
    this.loginKey = login.toLowerCase();
  }

  createRepo(input: Record<string, unknown>): Repo {
    const { name, description } = input;
    if (typeof name !== 'string' || !/^[a-zA-Z0-9._-]{1,100}$/.test(name) || name === '.' || name === '..') {
      throw new HttpError(422, 'Invalid repository name');
    }
    const key = name.toLowerCase();
    if (this.repos.has(key)) throw new HttpError(422, 'Repository already exists');
    const repo: Repo = {
      info: {
        id: this.nextId++,
        name,
        private: input.private === true,
        description: typeof description === 'string' ? description : '',
        default_branch: 'main',
        owner: this.viewer,
        full_name: this.viewer.login + '/' + name,
      },
      refs: new Refs(),
      objects: new Map(),
      labels: new Map(),
      issues: new Map(),
      comments: new Map(),
      pulls: new Map(),
      nextNumber: 1,
    };
    this.repos.set(key, repo);
    return repo;
  }

  findRepo(owner: string, name: string): Repo | undefined {
    return owner.toLowerCase() === this.loginKey ? this.repos.get(name.toLowerCase()) : undefined;
  }

  requireRepo(owner: string, name: string): Repo {
    return this.findRepo(owner, name) ?? missing();
  }

  createIssue(repo: Repo, input: Record<string, unknown>): Issue {
    const labels = input.labels === undefined ? [] : input.labels;
    if (!Array.isArray(labels)) throw new HttpError(422, 'Invalid labels');
    const resolved = labels.map(name => {
      const label = typeof name === 'string' && repo.labels.get(name);
      if (!label) throw new HttpError(422, 'Unknown label');
      return label;
    });
    if (typeof input.title !== 'string' || !input.title.trim()) throw new HttpError(422, 'Issue title is required');
    const issue: Issue = {
      id: this.nextId++, number: repo.nextNumber++, title: input.title,
      body: typeof input.body === 'string' ? input.body : '', state: 'open', labels: resolved,
    };
    repo.issues.set(issue.number, issue);
    return issue;
  }

  updateIssue(repo: Repo, number: number, input: Record<string, unknown>): Issue {
    const current = repo.issues.get(number) ?? missing();
    const { state, title, body } = input;
    const issue: Issue = {
      ...current,
      state: state === 'open' || state === 'closed' ? state : current.state,
      title: typeof title === 'string' ? title : current.title,
      body: typeof body === 'string' ? body : current.body,
    };
    repo.issues.set(number, issue);
    return issue;
  }

  addComment(repo: Repo, issue: Issue, body: unknown): Comment {
    if (typeof body !== 'string' || !body.trim()) throw new HttpError(422, 'Comment body is required');
    const comment = { id: this.nextId++, body };
    const comments = repo.comments.get(issue.number);
    if (comments) comments.push(comment);
    else repo.comments.set(issue.number, [comment]);
    return comment;
  }
}

export function listIssues(repo: Repo, state: string, ascending = false) {
  const issues = [...repo.issues.values()].filter(issue => state === 'all' || issue.state === state);
  return ascending ? issues : issues.reverse();
}
