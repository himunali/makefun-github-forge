// The forge's whole state: one in-memory store, no clock, no randomness.
// Every timestamp comes from MAKEFUN_TRIAL_NOW plus a logical counter that
// advances once per mutation, so the same request sequence in the same order
// always builds the same state. Every id is a sequential counter. Git object
// ids are content-addressed and deterministic by construction.
//
// Ownership: the bearer token is opaque (shpat_<hex>), so the token's login
// cannot be decoded from it. The store learns it instead: the first request
// that names an existing repository under an owner binds that repository (and
// the viewer) to that owner. Later requests under another owner for the same
// repository name answer 404, never 403.

import {
  buildCommit,
  buildTree,
  collectClosure,
  flattenTree,
  hashObject,
  isAncestor,
  listCommits,
  mergeBase,
  parseCommit,
  parseTree,
  peelObject,
  sortTreeEntries,
  ZERO_OID,
  type GitObject,
  type GitObjectType,
} from './gitobj.ts';

export const DEFAULT_BRANCH = 'main';

/** Parse MAKEFUN_TRIAL_NOW (ISO text, epoch seconds or epoch ms) into seconds. */
function parseTrialNow(raw: string | undefined): number {
  if (raw === undefined || raw === '') return 1735689600; // 2025-01-01T00:00:00Z
  if (/^-?\d+$/.test(raw.trim())) {
    const n = Number(raw.trim());
    if (n > 100000000000) return Math.floor(n / 1000);
    return n;
  }
  const ms = Date.parse(raw.trim());
  if (Number.isFinite(ms)) return Math.floor(ms / 1000);
  return 1735689600;
}

/** Format epoch seconds as an ISO-8601 UTC string without touching the clock. */
export function isoFromSec(sec: number): string {
  const days = Math.floor(sec / 86400);
  const rem = sec - days * 86400;
  const hh = Math.floor(rem / 3600);
  const mm = Math.floor((rem - hh * 3600) / 60);
  const ss = rem - hh * 3600 - mm * 60;
  // Civil date from days since epoch (Howard Hinnant's algorithm).
  const z = days + 719468;
  const era = Math.floor(z / 146097);
  const doe = z - era * 146097;
  const yoe = Math.floor((doe - Math.floor(doe / 1460) + Math.floor(doe / 36524) - Math.floor(doe / 146096)) / 365);
  const y = yoe + era * 400;
  const doy = doe - (365 * yoe + Math.floor(yoe / 4) - Math.floor(yoe / 100));
  const mp = Math.floor((5 * doy + 2) / 153);
  const d = doy - Math.floor((153 * mp + 2) / 5) + 1;
  const m = mp < 10 ? mp + 3 : mp - 9;
  const year = m <= 2 ? y + 1 : y;
  const pad = (n: number, w = 2): string => String(n).padStart(w, '0');
  return `${pad(year, 4)}-${pad(m)}-${pad(d)}T${pad(hh)}:${pad(mm)}:${pad(ss)}Z`;
}

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string;
}

export interface Comment {
  id: number;
  body: string;
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
}

export interface Issue {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  comments: Comment[];
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
}

export interface PullRequest {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
}

export interface Repo {
  id: number;
  name: string;
  owner: string | null;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  pulls: Map<number, PullRequest>;
  nextNumber: number;
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
  pushedAt: string | null;
}

export class Store {
  readonly token: string;
  readonly baseSec: number;
  private seq = 0;
  private reqSeq = 0;
  private repoIdSeq = 0;
  private objectIdSeq = 0;
  private labelIdSeq = 0;
  private commentIdSeq = 0;
  reposByName = new Map<string, Repo>();
  reposById = new Map<number, Repo>();
  viewerLogin: string | null = null;

  constructor() {
    this.token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
    this.baseSec = parseTrialNow(process.env.MAKEFUN_TRIAL_NOW);
  }

  /** The next logical timestamp (seconds since epoch) for a mutation. */
  tickSec(): number {
    this.seq += 1;
    return this.baseSec + this.seq;
  }

  /** A fresh request id for X-GitHub-Request-Id (sequential, deterministic). */
  nextRequestId(): string {
    this.reqSeq += 1;
    return `req-${this.reqSeq}`;
  }

