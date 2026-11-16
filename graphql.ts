// GitHub GraphQL v4: a small parser for the documents gh and
// github-mcp-server actually send, an executor over the same store REST
// uses, and the __type introspection those clients feature-detect with.
// Supports queries, mutations, variables, aliases, inline fragments,
// input objects, enums, nulls, lists, and batched (array) request bodies.

import { isoFromSec, type Issue, type PullRequest, type Repo, type Store } from './store.ts';

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

interface Selection {
  name: string;
  alias: string | null;
  args: Record<string, unknown>;
  selections: Selection[];
  fragmentType: string | null;
}

interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: Record<string, { type: string; defaultValue: unknown }>;
  selections: Selection[];
}

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'int'; value: string }
  | { kind: 'float'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'punct'; value: string };

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i] as string;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '"') {
      let value = '';
      i += 1;
      while (i < src.length && src[i] !== '"') {
        if (src[i] === '\\' && i + 1 < src.length) {
          const esc = src[i + 1] as string;
          if (esc === 'n') value += '\n';
          else if (esc === 't') value += '\t';
          else if (esc === 'r') value += '\r';
          else value += esc;
          i += 2;
        } else {
          value += src[i];
          i += 1;
        }
      }
      i += 1;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (/[0-9-]/.test(ch) && (ch !== '-' || /[0-9]/.test(src[i + 1] ?? ''))) {
      let j = i;
      if (src[j] === '-') j += 1;
      while (j < src.length && /[0-9]/.test(src[j] as string)) j += 1;
      let isFloat = false;
      if (src[j] === '.') {
        isFloat = true;
        j += 1;
        while (j < src.length && /[0-9]/.test(src[j] as string)) j += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if (/[_A-Za-z]/.test(ch)) {
      let j = i;
      while (j < src.length && /[_A-Za-z0-9]/.test(src[j] as string)) j += 1;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if (ch === '.' && src.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('!$&():=[]{}|'.includes(ch) || ch === '@') {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} in GraphQL document`);
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const token = this.tokens[this.pos];
    if (!token) throw new Error('unexpected end of GraphQL document');
    this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) {
      throw new Error(`expected ${value} in GraphQL document, got ${token.kind} ${JSON.stringify(token.value)}`);
    }
  }

  private acceptPunct(value: string): boolean {
    const token = this.peek();
    if (token && token.kind === 'punct' && token.value === value) {
      this.pos += 1;
      return true;
    }
    return false;
  }

  parseDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.pos < this.tokens.length) {
      operations.push(this.parseOperation());
    }
    return operations;
  }

  private parseOperation(): Operation {
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const first = this.peek();
    if (first && first.kind === 'name' && (first.value === 'query' || first.value === 'mutation')) {
      kind = first.value;
      this.next();
      const maybeName = this.peek();
      if (maybeName && maybeName.kind === 'name') {
        name = maybeName.value;
        this.next();
      }
    }
    const variables: Record<string, { type: string; defaultValue: unknown }> = {};
    if (this.acceptPunct('(')) {
      while (!this.acceptPunct(')')) {
        const dollar = this.next();
        if (dollar.kind !== 'punct' || dollar.value !== '$') throw new Error('expected $ in variable definition');
        const varName = this.next();
        if (varName.kind !== 'name') throw new Error('expected a variable name');
        this.expectPunct(':');
        const type = this.parseType();
        let defaultValue: unknown = undefined;
        if (this.acceptPunct('=')) defaultValue = this.parseValueLiteral(null);
        variables[varName.value] = { type, defaultValue };
      }
    }
    const selections = this.parseSelectionSet();
    return { kind, name, variables, selections };
  }

  private parseType(): string {
    const token = this.next();
    let type: string;
    if (token.kind === 'punct' && token.value === '[') {
      type = `[${this.parseType()}]`;
    } else if (token.kind === 'name') {
      type = token.value;
    } else {
      throw new Error('expected a type in GraphQL document');
    }
    if (this.acceptPunct('!')) type += '!';
    return type;
  }

  private parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (!this.acceptPunct('}')) {
      selections.push(this.parseSelection());
    }
    return selections;
  }

  private parseSelection(): Selection {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '...') {
      let fragmentType: string | null = null;
      const next = this.peek();
      if (next && next.kind === 'name' && next.value === 'on') {
        this.next();
        const typeName = this.next();
        if (typeName.kind !== 'name') throw new Error('expected a type name after on');
        fragmentType = typeName.value;
      } else if (next && next.kind === 'name') {
        // Named fragment spread: unsupported by the documents we serve; treat as a
        // fragment on nothing so the fields are skipped rather than crashing.
        this.next();
      }
      const selections = this.parseSelectionSet();
      return { name: '', alias: null, args: {}, selections, fragmentType };
    }
    if (token.kind !== 'name') throw new Error('expected a field name in GraphQL document');
    let name = token.value;
    let alias: string | null = null;
    if (this.acceptPunct(':')) {
      alias = name;
      const real = this.next();
      if (real.kind !== 'name') throw new Error('expected a field name after an alias');
      name = real.value;
    }
    const args: Record<string, unknown> = {};
    if (this.acceptPunct('(')) {
      while (!this.acceptPunct(')')) {
        const argName = this.next();
        if (argName.kind !== 'name') throw new Error('expected an argument name');
        this.expectPunct(':');
        args[argName.value] = this.parseValueLiteral(null);
      }
    }
    // Directives (@skip/@include) are not sent by the suite's documents; skip them.
    while (this.acceptPunct('@')) {
      const dir = this.next();
      if (dir.kind !== 'name') throw new Error('expected a directive name');
      if (this.acceptPunct('(')) {
        while (!this.acceptPunct(')')) {
          const argName = this.next();
          this.expectPunct(':');
          this.parseValueLiteral(null);
          void argName;
        }
      }
    }
    let selections: Selection[] = [];
    const next = this.peek();
    if (next && next.kind === 'punct' && next.value === '{') {
      selections = this.parseSelectionSet();
    }
    return { name, alias, args, selections, fragmentType: null };
  }

  private parseValueLiteral(variables: Record<string, unknown> | null): unknown {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') {
      const nameToken = this.next();
      if (nameToken.kind !== 'name') throw new Error('expected a variable name');
      if (variables && nameToken.value in variables) return variables[nameToken.value];
      return { __variable: nameToken.value };
    }
    if (token.kind === 'string') return token.value;
    if (token.kind === 'int') return Number(token.value);
    if (token.kind === 'float') return Number(token.value);
    if (token.kind === 'name') {
      if (token.value === 'true') return true;
      if (token.value === 'false') return false;
      if (token.value === 'null') return null;
      return token.value; // an enum value
    }
    if (token.kind === 'punct' && token.value === '[') {
      const list: unknown[] = [];
      while (!this.acceptPunct(']')) list.push(this.parseValueLiteral(variables));
      return list;
    }
    if (token.kind === 'punct' && token.value === '{') {
      const obj: Record<string, unknown> = {};
      while (!this.acceptPunct('}')) {
        const key = this.next();
        if (key.kind !== 'name') throw new Error('expected an object key');
        this.expectPunct(':');
        obj[key.value] = this.parseValueLiteral(variables);
      }
      return obj;
    }
    throw new Error(`unexpected value token ${token.kind} ${JSON.stringify(token.value)}`);
  }
}

/** Replace {__variable: name} markers with the operation's variable values. */
function resolveVariables(value: unknown, variables: Record<string, unknown>): unknown {
  if (Array.isArray(value)) return value.map((v) => resolveVariables(v, variables));
  if (value !== null && typeof value === 'object') {
    const rec = value as Record<string, unknown>;
    if (typeof rec.__variable === 'string') return variables[rec.__variable] ?? null;
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(rec)) out[k] = resolveVariables(v, variables);
    return out;
  }
  return value;
}

// ---------------------------------------------------------------------------
// schema (for __type introspection)
// ---------------------------------------------------------------------------

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'body', 'state', 'url', 'createdAt', 'updatedAt', 'closedAt', 'author', 'labels', 'comments', 'repository'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'files', 'url', 'createdAt', 'updatedAt', 'mergedAt', 'mergeCommitSha', 'author'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'url', 'description', 'createdAt', 'updatedAt'],
  User: ['id', 'login', 'name', 'email', 'url'],
  SearchType: [],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo'],
  PullRequestChangedFile: ['path', 'filename', 'additions', 'deletions', 'changes', 'status'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  Ref: ['name', 'target'],
};

const ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'COMMIT', 'DISCUSSION', 'CODE'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
};

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

type CtxValue =
  | { __type: 'query' }
  | { __type: 'repository'; repo: Repo }
  | { __type: 'issue'; repo: Repo; issue: Issue }
  | { __type: 'pull'; repo: Repo; pull: PullRequest }
  | { __type: 'user'; login: string }
  | { __type: 'issues'; repo: Repo; list: Issue[] }
  | { __type: 'pulls'; repo: Repo; list: PullRequest[] }
  | { __type: 'files'; repo: Repo; pull: PullRequest }
  | { __type: 'comment'; repo: Repo; issue: Issue; commentId: number; body: string }
  | { __type: 'ref'; name: string }
  | { __type: 'pageInfo'; hasNextPage: boolean; endCursor: string | null; startCursor: string | null };

class Executor {
  errors: Array<{ message: string }> = [];
  private readonly store: Store;
  constructor(store: Store) {
    this.store = store;
  }

  execute(op: Operation, variables: Record<string, unknown>): Record<string, unknown> {
    const data: Record<string, unknown> = {};
    for (const selection of op.selections) {
      const value = this.resolveRoot(selection, variables);
      data[selection.alias ?? selection.name] = value;
    }
    return data;
  }

  private fail(message: string): null {
    this.errors.push({ message });
    return null;
  }

  private resolveRoot(sel: Selection, variables: Record<string, unknown>): unknown {
    const args = resolveVariables(sel.args, variables) as Record<string, unknown>;
    switch (sel.name) {
      case 'viewer': {
        const login = this.store.viewerLogin;
        return this.resolveFields({ __type: 'user', login: login ?? '' }, sel.selections, variables);
      }
      case 'repository': {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        const repo = this.store.resolveRepo(owner, name);
        if (!repo) return null;
        return this.resolveFields({ __type: 'repository', repo }, sel.selections, variables);
      }
      case 'node': {
        const id = String(args.id ?? '');
        const decoded = this.store.decodeGlobalId(id);
        if (!decoded) return null;
        if (decoded.kind === 'Repository') {
          const repo = this.store.reposById.get(Number(decoded.parts[0]));
          if (!repo) return null;
          return this.resolveFields({ __type: 'repository', repo }, sel.selections, variables, 'Repository');
        }
        if (decoded.kind === 'Issue') {
          const repo = this.store.reposById.get(Number(decoded.parts[0]));
          const issue = repo?.issues.get(Number(decoded.parts[1]));
          if (!repo || !issue) return null;
          return this.resolveFields({ __type: 'issue', repo, issue }, sel.selections, variables, 'Issue');
        }
        if (decoded.kind === 'PullRequest') {
          const repo = this.store.reposById.get(Number(decoded.parts[0]));
          const pull = repo?.pulls.get(Number(decoded.parts[1]));
          if (!repo || !pull) return null;
          return this.resolveFields({ __type: 'pull', repo, pull }, sel.selections, variables, 'PullRequest');
        }
        return null;
      }
      case '__type': {
        const name = String(args.name ?? '');
        const fields = TYPE_FIELDS[name];
        const enumValues = ENUM_VALUES[name];
        if (fields === undefined && enumValues === undefined) return null;
        const typeObj: Record<string, unknown> = { name, kind: enumValues !== undefined ? 'ENUM' : 'OBJECT' };
        return this.resolveTypeFields(typeObj, fields ?? [], enumValues ?? [], sel.selections);
      }
      case '__schema':
        return this.resolveFields({ __type: 'query' }, sel.selections, variables);
      case 'createIssue':
        return this.mutateCreateIssue(sel, args, variables);
      case 'closeIssue':
        return this.mutateCloseIssue(sel, args, variables);
      case 'createPullRequest':
        return this.mutateCreatePullRequest(sel, args, variables);
      case 'mergePullRequest':
        return this.mutateMergePullRequest(sel, args, variables);
      case 'addComment':
        return this.mutateAddComment(sel, args, variables);
      default:
        return this.fail(`Cannot query field "${sel.name}" on type "Query".`);
    }
  }

  private resolveTypeFields(
    typeObj: Record<string, unknown>,
    fields: string[],
    enumValues: string[],
    selections: Selection[],
  ): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      const key = sel.alias ?? sel.name;
      if (sel.name === 'name') out[key] = typeObj.name;
      else if (sel.name === 'kind') out[key] = typeObj.kind;
      else if (sel.name === 'fields') {
        out[key] = fields.map((name) => {
          const fieldObj: Record<string, unknown> = { name };
          for (const sub of sel.selections) {
            if (sub.name === 'name') fieldObj[sub.alias ?? 'name'] = name;
            else if (sub.name === 'description') fieldObj[sub.alias ?? 'description'] = null;
          }
          return fieldObj;
        });
      } else if (sel.name === 'enumValues') {
        out[key] = enumValues.map((name) => {
          const enumObj: Record<string, unknown> = { name };
          for (const sub of sel.selections) {
            if (sub.name === 'name') enumObj[sub.alias ?? 'name'] = name;
            else if (sub.name === 'description') enumObj[sub.alias ?? 'description'] = null;
          }
          return enumObj;
        });
      } else {
        this.fail(`Cannot query field "${sel.name}" on type "__Type".`);
        out[key] = null;
      }
    }
    return out;
  }

  /** Resolve the sub-selections of one object value. */
  private resolveFields(value: CtxValue, selections: Selection[], variables: Record<string, unknown>, fragmentType?: string): unknown {
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      if (sel.name === '' && sel.fragmentType !== null) {
        // Inline fragment: apply only when the type matches (or unconditionally for `... on Node`).
        const typeName = typeNameOf(value);
        if (fragmentType !== undefined && sel.fragmentType !== fragmentType) continue;
        if (sel.fragmentType !== typeName && sel.fragmentType !== 'Node') continue;
        const nested = this.resolveFields(value, sel.selections, variables) as Record<string, unknown>;
        Object.assign(out, nested);
        continue;
      }
      if (sel.name === '') continue;
      out[sel.alias ?? sel.name] = this.resolveField(value, sel, variables);
    }
    return out;
  }

  private resolveField(value: CtxValue, sel: Selection, variables: Record<string, unknown>): unknown {
    const args = resolveVariables(sel.args, variables) as Record<string, unknown>;
    const sub = sel.selections;
    const hasSub = sub.length > 0;
    switch (value.__type) {
      case 'query':
        return this.fail(`Cannot query field "${sel.name}" on type "Query".`);
      case 'user': {
        if (sel.name === 'login') return value.login;
        if (sel.name === 'id') return this.store.globalId('User', value.login);
        if (sel.name === 'name') return value.login;
        return this.fail(`Cannot query field "${sel.name}" on type "User".`);
      }
      case 'repository': {
        const repo = value.repo;
        switch (sel.name) {
          case 'id': return this.store.repoGlobalId(repo);
          case 'databaseId': return repo.id;
          case 'name': return repo.name;
          case 'nameWithOwner': return `${this.store.ownerLogin(repo)}/${repo.name}`;
          case 'description': return repo.description;
          case 'url': return `https://example.invalid/${this.store.ownerLogin(repo)}/${repo.name}`;
          case 'createdAt': return repo.createdAt;
          case 'updatedAt': return repo.updatedAt;
          case 'owner': return hasSub ? this.resolveFields({ __type: 'user', login: this.store.ownerLogin(repo) }, sub, variables) : this.store.ownerLogin(repo);
          case 'defaultBranchRef': {
            if (!hasSub) return { name: repo.defaultBranch };
            return this.resolveFields({ __type: 'ref', name: repo.defaultBranch }, sub, variables);
          }
          case 'viewerPermission': return 'ADMIN';
          case 'hasIssuesEnabled': return true;
          case 'issue': {
            const issue = repo.issues.get(Number(args.number));
            if (!issue) return null;
            return this.resolveFields({ __type: 'issue', repo, issue }, sub, variables);
          }
          case 'issues': return this.issuesConnection(repo, args, sub, variables);
          case 'pullRequests': return this.pullsConnection(repo, args, sub, variables);
          case 'pullRequest': {
            const pull = repo.pulls.get(Number(args.number));
            if (!pull) return null;
            return this.resolveFields({ __type: 'pull', repo, pull }, sub, variables);
          }
          default:
            return this.fail(`Cannot query field "${sel.name}" on type "Repository".`);
        }
      }
      case 'issue': {
        const issue = value.issue;
        switch (sel.name) {
          case 'id': return this.store.issueGlobalId(value.repo, issue);
          case 'number': return issue.number;
          case 'title': return issue.title;
          case 'body': return issue.body;
          case 'state': return issue.state.toUpperCase();
          case 'url': return `https://example.invalid/${this.store.ownerLogin(value.repo)}/${value.repo.name}/issues/${issue.number}`;
          case 'createdAt': return issue.createdAt;
          case 'updatedAt': return issue.updatedAt;
          case 'closedAt': return issue.closedAt;
          case 'repository': return hasSub ? this.resolveFields({ __type: 'repository', repo: value.repo }, sub, variables) : null;
          case 'labels': return issue.labels.map((l) => l.name);
          case 'comments': return issue.comments.length;
          default:
            return this.fail(`Cannot query field "${sel.name}" on type "Issue".`);
        }
      }
      case 'pull': {
        const pull = value.pull;
        switch (sel.name) {
          case 'id': return this.store.pullGlobalId(value.repo, pull);
          case 'number': return pull.number;
          case 'title': return pull.title;
          case 'body': return pull.body;
          case 'state': return pull.merged ? 'MERGED' : pull.state.toUpperCase();
          case 'merged': return pull.merged;
          case 'mergeable': return this.store.prMergeable(value.repo, pull);
          case 'headRefName': return pull.head;
          case 'baseRefName': return pull.base;
          case 'headRef': return hasSub ? this.resolveFields({ __type: 'ref', name: pull.head }, sub, variables) : { name: pull.head };
          case 'baseRef': return hasSub ? this.resolveFields({ __type: 'ref', name: pull.base }, sub, variables) : { name: pull.base };
          case 'url': return `https://example.invalid/${this.store.ownerLogin(value.repo)}/${value.repo.name}/pull/${pull.number}`;
          case 'createdAt': return pull.createdAt;
          case 'updatedAt': return pull.updatedAt;
          case 'mergedAt': return pull.mergedAt;
          case 'mergeCommitSha': return pull.mergeCommitSha;
          case 'files': return this.filesConnection(value.repo, pull, args, sub, variables);
          default:
            return this.fail(`Cannot query field "${sel.name}" on type "PullRequest".`);
        }
      }
      case 'ref': {
        if (sel.name === 'name') return value.name;
        return this.fail(`Cannot query field "${sel.name}" on type "Ref".`);
      }
      case 'issues':
      case 'pulls':
      case 'files': {
        return this.resolveConnection(value, sel, variables);
      }
      case 'comment': {
        if (sel.name === 'id') return this.store.globalId('IssueComment', value.repo.id, value.issue.number, value.commentId);
        if (sel.name === 'body') return value.body;
        return this.fail(`Cannot query field "${sel.name}" on type "IssueComment".`);
      }
      case 'pageInfo': {
        if (sel.name === 'hasNextPage') return value.hasNextPage;
        if (sel.name === 'hasPreviousPage') return false;
        if (sel.name === 'startCursor') return value.startCursor;
        if (sel.name === 'endCursor') return value.endCursor;
        return this.fail(`Cannot query field "${sel.name}" on type "PageInfo".`);
      }
      default:
        return this.fail(`Cannot query field "${sel.name}".`);
    }
  }

  private resolveConnection(value: CtxValue, sel: Selection, variables: Record<string, unknown>): unknown {
    if (value.__type === 'issues' || value.__type === 'pulls' || value.__type === 'files') {
      // A connection object asked for a field directly (nodes/pageInfo are
      // handled by the connection builders, which return plain objects).
      return this.fail(`Cannot query field "${sel.name}" on a connection.`);
    }
    return null;
  }

  private connectionObject(
    nodes: Array<CtxValue>,
    pageInfo: { hasNextPage: boolean; endCursor: string | null; startCursor: string | null },
    selections: Selection[],
    variables: Record<string, unknown>,
  ): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      const key = sel.alias ?? sel.name;
      if (sel.name === 'nodes') {
        out[key] = nodes.map((node) => this.resolveFields(node, sel.selections, variables));
      } else if (sel.name === 'edges') {
        out[key] = nodes.map((node) => {
          const edge: Record<string, unknown> = {};
          for (const sub of sel.selections) {
            if (sub.name === 'node') edge[sub.alias ?? 'node'] = this.resolveFields(node, sub.selections, variables);
            else if (sub.name === 'cursor') edge[sub.alias ?? 'cursor'] = '';
          }
          return edge;
        });
      } else if (sel.name === 'pageInfo') {
        out[key] = this.resolveFields({ __type: 'pageInfo', ...pageInfo }, sel.selections, variables);
      } else if (sel.name === 'totalCount') {
        out[key] = nodes.length;
      } else {
        this.fail(`Cannot query field "${sel.name}" on a connection.`);
        out[key] = null;
      }
    }
    return out;
  }

  private issuesConnection(repo: Repo, args: Record<string, unknown>, selections: Selection[], variables: Record<string, unknown>): unknown {
    let list = [...repo.issues.values()];
    const states = args.states;
    if (Array.isArray(states) && states.length > 0) {
      const wanted = new Set(states.map((s) => String(s).toUpperCase()));
      list = list.filter((issue) => wanted.has(issue.state.toUpperCase()));
    }
    const orderBy = args.orderBy;
    if (orderBy !== null && typeof orderBy === 'object') {
      const ob = orderBy as Record<string, unknown>;
      const field = String(ob.field ?? 'CREATED_AT');
      const direction = String(ob.direction ?? 'ASC');
      if (field === 'CREATED_AT') {
        list.sort((a, b) => (direction === 'DESC' ? b.number - a.number : a.number - b.number));
      }
    } else {
      list.sort((a, b) => a.number - b.number);
    }
    const first = typeof args.first === 'number' ? args.first : list.length;
    const after = typeof args.after === 'string' ? decodeCursor(args.after) : null;
    let start = 0;
    if (after !== null) {
      const idx = list.findIndex((issue) => issue.number === after);
      start = idx >= 0 ? idx + 1 : 0;
    }
    const sliced = list.slice(start, start + first);
    const hasNextPage = start + first < list.length;
    const endCursor = sliced.length > 0 ? encodeCursor((sliced[sliced.length - 1] as Issue).number) : null;
    const nodes: CtxValue[] = sliced.map((issue) => ({ __type: 'issue', repo, issue }));
    return this.connectionObject(nodes, { hasNextPage, endCursor, startCursor: sliced.length > 0 ? encodeCursor((sliced[0] as Issue).number) : null }, selections, variables);
  }

  private pullsConnection(repo: Repo, args: Record<string, unknown>, selections: Selection[], variables: Record<string, unknown>): unknown {
    let list = [...repo.pulls.values()];
    const states = args.states;
    if (Array.isArray(states) && states.length > 0) {
      const wanted = new Set(states.map((s) => String(s).toUpperCase()));
      list = list.filter((pr) => wanted.has(pr.merged ? 'MERGED' : pr.state.toUpperCase()));
    } else {
      list = list.filter((pr) => pr.state === 'open');
    }
    list.sort((a, b) => b.number - a.number);
    const first = typeof args.first === 'number' ? args.first : list.length;
    const after = typeof args.after === 'string' ? decodeCursor(args.after) : null;
    let start = 0;
    if (after !== null) {
      const idx = list.findIndex((pr) => pr.number === after);
      start = idx >= 0 ? idx + 1 : 0;
    }
    const sliced = list.slice(start, start + first);
    const hasNextPage = start + first < list.length;
    const endCursor = sliced.length > 0 ? encodeCursor((sliced[sliced.length - 1] as PullRequest).number) : null;
    const nodes: CtxValue[] = sliced.map((pull) => ({ __type: 'pull', repo, pull }));
    return this.connectionObject(nodes, { hasNextPage, endCursor, startCursor: sliced.length > 0 ? encodeCursor((sliced[0] as PullRequest).number) : null }, selections, variables);
  }

  private filesConnection(repo: Repo, pull: PullRequest, args: Record<string, unknown>, selections: Selection[], variables: Record<string, unknown>): unknown {
    const files = this.store.prFiles(repo, pull);
    const first = typeof args.first === 'number' ? args.first : files.length;
    const sliced = files.slice(0, first);
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      const key = sel.alias ?? sel.name;
      if (sel.name === 'nodes') {
        out[key] = sliced.map((file) => {
          const node: Record<string, unknown> = {};
          for (const sub of sel.selections) {
            const subKey = sub.alias ?? sub.name;
            if (sub.name === 'path') node[subKey] = file.path;
            else if (sub.name === 'filename') node[subKey] = file.path;
            else if (sub.name === 'additions') node[subKey] = file.additions;
            else if (sub.name === 'deletions') node[subKey] = file.deletions;
            else if (sub.name === 'changes') node[subKey] = file.changes;
            else if (sub.name === 'status') node[subKey] = file.status;
            else {
              this.fail(`Cannot query field "${sub.name}" on type "PullRequestChangedFile".`);
              node[subKey] = null;
            }
          }
          return node;
        });
      } else if (sel.name === 'pageInfo') {
        out[key] = this.resolveFields({ __type: 'pageInfo', hasNextPage: first < files.length, endCursor: null, startCursor: null }, sel.selections, variables);
      } else if (sel.name === 'totalCount') {
        out[key] = files.length;
      } else {
        this.fail(`Cannot query field "${sel.name}" on type "PullRequestChangedFileConnection".`);
        out[key] = null;
      }
    }
    return out;
  }

  // ------------------------------------------------------------------ mutations

  private mutationPayload(
    sel: Selection,
    payload: Record<string, unknown> | null,
    variables: Record<string, unknown>,
    errorMessage?: string,
  ): unknown {
    if (errorMessage !== undefined) {
      this.fail(errorMessage);
      return null;
    }
    if (payload === null) return null;
    const out: Record<string, unknown> = {};
    for (const sub of sel.selections) {
      const key = sub.alias ?? sub.name;
      const value = payload[sub.name];
      if (value === undefined) {
        this.fail(`Cannot query field "${sub.name}" on a mutation payload.`);
        out[key] = null;
        continue;
      }
      if (value !== null && typeof value === 'object' && '___ctx' in (value as Record<string, unknown>)) {
        const ctx = (value as Record<string, unknown>).___ctx as CtxValue;
        out[key] = this.resolveFields(ctx, sub.selections, variables);
      } else if (value !== null && typeof value === 'object' && '___edge' in (value as Record<string, unknown>)) {
        const edge = (value as Record<string, unknown>).___edge as { ctx: CtxValue };
        const edgeOut: Record<string, unknown> = {};
        for (const edgeSel of sub.selections) {
          if (edgeSel.name === 'node') edgeOut[edgeSel.alias ?? 'node'] = this.resolveFields(edge.ctx, edgeSel.selections, variables);
          else if (edgeSel.name === 'cursor') edgeOut[edgeSel.alias ?? 'cursor'] = '';
          else {
            this.fail(`Cannot query field "${edgeSel.name}" on an edge.`);
            edgeOut[edgeSel.alias ?? edgeSel.name] = null;
          }
        }
        out[key] = edgeOut;
      } else {
        out[key] = value;
      }
    }
    return out;
  }

  private mutateCreateIssue(sel: Selection, args: Record<string, unknown>, variables: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const decoded = this.store.decodeGlobalId(String(input.repositoryId ?? ''));
    const repo = decoded?.kind === 'Repository' ? this.store.reposById.get(Number(decoded.parts[0])) : undefined;
    if (!repo) return this.mutationPayload(sel, null, variables, 'Repository not found.');
    const created = this.store.createIssue(repo, String(input.title ?? ''), String(input.body ?? ''), []);
    if ('error' in created) return this.mutationPayload(sel, null, variables, created.error);
    return this.mutationPayload(
      sel,
      { issue: { ___ctx: { __type: 'issue', repo, issue: created } as CtxValue } },
      variables,
    );
  }

  private mutateCloseIssue(sel: Selection, args: Record<string, unknown>, variables: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const decoded = this.store.decodeGlobalId(String(input.issueId ?? ''));
    const repo = decoded?.kind === 'Issue' ? this.store.reposById.get(Number(decoded.parts[0])) : undefined;
    const issue = repo?.issues.get(Number(decoded?.parts[1] ?? NaN));
    if (!repo || !issue) return this.mutationPayload(sel, null, variables, 'Issue not found.');
    this.store.closeIssue(repo, issue);
    return this.mutationPayload(
      sel,
      { issue: { ___ctx: { __type: 'issue', repo, issue } as CtxValue } },
      variables,
    );
  }

  private mutateCreatePullRequest(sel: Selection, args: Record<string, unknown>, variables: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const decoded = this.store.decodeGlobalId(String(input.repositoryId ?? ''));
    const repo = decoded?.kind === 'Repository' ? this.store.reposById.get(Number(decoded.parts[0])) : undefined;
    if (!repo) return this.mutationPayload(sel, null, variables, 'Repository not found.');
    const head = String(input.headRefName ?? '');
    const base = String(input.baseRefName ?? '');
    const created = this.store.createPull(repo, String(input.title ?? ''), String(input.body ?? ''), head, base);
    if ('error' in created) {
      // An open PR for the same head/base already exists: answer it instead.
      const existing = [...repo.pulls.values()].find((pr) => pr.state === 'open' && pr.head === head && pr.base === base);
      if (existing) {
        return this.mutationPayload(
          sel,
          { pullRequest: { ___ctx: { __type: 'pull', repo, pull: existing } as CtxValue } },
          variables,
        );
      }
      return this.mutationPayload(sel, null, variables, created.error);
    }
    return this.mutationPayload(
      sel,
      { pullRequest: { ___ctx: { __type: 'pull', repo, pull: created } as CtxValue } },
      variables,
    );
  }

  private mutateMergePullRequest(sel: Selection, args: Record<string, unknown>, variables: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const decoded = this.store.decodeGlobalId(String(input.pullRequestId ?? ''));
    const repo = decoded?.kind === 'PullRequest' ? this.store.reposById.get(Number(decoded.parts[0])) : undefined;
    const pull = repo?.pulls.get(Number(decoded?.parts[1] ?? NaN));
    if (!repo || !pull) return this.mutationPayload(sel, null, variables, 'Pull request not found.');
    const merged = this.store.mergePull(repo, pull);
    if (!merged.ok) return this.mutationPayload(sel, null, variables, merged.message);
    return this.mutationPayload(
      sel,
      { pullRequest: { ___ctx: { __type: 'pull', repo, pull } as CtxValue } },
      variables,
    );
  }

  private mutateAddComment(sel: Selection, args: Record<string, unknown>, variables: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const decoded = this.store.decodeGlobalId(String(input.subjectId ?? ''));
    const repo = decoded?.kind === 'Issue' ? this.store.reposById.get(Number(decoded.parts[0])) : undefined;
    const issue = repo?.issues.get(Number(decoded?.parts[1] ?? NaN));
    if (!repo || !issue) return this.mutationPayload(sel, null, variables, 'Issue not found.');
    const comment = this.store.addComment(repo, issue, String(input.body ?? ''));
    return this.mutationPayload(
      sel,
      {
        commentEdge: {
          ___edge: { ctx: { __type: 'comment', repo, issue, commentId: comment.id, body: comment.body } as CtxValue },
        },
      },
      variables,
    );
  }
}

