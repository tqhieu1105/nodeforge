import type { IncomingMessage, ServerResponse } from 'node:http';
import { headRef } from './git/objects.ts';
import { json, missing, readJson, record } from './http.ts';
import { createPull, isMergeable, listPulls, mergePull, pullFiles } from './pulls.ts';
import { listIssues, type Repo, type Store } from './store.ts';

type Values = Record<string, unknown>;
type Selection = { name: string; key: string; args: Values; fields: Selection[]; on?: string };

class Document {
  private tokens: string[] = [];
  private index = 0;
  private variables: Values;

  constructor(query: string, variables: Values) {
    this.variables = Object.assign(Object.create(null), variables);
    const lexer = /(?:[\s,\uFEFF]+|#[^\r\n]*)|("(?:[^"\\\r\n]|\\.)*"|\.\.\.|[_A-Za-z][_0-9A-Za-z]*|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|[!$():={}\[\]])/gy;
    let end = 0;
    for (let match; (match = lexer.exec(query));) {
      if (match[1]) this.tokens.push(match[1]);
      end = lexer.lastIndex;
    }
    if (end !== query.length) throw new Error('Invalid GraphQL token');
  }

  private peek() { return this.tokens[this.index]; }

  private take() {
    const token = this.tokens[this.index++];
    if (!token) throw new Error('Unexpected end of GraphQL document');
    return token;
  }

  private accept(token: string) {
    if (this.peek() !== token) return false;
    this.index++;
    return true;
  }

  private expect(token: string) {
    if (!this.accept(token)) throw new Error('Expected ' + token);
  }

  private name() {
    const token = this.take();
    if (!/^[_A-Za-z]/.test(token)) throw new Error('Expected GraphQL name');
    return token;
  }

  parse() {
    let type = 'Query';
    if (this.peek() !== '{') {
      const operation = this.name();
      if (operation === 'mutation') type = 'Mutation';
      else if (operation !== 'query') throw new Error('Unsupported GraphQL operation ' + operation);
      if (this.peek() !== '(' && this.peek() !== '{') this.name();
      if (this.accept('(')) {
        while (!this.accept(')')) {
          this.expect('$');
          const variable = this.name();
          this.expect(':');
          this.variableType(0);
          if (this.accept('=')) {
            const fallback = this.value(0);
            if (!Object.hasOwn(this.variables, variable)) this.variables[variable] = fallback;
          }
        }
      }
    }
    const fields = this.selection(0);
    if (this.peek()) throw new Error('Unexpected tokens after operation');
    return { type, fields };
  }

  private variableType(depth: number) {
    if (depth >= 32) throw new Error('GraphQL type is too deep');
    if (this.accept('[')) {
      this.variableType(depth + 1);
      this.expect(']');
    } else this.name();
    this.accept('!');
  }

  private value(depth: number): unknown {
    if (depth >= 32) throw new Error('GraphQL value is too deep');
    if (this.accept('$')) return this.variables[this.name()];
    if (this.accept('{')) return this.properties('}', depth + 1);
    if (this.accept('[')) {
      const items: unknown[] = [];
      while (!this.accept(']')) items.push(this.value(depth + 1));
      return items;
    }
    const atom = this.take();
    if (/^["\d-]/.test(atom) || atom === 'null' || atom === 'true' || atom === 'false') return JSON.parse(atom);
    if (!/^[_A-Za-z]/.test(atom)) throw new Error('Invalid GraphQL value');
    return atom;
  }

  private properties(end: string, depth: number): Values {
    const values = Object.create(null) as Values;
    while (!this.accept(end)) {
      const name = this.name();
      this.expect(':');
      values[name] = this.value(depth);
    }
    return values;
  }

  private selection(depth: number): Selection[] {
    if (depth >= 32) throw new Error('GraphQL selection is too deep');
    this.expect('{');
    const result: Selection[] = [];
    while (!this.accept('}')) {
      if (this.accept('...')) {
        this.expect('on');
        result.push({ name: '', key: '', args: {}, on: this.name(), fields: this.selection(depth + 1) });
        continue;
      }
      const key = this.name();
      const name = this.accept(':') ? this.name() : key;
      const args = this.accept('(') ? this.properties(')', 0) : {};
      const fields = this.peek() === '{' ? this.selection(depth + 1) : [];
      result.push({ key, name, args, fields });
    }
    if (!result.length) throw new Error('Empty GraphQL selection');
    return result;
  }
}

const schema: Record<string, Record<string, string>> = {
  Query: { viewer: 'User', repository: 'Repository', node: 'Node', __type: '__Type' },
  Mutation: { createIssue: 'IssuePayload', closeIssue: 'IssuePayload', addComment: 'CommentPayload', createPullRequest: 'PullPayload', mergePullRequest: 'PullPayload' },
  Node: { id: '' },
  User: { login: '' },
  Repository: {
    id: '', databaseId: '', name: '', nameWithOwner: '', owner: 'User', defaultBranchRef: 'Ref',
    viewerPermission: '', hasIssuesEnabled: '', issue: 'Issue', issues: 'Issues', pullRequest: 'PullRequest', pullRequests: 'PullRequests',
  },
  Ref: { name: '' },
  Issue: { id: '', number: '', title: '', body: '', state: '' },
  PullRequest: {
    id: '', number: '', title: '', body: '', state: '', merged: '', mergeable: '', headRefName: '', baseRefName: '', files: 'Files',
  },
  IssueComment: { id: '', body: '' },
  Issues: { nodes: 'Issue', edges: 'IssueEdge', pageInfo: 'PageInfo', totalCount: '' },
  IssueEdge: { node: 'Issue', cursor: '' },
  PullRequests: { nodes: 'PullRequest', edges: 'PullEdge', pageInfo: 'PageInfo', totalCount: '' },
  PullEdge: { node: 'PullRequest', cursor: '' },
  Files: { nodes: 'File', edges: 'FileEdge', pageInfo: 'PageInfo', totalCount: '' },
  FileEdge: { node: 'File', cursor: '' },
  File: { path: '', additions: '', deletions: '' },
  PageInfo: { endCursor: '', startCursor: '', hasNextPage: '', hasPreviousPage: '' },
  __Type: { name: '', kind: '', fields: '__Field', enumValues: '__EnumValue' },
  __Field: { name: '' },
  __EnumValue: { name: '' },
  IssuePayload: { issue: 'Issue' },
  PullPayload: { pullRequest: 'PullRequest' },
  CommentPayload: { commentEdge: 'CommentEdge' },
  CommentEdge: { node: 'IssueComment' },
};

function validate(type: string, fields: Selection[]) {
  for (const field of fields) {
    if (field.on) {
      if (!Object.hasOwn(schema, field.on)) throw new Error('Unknown type ' + field.on);
      validate(field.on, field.fields);
      continue;
    }
    const definitions = schema[type];
    if (field.name === '__typename' && !field.fields.length) continue;
    if (!Object.hasOwn(definitions, field.name)) throw new Error('Cannot query field "' + field.name + '" on type "' + type + '".');
    const nested = definitions[field.name];
    if (nested) validate(nested, field.fields);
    if (Boolean(nested) !== Boolean(field.fields.length)) throw new Error('Invalid selection for field "' + field.name + '".');
  }
}

const globalId = (type: string, id: number, number = 0) => Buffer.from(type + ':' + id + ':' + number).toString('base64url');
const string = (value: unknown) => typeof value === 'string' ? value : '';
const cursor = (key: string) => Buffer.from('cursor:' + key).toString('base64url');

type ObjectView = { type: string; fields: Values; resolve?: (name: string, args: Values) => unknown };
const view = (type: string, fields: Values, resolve?: ObjectView['resolve']): ObjectView => ({ type, fields, resolve });

function project(object: ObjectView, fields: Selection[]): Values {
  const result = Object.create(null) as Values;
  for (const field of fields) {
    if (field.on) {
      if (field.on === object.type || field.on === 'Node') Object.assign(result, project(object, field.fields));
      continue;
    }
    const value = field.name === '__typename' ? object.type :
      Object.hasOwn(object.fields, field.name) ? object.fields[field.name] : object.resolve?.(field.name, field.args);
    if (value == null) result[field.key] = null;
    else if (!field.fields.length) result[field.key] = value;
    else if (Array.isArray(value)) result[field.key] = value.map(item => project(item, field.fields));
    else result[field.key] = project(value as ObjectView, field.fields);
  }
  return result;
}

function connection(type: string, edgeType: string, entries: [string, ObjectView][], args: Values) {
  const offset = typeof args.after === 'string' ? entries.findIndex(([key]) => cursor(key) === args.after) + 1 : 0;
  const count = typeof args.first === 'number' && Number.isSafeInteger(args.first) ? Math.min(100, Math.max(0, args.first)) : 30;
  const page = entries.slice(offset, offset + count);
  return view(type, {
    nodes: page.map(([, node]) => node),
    edges: page.map(([key, node]) => view(edgeType, { cursor: cursor(key), node })),
    totalCount: entries.length,
    pageInfo: view('PageInfo', {
      hasNextPage: offset + page.length < entries.length, hasPreviousPage: offset > 0,
      startCursor: page.length ? cursor(page[0][0]) : null, endCursor: page.length ? cursor(page.at(-1)![0]) : null,
    }),
  });
}

function issueView(repo: Repo, issue?: import('./store.ts').Issue) {
  if (!issue) return null;
  return view('Issue', { ...issue, id: globalId('Issue', repo.info.id, issue.number), state: issue.state.toUpperCase() });
}

function pullView(repo: Repo, pull?: import('./store.ts').Pull) {
  if (!pull) return null;
  return view('PullRequest', {
    id: globalId('PullRequest', repo.info.id, pull.number), number: pull.number, title: pull.title, body: pull.body,
    headRefName: pull.head.ref, baseRefName: pull.base.ref, state: pull.merged ? 'MERGED' : pull.state.toUpperCase(), merged: pull.merged,
  }, (name, args) => {
    if (name === 'mergeable') return isMergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
    return connection('Files', 'FileEdge', pullFiles(repo, pull).map(file => [file.filename, view('File', { ...file, path: file.filename })]), args);
  });
}

function repositoryView(repo?: Repo) {
  if (!repo) return null;
  const info = repo.info;
  return view('Repository', {
    id: globalId('Repository', info.id), databaseId: info.id, name: info.name, nameWithOwner: info.full_name,
    owner: view('User', info.owner), viewerPermission: 'ADMIN', hasIssuesEnabled: true,
    defaultBranchRef: repo.refs.has(headRef(info.default_branch)) ? view('Ref', { name: info.default_branch }) : null,
  }, (name, args) => {
    if (name === 'issue') return issueView(repo, repo.issues.get(Number(args.number)));
    if (name === 'pullRequest') return pullView(repo, repo.pulls.get(Number(args.number)));
    const states = Array.isArray(args.states) ? args.states : undefined;
    if (name === 'issues') {
      const ascending = (args.orderBy as Values | undefined)?.direction === 'ASC';
      const entries = listIssues(repo, 'all', ascending).filter(issue => !states || states.includes(issue.state.toUpperCase()));
      return connection('Issues', 'IssueEdge', entries.map(issue => [String(issue.number), issueView(repo, issue)!]), args);
    }
    const entries = listPulls(repo, 'all').filter(pull => !states || states.includes(pull.merged ? 'MERGED' : pull.state.toUpperCase()));
    return connection('PullRequests', 'PullEdge', entries.map(pull => [String(pull.number), pullView(repo, pull)!]), args);
  });
}

function lookupId(store: Store, id: unknown, expected?: string) {
  if (typeof id !== 'string' || id.length > 100) return undefined;
  const [type, repoId, number] = Buffer.from(id, 'base64url').toString().split(':');
  const numericId = Number(repoId);
  const item = Number(number);
  if ((expected && expected !== type) || globalId(type, numericId, item) !== id) return undefined;
  for (const repo of store.repos.values()) if (repo.info.id === numericId) return { type, repo, item };
  return undefined;
}

function typeView(name: string) {
  if (name === 'SearchType') {
    return view('__Type', { name, kind: 'ENUM', fields: null, enumValues: ['ISSUE', 'REPOSITORY', 'USER'].map(name => view('__EnumValue', { name })) });
  }
  if (!Object.hasOwn(schema, name)) return null;
  return view('__Type', { name, kind: name === 'Node' ? 'INTERFACE' : 'OBJECT', enumValues: null,
    fields: Object.keys(schema[name]).map(name => view('__Field', { name })),
  });
}

function query(store: Store) {
  return view('Query', { viewer: view('User', store.viewer) }, (name, args) => {
    if (name === '__type') return typeView(string(args.name));
    if (name === 'repository') return repositoryView(store.findRepo(string(args.owner), string(args.name)));
    const match = lookupId(store, args.id);
    if (!match) return null;
    if (match.type === 'Repository') return repositoryView(match.repo);
    if (match.type === 'Issue') return issueView(match.repo, match.repo.issues.get(match.item));
    if (match.type === 'PullRequest') return pullView(match.repo, match.repo.pulls.get(match.item));
    return null;
  });
}

function mutation(store: Store) {
  return view('Mutation', {}, (name, args) => {
    const input = record(args.input);
    if (name === 'createIssue' || name === 'createPullRequest') {
      const { repo } = lookupId(store, input.repositoryId, 'Repository') ?? missing();
      if (name === 'createIssue') return view('IssuePayload', { issue: issueView(repo, store.createIssue(repo, input)) });
      const pull = createPull(store, repo, { ...input, head: input.headRefName, base: input.baseRefName });
      return view('PullPayload', { pullRequest: pullView(repo, pull) });
    }
    if (name === 'mergePullRequest') {
      const { repo, item } = lookupId(store, input.pullRequestId, 'PullRequest') ?? missing();
      const pull = repo.pulls.get(item) ?? missing();
      mergePull(repo, pull, string(input.mergeMethod).toLowerCase() || 'squash');
      return view('PullPayload', { pullRequest: pullView(repo, pull) });
    }
    const { repo, item } = lookupId(store, name === 'closeIssue' ? input.issueId : input.subjectId, 'Issue') ?? missing();
    const issue = repo.issues.get(item) ?? missing();
    if (name === 'closeIssue') {
      issue.state = 'closed';
      return view('IssuePayload', { issue: issueView(repo, issue) });
    }
    const comment = store.addComment(repo, issue, input.body);
    const node = view('IssueComment', { id: globalId('IssueComment', repo.info.id, comment.id), body: comment.body });
    return view('CommentPayload', { commentEdge: view('CommentEdge', { node }) });
  });
}

function execute(store: Store, request: unknown) {
  try {
    const { query: source, variables } = record(request);
    if (typeof source !== 'string') throw new Error('GraphQL query is required');
    const { type, fields } = new Document(source, variables == null ? {} : record(variables)).parse();
    validate(type, fields);
    return { data: project(type === 'Mutation' ? mutation(store) : query(store), fields) };
  } catch (error) {
    return { errors: [{ message: error instanceof Error ? error.message : 'GraphQL execution failed' }] };
  }
}

export async function graphql(req: IncomingMessage, res: ServerResponse, store: Store) {
  const body = await readJson(req);
  return json(res, 200, Array.isArray(body) ? body.map(item => execute(store, item)) : execute(store, body));
}