  /** The viewer identity string used as a git author/committer. */
  viewerIdentity(): string {
    const login = this.viewerLogin ?? 'owner';
    return `${login} <${login}@example.invalid>`;
  }

  /** The login to show as a repository's owner (bound, learned, or a fallback). */
  ownerLogin(repo: Repo): string {
    return repo.owner ?? this.viewerLogin ?? 'owner';
  }

  // ------------------------------------------------------------------ repos

  createRepo(name: string, description: string, isPrivate: boolean): Repo | null {
    if (this.reposByName.has(name)) return null;
    const sec = this.tickSec();
    const id = ++this.repoIdSeq;
    const repo: Repo = {
      id,
      name,
      owner: this.viewerLogin,
      description,
      isPrivate,
      defaultBranch: DEFAULT_BRANCH,
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
      createdSeq: this.seq,
      createdAt: isoFromSec(sec),
      updatedAt: isoFromSec(sec),
      pushedAt: null,
    };
    this.reposByName.set(name, repo);
    this.reposById.set(id, repo);
    return repo;
  }

  deleteRepo(repo: Repo): void {
    this.reposByName.delete(repo.name);
    this.reposById.delete(repo.id);
  }

  /** Bind an unbound repository to `owner` (first sight wins); false when the owner mismatches. */
  bindOwner(repo: Repo, owner: string): boolean {
    if (repo.owner === null) {
      repo.owner = owner;
      if (this.viewerLogin === null) this.viewerLogin = owner;
      return true;
    }
    return repo.owner === owner;
  }

  /** Resolve a repository by owner + name, learning the owner when unbound. Null = 404. */
  resolveRepo(owner: string, name: string): Repo | null {
    const repo = this.reposByName.get(name);
    if (!repo) return null;
    if (!this.bindOwner(repo, owner)) return null;
    return repo;
  }

  /** Resolve a repository by name alone, without learning (used by MCP full_name). */
  repoByName(name: string): Repo | undefined {
    return this.reposByName.get(name);
  }

  listRepos(): Repo[] {
    return [...this.reposByName.values()].sort((a, b) => a.createdSeq - b.createdSeq);
  }

  // ------------------------------------------------------------------ git refs

  refTip(repo: Repo, ref: string): string | null {
    return repo.refs.get(ref) ?? null;
  }

  setRef(repo: Repo, ref: string, oid: string): void {
    repo.refs.set(ref, oid);
  }

  deleteRef(repo: Repo, ref: string): void {
    repo.refs.delete(ref);
  }

  lookup(repo: Repo): (oid: string) => GitObject | undefined {
    return (oid: string) => repo.objects.get(oid);
  }

  putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  /** The peeled commit a ref points at (or null when the ref does not exist). */
  refCommit(repo: Repo, ref: string): string | null {
    const tip = repo.refs.get(ref);
    if (!tip) return null;
    return peelObject(this.lookup(repo), tip).oid;
  }

  /** The branch names of a repository, sorted. */
  branchNames(repo: Repo): string[] {
    const names: string[] = [];
    for (const ref of repo.refs.keys()) {
      if (ref.startsWith('refs/heads/')) names.push(ref.slice('refs/heads/'.length));
    }
    return names.sort();
  }

  /** Build (and store) the nested trees for a flat path -> blob map; returns the root tree oid. */
  buildTreeFromFiles(repo: Repo, files: Map<string, { oid: string; mode: string }>): string {
    interface Node {
      files: Map<string, { oid: string; mode: string }>;
      dirs: Map<string, Node>;
    }
    const make = (): Node => ({ files: new Map(), dirs: new Map() });
    const root = make();
    for (const [path, entry] of files) {
      const segments = path.split('/');
      let node = root;
      for (let i = 0; i < segments.length - 1; i += 1) {
        const seg = segments[i] as string;
        if (!node.dirs.has(seg)) node.dirs.set(seg, make());
        node = node.dirs.get(seg) as Node;
      }
      node.files.set(segments[segments.length - 1] as string, entry);
    }
    const build = (node: Node): string => {
      const entries: { mode: string; name: string; oid: string }[] = [];
      for (const [name, entry] of node.files) entries.push({ mode: entry.mode, name, oid: entry.oid });
      for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
      return this.putObject(repo, 'tree', buildTree(entries));
    };
    return build(root);
  }

