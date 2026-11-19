// Git smart HTTP: info/refs advertisements, protocol v2 upload-pack
// (ls-refs + fetch, with shallow/deepen), protocol v0 upload-pack fallback,
// and protocol v0 receive-pack (push) with per-ref fast-forward checks and
// a sideband-framed report-status answer.

import {
  collectClosure,
  decodePktLines,
  encodePktLines,
  hashObject,
  isAncestor,
  isObjectId,
  parseCommit,
  parseTag,
  parseTree,
  peelObject,
  pktDelim,
  pktFlush,
  pktLine,
  readPack,
  writePack,
  ZERO_OID,
  type GitObject,
} from './gitobj.ts';
import type { Repo, Store } from './store.ts';

export interface GitRequest {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: Record<string, string>;
  body: Buffer;
}

export interface GitResponse {
  status: number;
  contentType: string;
  body: Buffer;
  extraHeaders?: Record<string, string>;
}

const UPLOAD_AD = 'application/x-git-upload-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_AD = 'application/x-git-receive-pack-advertisement';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

/** Sorted ref names of a repository, with their tip oids. */
function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  return [...repo.refs.entries()]
    .map(([name, oid]) => ({ name, oid }))
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

/** The commit an annotated tag peels to (itself when it is not a tag). */
function peelToCommit(repo: Repo, oid: string): string | null {
  const peeled = peelObject((id) => repo.objects.get(id), oid);
  return peeled.type === 'commit' ? peeled.oid : null;
}

/** The default branch's tip (or the zero oid when the repository is unborn). */
function headOid(repo: Repo): string {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID;
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

function v2Advertisement(): Buffer {
  return V2_ADVERTISEMENT;
}

/** The v2 advertisement never varies: prebuilt once, shared by every call. */
const V2_ADVERTISEMENT: Buffer = encodePktLines([
  'version 2\n',
  'agent=makefun-forge/1.0\n',
  'ls-refs=unborn\n',
  'fetch=shallow\n',
  'object-format=sha1\n',
  'flush',
]);

/**
 * One pkt-line frame for a text line, as a string: the 4-hex length prefix
 * plus the payload. String framing lets advertisement / ls-refs answers be
 * built as a single string and turned into one Buffer, instead of one
 * Buffer.alloc + Buffer.concat per line.
 */
function pktLineStr(payload: string): string {
  const len = Buffer.byteLength(payload, 'utf8') + 4;
  return len.toString(16).padStart(4, '0') + payload;
}

function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps =
    service === 'git-upload-pack'
      ? 'side-band-64k thin-pack ofs-delta shallow multi_ack multi_ack_detailed symref=HEAD:refs/heads/main agent=makefun-forge/1.0'
      : 'report-status side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0';
  let out = pktLineStr(`# service=${service}\n`) + '0000';
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    out += pktLineStr(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    const head = headOid(repo);
    out += pktLineStr(`${head} HEAD\0${caps}\n`);
    for (const ref of refs) {
      out += pktLineStr(`${ref.oid} ${ref.name}\n`);
      const obj = repo.objects.get(ref.oid);
      if (obj && obj.type === 'tag') {
        const peeled = peelToCommit(repo, ref.oid);
        if (peeled) out += pktLineStr(`${peeled} ${ref.name}^{}\n`);
      }
    }
  }
  out += '0000';
  return Buffer.from(out, 'utf8');
}

function infoRefs(store: Store, repo: Repo, service: 'git-upload-pack' | 'git-receive-pack', protocol: string | null): GitResponse {
  const isV2 = service === 'git-upload-pack' && protocol === 'version=2';
  if (isV2) {
    return { status: 200, contentType: UPLOAD_AD, body: v2Advertisement() };
  }
  return { status: 200, contentType: service === 'git-upload-pack' ? UPLOAD_AD : RECEIVE_AD, body: v0Advertisement(repo, service) };
}

// ---------------------------------------------------------------------------
// v2 ls-refs
// ---------------------------------------------------------------------------

