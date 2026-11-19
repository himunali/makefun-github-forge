// Git object model: SHA-1 ids, pkt-line framing, packfile read/write and the
// graph walks (reachability, ancestry, merge-base, tree flattening) that the
// smart-HTTP handlers and the REST contents/pull-request code share.
// Standard library only: node:crypto for SHA-1, node:zlib for pack streams.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0'.repeat(40);

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: GitObjectType;
  data: Buffer;
}

export type ObjectLookup = (oid: string) => GitObject | undefined;

const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** The SHA-1 object id of `type` + `data`, git's `<type> <size>\0<data>` hash. */
export function hashObject(type: GitObjectType, data: Uint8Array): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return createHash('sha1').update(head).update(data).digest('hex');
}

export function isObjectId(value: string): boolean {
  return /^[0-9a-f]{40}$/.test(value);
}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

/** One pkt-line: a 4-hex-digit length (including the 4 digits) + payload. */
export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, body]);
}

export function pktFlush(): Buffer {
  return Buffer.from('0000', 'ascii');
}

export function pktDelim(): Buffer {
  return Buffer.from('0001', 'ascii');
}

export interface DecodedPacket {
  kind: 'data' | 'flush' | 'delim';
  payload: Buffer;
}

/**
 * Decode pkt-lines from the front of `buf`. Returns the packets and the offset
 * where the pkt-line stream stopped (the byte after the terminating flush, or
 * the first byte that is not a pkt-line — e.g. a raw packfile). Throws on a
 * malformed length so callers can answer 400 instead of crashing.
 */
export function decodePktLines(buf: Buffer, maxPackets = 100000): { packets: DecodedPacket[]; offset: number } {
  const packets: DecodedPacket[] = [];
  let offset = 0;
  while (offset + 4 <= buf.length) {
    // Manual hex check: the regex /^[0-9a-fA-F]{4}$/ on every packet was
    // measurable in push/fetch paths that decode many small packets.
    const h0 = buf[offset] as number;
    const h1 = buf[offset + 1] as number;
    const h2 = buf[offset + 2] as number;
    const h3 = buf[offset + 3] as number;
    const isHex = (c: number): boolean =>
      (c >= 0x30 && c <= 0x39) || (c >= 0x61 && c <= 0x66) || (c >= 0x41 && c <= 0x46);
    if (!isHex(h0) || !isHex(h1) || !isHex(h2) || !isHex(h3)) {
      throw new Error(`bad pkt-line length ${JSON.stringify(buf.subarray(offset, offset + 4).toString('ascii'))} at offset ${offset}`);
    }
    const len = parseInt(buf.subarray(offset, offset + 4).toString('ascii'), 16);
    if (len === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      return { packets, offset: offset + 4 };
    }
    if (len === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len < 4 || offset + len > buf.length) {
      throw new Error(`pkt-line length ${len} overruns the body at offset ${offset}`);
    }
    packets.push({ kind: 'data', payload: Buffer.from(buf.subarray(offset + 4, offset + len)) });
    offset += len;
    if (packets.length >= maxPackets) throw new Error('too many pkt-lines');
  }
  return { packets, offset };
}

/** Encode a list of text lines (each ending in \n) plus a trailing flush. */
export function encodePktLines(lines: readonly (string | 'flush' | 'delim')[]): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    if (line === 'flush') parts.push(pktFlush());
    else if (line === 'delim') parts.push(pktDelim());
    else parts.push(pktLine(line));
  }
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// trees and commits
// ---------------------------------------------------------------------------

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

/** Parse a tree object's entries. */
export function parseTree(data: Uint8Array): TreeEntry[] {
  const buf = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < buf.length) {
    const space = buf.indexOf(0x20, offset);
    if (space < 0) throw new Error('tree entry has no mode');
    const mode = buf.subarray(offset, space).toString('ascii');
    const nul = buf.indexOf(0, space + 1);
    if (nul < 0 || nul + 21 > buf.length) throw new Error('tree entry has no name/oid');
    const name = buf.subarray(space + 1, nul).toString('utf8');
    const oid = buf.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

/** Git's tree sort: entries ordered by name, directories compared as name + '/'. */
export function sortTreeEntries(entries: readonly TreeEntry[]): TreeEntry[] {
  const key = (e: TreeEntry): string => (e.mode === '40000' ? `${e.name}/` : e.name);
  return [...entries].sort((a, b) => (key(a) < key(b) ? -1 : key(a) > key(b) ? 1 : 0));
}

/** Build a tree object's bytes from entries (sorted git's way). */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const parts: Buffer[] = [];
  for (const entry of sortTreeEntries(entries)) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  authorTs: number;
  committer: string;
  committerTs: number;
  message: string;
}