  /** The flat file map at a commit (empty when the commit is unknown). */
  filesAtCommit(repo: Repo, commitOid: string): Map<string, { oid: string; mode: string }> {
    const obj = repo.objects.get(commitOid);
    if (!obj || obj.type !== 'commit') return new Map();
    return flattenTree(this.lookup(repo), parseCommit(obj.data).tree);
  }

  /** The flat file map at a ref's tip (empty when the ref is unborn). */
  filesAtRef(repo: Repo, ref: string): Map<string, { oid: string; mode: string }> {
    const commit = this.refCommit(repo, ref);
    if (!commit) return new Map();
    return this.filesAtCommit(repo, commit);
  }

  // ------------------------------------------------------------- contents API

  /**
   * Write one file on `branch` (creating the branch from the default tip when
   * it does not exist yet, and the repository's first commit when unborn).
   * Returns the new commit sha and blob sha, or an error status + message.
   */
  commitFile(
    repo: Repo,
    options: { path: string; content: Buffer; message: string; branch: string; sha?: string },
  ): { ok: true; commitSha: string; blobSha: string; created: boolean } | { ok: false; status: number; message: string } {
    const branchRef = `refs/heads/${options.branch}`;
    let parent: string | null = this.refCommit(repo, branchRef);
    if (parent === null && options.branch !== repo.defaultBranch) {
      parent = this.refCommit(repo, `refs/heads/${repo.defaultBranch}`);
    }
    const files = parent ? this.filesAtCommit(repo, parent) : new Map<string, { oid: string; mode: string }>();
    const existing = files.get(options.path);
    if (existing && options.sha !== undefined && options.sha.toLowerCase() !== existing.oid.toLowerCase()) {
      return { ok: false, status: 409, message: 'sha does not match the current file (stale sha)' };
    }
    if (!existing && options.sha !== undefined) {
      return { ok: false, status: 409, message: 'sha was given but the file does not exist' };
    }
    const blobSha = this.putObject(repo, 'blob', options.content);
    files.set(options.path, { oid: blobSha, mode: '100644' });
    const treeOid = this.buildTreeFromFiles(repo, files);
    const sec = this.tickSec();
    const identity = this.viewerIdentity();
    const commitData = buildCommit({
      tree: treeOid,
      parents: parent ? [parent] : [],
      author: identity,
      committer: identity,
      timestamp: sec,
      message: options.message,
    });
    const commitSha = this.putObject(repo, 'commit', commitData);
    repo.refs.set(branchRef, commitSha);
    repo.updatedAt = isoFromSec(sec);
    repo.pushedAt = isoFromSec(sec);
    return { ok: true, commitSha, blobSha, created: !existing };
  }

  /** Delete one file on the default branch (or `branch`); returns the new commit sha or an error. */
  deleteFile(
    repo: Repo,
    options: { path: string; message: string; branch: string; sha?: string },
  ): { ok: true; commitSha: string } | { ok: false; status: number; message: string } {
    const branchRef = `refs/heads/${options.branch}`;
    const parent = this.refCommit(repo, branchRef);
    if (!parent) return { ok: false, status: 404, message: 'Not Found' };
    const files = this.filesAtCommit(repo, parent);
    const existing = files.get(options.path);
    if (!existing) return { ok: false, status: 404, message: 'Not Found' };
    if (options.sha !== undefined && options.sha.toLowerCase() !== existing.oid.toLowerCase()) {
      return { ok: false, status: 409, message: 'sha does not match the current file (stale sha)' };
    }
    files.delete(options.path);
    const treeOid = this.buildTreeFromFiles(repo, files);
    const sec = this.tickSec();
    const identity = this.viewerIdentity();
    const commitSha = this.putObject(
      repo,
      'commit',
      buildCommit({
        tree: treeOid,
        parents: [parent],
        author: identity,
        committer: identity,
        timestamp: sec,
        message: options.message,
      }),
    );
    repo.refs.set(branchRef, commitSha);
    repo.updatedAt = isoFromSec(sec);
    repo.pushedAt = isoFromSec(sec);
    return { ok: true, commitSha };
  }

  // ------------------------------------------------------------------ labels

