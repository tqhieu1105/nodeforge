import type { IncomingMessage, ServerResponse } from 'node:http';

export class HttpError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export function json(res: ServerResponse, status: number, data?: unknown, headers?: Record<string, string>): void {
  const body = data === undefined ? undefined : JSON.stringify(data);
  res.setHeader('content-type', 'application/json; charset=utf-8');
  if (body !== undefined) res.setHeader('content-length', Buffer.byteLength(body));
  res.writeHead(status, headers);
  res.end(body);
}

export async function bytes(req: IncomingMessage, limit: number): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let length = 0;
  for await (const chunk of req) {
    length += chunk.length;
    if (length <= limit) chunks.push(chunk);
  }
  if (length > limit) throw new HttpError(400, 'Request body is too large');
  return chunks.length === 1 ? chunks[0] : Buffer.concat(chunks, length);
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
