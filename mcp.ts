import type { IncomingMessage, ServerResponse } from 'node:http';
import { HttpError, json, missing, readJson } from './http.ts';
import { createPull, mergePull } from './pulls.ts';
import { listIssues, type Store } from './store.ts';

const revision = '2026-07-28';
const serverInfo = { name: 'makefun-forge', version: '1.0.0' };
const capabilities = { tools: {} };
const headers = { 'mcp-protocol-version': revision };
type Params = Record<string, unknown>;
type PropertyType = 'string' | 'boolean' | 'integer';

const repoFields = { owner: 'string', repo: 'string' } as const;
const repoRequired = ['owner', 'repo'];

function tool(name: string, description: string, fields: Record<string, PropertyType>, required: string[]) {
  return {
    name, description,
    inputSchema: {
      type: 'object',
      properties: Object.fromEntries(Object.entries(fields).map(([key, type]) => [key, { type }])),
      required,
    },
  };
}

const tools = [
  tool('create_repo', 'Create a repository for the authenticated user.', { name: 'string', description: 'string', private: 'boolean' }, ['name']),
  tool('get_repo', 'Read a repository by owner and name.', repoFields, repoRequired),
  tool('create_issue', 'Open an issue in a repository.', { ...repoFields, title: 'string', body: 'string' }, [...repoRequired, 'title']),
  tool('list_issues', 'List issues in a repository with an optional state filter.', { ...repoFields, state: 'string' }, repoRequired),
  tool('create_pull_request', 'Open a pull request from one branch into another.', {
    ...repoFields, title: 'string', head: 'string', base: 'string', body: 'string',
  }, [...repoRequired, 'title', 'head', 'base']),
  tool('merge_pull_request', 'Squash merge an open pull request into its base branch.', { ...repoFields, pull_number: 'integer' }, [...repoRequired, 'pull_number']),
];
const toolsByName = new Map(tools.map(item => [item.name, item]));

function isRecord(value: unknown): value is Params {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function rpcError(id: unknown, code: number, message: string) {
  return { jsonrpc: '2.0', id, error: { code, message } };
}

function callTool(store: Store, name: string, args: Params) {
  if (name === 'create_repo') return store.createRepo(args).info;
  const repo = store.requireRepo(args.owner as string, args.repo as string);
  switch (name) {
    case 'get_repo': return repo.info;
    case 'create_issue': return store.createIssue(repo, args);
    case 'list_issues': return { issues: listIssues(repo, typeof args.state === 'string' ? args.state : 'open') };
    case 'create_pull_request': return createPull(store, repo, args);
    case 'merge_pull_request': return mergePull(repo, repo.pulls.get(args.pull_number as number) ?? missing());
    default: throw new Error('Unknown tool dispatch');
  }
}

export async function mcp(req: IncomingMessage, res: ServerResponse, store: Store) {
  let body: unknown;
  try {
    body = await readJson(req);
  } catch (error) {
    if (!(error instanceof HttpError)) throw error;
    return json(res, 400, rpcError(null, -32700, 'Parse error'), headers);
  }

  if (!isRecord(body) || body.jsonrpc !== '2.0' || typeof body.method !== 'string' ||
      (Object.hasOwn(body, 'id') && body.id !== null && typeof body.id !== 'string' && typeof body.id !== 'number')) {
    return json(res, 200, rpcError(null, -32600, 'Invalid request'), headers);
  }
  if (!Object.hasOwn(body, 'id')) return json(res, 202, undefined, headers);
  const { id, method, params } = body;
  const send = (result: unknown) => json(res, 200, { jsonrpc: '2.0', id, result }, headers);
  const error = (code: number, message: string) => json(res, 200, rpcError(id, code, message), headers);

  if (method === 'server/discover') {
    return send({
      supportedVersions: [revision], capabilities,
      _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo },
    });
  }
  if (method === 'tools/list') return send({ tools });
  if (method !== 'tools/call') return error(-32601, 'Method not found');

  const definition = isRecord(params) && typeof params.name === 'string' ? toolsByName.get(params.name) : undefined;
  if (!definition) return error(-32602, 'Unknown tool');
  const args = (params as Params).arguments ?? {};
  if (!isRecord(args)) return error(-32602, 'Tool arguments must be an object');
  for (const key of definition.inputSchema.required) {
    if (!Object.hasOwn(args, key)) return error(-32602, 'Missing required argument ' + key);
  }
  for (const [key, { type }] of Object.entries(definition.inputSchema.properties)) {
    if (!Object.hasOwn(args, key)) continue;
    const value = args[key];
    if (type === 'integer' ? !Number.isSafeInteger(value) : typeof value !== type) {
      return error(-32602, 'Invalid argument ' + key);
    }
  }
  try {
    const result = callTool(store, definition.name, args);
    return send({ content: [{ type: 'text', text: JSON.stringify(result) }], structuredContent: result });
  } catch (failure) {
    if (!(failure instanceof HttpError)) throw failure;
    return send({ content: [{ type: 'text', text: failure.message }], isError: true });
  }
}
