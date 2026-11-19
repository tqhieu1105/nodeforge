import { hash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import { objectId, type GitObject, type Kind, type Objects } from './objects.ts';

const kinds: (Kind | undefined)[] = [undefined, 'commit', 'tree', 'blob', 'tag'];
const digest = (data: Buffer) => hash('sha1', data, 'buffer');
const MAX_OBJECT = 32 * 1024 * 1024;

class Reader {
  data: Buffer;
  offset: number;

  constructor(data: Buffer, offset = 0) {
    this.data = data;
    this.offset = offset;
  }

  take(length: number) {
    const end = this.offset + length;
    if (end > this.data.length) throw new Error('Truncated pack');
    const result = this.data.subarray(this.offset, end);
    this.offset = end;
    return result;
  }

  byte() {
    if (this.offset >= this.data.length) throw new Error('Truncated pack');
    return this.data[this.offset++];
  }

  size(value = 0, shift = 0, continued = true) {
    for (; continued; shift += 7) {
      if (shift > 49) throw new Error('Invalid pack size');
      const byte = this.byte();
      value += (byte & 127) * 2 ** shift;
      continued = (byte & 128) !== 0;
    }
    if (value > MAX_OBJECT) throw new Error('Pack object too large');
    return value;
  }
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  const input = new Reader(delta);
  if (input.size() !== base.length) throw new Error('Delta base size mismatch');
  const result = Buffer.allocUnsafe(input.size());
  let offset = 0;
  while (input.offset < delta.length) {
    const command = input.byte();
    if (command & 128) {
      let start = 0;
      let size = 0;
      for (let i = 0; i < 4; i++) if (command & (1 << i)) start += input.byte() * 256 ** i;
      for (let i = 0; i < 3; i++) if (command & (16 << i)) size += input.byte() * 256 ** i;
      size ||= 65536;
      if (start + size > base.length || offset + size > result.length) throw new Error('Invalid delta copy');
      base.copy(result, offset, start, start + size);
      offset += size;
    } else {
      if (!command || offset + command > result.length) throw new Error('Invalid delta insert');
      input.take(command).copy(result, offset);
      offset += command;
    }
  }
  if (offset !== result.length) throw new Error('Delta result size mismatch');
  return result;
}

export function unpack(pack: Buffer, existing: Objects): Objects {
  if (pack.length < 32 || pack.toString('ascii', 0, 4) !== 'PACK') throw new Error('Invalid pack header');
  const version = pack.readUInt32BE(4);
  const count = pack.readUInt32BE(8);
  const data = pack.subarray(0, -20);
  if ((version !== 2 && version !== 3) || count > data.length || !digest(data).equals(pack.subarray(-20))) {
    throw new Error('Invalid pack version, count or checksum');
  }

  const incoming: Objects = new Map();
  const resolved = new Map<number, GitObject>();
  const pending: { offset: number; base: string | number; delta: Buffer }[] = [];
  const input = new Reader(data, 12);
  let expanded = 0;
  function add(offset: number, kind: Kind, data: Buffer) {
    expanded += data.length;
    if (expanded > 128 * 1024 * 1024) throw new Error('Expanded pack too large');
    const object = { kind, data };
    resolved.set(offset, object);
    incoming.set(objectId(kind, data), object);
  }
  function find(base: string | number) {
    return typeof base === 'number' ? resolved.get(base) : incoming.get(base) ?? existing.get(base);
  }
  for (let i = 0; i < count; i++) {
    const offset = input.offset;
    const first = input.byte();
    const kind = (first >> 4) & 7;
    const size = input.size(first & 15, 4, (first & 128) !== 0);
    let base: string | number | undefined;
    if (kind === 6) {
      let byte = input.byte();
      let distance = byte & 127;
      while (byte & 128) {
        byte = input.byte();
        distance = (distance + 1) * 128 + (byte & 127);
        if (distance > offset) throw new Error('Invalid delta offset');
      }
      base = offset - distance;
      if (base < 12 || base >= offset) throw new Error('Invalid delta base offset');
    } else if (kind === 7) {
      base = input.take(20).toString('hex');
    } else if (!kinds[kind]) {
      throw new Error('Invalid pack object type');
    }

    const stream = inflateSync(data.subarray(input.offset), {
      info: true, chunkSize: Math.max(64, Math.min(16384, size + 1)), maxOutputLength: Math.max(1, size),
    }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    input.offset += stream.engine.bytesWritten;
    if (stream.buffer.length !== size) throw new Error('Pack object size mismatch');
    if (base === undefined) {
      add(offset, kinds[kind]!, stream.buffer);
    } else {
      const object = find(base);
      if (object) add(offset, object.kind, applyDelta(object.data, stream.buffer));
      else pending.push({ offset, base, delta: stream.buffer });
    }
  }
  if (input.offset !== data.length) throw new Error('Unexpected bytes after pack objects');
  for (let waiting = pending; waiting.length;) {
    const next: typeof pending = [];
    for (const entry of waiting) {
      const base = find(entry.base);
      if (base) add(entry.offset, base.kind, applyDelta(base.data, entry.delta));
      else next.push(entry);
    }
    if (next.length === waiting.length) throw new Error('Missing pack delta base');
    waiting = next;
  }
  return incoming;
}

function entryHeader(kind: Kind, length: number): Buffer {
  const bytes = [(kinds.indexOf(kind) << 4) | (length & 15)];
  for (length = Math.floor(length / 16); length > 0; length = Math.floor(length / 128)) {
    bytes[bytes.length - 1] |= 128;
    bytes.push(length & 127);
  }
  return Buffer.from(bytes);
}

export function packObjects(objects: readonly GitObject[]): Buffer {
  const header = Buffer.alloc(12);
  header.write('PACK');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  const parts = [header];
  for (const { kind, data } of objects) {
    parts.push(entryHeader(kind, data.length), deflateSync(data, { level: 1, chunkSize: 4096 }));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, digest(body)]);
}