  createLabel(repo: Repo, name: string, color: string, description = ''): Label {
    const existing = repo.labels.get(name);
    if (existing) return existing;
    const label: Label = { id: ++this.labelIdSeq, name, color: color || 'ededed', description };
    repo.labels.set(name, label);
    return label;
  }

  // ------------------------------------------------------------------ issues

  createIssue(repo: Repo, title: string, body: string, labelNames: readonly string[]): Issue | { error: string } {
    const labels: Label[] = [];
    for (const name of labelNames) {
      const label = repo.labels.get(name);
      if (!label) return { error: `Label does not exist: ${name}` };
      labels.push(label);
    }
    const number = repo.nextNumber++;
    const sec = this.tickSec();
    const issue: Issue = {
      number,
      id: ++this.objectIdSeq,
      title,
      body,
      state: 'open',
      labels,
      comments: [],
      createdSeq: this.seq,
      createdAt: isoFromSec(sec),
      updatedAt: isoFromSec(sec),
      closedAt: null,
    };
    repo.issues.set(number, issue);
    return issue;
  }

  addComment(repo: Repo, issue: Issue, body: string): Comment {
    const sec = this.tickSec();
    const comment: Comment = {
      id: ++this.commentIdSeq,
      body,
      createdSeq: this.seq,
      createdAt: isoFromSec(sec),
      updatedAt: isoFromSec(sec),
    };
    issue.comments.push(comment);
    issue.updatedAt = isoFromSec(sec);
    return comment;
  }

  closeIssue(repo: Repo, issue: Issue): void {
    if (issue.state === 'closed') return;
    const sec = this.tickSec();
    issue.state = 'closed';
    issue.closedAt = isoFromSec(sec);
    issue.updatedAt = isoFromSec(sec);
  }

  reopenIssue(repo: Repo, issue: Issue): void {
    if (issue.state === 'open') return;
    const sec = this.tickSec();
    issue.state = 'open';
    issue.closedAt = null;
    issue.updatedAt = isoFromSec(sec);
  }

  listIssues(repo: Repo, state: string): Issue[] {
    const all = [...repo.issues.values()];
    if (state === 'all') return all;
    return all.filter((issue) => issue.state === state);
  }

  // -------------------------------------------------------------------- pulls

  createPull(repo: Repo, title: string, body: string, head: string, base: string): PullRequest | { error: string } {
    const headRef = `refs/heads/${head}`;
    const baseRef = `refs/heads/${base}`;
    const headTip = repo.refs.get(headRef);
    const baseTip = repo.refs.get(baseRef);
    if (!headTip) return { error: `head branch ${head} does not exist` };
    if (!baseTip) return { error: `base branch ${base} does not exist` };
    for (const pr of repo.pulls.values()) {
      if (pr.state === 'open' && pr.head === head && pr.base === base) {
        return { error: 'A pull request already exists for this head and base' };
      }
    }
    const number = repo.nextNumber++;
    const sec = this.tickSec();
    const pr: PullRequest = {
      number,
      id: ++this.objectIdSeq,
      title,
      body,
      state: 'open',
      head,
      base,
      headSha: peelObject(this.lookup(repo), headTip).oid,
      baseSha: peelObject(this.lookup(repo), baseTip).oid,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      createdSeq: this.seq,
      createdAt: isoFromSec(sec),
      updatedAt: isoFromSec(sec),
      closedAt: null,
    };
    repo.pulls.set(number, pr);
    return pr;
  }

  /** The file changes of a pull request, head vs merge-base, with line counts. */
  prFiles(repo: Repo, pr: PullRequest): Array<{ path: string; status: string; additions: number; deletions: number; changes: number }> {
    const lookup = this.lookup(repo);
    const headCommit = this.refCommit(repo, `refs/heads/${pr.head}`);
    const baseCommit = this.refCommit(repo, `refs/heads/${pr.base}`);
    if (!headCommit || !baseCommit) return [];
    const ancestor = mergeBase(lookup, headCommit, baseCommit) ?? baseCommit;
    const baseFiles = this.filesAtCommit(repo, ancestor);
    const headFiles = this.filesAtCommit(repo, headCommit);
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
    const out: Array<{ path: string; status: string; additions: number; deletions: number; changes: number }> = [];
    for (const path of [...paths].sort()) {
      const before = baseFiles.get(path);
      const after = headFiles.get(path);
      if (before && after && before.oid === after.oid) continue;
      let status = 'modified';
      if (!before) status = 'added';
      else if (!after) status = 'removed';
      const beforeText = before ? (repo.objects.get(before.oid)?.data.toString('utf8') ?? '') : '';
      const afterText = after ? (repo.objects.get(after.oid)?.data.toString('utf8') ?? '') : '';
      const { additions, deletions } = lineDiff(beforeText, afterText);
      out.push({ path, status, additions, deletions, changes: additions + deletions });
    }
    return out;
  }

