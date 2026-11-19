import { HttpError } from '../http.ts';

export const FLUSH = Buffer.from('0000');
export const DELIM = Buffer.from('0001');

export function packet(text: string | Buffer): Buffer {
  const payload = typeof text === 'string' ? Buffer.from(text) : text;
  const result = Buffer.allocUnsafe(payload.length + 4);
  result.write(result.length.toString(16).padStart(4, '0'), 0, 4, 'ascii');
  payload.copy(result, 4);
  return result;
}

export function band(data: Buffer): Buffer[] {
  const parts: Buffer[] = [];
  for (let start = 0; start < data.length; start += 65515) {
    const size = Math.min(65515, data.length - start);
    const result = Buffer.allocUnsafe(size + 5);
    result.write(result.length.toString(16).padStart(4, '0'), 0, 4, 'ascii');
    result[4] = 1;
    data.copy(result, 5, start, start + size);
    parts.push(result);
  }
  return parts;
}

export function readPackets(data: Buffer, stopAtFlush: boolean) {
  const lines: string[] = [];
  let offset = 0;
  while (offset < data.length) {
    const header = data.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new HttpError(400, 'Malformed pkt-line');
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      offset += 4;
      if (stopAtFlush) return { lines, offset };
    } else if (length === 1 || length === 2) {
      offset += 4;
    } else {
      if (length < 4 || length > 65520 || offset + length > data.length) throw new HttpError(400, 'Malformed pkt-line');
      lines.push(data.toString('utf8', offset + 4, offset + length).replace(/\n$/, ''));
      offset += length;
    }
  }
  if (stopAtFlush) throw new HttpError(400, 'Missing pkt-line flush');
  return { lines, offset };
}