function lsRefsV2(repo: Repo, args: { peel: boolean; symrefs: boolean; unborn: boolean; prefixes: string[] }): Buffer {
  let out = '';
  const head = headOid(repo);
  const keep = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((p) => name.startsWith(p));
  // HEAD is a ref like any other for prefix filtering: a `ref-prefix refs/heads/`
  // request must not see it, and an unmatched prefix must see nothing at all.
  if (keep('HEAD')) {
    if (head === ZERO_OID) {
      if (args.unborn) out += pktLineStr('unborn HEAD symref-target:refs/heads/main\n');
    } else {
      out += pktLineStr(`${head} HEAD symref-target:refs/heads/main\n`);
    }
  }
  for (const ref of sortedRefs(repo)) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    const obj = repo.objects.get(ref.oid);
    if (args.peel && obj && obj.type === 'tag') {
      const peeled = peelToCommit(repo, ref.oid);
      if (peeled) line += ` peeled:${peeled}`;
    }
    out += pktLineStr(`${line}\n`);
  }
  out += '0000';
  return Buffer.from(out, 'utf8');
}

// ---------------------------------------------------------------------------
// fetch (v2 and v0 share the object selection)
// ---------------------------------------------------------------------------

interface FetchPlan {
  wants: string[];
  haves: string[];
  shallowBoundaries: string[];
  depth: number | null;
}

interface FetchSelection {
  /** Objects to send, sorted by oid for a deterministic pack. */
  send: GitObject[];
  sendOids: Set<string>;
  shallow: string[];
  unshallow: string[];
  acks: string[];
  hasCommon: boolean;
}

/** Choose the objects a fetch sends: wants minus what the client already has. */
function selectFetch(repo: Repo, plan: FetchPlan): FetchSelection {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const stopAt = new Set(plan.shallowBoundaries);
  // The want walk is bounded by `depth` alone: the client's shallow boundaries
  // are commits it holds without their parents, not places to stop walking
  // from a want — stopping there would make a `deepen` fetch send nothing
  // (the want itself is the boundary). The boundaries only limit the *have*
  // walk: a have the client holds shallowly does not imply its ancestors.
  const wantClosure = collectClosure(lookup, plan.wants, { depthLimit: plan.depth ?? undefined });
  const haveClosure = plan.haves.length
    ? collectClosure(lookup, plan.haves, { stopAt })
    : { objects: new Set<string>(), commitDist: new Map<string, number>(), boundary: [] as string[] };
  const sendOids = new Set<string>();
  for (const oid of wantClosure.objects) {
    if (!haveClosure.objects.has(oid)) sendOids.add(oid);
  }
  const send: GitObject[] = [];
  for (const oid of [...sendOids].sort()) {
    const obj = repo.objects.get(oid);
    if (obj) send.push({ type: obj.type, data: obj.data });
  }
  // Shallow boundary: an included commit whose parents were cut off.
  const shallow: string[] = [];
  if (plan.depth !== null || plan.shallowBoundaries.length > 0) {
    for (const oid of wantClosure.boundary) {
      if (sendOids.has(oid) || haveClosure.objects.has(oid)) shallow.push(oid);
    }
  }
  const unshallow: string[] = [];
  for (const old of plan.shallowBoundaries) {
    if (wantClosure.objects.has(old) && !shallow.includes(old)) unshallow.push(old);
  }
  // Acknowledgments: a have that is an ancestor-or-equal of a want.
  const acks: string[] = [];
  for (const have of plan.haves) {
    const peeled = peelObject(lookup, have);
    let common = false;
    for (const want of plan.wants) {
      const wantPeeled = peelObject(lookup, want);
      if (peeled.oid === wantPeeled.oid || isAncestor(lookup, peeled.oid, wantPeeled.oid)) {
        common = true;
        break;
      }
    }
    if (common) acks.push(have);
  }
  return { send, sendOids, shallow, unshallow, acks, hasCommon: acks.length > 0 };
}

/** Largest pack bytes in one sideband packet: a pkt-line is at most 65520 bytes
 *  (4 length bytes + payload), and the payload carries one band byte, so the
 *  pack chunk is 65520 - 4 - 1. A 65516-byte chunk made a 65521-byte pkt-line,
 *  which a strict decoder rejects. */
const SIDEBAND_MAX_CHUNK = 65515;