function typeNameOf(value: CtxValue): string {
  switch (value.__type) {
    case 'repository': return 'Repository';
    case 'issue': return 'Issue';
    case 'pull': return 'PullRequest';
    case 'user': return 'User';
    case 'ref': return 'Ref';
    case 'comment': return 'IssueComment';
    default: return '';
  }
}

function encodeCursor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function decodeCursor(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    if (m) return Number(m[1]);
    const n = Number(text);
    return Number.isFinite(n) ? n : null;
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// HTTP entry point
// ---------------------------------------------------------------------------

export interface GraphQLRequest {
  method: string;
  bodyText: string;
  body: unknown;
}

export function handleGraphQL(store: Store, req: GraphQLRequest): { status: number; body: unknown } {
  if (req.method !== 'POST') return { status: 405, body: { message: 'Method Not Allowed' } };
  let payload: unknown = req.body;
  if (typeof payload === 'string') {
    try {
      payload = JSON.parse(payload);
    } catch {
      return { status: 400, body: { errors: [{ message: 'Request body is not valid JSON' }] } };
    }
  }
  const runOne = (doc: unknown): Record<string, unknown> => {
    if (doc === null || typeof doc !== 'object' || Array.isArray(doc)) {
      return { data: null, errors: [{ message: 'Each operation must be an object with a query string' }] };
    }
    const rec = doc as Record<string, unknown>;
    const query = rec.query;
    if (typeof query !== 'string' || query.trim() === '') {
      return { data: null, errors: [{ message: 'query is required' }] };
    }
    const variables = (rec.variables !== null && typeof rec.variables === 'object' && !Array.isArray(rec.variables)
      ? (rec.variables as Record<string, unknown>)
      : {}) as Record<string, unknown>;
    let operations: Operation[];
    try {
      operations = new Parser(tokenize(query)).parseDocument();
    } catch (err) {
      return { data: null, errors: [{ message: (err as Error).message }] };
    }
    if (operations.length === 0) return { data: null, errors: [{ message: 'No operation in the document' }] };
    const op = operations[0] as Operation;
    const executor = new Executor(store);
    const data = executor.execute(op, variables);
    const result: Record<string, unknown> = { data };
    if (executor.errors.length > 0) result.errors = executor.errors;
    return result;
  };
  if (Array.isArray(payload)) {
    return { status: 200, body: payload.map((doc) => runOne(doc)) };
  }
  return { status: 200, body: runOne(payload) };
}

export { isoFromSec };