  /** 'MERGEABLE' when a squash merge would apply cleanly, else 'CONFLICTING'. */
  prMergeable(repo: Repo, pr: PullRequest): 'MERGEABLE' | 'CONFLICTING' {
    return this.mergeConflict(repo, pr) === null ? 'MERGEABLE' : 'CONFLICTING';
  }

  /** The conflicting paths of a squash merge, or null when it applies cleanly. */
  mergeConflict(repo: Repo, pr: PullRequest): string[] | null {
    const lookup = this.lookup(repo);
    const headCommit = this.refCommit(repo, `refs/heads/${pr.head}`);
    const baseCommit = this.refCommit(repo, `refs/heads/${pr.base}`);
    if (!headCommit || !baseCommit) return ['(missing branch)'];
    const ancestor = mergeBase(lookup, headCommit, baseCommit) ?? baseCommit;
    const baseFiles = this.filesAtCommit(repo, ancestor);
    const headFiles = this.filesAtCommit(repo, headCommit);
    const tipFiles = this.filesAtCommit(repo, baseCommit);
    const conflicts: string[] = [];
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys(), ...tipFiles.keys()]);
    for (const path of paths) {
      const anc = baseFiles.get(path);
      const head = headFiles.get(path);
      const tip = tipFiles.get(path);
      const headChanged = (head?.oid ?? null) !== (anc?.oid ?? null);
      const tipChanged = (tip?.oid ?? null) !== (anc?.oid ?? null);
      if (headChanged && tipChanged && (head?.oid ?? null) !== (tip?.oid ?? null)) conflicts.push(path);
    }
    return conflicts.length ? conflicts : null;
  }

  /**
   * Squash-merge a pull request onto its base branch: one new commit whose
   * tree is the base tip with the head's changes applied. Returns the merge
   * commit sha, or an error status + message.
   */
  mergePull(repo: Repo, pr: PullRequest): { ok: true; sha: string } | { ok: false; status: number; message: string } {
    if (pr.state === 'closed') {
      return { ok: false, status: 405, message: pr.merged ? 'Pull request is already merged' : 'Pull request is closed' };
    }
    const conflict = this.mergeConflict(repo, pr);
    if (conflict !== null) {
      return { ok: false, status: 405, message: `Merge conflict in ${conflict.join(', ')}` };
    }
    const lookup = this.lookup(repo);
    const headCommit = this.refCommit(repo, `refs/heads/${pr.head}`);
    const baseCommit = this.refCommit(repo, `refs/heads/${pr.base}`);
    if (!headCommit || !baseCommit) return { ok: false, status: 404, message: 'Not Found' };
    const ancestor = mergeBase(lookup, headCommit, baseCommit) ?? baseCommit;
    const baseFiles = this.filesAtCommit(repo, ancestor);
    const headFiles = this.filesAtCommit(repo, headCommit);
    const merged = this.filesAtCommit(repo, baseCommit);
    for (const [path, entry] of headFiles) merged.set(path, entry);
    for (const path of baseFiles.keys()) {
      if (!headFiles.has(path)) merged.delete(path);
    }
    const treeOid = this.buildTreeFromFiles(repo, merged);
    const sec = this.tickSec();
    const identity = this.viewerIdentity();
    const message = `${pr.title} (#${pr.number})`;
    const commitSha = this.putObject(
      repo,
      'commit',
      buildCommit({
        tree: treeOid,
        parents: [baseCommit],
        author: identity,
        committer: identity,
        timestamp: sec,
        message,
      }),
    );
    repo.refs.set(`refs/heads/${pr.base}`, commitSha);
    pr.state = 'closed';
    pr.merged = true;
    pr.mergedAt = isoFromSec(sec);
    pr.mergeCommitSha = commitSha;
    pr.closedAt = isoFromSec(sec);
    pr.updatedAt = isoFromSec(sec);
    pr.baseSha = commitSha;
    repo.updatedAt = isoFromSec(sec);
    repo.pushedAt = isoFromSec(sec);
    return { ok: true, sha: commitSha };
  }

  // -------------------------------------------------------------- global ids

  /** An opaque, stable Relay-style global id (base64 of `Kind:parts...`). */
  globalId(kind: string, ...parts: Array<string | number>): string {
    return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64');
  }

  decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
    try {
      const text = Buffer.from(id, 'base64').toString('utf8');
      const [kind, ...parts] = text.split(':');
      if (!kind || parts.length === 0) return null;
      return { kind, parts };
    } catch {
      return null;
    }
  }

  repoGlobalId(repo: Repo): string {
    return this.globalId('Repository', repo.id);
  }

  issueGlobalId(repo: Repo, issue: Issue): string {
    return this.globalId('Issue', repo.id, issue.number);
  }

  pullGlobalId(repo: Repo, pr: PullRequest): string {
    return this.globalId('PullRequest', repo.id, pr.number);
  }

  commentGlobalId(repo: Repo, issue: Issue, comment: Comment): string {
    return this.globalId('IssueComment', repo.id, issue.number, comment.id);
  }

  // --------------------------------------------------------------- JSON views

  repoJson(repo: Repo): Record<string, unknown> {
    const owner = this.ownerLogin(repo);
    return {
      id: repo.id,
      node_id: this.repoGlobalId(repo),
      name: repo.name,
      full_name: `${owner}/${repo.name}`,
      owner: { login: owner, id: 1000, type: 'User' },
      private: repo.isPrivate,
      description: repo.description,
      default_branch: repo.defaultBranch,
      html_url: `https://example.invalid/${owner}/${repo.name}`,
      url: `/api/v3/repos/${owner}/${repo.name}`,
      clone_url: `https://example.invalid/${owner}/${repo.name}.git`,
      git_url: `git://example.invalid/${owner}/${repo.name}.git`,
      ssh_url: `git@example.invalid:${owner}/${repo.name}.git`,
      created_at: repo.createdAt,
      updated_at: repo.updatedAt,
      pushed_at: repo.pushedAt,
      has_issues: true,
      has_projects: true,
      has_wiki: true,
      fork: false,
      archived: false,
      disabled: false,
      license: null,
      topics: [],
      visibility: repo.isPrivate ? 'private' : 'public',
      homepage: null,
      language: null,
      forks_count: 0,
      stargazers_count: 0,
      watchers_count: 0,
      open_issues_count: [...repo.issues.values()].filter((i) => i.state === 'open').length,
      permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
    };
  }

  labelJson(label: Label): Record<string, unknown> {
    return {
      id: label.id,
      node_id: this.globalId('Label', label.id),
      name: label.name,
      color: label.color,
      description: label.description,
      default: false,
      url: `/api/v3/labels/${label.id}`,
    };
  }

  issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
    const owner = this.ownerLogin(repo);
    return {
      id: issue.id,
      node_id: this.issueGlobalId(repo, issue),
      number: issue.number,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      labels: issue.labels.map((label) => this.labelJson(label)),
      user: { login: owner, id: 1000, type: 'User' },
      assignee: null,
      assignees: [],
      locked: false,
      comments: issue.comments.length,
      created_at: issue.createdAt,
      updated_at: issue.updatedAt,
      closed_at: issue.closedAt,
      html_url: `https://example.invalid/${owner}/${repo.name}/issues/${issue.number}`,
      url: `/api/v3/repos/${owner}/${repo.name}/issues/${issue.number}`,
      repository_url: `/api/v3/repos/${owner}/${repo.name}`,
      author_association: 'OWNER',
    };
  }

  commentJson(repo: Repo, issue: Issue, comment: Comment): Record<string, unknown> {
    const owner = this.ownerLogin(repo);
    return {
      id: comment.id,
      node_id: this.commentGlobalId(repo, issue, comment),
      body: comment.body,
      user: { login: owner, id: 1000, type: 'User' },
      created_at: comment.createdAt,
      updated_at: comment.updatedAt,
      html_url: `https://example.invalid/${owner}/${repo.name}/issues/${issue.number}#issuecomment-${comment.id}`,
      url: `/api/v3/repos/${owner}/${repo.name}/issues/comments/${comment.id}`,
      issue_url: `/api/v3/repos/${owner}/${repo.name}/issues/${issue.number}`,
      author_association: 'OWNER',
    };
  }

  pullJson(repo: Repo, pr: PullRequest): Record<string, unknown> {
    const owner = this.ownerLogin(repo);
    const files = this.prFiles(repo, pr);
    const additions = files.reduce((sum, f) => sum + f.additions, 0);
    const deletions = files.reduce((sum, f) => sum + f.deletions, 0);
    return {
      id: pr.id,
      node_id: this.pullGlobalId(repo, pr),
      number: pr.number,
      title: pr.title,
      body: pr.body,
      state: pr.state,
      locked: false,
      user: { login: owner, id: 1000, type: 'User' },
      labels: [],
      head: {
        label: `${owner}:${pr.head}`,
        ref: pr.head,
        sha: pr.headSha,
        user: { login: owner, id: 1000, type: 'User' },
        repo: this.repoJson(repo),
      },
      base: {
        label: `${owner}:${pr.base}`,
        ref: pr.base,
        sha: pr.baseSha,
        user: { login: owner, id: 1000, type: 'User' },
        repo: this.repoJson(repo),
      },
      merged: pr.merged,
      mergeable: pr.state === 'open' ? this.prMergeable(repo, pr) === 'MERGEABLE' : null,
      mergeable_state: pr.state === 'open' ? (this.prMergeable(repo, pr) === 'MERGEABLE' ? 'clean' : 'dirty') : 'unknown',
      merge_commit_sha: pr.mergeCommitSha,
      merged_at: pr.mergedAt,
      merged_by: null,
      created_at: pr.createdAt,
      updated_at: pr.updatedAt,
      closed_at: pr.closedAt,
      html_url: `https://example.invalid/${owner}/${repo.name}/pull/${pr.number}`,
      url: `/api/v3/repos/${owner}/${repo.name}/pulls/${pr.number}`,
      diff_url: `https://example.invalid/${owner}/${repo.name}/pull/${pr.number}.diff`,
      patch_url: `https://example.invalid/${owner}/${repo.name}/pull/${pr.number}.patch`,
      additions,
      deletions,
      changed_files: files.length,
      commits: this.countPullCommits(repo, pr),
      comments: 0,
      review_comments: 0,
      author_association: 'OWNER',
    };
  }

  countPullCommits(repo: Repo, pr: PullRequest): number {
    const lookup = this.lookup(repo);
    const headCommit = this.refCommit(repo, `refs/heads/${pr.head}`);
    const baseCommit = this.refCommit(repo, `refs/heads/${pr.base}`);
    if (!headCommit || !baseCommit) return 0;
    const ancestor = mergeBase(lookup, headCommit, baseCommit);
    const headSet = new Set(listCommits(lookup, headCommit));
    if (!ancestor) return headSet.size;
    const baseSet = new Set(listCommits(lookup, baseCommit));
    let count = 0;
    for (const oid of headSet) if (!baseSet.has(oid)) count += 1;
    return count;
  }
}

/** A rough line diff: lines gained vs lines lost, by multiset comparison. */
export function lineDiff(before: string, after: string): { additions: number; deletions: number } {
  const countLines = (text: string): Map<string, number> => {
    const map = new Map<string, number>();
    if (text === '') return map;
    for (const line of text.split('\n')) map.set(line, (map.get(line) ?? 0) + 1);
    return map;
  };
  const a = countLines(before);
  const b = countLines(after);
  let additions = 0;
  let deletions = 0;
  for (const [line, n] of b) {
    const had = a.get(line) ?? 0;
    if (n > had) additions += n - had;
  }
  for (const [line, n] of a) {
    const has = b.get(line) ?? 0;
    if (n > has) deletions += n - has;
  }
  return { additions, deletions };
}

export { ZERO_OID };