/** Parse a commit object's headers and message. */
export function parseCommit(data: Uint8Array): CommitView {
  const text = (Buffer.isBuffer(data) ? data : Buffer.from(data)).toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let authorTs = 0;
  let committer = '';
  let committerTs = 0;
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      const m = /^(.*) (\d+) [+-]\d{4}$/.exec(line.slice(7));
      author = m ? m[1] : line.slice(7);
      authorTs = m ? Number(m[2]) : 0;
    } else if (line.startsWith('committer ')) {
      const m = /^(.*) (\d+) [+-]\d{4}$/.exec(line.slice(10));
      committer = m ? m[1] : line.slice(10);
      committerTs = m ? Number(m[2]) : 0;
    }
  }
  return { tree, parents, author, authorTs, committer, committerTs, message };
}

/** Build a commit object's bytes. `message` is stored with a trailing newline. */
export function buildCommit(fields: {
  tree: string;
  parents: readonly string[];
  author: string;
  committer: string;
  timestamp: number;
  message: string;
}): Buffer {
  const lines: string[] = [`tree ${fields.tree}`];
  for (const parent of fields.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${fields.author} ${fields.timestamp} +0000`);
  lines.push(`committer ${fields.committer} ${fields.timestamp} +0000`);
  const msg = fields.message.endsWith('\n') ? fields.message : `${fields.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n\n${msg}`, 'utf8');
}

export interface TagView {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  taggerTs: number;
  message: string;
}

/** Parse an annotated tag object's headers and message. */
export function parseTag(data: Uint8Array): TagView {
  const text = (Buffer.isBuffer(data) ? data : Buffer.from(data)).toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  let taggerTs = 0;
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) {
      const m = /^(.*) (\d+) [+-]\d{4}$/.exec(line.slice(7));
      tagger = m ? m[1] : line.slice(7);
      taggerTs = m ? Number(m[2]) : 0;
    }
  }
  return { object, type, tag, tagger, taggerTs, message };
}

/** Build an annotated tag object's bytes. */
export function buildTag(fields: {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  timestamp: number;
  message: string;
}): Buffer {
  const msg = fields.message.endsWith('\n') ? fields.message : `${fields.message}\n`;
  return Buffer.from(
    `object ${fields.object}\ntype ${fields.type}\ntag ${fields.tag}\ntagger ${fields.tagger} ${fields.timestamp} +0000\n\n${msg}`,
    'utf8',
  );
}

// ---------------------------------------------------------------------------
// deltas and packfiles
// ---------------------------------------------------------------------------

function readVarintSize(buf: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buf[start];
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while (byte & 0x80) {
    if (offset >= buf.length) throw new Error('pack entry size varint is truncated');
    byte = buf[offset++];
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
    if (shift > 63) throw new Error('pack entry size varint is too long');
  }
  return { size, next: offset, first };
}

function readOfsDeltaBase(buf: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buf[start];
  let offset = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    if (offset >= buf.length) throw new Error('ofs-delta base offset is truncated');
    byte = buf[offset++];
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { baseOffset: value, next: offset };
}

/** Inflate one zlib stream starting at `offset`; returns its bytes and the offset just past it. */
export function inflateAt(buf: Buffer, offset: number): { data: Buffer; next: number } {
  const view = buf.subarray(offset);
  let result: { buffer: Buffer; engine: { bytesWritten: number } };
  try {
    result = inflateSync(view, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  } catch (err) {
    throw new Error(`zlib stream at offset ${offset} did not inflate: ${(err as Error).message}`);
  }
  const written = result.engine.bytesWritten;
  if (!written || written <= 0) throw new Error(`zlib stream at offset ${offset} consumed no bytes`);
  // result.buffer already is the inflated Buffer; copying it again was pure waste.
  return { data: result.buffer, next: offset + written };
}

/** Apply a git delta instruction stream to a base object's bytes. */
export function applyDelta(base: Uint8Array, delta: Uint8Array): Buffer {
  const baseBuf = Buffer.isBuffer(base) ? base : Buffer.from(base);
  const deltaBuf = Buffer.isBuffer(delta) ? delta : Buffer.from(delta);
  let offset = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      if (offset >= deltaBuf.length) throw new Error('delta size varint is truncated');
      const byte = deltaBuf[offset++];
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
      if (shift > 63) throw new Error('delta size varint is too long');
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== baseBuf.length) throw new Error(`delta expects a ${sourceSize}-byte base, got ${baseBuf.length}`);
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (offset < deltaBuf.length) {
    const op = deltaBuf[offset++];
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) {
          if (offset >= deltaBuf.length) throw new Error('delta copy offset is truncated');
          copyOffset += deltaBuf[offset++] * 2 ** shift;
        }
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) {
          if (offset >= deltaBuf.length) throw new Error('delta copy size is truncated');
          copySize += deltaBuf[offset++] * 2 ** shift;
        }
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      if (copyOffset + copySize > baseBuf.length) throw new Error('delta copy is out of the base');
      if (written + copySize > targetSize) throw new Error('delta writes past its target size');
      baseBuf.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      if (offset + literal > deltaBuf.length) throw new Error('delta insert is truncated');
      if (written + literal > targetSize) throw new Error('delta writes past its target size');
      deltaBuf.copy(out, written, offset, offset + literal);
      offset += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0 is invalid');
    }
  }
  if (written !== targetSize) throw new Error(`delta produced ${written} of ${targetSize} bytes`);
  return out;
}

interface RawEntry {
  offset: number;
  typeCode: number;
  size: number;
  data: Buffer;
  baseOffset?: number;
  baseOid?: string;
  resolvedType?: GitObjectType;
  resolved?: Buffer;
  oid?: string;
}

/**
 * Read a packfile (version 2 or 3) and resolve every object, including
 * OFS_DELTA and REF_DELTA (whose base may live in the pack or in `resolveBase`,
 * for thin packs). Throws on a bad header, a bad trailer checksum, a truncated
 * entry or an unresolvable delta.
 */
export function readPack(pack: Uint8Array, resolveBase?: ObjectLookup): Map<string, GitObject> {
  const buf = Buffer.isBuffer(pack) ? pack : Buffer.from(pack);
  if (buf.length < 32) throw new Error(`pack is ${buf.length} bytes, too short`);
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack does not start with PACK');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const declaredCount = buf.readUInt32BE(8);
  const trailerStart = buf.length - 20;
  const trailer = buf.subarray(trailerStart).toString('hex');
  const computed = createHash('sha1').update(buf.subarray(0, trailerStart)).digest('hex');
  if (trailer !== computed) throw new Error('pack trailer checksum does not match its bytes');

  const raw: RawEntry[] = [];
  const byOffset = new Map<number, RawEntry>();
  let offset = 12;
  for (let index = 0; index < declaredCount; index += 1) {
    const entryOffset = offset;
    const header = readVarintSize(buf, offset);
    offset = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: RawEntry = { offset: entryOffset, typeCode, size: header.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsDeltaBase(buf, offset);
      offset = base.next;
      if (base.baseOffset <= 0 || base.baseOffset > entryOffset) {
        throw new Error(`ofs-delta at ${entryOffset} names an invalid base offset ${base.baseOffset}`);
      }
      entry.baseOffset = entryOffset - base.baseOffset;
    } else if (typeCode === 7) {
      if (offset + 20 > trailerStart) throw new Error('ref-delta base id is truncated');
      entry.baseOid = buf.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (!CODE_TYPE[typeCode]) {
      throw new Error(`pack entry ${index} has unknown type code ${typeCode}`);
    }
    const inflated = inflateAt(buf.subarray(0, trailerStart), offset);
    if (inflated.data.length !== header.size) {
      throw new Error(`pack entry at ${entryOffset} declares ${header.size} bytes but inflated to ${inflated.data.length}`);
    }
    entry.data = inflated.data;
    offset = inflated.next;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  if (offset !== trailerStart) throw new Error(`pack has ${trailerStart - offset} stray bytes before its trailer`);

  const objects = new Map<string, GitObject>();
  const resolvedByOid = new Map<string, RawEntry>();
  const resolving = new Set<number>();
  const resolve = (entry: RawEntry): { type: GitObjectType; data: Buffer; oid: string } => {
    if (entry.resolved && entry.resolvedType && entry.oid) {
      return { type: entry.resolvedType, data: entry.resolved, oid: entry.oid };
    }
    if (resolving.has(entry.offset)) throw new Error(`delta cycle at pack offset ${entry.offset}`);
    resolving.add(entry.offset);
    let type: GitObjectType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset as number);
      if (!baseEntry) throw new Error(`ofs-delta base at offset ${entry.baseOffset} is not in the pack`);
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const wanted = entry.baseOid as string;
      const baseEntry = resolvedByOid.get(wanted);
      if (baseEntry) {
        const base = resolve(baseEntry);
        type = base.type;
        data = applyDelta(base.data, entry.data);
      } else {
        const external = resolveBase ? resolveBase(wanted) : undefined;
        if (!external) throw new Error(`ref-delta base ${wanted} is neither in the pack nor on the server`);
        type = external.type;
        data = applyDelta(external.data, entry.data);
      }
    } else {
      type = CODE_TYPE[entry.typeCode] as GitObjectType;
      data = entry.data;
    }
    const oid = hashObject(type, data);
    entry.resolved = data;
    entry.resolvedType = type;
    entry.oid = oid;
    resolvedByOid.set(oid, entry);
    resolving.delete(entry.offset);
    return { type, data, oid };
  };
  for (const entry of raw) {
    const resolved = resolve(entry);
    if (!objects.has(resolved.oid)) objects.set(resolved.oid, { type: resolved.type, data: resolved.data });
  }
  return objects;
}

function writePackObjectHeader(type: GitObjectType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a packfile from plain objects (no deltas), in the caller's order.
 *  Deflate level 1: pack bytes travel over loopback to the suite, so ratio
 *  barely matters and level 1 is several times faster than the default 6.
 *  The stream stays a valid zlib stream any git client inflates. */
export function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    parts.push(writePackObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data, { level: 1 }));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

// ---------------------------------------------------------------------------
// graph walks
// ---------------------------------------------------------------------------

/** Peel a tag (possibly chained) to the object it ultimately names. */
export function peelObject(lookup: ObjectLookup, oid: string): { type: GitObjectType; oid: string } {
  let current = oid;
  for (let hops = 0; hops < 16; hops += 1) {
    const obj = lookup(current);
    if (!obj || obj.type !== 'tag') return { type: obj ? obj.type : 'commit', oid: current };
    current = parseTag(obj.data).object;
  }
  return { type: 'commit', oid: current };
}

export interface ClosureResult {
  /** Every object id in the closure, including the tips themselves. */
  objects: Set<string>;
  /** Commit distance from the nearest tip (0 for the tips). */
  commitDist: Map<string, number>;
  /** Commits in the closure whose parents were cut off (shallow boundary). */
  boundary: string[];
}

/**
 * The closure of `tips`: every object reachable from them. `depthLimit` caps
 * commit generations (1 = only the tip commits themselves). `stopAt` names
 * commits whose parents are not traversed (a client's existing shallow
 * boundaries). Tags are peeled; their target is walked.
 */
export function collectClosure(
  lookup: ObjectLookup,
  tips: readonly string[],
  options: { depthLimit?: number; stopAt?: ReadonlySet<string> } = {},
): ClosureResult {
  const objects = new Set<string>();
  const commitDist = new Map<string, number>();
  const queue: Array<{ oid: string; dist: number }> = [];
  const seenCommit = new Set<string>();
  for (const tip of tips) {
    const peeled = peelObject(lookup, tip);
    const obj = lookup(tip);
    if (obj && obj.type === 'tag') objects.add(tip);
    if (peeled.type === 'commit') {
      if (!seenCommit.has(peeled.oid)) {
        seenCommit.add(peeled.oid);
        commitDist.set(peeled.oid, 0);
        queue.push({ oid: peeled.oid, dist: 0 });
      }
    } else {
      const o = lookup(peeled.oid);
      if (o) objects.add(peeled.oid);
    }
  }
  const boundary: string[] = [];
  // Index-based queue: Array.shift() is O(n) per pop and these walks run
  // once per fetch/clone, so a shifting queue is quadratic in history size.
  let head = 0;
  while (head < queue.length) {
    const item = queue[head++] as { oid: string; dist: number };
    const commitObj = lookup(item.oid);
    if (!commitObj || commitObj.type !== 'commit') continue;
    objects.add(item.oid);
    const view = parseCommit(commitObj.data);
    const treeObj = lookup(view.tree);
    if (treeObj) {
      objects.add(view.tree);
      const walkTree = (treeOid: string): void => {
        const t = lookup(treeOid);
        if (!t || t.type !== 'tree') return;
        for (const entry of parseTree(t.data)) {
          objects.add(entry.oid);
          if (entry.mode === '40000') walkTree(entry.oid);
        }
      };
      walkTree(view.tree);
    }
    const limited = options.depthLimit !== undefined && item.dist + 1 >= options.depthLimit;
    const stopped = options.stopAt !== undefined && options.stopAt.has(item.oid);
    if (limited || stopped) {
      if (view.parents.length > 0) boundary.push(item.oid);
      continue;
    }
    for (const parent of view.parents) {
      if (!seenCommit.has(parent)) {
        seenCommit.add(parent);
        commitDist.set(parent, item.dist + 1);
        queue.push({ oid: parent, dist: item.dist + 1 });
      }
    }
  }
  return { objects, commitDist, boundary };
}

/** True when `ancestor` is `descendant` itself or one of its ancestors. */
export function isAncestor(lookup: ObjectLookup, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const queue: string[] = [descendant];
  let head = 0;
  while (head < queue.length) {
    const oid = queue[head++] as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = lookup(oid);
    if (!obj) continue;
    if (obj.type === 'tag') {
      queue.push(parseTag(obj.data).object);
      continue;
    }
    if (obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (parent === ancestor) return true;
      queue.push(parent);
    }
  }
  return false;
}

/** The best common ancestor of two commits (first found walking from `b`). */
export function mergeBase(lookup: ObjectLookup, a: string, b: string): string | null {
  const ancestorsA = new Set<string>();
  const queueA: string[] = [peelObject(lookup, a).oid];
  let headA = 0;
  while (headA < queueA.length) {
    const oid = queueA[headA++] as string;
    if (ancestorsA.has(oid)) continue;
    ancestorsA.add(oid);
    const obj = lookup(oid);
    if (obj && obj.type === 'commit') queueA.push(...parseCommit(obj.data).parents);
  }
  const seen = new Set<string>();
  const queueB: string[] = [peelObject(lookup, b).oid];
  let headB = 0;
  while (headB < queueB.length) {
    const oid = queueB[headB++] as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (ancestorsA.has(oid)) return oid;
    const obj = lookup(oid);
    if (obj && obj.type === 'commit') queueB.push(...parseCommit(obj.data).parents);
  }
  return null;
}

/** Flatten a tree into path -> {oid, mode}, walking subdirectories. */
export function flattenTree(lookup: ObjectLookup, treeOid: string, prefix = ''): Map<string, { oid: string; mode: string }> {
  const out = new Map<string, { oid: string; mode: string }>();
  const walk = (oid: string, dir: string): void => {
    const obj = lookup(oid);
    if (!obj || obj.type !== 'tree') return;
    for (const entry of parseTree(obj.data)) {
      const path = dir === '' ? entry.name : `${dir}/${entry.name}`;
      if (entry.mode === '40000') walk(entry.oid, path);
      else out.set(path, { oid: entry.oid, mode: entry.mode });
    }
  };
  walk(treeOid, prefix);
  return out;
}

/** List every commit reachable from `tip`, newest-first by discovery order. */
export function listCommits(lookup: ObjectLookup, tip: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const queue: string[] = [peelObject(lookup, tip).oid];
  let head = 0;
  while (head < queue.length) {
    const oid = queue[head++] as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    out.push(oid);
    queue.push(...parseCommit(obj.data).parents);
  }
  return out;
}