/**
 * Memoized packs. writePack is a pure function of the (type, data) list, and
 * objects are content-addressed and immutable, so one sorted oid set always
 * builds the same pack bytes. A clone run fetches the same tip fifty times;
 * building that pack once and reusing its bytes is what real forges do with
 * pack reuse (git's pack.allowPackReuse). Bounded so a long trial cannot
 * grow it without limit; the cached Buffer is only ever read via subarray.
 */
const PACK_CACHE_MAX = 128;
const packCache = new Map<string, Buffer>();
function packFor(sendOids: Set<string>, send: GitObject[]): Buffer {
  const key = [...sendOids].sort().join(',');
  const hit = packCache.get(key);
  if (hit) return hit;
  const pack = writePack(send);
  if (packCache.size >= PACK_CACHE_MAX) {
    const oldest = packCache.keys().next().value;
    if (oldest !== undefined) packCache.delete(oldest);
  }
  packCache.set(key, pack);
  return pack;
}

function sidebandPackets(pack: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const max = SIDEBAND_MAX_CHUNK;
  for (let offset = 0; offset < pack.length; offset += max) {
    const chunkLen = Math.min(pack.length - offset, max);
    // One allocation for the whole sideband packet (4 length bytes + 1 band
    // byte + chunk) instead of Buffer.concat([band, chunk]) then pktLine's
    // own concat: two fewer allocations per packet.
    const total = chunkLen + 5;
    const packet = Buffer.allocUnsafe(total);
    packet.write(total.toString(16).padStart(4, '0'), 0, 4, 'ascii');
    packet[4] = 1;
    pack.copy(packet, 5, offset, offset + chunkLen);
    out.push(packet);
  }
  return out;
}

