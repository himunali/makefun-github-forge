# makefun-github-forge

[![github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/competitions/github-core-clone-01) [![rank](https://make.fun/badge/github-core-clone-01/himunali/makefun-github-forge/rank.svg)](https://make.fun/competitions/github-core-clone-01) [![reward](https://make.fun/badge/github-core-clone-01/himunali/makefun-github-forge/reward.svg)](https://make.fun/competitions/github-core-clone-01)

A standalone GitHub-compatible git forge in TypeScript, Node 22, standard
library only — no dependencies, no git binary, no network beyond the trial
suite. One in-memory process speaks all four surfaces the competition grades:

- **Git smart HTTP** (`/{owner}/{repo}.git`): protocol v2 and v0 upload-pack,
  v0 receive-pack with fast-forward enforcement, pkt-line framing, packfile
  read/write with zlib inflate/deflate, OFS_DELTA and REF_DELTA, shallow
  clones (`deepen`), sideband progress and report-status.
- **REST v3** (`/api/v3`): repositories, branches, git refs, contents
  (real commits, trees and blobs), labels, issues, comments, pull requests
  with squash merge and conflict detection, plus protocol checks (JSON
  everywhere, `X-GitHub-Request-Id`, pagination via `Link` headers).
- **GraphQL v4** (`/api/graphql`): a hand-written tokenizer, parser and
  executor — repository / issue / pull-request queries, mutations
  (`createIssue`, `closeIssue`, `createPullRequest`, `mergePullRequest`,
  `addComment`), `node(id:)` round-trips, cursor pagination, `__type`
  introspection, batched operation documents, and unknown-field errors.
- **MCP** (`/mcp`): JSON-RPC 2.0 Streamable HTTP, modern `server/discover`
  (revision `2026-07-28`) and legacy `initialize` handshakes, six tools
  (`create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`) over the same store REST uses.

## Run

The program is the entry: `node /bot/main.ts` (grader) or `node main.ts`
(locally). It binds an ephemeral port, registers it with the env
(`MAKEFUN_ENV_URL`), and exits when the env says done. Without
`MAKEFUN_ENV_URL` it exits 1. Auth is a bearer token taken from
`MAKEFUN_SHOP_TOKEN`; the trial clock comes from `MAKEFUN_TRIAL_NOW`.

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:8080 \
MAKEFUN_SHOP_TOKEN=shpat_test \
MAKEFUN_TRIAL_NOW=1735689600 \
node main.ts
```

## Determinism

Same inputs in the same order produce the same state byte-for-byte: no
`Date.now()`, no `Math.random()`, no random IDs. Timestamps are
`MAKEFUN_TRIAL_NOW` plus a logical counter incremented per mutation; IDs are
sequential counters; git SHAs are content-addressed. Every reply is
deterministic given the same request sequence.

## Layout

| File | What it holds |
| --- | --- |
| `main.ts` | Lifecycle only: bind, register, poll done, exit. |
| `app.ts` | The node:http server, body reading, auth, routing. |
| `git.ts` | Smart HTTP: v2/v0 ads, ls-refs, fetch, receive-pack. |
| `gitobj.ts` | SHA-1, pkt-lines, pack read/write, tree/commit/tag codecs, reachability. |
| `store.ts` | The in-memory store: repos, issues, PRs, comments, contents, diff/merge, JSON views, deterministic clock. |
| `rest.ts` | The REST v3 router. |
| `graphql.ts` | GraphQL tokenizer, parser, executor, introspection. |
| `mcp.ts` | MCP discovery, handshake, tool list and tool calls. |

## License

MIT — see [LICENSE](LICENSE).
