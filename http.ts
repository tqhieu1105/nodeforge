import type { IncomingMessage, ServerResponse } from 'node:http';

export class HttpError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export function json(res: ServerResponse, status: number, data?: unknown, headers?: Record<string, string>): void {
  jsonText(res, status, data === undefined ? undefined : JSON.stringify(data), headers);
}

export function jsonText(res: ServerResponse, status: number, body?: string, headers?: Record<string, string>): void {
  res.statusCode = status;
  res.setHeader('content-type', 'application/json; charset=utf-8');
  if (headers) {
    for (const name in headers) res.setHeader(name, headers[name]);
  }
  res.end(body);
}

export function bytes(req: IncomingMessage, limit: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    let first: Buffer | undefined;
    let chunks: Buffer[] | undefined;
    let length = 0;
    req.once('error', reject);
    req.once('end', () => {
      if (length > limit) reject(new HttpError(400, 'Request body is too large'));
      else if (chunks) resolve(Buffer.concat(chunks, length));
      else resolve(first ?? Buffer.alloc(0));
    });
    req.on('data', (chunk: Buffer) => {
      length += chunk.length;
      if (length > limit) return;
      if (!first) first = chunk;
      else (chunks ??= [first]).push(chunk);
    });
  });
}

export async function readJson(req: IncomingMessage): Promise<unknown> {
  const raw = await bytes(req, 16 * 1024 * 1024);
  try {
    return JSON.parse(raw.toString('utf8'));
  } catch {
    throw new HttpError(400, 'Malformed JSON');
  }
}

export function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new HttpError(400, 'Expected a JSON object');
  return value as Record<string, unknown>;
}

export function missing(): never {
  throw new HttpError(404, 'Not Found');
}
