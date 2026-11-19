import { createServer } from 'node:http';
import { smartHttp } from './git/smart.ts';
import { graphql } from './graphql.ts';
import { HttpError, json, missing } from './http.ts';
import { mcp } from './mcp.ts';
import { rest } from './rest.ts';
import { Store } from './store.ts';

export function createApp() {
  const token = process.env.MAKEFUN_SHOP_TOKEN;
  const authorization = token ? 'Bearer ' + token : undefined;
  // The kit binds its opaque bearer token to the fixture's viewer account.
  const store = new Store(process.env.FORGE_LOGIN ?? 'adaiversen');
  let requestId = 0;

  return createServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', 'http://forge');
      const isRest = url.pathname === '/api/v3' || url.pathname.startsWith('/api/v3/');
      if (isRest) res.setHeader('x-github-request-id', (++requestId).toString(16));
      if (req.method === 'GET' && url.pathname === '/healthz') return json(res, 200, { ok: true });
      if (!authorization || req.headers.authorization !== authorization) throw new HttpError(401, 'Bad credentials');
      if (isRest) return await rest(req, res, url, store);
      if (req.method === 'POST' && url.pathname === '/api/graphql') return await graphql(req, res, store);
      if (req.method === 'POST' && url.pathname === '/mcp') return await mcp(req, res, store);
      if (url.pathname.includes('.git/')) return await smartHttp(req, res, url, store);
      return missing();
    } catch (error) {
      if (!req.complete) req.resume();
      if (res.headersSent) return res.destroy();
      if (error instanceof HttpError) return json(res, error.status, { message: error.message });
      if (error instanceof URIError) return json(res, 400, { message: 'Malformed URL' });
      return json(res, 500, { message: 'Internal Server Error' });
    }
  });
}
