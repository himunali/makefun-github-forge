// The REST v3 surface under /api/v3: repositories, branches, commits,
// contents, labels, issues, comments and pull requests. Every reply is JSON
// and carries X-GitHub-Request-Id; errors carry a string message.

import { parseCommit, parseTree } from './gitobj.ts';
import { isoFromSec, type Issue, type Repo, type Store } from './store.ts';

export interface RestRequest {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: Record<string, string>;
  bodyText: string;
  body: unknown;
  host: string;
}

export interface RestResponse {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

const json = (status: number, body: unknown, headers: Record<string, string> = {}): RestResponse => ({
  status,
  body,
  headers,
});

const err = (status: number, message: string): RestResponse => json(status, { message });

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function bodyOf(req: RestRequest): Record<string, unknown> {
  return isRecord(req.body) ? req.body : {};
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function bool(v: unknown, fallback = false): boolean {
  return typeof v === 'boolean' ? v : fallback;
}

/** Paginate a list with GitHub's page/per_page + Link rel="next" behaviour. */
function paginate<T>(req: RestRequest, items: readonly T[]): { page: T[]; link: string | null } {
  const perPageRaw = Number(req.query.get('per_page') ?? '30');
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(100, Math.trunc(perPageRaw)) : 30;
  const pageRaw = Number(req.query.get('page') ?? '1');
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? Math.trunc(pageRaw) : 1;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | null = null;
  if (start + perPage < items.length) {
    const url = new URL(`http://${req.host}${req.path}`);
    url.searchParams.set('per_page', String(perPage));
    url.searchParams.set('page', String(page + 1));
    link = `<${url.toString()}>; rel="next"`;
  }
  return { page: slice, link };
}

function withLink(res: RestResponse, link: string | null): RestResponse {
  if (link) {
    res.headers = { ...(res.headers ?? {}), link };
  }
  return res;
}

function decodePathSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/** Resolve /repos/{owner}/{name} from path segments, or null when it is not that shape. */
function repoFromSegments(store: Store, segments: string[]): { repo: Repo; owner: string; name: string; tail: string[] } | null {
  if (segments.length < 4 || segments[0] !== 'api' || segments[1] !== 'v3' || segments[2] !== 'repos') return null;
  const owner = decodePathSegment(segments[3] as string);
  const name = decodePathSegment(segments[4] as string);
  const repo = store.resolveRepo(owner, name);
  if (!repo) return null;
  return { repo, owner, name, tail: segments.slice(5) };
}

function notFoundRepo(): RestResponse {
  return err(404, 'Not Found');
}

/** The commit objects of a ref, newest first, as REST commit views. */
function commitsOf(store: Store, repo: Repo, ref: string): Array<Record<string, unknown>> {
  const tip = repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(ref);
  if (!tip) return [];
  const lookup = (oid: string) => repo.objects.get(oid);
  const out: Array<Record<string, unknown>> = [];
  const seen = new Set<string>();
  const queue: string[] = [tip];
  while (queue.length > 0) {
    const oid = queue.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    const view = parseCommit(obj.data);
    out.push(commitJson(store, repo, oid, view));
    queue.push(...view.parents);
  }
  return out;
}

function commitJson(store: Store, repo: Repo, oid: string, view: ReturnType<typeof parseCommit>): Record<string, unknown> {
  const owner = store.ownerLogin(repo);
  // The stored message ends with the newline buildCommit appends; the API
  // reports it without that trailing newline.
  const message = view.message.endsWith('\n') ? view.message.slice(0, -1) : view.message;
  return {
    sha: oid,
    node_id: store.globalId('Commit', oid),
    commit: {
      message,
      tree: { sha: view.tree },
      author: { name: view.author.split(' <')[0] ?? view.author, email: '', date: isoFromSec(view.authorTs || store.baseSec) },
      committer: { name: view.committer.split(' <')[0] ?? view.committer, email: '', date: isoFromSec(view.committerTs || store.baseSec) },
      verification: { verified: false, reason: 'unsigned' },
    },
    author: { login: owner, id: 1000, type: 'User' },
    committer: { login: owner, id: 1000, type: 'User' },
    parents: view.parents.map((p) => ({ sha: p })),
    url: `/api/v3/repos/${owner}/${repo.name}/commits/${oid}`,
    html_url: `https://example.invalid/${owner}/${repo.name}/commit/${oid}`,
  };
}

/** Resolve a contents ref (branch name or commit sha) to a commit oid. */
function resolveContentsRef(repo: Repo, ref: string | null): string | null {
  if (ref === null || ref === '') return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  const byBranch = repo.refs.get(`refs/heads/${ref}`);
  if (byBranch) return byBranch;
  if (repo.objects.has(ref)) return ref;
  return null;
}

/** GET one file or one directory listing from a commit's tree. */
function contentsAt(store: Store, repo: Repo, commitOid: string, path: string): RestResponse {
  const owner = store.ownerLogin(repo);
  const obj = repo.objects.get(commitOid);
  if (!obj || obj.type !== 'commit') return err(404, 'Not Found');
  const treeOid = parseCommit(obj.data).tree;
  const base = `/api/v3/repos/${owner}/${repo.name}/contents`;
  if (path === '') {
    const tree = repo.objects.get(treeOid);
    if (!tree) return err(404, 'Not Found');
    const entries = parseTree(tree.data).map((entry) => ({
      type: entry.mode === '40000' ? 'dir' : 'file',
      name: entry.name,
      path: entry.name,
      sha: entry.oid,
      size: entry.mode === '40000' ? 0 : (repo.objects.get(entry.oid)?.data.length ?? 0),
      url: `${base}/${entry.name}`,
      html_url: `https://example.invalid/${owner}/${repo.name}/blob/${repo.defaultBranch}/${entry.name}`,
      git_url: `/api/v3/repos/${owner}/${repo.name}/git/blobs/${entry.oid}`,
      download_url: null,
    }));
    return json(200, entries);
  }
  const segments = path.split('/').filter((s) => s !== '');
  let currentTree = treeOid;
  for (let i = 0; i < segments.length; i += 1) {
    const treeObj = repo.objects.get(currentTree);
    if (!treeObj || treeObj.type !== 'tree') return err(404, 'Not Found');
    const entry = parseTree(treeObj.data).find((e) => e.name === segments[i]);
    if (!entry) return err(404, 'Not Found');
    const last = i === segments.length - 1;
    if (!last) {
      if (entry.mode !== '40000') return err(404, 'Not Found');
      currentTree = entry.oid;
      continue;
    }
    if (entry.mode === '40000') {
      const dirObj = repo.objects.get(entry.oid);
      if (!dirObj || dirObj.type !== 'tree') return err(404, 'Not Found');
      const entries = parseTree(dirObj.data).map((child) => ({
        type: child.mode === '40000' ? 'dir' : 'file',
        name: child.name,
        path: `${path}/${child.name}`,
        sha: child.oid,
        size: child.mode === '40000' ? 0 : (repo.objects.get(child.oid)?.data.length ?? 0),
        url: `${base}/${path}/${child.name}`,
        html_url: `https://example.invalid/${owner}/${repo.name}/blob/${repo.defaultBranch}/${path}/${child.name}`,
        git_url: `/api/v3/repos/${owner}/${repo.name}/git/blobs/${child.oid}`,
        download_url: null,
      }));
      return json(200, entries);
    }
    const blob = repo.objects.get(entry.oid);
    if (!blob) return err(404, 'Not Found');
    return json(200, {
      type: 'file',
      encoding: 'base64',
      size: blob.data.length,
      name: segments[segments.length - 1],
      path,
      content: blob.data.toString('base64'),
      sha: entry.oid,
      url: `${base}/${path}`,
      html_url: `https://example.invalid/${owner}/${repo.name}/blob/${repo.defaultBranch}/${path}`,
      git_url: `/api/v3/repos/${owner}/${repo.name}/git/blobs/${entry.oid}`,
      download_url: null,
    });
  }
  return err(404, 'Not Found');
}

function labelNamesOf(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  const out: string[] = [];
  for (const entry of value) {
    if (typeof entry === 'string') out.push(entry);
    else if (isRecord(entry) && typeof entry.name === 'string') out.push(entry.name);
  }
  return out;
}

function stripBranchPrefix(ref: string, owner: string): string {
  if (ref.includes(':')) {
    const parts = ref.split(':');
    return parts[parts.length - 1] as string;
  }
  void owner;
  return ref;
}

/** Route one REST request. Returns null when the path is not under /api/v3. */
export function handleRest(store: Store, req: RestRequest): RestResponse | null {
  const segments = req.path.split('/').filter((s) => s !== '');
  if (segments.length < 2 || segments[0] !== 'api' || segments[1] !== 'v3') return null;
  const method = req.method;

  // API root.
  if ((req.path === '/api/v3' || req.path === '/api/v3/') && method === 'GET') {
    return json(200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
  }
  // Meta.
  if (req.path === '/api/v3/meta' && method === 'GET') {
    return json(200, { verifiable_password_authentication: false, installed_version: '1.0.0', github_services_sha: '0000000000000000000000000000000000000000' });
  }
  // The token's own user.
  if (req.path === '/api/v3/user' && method === 'GET') {
    if (!store.viewerLogin) return err(404, 'Not Found');
    const login = store.viewerLogin;
    return json(200, {
      login,
      id: 1000,
      node_id: store.globalId('User', login),
      type: 'User',
      name: login,
      email: `${login}@example.invalid`,
      html_url: `https://example.invalid/${login}`,
      url: '/api/v3/user',
    });
  }
  // The viewer's repositories.
  if (req.path === '/api/v3/user/repos' && method === 'GET') {
    const { page, link } = paginate(req, store.listRepos().map((repo) => store.repoJson(repo)));
    return withLink(json(200, page), link);
  }
  if (req.path === '/api/v3/user/repos' && method === 'POST') {
    const body = bodyOf(req);
    const name = str(body.name);
    if (name === '') return err(422, 'name is required');
    const created = store.createRepo(name, str(body.description), bool(body.private, false));
    if (!created) return err(422, 'Repository already exists');
    return json(201, store.repoJson(created));
  }

  // /repos/{owner}/{name}[...]
  const hit = repoFromSegments(store, segments);
  if (segments.length >= 4 && segments[0] === 'api' && segments[1] === 'v3' && segments[2] === 'repos') {
    if (!hit) return notFoundRepo();
    const { repo, owner, name } = hit;
    const tail = hit.tail;

    // GET / repos, PATCH / repos, DELETE / repos
    if (tail.length === 0) {
      if (method === 'GET') return json(200, store.repoJson(repo));
      if (method === 'PATCH') {
        const body = bodyOf(req);
        if (typeof body.description === 'string') repo.description = body.description;
        if (typeof body.private === 'boolean') repo.isPrivate = body.private;
        if (typeof body.default_branch === 'string' && repo.refs.has(`refs/heads/${body.default_branch}`)) {
          repo.defaultBranch = body.default_branch;
        }
        return json(200, store.repoJson(repo));
      }
      if (method === 'DELETE') {
        store.deleteRepo(repo);
        return { status: 204, body: null };
      }
      return err(404, 'Not Found');
    }

    // Branches.
    if (tail.length === 1 && tail[0] === 'branches' && method === 'GET') {
      const branches = store.branchNames(repo).map((branch) => ({
        name: branch,
        commit: { sha: repo.refs.get(`refs/heads/${branch}`) ?? '', url: '' },
        protected: false,
      }));
      return json(200, branches);
    }
    if (tail.length === 2 && tail[0] === 'branches' && method === 'GET') {
      const branch = decodePathSegment(tail[1] as string);
      const tip = repo.refs.get(`refs/heads/${branch}`);
      if (!tip) return err(404, 'Not Found');
      return json(200, { name: branch, commit: { sha: tip, url: '' }, protected: false });
    }

    // Commits.
    if (tail.length === 1 && tail[0] === 'commits' && method === 'GET') {
      const ref = req.query.get('sha') ?? repo.defaultBranch;
      return json(200, commitsOf(store, repo, ref));
    }

    // Git Data: refs.
    if (tail.length >= 2 && tail[0] === 'git' && tail[1] === 'refs') {
      if (tail.length === 2 && method === 'POST') {
        const body = bodyOf(req);
        const ref = str(body.ref);
        const sha = str(body.sha);
        if (!ref.startsWith('refs/') || sha === '') return err(422, 'ref and sha are required');
        if (repo.refs.has(ref)) return err(422, 'Reference already exists');
        if (!repo.objects.has(sha)) return err(422, 'Object does not exist');
        repo.refs.set(ref, sha);
        const obj = repo.objects.get(sha);
        return json(201, { ref, node_id: store.globalId('Ref', ref), url: '', object: { sha, type: obj ? obj.type : 'commit', url: '' } });
      }
      if (tail.length === 2 && method === 'GET') {
        const refs = [...repo.refs.entries()].map(([ref, sha]) => ({
          ref,
          node_id: store.globalId('Ref', ref),
          url: '',
          object: { sha, type: repo.objects.get(sha)?.type ?? 'commit', url: '' },
        }));
        return json(200, refs);
      }
      if (tail.length >= 3 && method === 'GET') {
        const kind = tail[2];
        const branch = tail.slice(3).map(decodePathSegment).join('/');
        const ref = `refs/${kind}/${branch}`;
        const tip = repo.refs.get(ref);
        if (!tip) return err(404, 'Not Found');
        return json(200, { ref, node_id: store.globalId('Ref', ref), url: '', object: { sha: tip, type: repo.objects.get(tip)?.type ?? 'commit', url: '' } });
      }
      return err(404, 'Not Found');
    }

    // Contents.
    if (tail.length >= 1 && tail[0] === 'contents') {
      const contentPath = tail.slice(1).map(decodePathSegment).join('/');
      if (method === 'GET') {
        const ref = req.query.get('ref');
        const commitOid = resolveContentsRef(repo, ref);
        if (!commitOid) return err(404, 'Not Found');
        return contentsAt(store, repo, commitOid, contentPath);
      }
      if (method === 'PUT') {
        const body = bodyOf(req);
        const contentB64 = str(body.content);
        if (contentB64 === '') return err(422, 'content is required');
        let content: Buffer;
        try {
          content = Buffer.from(contentB64, 'base64');
        } catch {
          return err(422, 'content is not valid base64');
        }
        const result = store.commitFile(repo, {
          path: contentPath,
          content,
          message: str(body.message, `Update ${contentPath}`),
          branch: str(body.branch, repo.defaultBranch),
          sha: typeof body.sha === 'string' ? body.sha : undefined,
        });
        if (!result.ok) return err(result.status, result.message);
        const blob = repo.objects.get(result.blobSha);
        return json(result.created ? 201 : 200, {
          content: {
            name: contentPath.split('/').pop() ?? contentPath,
            path: contentPath,
            sha: result.blobSha,
            size: content.length,
            encoding: 'base64',
            content: contentB64,
            url: '',
            html_url: '',
            git_url: '',
            download_url: null,
          },
          commit: { sha: result.commitSha, message: str(body.message, '') },
        });
      }
      if (method === 'DELETE') {
        const body = bodyOf(req);
        const result = store.deleteFile(repo, {
          path: contentPath,
          message: str(body.message, `Delete ${contentPath}`),
          branch: str(body.branch, repo.defaultBranch),
          sha: typeof body.sha === 'string' ? body.sha : undefined,
        });
        if (!result.ok) return err(result.status, result.message);
        return json(200, { commit: { sha: result.commitSha } });
      }
      return err(404, 'Not Found');
    }

    // Labels.
    if (tail.length === 1 && tail[0] === 'labels') {
      if (method === 'GET') return json(200, [...repo.labels.values()].map((label) => store.labelJson(label)));
      if (method === 'POST') {
        const body = bodyOf(req);
        const labelName = str(body.name);
        if (labelName === '') return err(422, 'name is required');
        const label = store.createLabel(repo, labelName, str(body.color, 'ededed'), str(body.description));
        return json(201, store.labelJson(label));
      }
      return err(404, 'Not Found');
    }

    // Issues.
    if (tail.length >= 1 && tail[0] === 'issues') {
      if (tail.length === 1) {
        if (method === 'GET') {
          const state = req.query.get('state') ?? 'open';
          let issues = store.listIssues(repo, state);
          const sort = req.query.get('sort') ?? 'created';
          const direction = req.query.get('direction') ?? 'desc';
          if (sort === 'created') {
            issues = [...issues].sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
          }
          const { page, link } = paginate(req, issues.map((issue) => store.issueJson(repo, issue)));
          return withLink(json(200, page), link);
        }
        if (method === 'POST') {
          const body = bodyOf(req);
          const title = str(body.title);
          if (title === '') return err(422, 'title is required');
          const created = store.createIssue(repo, title, str(body.body), labelNamesOf(body.labels));
          if ('error' in created) return err(422, created.error);
          return json(201, store.issueJson(repo, created));
        }
        return err(404, 'Not Found');
      }
      const number = Number(tail[1]);
      if (!Number.isInteger(number) || number < 1) return err(404, 'Not Found');
      const issue: Issue | undefined = repo.issues.get(number);
      if (tail.length === 2) {
        if (!issue) return err(404, 'Not Found');
        if (method === 'GET') return json(200, store.issueJson(repo, issue));
        if (method === 'PATCH') {
          const body = bodyOf(req);
          if (typeof body.title === 'string') issue.title = body.title;
          if (typeof body.body === 'string') issue.body = body.body;
          if (body.state === 'closed') store.closeIssue(repo, issue);
          else if (body.state === 'open') store.reopenIssue(repo, issue);
          if (Array.isArray(body.labels)) {
            const names = labelNamesOf(body.labels);
            const labels = [];
            for (const labelName of names) {
              const label = repo.labels.get(labelName);
              if (!label) return err(422, `Label does not exist: ${labelName}`);
              labels.push(label);
            }
            issue.labels = labels;
          }
          return json(200, store.issueJson(repo, issue));
        }
        return err(404, 'Not Found');
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (!issue) return err(404, 'Not Found');
        if (method === 'GET') {
          return json(200, issue.comments.map((comment) => store.commentJson(repo, issue, comment)));
        }
        if (method === 'POST') {
          const body = bodyOf(req);
          const commentBody = str(body.body);
          if (commentBody === '') return err(422, 'body is required');
          const comment = store.addComment(repo, issue, commentBody);
          return json(201, store.commentJson(repo, issue, comment));
        }
        return err(404, 'Not Found');
      }
      return err(404, 'Not Found');
    }

    // Pull requests.
    if (tail.length >= 1 && tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'GET') {
          const state = req.query.get('state') ?? 'open';
          let pulls = [...repo.pulls.values()];
          if (state !== 'all') pulls = pulls.filter((pr) => pr.state === state);
          pulls.sort((a, b) => b.number - a.number);
          const { page, link } = paginate(req, pulls.map((pr) => store.pullJson(repo, pr)));
          return withLink(json(200, page), link);
        }
        if (method === 'POST') {
          const body = bodyOf(req);
          const title = str(body.title);
          const head = stripBranchPrefix(str(body.head), owner);
          const base = stripBranchPrefix(str(body.base), owner);
          if (title === '' || head === '' || base === '') return err(422, 'title, head and base are required');
          const created = store.createPull(repo, title, str(body.body), head, base);
          if ('error' in created) return err(422, created.error);
          return json(201, store.pullJson(repo, created));
        }
        return err(404, 'Not Found');
      }
      const number = Number(tail[1]);
      if (!Number.isInteger(number) || number < 1) return err(404, 'Not Found');
      const pr = repo.pulls.get(number);
      if (tail.length === 2) {
        if (!pr) return err(404, 'Not Found');
        if (method === 'GET') return json(200, store.pullJson(repo, pr));
        return err(404, 'Not Found');
      }
      if (tail.length === 3 && tail[2] === 'files' && method === 'GET') {
        if (!pr) return err(404, 'Not Found');
        const files = store.prFiles(repo, pr).map((f) => ({
          sha: '',
          filename: f.path,
          status: f.status,
          additions: f.additions,
          deletions: f.deletions,
          changes: f.changes,
          blob_url: '',
          raw_url: '',
          contents_url: '',
          patch: '',
        }));
        return json(200, files);
      }
      if (tail.length === 3 && tail[2] === 'merge' && method === 'PUT') {
        if (!pr) return err(404, 'Not Found');
        const merged = store.mergePull(repo, pr);
        if (!merged.ok) return err(merged.status, merged.message);
        return json(200, { sha: merged.sha, merged: true, message: 'Pull request successfully merged' });
      }
      return err(404, 'Not Found');
    }

    void name;
    return err(404, 'Not Found');
  }

  return err(404, 'Not Found');
}
