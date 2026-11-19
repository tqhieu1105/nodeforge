import type { Objects } from './git/objects.ts';
import { HttpError, missing } from './http.ts';

export type Label = { name: string; color: string };
export type Issue = {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
};
export type Comment = { id: number; body: string };

export type Repository = {
  id: number;
  name: string;
  private: boolean;
  description: string;
  default_branch: string;
  owner: { login: string };
  full_name: string;
};

export type Repo = {
  info: Repository;
  refs: Map<string, string>;
  objects: Objects;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  comments: Map<number, Comment[]>;
  nextNumber: number;
};

export class Store {
  readonly viewer: { login: string };
  readonly repos = new Map<string, Repo>();
  nextId = 1;

  constructor(login: string) {
    this.viewer = { login };
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
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: new Map(),
      comments: new Map(),
      nextNumber: 1,
    };
    this.repos.set(key, repo);
    return repo;
  }

  findRepo(owner: string, name: string): Repo | undefined {
    return owner.toLowerCase() === this.viewer.login.toLowerCase() ? this.repos.get(name.toLowerCase()) : undefined;
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