function fetchV2(repo: Repo, plan: FetchPlan): Buffer {
  const sel = selectFetch(repo, plan);
  const parts: Buffer[] = [];
  // acknowledgments: only when the client sent haves. A fresh clone sends
  // none, and real git errors if the section appears anyway (it expects the
  // packfile header next). With haves, ACK each common have + 'ready', else NAK.
  if (plan.haves.length > 0) {
    parts.push(pktLine('acknowledgments\n'));
    if (sel.hasCommon) {
      for (const ack of sel.acks) parts.push(pktLine(`ACK ${ack}\n`));
      parts.push(pktLine('ready\n'));
    } else {
      parts.push(pktLine('NAK\n'));
    }
    parts.push(pktDelim());
  }
  // shallow-info
  if (sel.shallow.length > 0 || sel.unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of sel.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of sel.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(pktDelim());
  }
  // packfile
  if (sel.send.length > 0) {
    parts.push(pktLine('packfile\n'));
    const pack = packFor(sel.sendOids, sel.send);
    for (const packet of sidebandPackets(pack)) parts.push(packet);
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

function fetchV0(repo: Repo, plan: FetchPlan): Buffer {
  const sel = selectFetch(repo, plan);
  const parts: Buffer[] = [];
  if (sel.hasCommon) {
    for (const ack of sel.acks) parts.push(pktLine(`ACK ${ack}\n`));
  } else {
    parts.push(pktLine('NAK\n'));
  }
  for (const oid of sel.shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of sel.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  if (sel.send.length > 0) {
    const pack = packFor(sel.sendOids, sel.send);
    for (const packet of sidebandPackets(pack)) parts.push(packet);
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// upload-pack POST
// ---------------------------------------------------------------------------

function parseV2Request(body: Buffer): { command: string; args: Map<string, string[]> } {
  const { packets, offset } = decodePktLines(body);
  void offset;
  let command = '';
  const args = new Map<string, string[]>();
  let afterDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') break;
    if (packet.kind === 'delim') {
      afterDelim = true;
      continue;
    }
    const line = packet.payload.toString('utf8');
    if (!afterDelim) {
      const m = /^command=(\S+)\n?$/.exec(line);
      if (m) command = m[1] as string;
      continue;
    }
    const trimmed = line.replace(/\n$/, '');
    const space = trimmed.indexOf(' ');
    const key = space < 0 ? trimmed : trimmed.slice(0, space);
    const value = space < 0 ? '' : trimmed.slice(space + 1);
    const list = args.get(key) ?? [];
    list.push(value);
    args.set(key, list);
  }
  return { command, args };
}

function handleUploadPack(repo: Repo, req: GitRequest): GitResponse {
  const protocol = req.headers['git-protocol'] ?? null;
  if (protocol === 'version=2') {
    let parsed: { command: string; args: Map<string, string[]> };
    try {
      parsed = parseV2Request(req.body);
    } catch {
      return { status: 400, contentType: 'text/plain; charset=utf-8', body: Buffer.from('bad pkt-line\n') };
    }
    if (parsed.command === 'ls-refs') {
      const args = parsed.args;
      const body = lsRefsV2(repo, {
        peel: args.has('peel'),
        symrefs: args.has('symrefs'),
        unborn: args.has('unborn'),
        prefixes: args.get('ref-prefix') ?? [],
      });
      return { status: 200, contentType: UPLOAD_RESULT, body };
    }
    if (parsed.command === 'fetch') {
      const args = parsed.args;
      const deepenRaw = args.get('deepen')?.[0];
      const plan: FetchPlan = {
        wants: args.get('want') ?? [],
        haves: args.get('have') ?? [],
        shallowBoundaries: args.get('shallow') ?? [],
        depth: deepenRaw !== undefined ? Number(deepenRaw) : null,
      };
      return { status: 200, contentType: UPLOAD_RESULT, body: fetchV2(repo, plan) };
    }
    return { status: 400, contentType: 'text/plain; charset=utf-8', body: Buffer.from('unknown command\n') };
  }
  // Protocol v0 fetch: wants, then flush, then haves + done + flush. The
  // request has two pkt-line sections, so decode past the first flush to
  // reach the negotiation section; decoding only the first section would
  // silently drop every `have` and re-send the whole history.
  let packets: DecodedPacket[] = [];
  try {
    let offset = 0;
    while (offset < req.body.length) {
      const decoded = decodePktLines(req.body.subarray(offset));
      packets.push(...decoded.packets);
      if (decoded.offset <= 0) break;
      offset += decoded.offset;
    }
  } catch {
    return { status: 400, contentType: 'text/plain; charset=utf-8', body: Buffer.from('bad pkt-line\n') };
  }
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowBoundaries: string[] = [];
  let depth: number | null = null;
  let section = 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      section += 1;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) wants.push(line.slice(5).split(' ')[0] as string);
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) shallowBoundaries.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
  }
  void section;
  return {
    status: 200,
    contentType: UPLOAD_RESULT,
    body: fetchV0(repo, { wants, haves, shallowBoundaries, depth }),
  };
}

// ---------------------------------------------------------------------------
// receive-pack POST
// ---------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function handleReceivePack(repo: Repo, req: GitRequest): GitResponse {
  let commands: PushCommand[] = [];
  let packStart = 0;
  try {
    const decoded = decodePktLines(req.body);
    packStart = decoded.offset;
    for (const packet of decoded.packets) {
      if (packet.kind !== 'data') continue;
      const line = packet.payload.toString('utf8');
      const nul = line.indexOf('\0');
      const clean = (nul >= 0 ? line.slice(0, nul) : line).replace(/\n$/, '');
      const parts = clean.split(' ');
      if (parts.length >= 3) {
        commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts[2] as string });
      }
    }
  } catch {
    return { status: 400, contentType: 'text/plain; charset=utf-8', body: Buffer.from('bad pkt-line\n') };
  }
  if (commands.length === 0) {
    return { status: 400, contentType: 'text/plain; charset=utf-8', body: Buffer.from('no commands\n') };
  }
  const hasUpdates = commands.some((c) => c.newOid !== ZERO_OID);
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (hasUpdates && packStart < req.body.length) {
    try {
      const objects = readPack(req.body.subarray(packStart), (oid) => repo.objects.get(oid));
      for (const [oid, obj] of objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
      }
    } catch (err) {
      unpackOk = false;
      unpackDetail = (err as Error).message;
    }
  } else if (hasUpdates && packStart >= req.body.length) {
    unpackOk = false;
    unpackDetail = 'unpacker error: no packfile';
  }
  const reportLines: string[] = [];
  if (!unpackOk) {
    reportLines.push(`unpack ${unpackDetail}\n`);
    for (const cmd of commands) reportLines.push(`ng ${cmd.ref} unpack failed\n`);
  } else {
    reportLines.push('unpack ok\n');
    for (const cmd of commands) {
      const result = applyPushCommand(repo, cmd);
      if (result.ok) reportLines.push(`ok ${cmd.ref}\n`);
      else reportLines.push(`ng ${cmd.ref} ${result.reason}\n`);
    }
  }
  // The report-status stream is pkt-lined inside band 1 and ends with its own
  // flush inside band 1; the outer stream then ends with a flush too. Real
  // git waits for the inner flush before it trusts the report.
  const reportBody = Buffer.concat([encodePktLines(reportLines), pktFlush()]);
  const parts: Buffer[] = [];
  const max = SIDEBAND_MAX_CHUNK;
  for (let offset = 0; offset < reportBody.length; offset += max) {
    const chunkLen = Math.min(reportBody.length - offset, max);
    const total = chunkLen + 5;
    const packet = Buffer.allocUnsafe(total);
    packet.write(total.toString(16).padStart(4, '0'), 0, 4, 'ascii');
    packet[4] = 1;
    reportBody.copy(packet, 5, offset, offset + chunkLen);
    parts.push(packet);
  }
  parts.push(pktFlush());
  return { status: 200, contentType: RECEIVE_RESULT, body: Buffer.concat(parts) };
}

