// The HTTP server: one node:http server, no dependencies. It reads each
// request body fully, enforces the bearer token on every surface except
// /healthz, routes to the git / REST / GraphQL / MCP handlers, and answers
// every REST reply as JSON with a non-empty X-GitHub-Request-Id.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleGit, type GitRequest } from './git.ts';
import { handleRest, type RestRequest } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { Store } from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';

function header(req: IncomingMessage, name: string): string | null {
  const value = req.headers[name.toLowerCase()];
  if (Array.isArray(value)) return value[0] ?? null;
  return value ?? null;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      chunks.push(buf);
      total += buf.length;
      if (total > 64 * 1024 * 1024) {
        reject(new Error('request body too large'));
        req.destroy();
      }
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer, extra: Record<string, string> = {}): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length, ...extra });
  res.end(body);
}

function sendJson(res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}): void {
  const text = body === null ? '' : `${JSON.stringify(body)}\n`;
  const buf = Buffer.from(text, 'utf8');
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': buf.length, ...extra });
  res.end(buf);
}

/** Build the server. It binds nothing until the caller listens on it. */
export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      const requestId = store.nextRequestId();
      try {
        const method = req.method ?? 'GET';
        let pathname = '/';
        let query = new URLSearchParams();
        try {
          const url = new URL(req.url ?? '/', 'http://entry');
          pathname = url.pathname;
          query = url.searchParams;
        } catch {
          pathname = '/';
        }
        const host = header(req, 'host') ?? '127.0.0.1';
        // Node already lowercases incoming header names, so req.headers is
        // the lowered map the git handler wants; no per-request copy.
        const headers: Record<string, string> = req.headers as Record<string, string>;

        // Health probe: no auth, always up.
        if (method === 'GET' && pathname === '/healthz') {
          sendJson(res, 200, { ok: true }, { 'x-github-request-id': requestId });
          return;
        }

        // Every other surface needs the trial's bearer token when one is set.
        const auth = header(req, 'authorization');
        const tokenOk = store.token === '' || auth === `Bearer ${store.token}`;
        if (!tokenOk) {
          sendJson(res, 401, { message: 'Unauthorized' }, { 'x-github-request-id': requestId });
          return;
        }

        const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);
        // Lazily decoded: git routes work on the raw Buffer and never need
        // the UTF-8 text, so decoding every pack body to a string (and
        // throwing it away) was pure waste on pushes and fetches.
        let bodyTextCache: string | null = null;
        const bodyText = (): string => (bodyTextCache ??= body.toString('utf8'));

        // Git smart HTTP: /{owner}/{repo}.git/...
        if (pathname.includes('.git/') || pathname.endsWith('.git')) {
          const gitReq: GitRequest = { method, path: pathname, query, headers, body };
          const gitRes = handleGit(store, gitReq);
          if (gitRes !== null) {
            sendBuffer(res, gitRes.status, gitRes.contentType, gitRes.body, {
              'x-github-request-id': requestId,
              ...(gitRes.extraHeaders ?? {}),
            });
            return;
          }
        }

        // GraphQL.
        if (pathname === '/api/graphql') {
          if (method !== 'POST') {
            sendJson(res, 405, { message: 'Method Not Allowed' }, { 'x-github-request-id': requestId });
            return;
          }
          let parsed: unknown = null;
          let parseFailed = false;
          if (bodyText().trim() !== '') {
            try {
              parsed = JSON.parse(bodyText());
            } catch {
              parseFailed = true;
            }
          }
          if (parseFailed) {
            sendJson(res, 400, { errors: [{ message: 'Request body is not valid JSON' }] }, { 'x-github-request-id': requestId });
            return;
          }
          const result = handleGraphQL(store, { method, bodyText: bodyText(), body: parsed });
          sendJson(res, result.status, result.body, { 'x-github-request-id': requestId });
          return;
        }

        // MCP.
        if (pathname === '/mcp') {
          const result = handleMcp(store, { method, bodyText: bodyText(), body: null, headers });
          if (result.body === null) {
            res.writeHead(result.status, { 'x-github-request-id': requestId, ...(result.headers ?? {}) });
            res.end();
            return;
          }
          sendJson(res, result.status, result.body, { 'x-github-request-id': requestId, ...(result.headers ?? {}) });
          return;
        }

        // REST v3.
        if (pathname === '/api/v3' || pathname.startsWith('/api/v3/')) {
          let parsed: unknown = null;
          let parseFailed = false;
          const needsJson = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
          if (needsJson && bodyText().trim() !== '') {
            try {
              parsed = JSON.parse(bodyText());
            } catch {
              parseFailed = true;
            }
          }
          if (parseFailed) {
            sendJson(res, 400, { message: 'Invalid JSON body' }, { 'x-github-request-id': requestId });
            return;
          }
          const restReq: RestRequest = { method, path: pathname, query, headers, bodyText: bodyText(), body: parsed, host };
          const restRes = handleRest(store, restReq);
          if (restRes !== null) {
            if (restRes.status === 204) {
              res.writeHead(204, { 'content-type': JSON_TYPE, 'x-github-request-id': requestId, ...(restRes.headers ?? {}) });
              res.end();
              return;
            }
            if (restRes.bodyRaw !== undefined) {
              sendBuffer(res, restRes.status, JSON_TYPE, Buffer.from(restRes.bodyRaw, 'utf8'), { 'x-github-request-id': requestId, ...(restRes.headers ?? {}) });
              return;
            }
            sendJson(res, restRes.status, restRes.body, { 'x-github-request-id': requestId, ...(restRes.headers ?? {}) });
            return;
          }
        }

        // Anything else: a JSON 404, never HTML.
        sendJson(res, 404, { message: 'Not Found' }, { 'x-github-request-id': requestId });
      } catch (err) {
        try {
          sendJson(res, 500, { message: (err as Error).message || 'Internal Server Error' }, { 'x-github-request-id': requestId });
        } catch {
          try {
            res.end();
          } catch {
            // The socket is already gone; nothing left to do.
          }
        }
      }
    })();
  });
}
