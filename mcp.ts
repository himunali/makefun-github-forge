// Model Context Protocol over Streamable HTTP at /mcp: JSON-RPC 2.0 with
// plain JSON answers. Speaks the modern 2026-07-28 era (server/discover)
// and the legacy handshake (initialize + notifications/initialized), and
// offers the six github-mcp-server tools over the same store REST uses.

import type { Store } from './store.ts';

export interface McpRequest {
  method: string;
  bodyText: string;
  body: unknown;
  headers: Record<string, string>;
}

export interface McpResponse {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

const SERVER_NAME = 'makefun-github-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        owner: { type: 'string', description: 'The owner login (defaults to the authenticated user).' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
        full_name: { type: 'string', description: 'The full owner/repo name.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        full_name: { type: 'string', description: 'The full owner/repo name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
        full_name: { type: 'string', description: 'The full owner/repo name.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Filter by state: open, closed or all.' },
        full_name: { type: 'string', description: 'The full owner/repo name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
        full_name: { type: 'string', description: 'The full owner/repo name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request and close it.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
        number: { type: 'integer', description: 'Alias for pull_number.' },
        full_name: { type: 'string', description: 'The full owner/repo name.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

/** Split owner/repo out of a tool call's arguments (full_name included). */
function ownerRepo(args: Record<string, unknown>): { owner: string; repo: string } {
  let owner = str(args.owner ?? args.owner_login ?? args.login);
  let repo = str(args.repo ?? args.repository ?? args.repository_name ?? args.repo_name);
  const fullName = str(args.full_name ?? args.repo_full_name ?? args.repository_full_name);
  if ((owner === '' || repo === '') && fullName.includes('/')) {
    const [o, r] = fullName.split('/', 2);
    if (owner === '') owner = o ?? '';
    if (repo === '') repo = r ?? '';
  }
  return { owner, repo };
}

function toolResult(obj: unknown): Record<string, unknown> {
  const text = JSON.stringify(obj);
  return { content: [{ type: 'text', text }], structuredContent: obj, isError: false };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const normalized = name.toLowerCase().replace(/[^a-z0-9]/g, '');
  const match = (canonical: string): boolean =>
    normalized === canonical || normalized.endsWith(canonical);
  if (match('createrepo')) {
    const repoName = str(args.name ?? args.repo ?? args.repository);
    if (repoName === '') return toolError('Missing required argument: name');
    const description = str(args.description ?? args.body);
    const isPrivate = typeof args.private === 'boolean' ? args.private : false;
    const existing = store.repoByName(repoName);
    if (existing) return toolResult(store.repoJson(existing));
    const created = store.createRepo(repoName, description, isPrivate);
    if (!created) return toolError('Repository already exists');
    return toolResult(store.repoJson(created));
  }
  if (match('getrepo')) {
    const { owner, repo: repoName } = ownerRepo(args);
    if (owner === '' || repoName === '') return toolError('Missing required arguments: owner, repo');
    const repo = store.resolveRepo(owner, repoName);
    if (!repo) return toolError(`Repository not found: ${owner}/${repoName}`);
    return toolResult(store.repoJson(repo));
  }
  if (match('createissue')) {
    const { owner, repo: repoName } = ownerRepo(args);
    const title = str(args.title ?? args.issue_title);
    if (owner === '' || repoName === '' || title === '') {
      return toolError('Missing required arguments: owner, repo, title');
    }
    const repo = store.resolveRepo(owner, repoName);
    if (!repo) return toolError(`Repository not found: ${owner}/${repoName}`);
    const created = store.createIssue(repo, title, str(args.body ?? args.issue_body ?? args.description), []);
    if ('error' in created) return toolError(created.error);
    return toolResult(store.issueJson(repo, created));
  }
  if (match('listissues')) {
    const { owner, repo: repoName } = ownerRepo(args);
    if (owner === '' || repoName === '') return toolError('Missing required arguments: owner, repo');
    const repo = store.resolveRepo(owner, repoName);
    if (!repo) return toolError(`Repository not found: ${owner}/${repoName}`);
    const state = str(args.state ?? args.status, 'open');
    const issues = store.listIssues(repo, state === '' ? 'open' : state);
    const list = issues.map((issue) => store.issueJson(repo, issue));
    return toolResult({ issues: list });
  }
  if (match('createpullrequest')) {
    const { owner, repo: repoName } = ownerRepo(args);
    const title = str(args.title ?? args.pr_title);
    const head = str(args.head ?? args.head_branch ?? args.head_ref);
    const base = str(args.base ?? args.base_branch ?? args.base_ref);
    if (owner === '' || repoName === '' || title === '' || head === '' || base === '') {
      return toolError('Missing required arguments: owner, repo, title, head, base');
    }
    const repo = store.resolveRepo(owner, repoName);
    if (!repo) return toolError(`Repository not found: ${owner}/${repoName}`);
    const created = store.createPull(repo, title, str(args.body ?? args.description), head, base);
    if ('error' in created) return toolError(created.error);
    return toolResult(store.pullJson(repo, created));
  }
  if (match('mergepullrequest')) {
    const { owner, repo: repoName } = ownerRepo(args);
    const raw = args.pull_number ?? args.pull_request_number ?? args.pr_number ?? args.number;
    const number = typeof raw === 'number' ? raw : Number(raw);
    if (owner === '' || repoName === '' || !Number.isInteger(number) || number < 1) {
      return toolError('Missing required arguments: owner, repo, pull_number');
    }
    const repo = store.resolveRepo(owner, repoName);
    if (!repo) return toolError(`Repository not found: ${owner}/${repoName}`);
    const pr = repo.pulls.get(number);
    if (!pr) return toolError(`Pull request not found: ${number}`);
    const merged = store.mergePull(repo, pr);
    if (!merged.ok) return toolError(merged.message);
    return toolResult({ sha: merged.sha, merged: true, message: 'Pull request successfully merged', number });
  }
  return toolError(`Unknown tool: ${name}`);
}

export function handleMcp(store: Store, req: McpRequest): McpResponse {
  if (req.method === 'DELETE') {
    return { status: 200, body: {} };
  }
  if (req.method !== 'POST') {
    return { status: 405, body: { message: 'Method Not Allowed' } };
  }
  let payload: unknown;
  try {
    payload = JSON.parse(req.bodyText);
  } catch {
    return {
      status: 400,
      body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error: request body is not valid JSON' } },
    };
  }
  if (payload === null || typeof payload !== 'object' || Array.isArray(payload)) {
    return {
      status: 400,
      body: { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request: expected a JSON-RPC object' } },
    };
  }
  const rec = payload as Record<string, unknown>;
  const method = str(rec.method);
  const id = rec.id === undefined ? null : (rec.id as string | number | null);
  const params = isRecord(rec.params) ? rec.params : {};
  const ok = (result: unknown, extraHeaders?: Record<string, string>): McpResponse => ({
    status: 200,
    body: { jsonrpc: '2.0', id, result },
    headers: extraHeaders,
  });
  const rpcErr = (code: number, message: string, status = 200): McpResponse => ({
    status,
    body: { jsonrpc: '2.0', id, error: { code, message } },
  });
  // Notifications carry no id and expect no JSON-RPC answer.
  if (id === null && method.startsWith('notifications/')) {
    return { status: 202, body: null };
  }
  switch (method) {
    case 'server/discover':
      return ok({
        resultType: 'discover',
        supportedVersions: SUPPORTED_VERSIONS,
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
        capabilities: { tools: {}, resources: {}, prompts: {} },
        instructions: 'A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools.',
      });
    case 'initialize': {
      const requested = str((params as Record<string, unknown>).protocolVersion);
      const negotiated = SUPPORTED_VERSIONS.includes(requested) ? requested : '2025-11-25';
      return ok(
        {
          protocolVersion: negotiated,
          serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
          capabilities: { tools: {} },
        },
        { 'mcp-session-id': 'mcp-session-1' },
      );
    }
    case 'ping':
      return ok({});
    case 'tools/list':
      return ok({ tools: TOOLS });
    case 'tools/call': {
      const toolName = str(params.name);
      const args = isRecord(params.arguments) ? params.arguments : {};
      const known = TOOLS.some((tool) => {
        const n = tool.name.replace(/[^a-z0-9]/g, '');
        const given = toolName.toLowerCase().replace(/[^a-z0-9]/g, '');
        return given === n || given.endsWith(n);
      });
      if (!known) return rpcErr(-32602, `Unknown tool: ${toolName}`);
      return ok(callTool(store, toolName, args));
    }
    default:
      return rpcErr(-32601, `Method not found: ${method}`);
  }
}