/** Apply one push command with fast-forward enforcement. */
function applyPushCommand(repo: Repo, cmd: PushCommand): { ok: boolean; reason?: string } {
  if (!isObjectId(cmd.oldOid) || !isObjectId(cmd.newOid)) return { ok: false, reason: 'invalid object id' };
  const current = repo.refs.get(cmd.ref);
  if (current === undefined) {
    if (cmd.oldOid !== ZERO_OID) return { ok: false, reason: 'non-fast-forward' };
    if (cmd.newOid === ZERO_OID) return { ok: false, reason: 'cannot delete a ref that does not exist' };
    if (!repo.objects.has(cmd.newOid)) return { ok: false, reason: 'object not found' };
    repo.refs.set(cmd.ref, cmd.newOid);
    return { ok: true };
  }
  if (cmd.oldOid !== current) return { ok: false, reason: 'non-fast-forward' };
  if (cmd.newOid === ZERO_OID) {
    repo.refs.delete(cmd.ref);
    return { ok: true };
  }
  if (cmd.newOid === current) return { ok: true };
  if (!repo.objects.has(cmd.newOid)) return { ok: false, reason: 'object not found' };
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  if (!isAncestor(lookup, current, cmd.newOid)) return { ok: false, reason: 'non-fast-forward' };
  repo.refs.set(cmd.ref, cmd.newOid);
  return { ok: true };
}

// ---------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------

/** Route one git smart-HTTP request. Returns null when the path is not a git path. */
export function handleGit(store: Store, req: GitRequest): GitResponse | null {
  const segments = req.path.split('/').filter((s) => s !== '');
  if (segments.length < 2) return null;
  const repoSegment = segments[1] as string;
  if (!repoSegment.endsWith('.git')) return null;
  const owner = segments[0] as string;
  const repoName = repoSegment.slice(0, -4);
  const rest = segments.slice(2);
  const repo = store.resolveRepo(owner, repoName);
  if (!repo) {
    return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' })) };
  }
  if (req.method === 'GET' && rest.length === 2 && rest[0] === 'info' && rest[1] === 'refs') {
    const service = req.query.get('service');
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
      return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'unknown service' })) };
    }
    return infoRefs(store, repo, service, req.headers['git-protocol'] ?? null);
  }
  if (req.method === 'POST' && rest.length === 1 && rest[0] === 'git-upload-pack') {
    return handleUploadPack(repo, req);
  }
  if (req.method === 'POST' && rest.length === 1 && rest[0] === 'git-receive-pack') {
    return handleReceivePack(repo, req);
  }
  return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' })) };
}

export { hashObject, parseCommit, parseTree, parseTag };
