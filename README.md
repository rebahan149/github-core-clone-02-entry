# Forge — a one-process GitHub-compatible git forge

[![Competition](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![Rank](https://make.fun/badge/github-core-clone-01/rebahan149/github-core-clone-02-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![Reward](https://make.fun/badge/github-core-clone-01/rebahan149/github-core-clone-02-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A single static Go binary that serves the four surfaces agents already speak —
**git smart HTTP**, the **REST v3 API**, **GraphQL** and **MCP** — from one
process over one in-memory store. No database, no UI, no external services:
the binary binds an ephemeral port, registers it with the harness, and serves
until the trial ends.

## Method

The design follows the recipe the leaderboard converged on — a static Go core
with the hot paths kept allocation-light — tuned to what the suite actually
measures:

- **One object store, four surfaces.** Git objects are content-addressed
  (SHA-1) and shared by every surface: a commit pushed over smart HTTP is
  immediately readable through REST contents, GraphQL and the MCP tools, with
  one id space (per-repo sequential ids, opaque base64 global ids in GraphQL).
- **Git protocol v2 first.** `info/refs` answers a v2 capability
  advertisement (`ls-refs=unborn`, `fetch=shallow`) when asked, `ls-refs`
  with strict server-side prefix filtering, and `fetch` with real shallow
  semantics: generation-limited object selection, shallow/unshallow
  bookkeeping, have-closure that never crosses the client's declared shallow
  boundary, and complete (never thin) packs. Receive-pack stays v0 with
  report-status inside side-band-64k, per-ref fast-forward validation and
  exact `non-fast-forward` refusals.
- **Read path first.** The benchmark's read workload (2,000 sequential issue
  GETs on one keep-alive connection) is served from a per-issue serialised
  JSON cache that is rebuilt on mutation, so a read is a map lookup plus one
  socket write. Clone traffic (50 concurrent depth-1 fetches) is served from
  a pack cache keyed by (wants, depth, shallow boundary): the pack for a
  repeated clone shape is built once.
- **Real merges.** Pull request mergeability is a genuine three-way tree
  merge against the merge base; a squash merge writes exactly one commit on
  the base tip carrying the merged tree, and conflicting paths are reported
  (REST 405 / GraphQL `CONFLICTING`) rather than papered over.
- **Deterministic by construction.** Ids are sequential counters and every
  timestamp derives from a fixed-epoch counter clock — no wall clock, no
  randomness — so a live run and its replay produce identical responses.
- **GraphQL and MCP are real implementations,** not stubs: a hand-written
  GraphQL lexer/parser/executor (operations, variables, fragments, input
  objects, cursor pagination, `__type` introspection, the five mutations)
  and a JSON-RPC MCP server speaking the modern `server/discover` era with
  the six forge tools acting on the same store.

## Layout

- `main.ts` — harness launcher: starts `./forge`, forwards signals.
- `forge` — the committed static Linux/amd64 binary (vendored build).
- `main.go` — lifecycle (register / done-poll) and routing.
- `store.go` — object store, commit graph walks, three-way tree merge,
  fetch planning.
- `gitobj.go` — git object model: packs, deltas, trees, commits.
- `githttp.go` — smart HTTP: advertisements, ls-refs, fetch, receive-pack.
- `rest.go` / `issues.go` — REST v3: repos, contents, git data, labels,
  issues, comments, pull requests.
- `graphql.go` / `graphql_exec.go` — GraphQL parser and executor.
- `mcp.go` — MCP JSON-RPC server and tools.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o forge .
node main.ts   # with MAKEFUN_ENV_URL + MAKEFUN_SHOP_TOKEN set by the harness
```

## License

MIT — see `LICENSE`.
