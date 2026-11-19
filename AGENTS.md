# AGENTS.md — github-core-clone-01

You are building a standalone, GitHub-compatible git forge for the make.fun bounty **github-core-clone-01**. The grader runs this repository's entrypoint as one process and scores the HTTP server it starts. Work only from the official kit and from this file. Do not invent endpoints, status codes, or scoring rules.

This file will be uploaded with the transcript bundle. Keep it accurate. Do not put secrets, API keys, or personal details in it.

## Goal

Implement a complete forge that, in one Node 22 process with only the Node standard library and committed source:

- binds an ephemeral port (never 8080), registers it, serves for the whole trial, then exits 0 when the suite says done
- speaks git smart HTTP, GitHub REST v3, GitHub GraphQL v4, and MCP
- stores all data itself (no git binary, no npm install, no network except the suite and optional model gateway)
- is deterministic: two runs of the same trial must produce the same suite state export (pass/fail bits and counts)

The official local loop is the kit's suite, not ad-hoc curl. After each phase, run the local runner and fix every failure in that phase before moving on.

## How you must write the code (transcript gates)

This entry is graded only after the session transcripts pass make.fun's transcript gates (RULES.txt, "## The transcript gate"). Work in this already-open Codex session in `/workspace/makefun/repo`. Write the forge yourself in this session.

- **Write every application file through tool calls** (Write, Edit, apply_patch, or a heredoc that names the file). Code that appears only in your chat text or a fenced block does not count as a write. Do not ask the user to paste large amounts of forge code by hand: handwritten lines lower authored coverage. The transcripts must account for at least 30% of counted lines (authored or pasted), and the entrypoint `main.ts` must contain at least one line you authored (a line you wrote or edited in a tool call, not only a file you copied unchanged from the starter).
- **Revise iteratively.** After you write a file, run the kit's local grader (`sdk/run.ts` or `grade-local --driver process`), then edit that file (or another file `main.ts` runs, such as `app.ts`) again so its **final** content comes from at least two assistant messages that write or edit it. A file written once in full and never revised counts as 1 and fails thinking_revised. Keep the lifecycle contract in `main.ts`, but do not leave `main.ts` as an untouched copy of the starter; edit it at least once (for example to import your modules or add the local-dev helper) and again after a grader run if needed.
- **Take enough assistant turns.** Produce at least five assistant turns that include text or thinking, not only tool calls. Explain what you are changing when you edit.
- **Do not invent or rewrite transcripts.** Do not create a `handed_over` session file, a reconstructed log, or a summary of this session. Codex already records rollouts. Leave packaging of the bundle and submission to the user.
- **Do not start a nested agent run** that would become the build session, and do not tell the user to paste a hand-off from another chat as the opening prompt. This session is the build session.

## Layout and kit paths

This build machine is **Linux**. You run terminal commands in **bash**. Set these in each new terminal, and keep Node 22 first on `PATH` (the box default `node` is not the grader runtime):

```bash
export PATH=/workspace/makefun/node22/bin:$PATH
KIT=/workspace/makefun/github-core-clone-01-kit
REPO=/workspace/makefun/repo
```

Confirm `node -v` starts with `v22` after the `PATH` export before you grade or run the suite.

The forge you write is graded on **Linux** (`node /bot/main.ts` in the trial pod). Keep application code platform-neutral: `node:path` / `path.join`, `os.tmpdir()` for scratch (that is `/tmp` on this box and on the grader), forward slashes in git tree paths, no `child_process` shell, and no `git` binary. Never hard-code `C:\` or assume a drive letter in the forge.

- `$KIT` — unpacked kit (`github-core-clone-01-kit`)
- `$REPO` — this repository (the entry)

Authoritative sources, in this order:

1. `$KIT/RULES.txt`
2. `$KIT/program/env/src/` (and the byte-identical copies under `$KIT/sdk/conformance/`)
3. This file and `$KIT/starter/`

Do not trust blog posts or GitHub.com quirks that the suite does not check. The exact checks, titles, timeouts, and required replies are in:

- `$KIT/program/env/src/manifest.ts` — groups, scoring, T_ref
- `$KIT/program/env/src/checks-git.ts` — 30 git checks
- `$KIT/program/env/src/checks-rest.ts` — repos (15), issues (15), pulls (20), protocol (5)
- `$KIT/program/env/src/checks-graphql.ts` — 20 GraphQL checks
- `$KIT/program/env/src/checks-mcp.ts` — 15 MCP checks
- `$KIT/program/env/src/wire.ts` — scenario seed, tokens, budgets
- `$KIT/program/env/src/bench.ts` — four timed workloads
- `$KIT/program/env/src/calibration.json` — house T_ref medians
- `$KIT/starter/main.ts` and `app.ts` — lifecycle and starting surface

## Hard constraints (the trial pod)

- Entrypoint: `main.ts` at the repository root. The grader runs `node /bot/main.ts` on Node 22.18+ (TypeScript types are stripped by Node; no compile step). A `main.py` (Python 3.13) entry is also legal, but this repo is Node.
- Standard library only at runtime. No `npm install`, no native addons, no package manager, no internet. Node built-ins such as `node:http`, `node:zlib`, `node:crypto`, `node:fs`, `node:path`, `node:buffer`, `node:stream` are allowed. A statically linked Linux amd64 binary you commit is allowed and counts toward size caps.
- No `git` binary exists in the pod. Implement packfiles, objects, pkt-line, sideband, and zlib yourself.
- `/bot` (this repo at the submitted commit, on the Linux grader) is **read-only**. Write only under `/tmp` in the trial. In code, use `os.tmpdir()` from `node:os` (that is `/tmp` on this Linux box and on the grader).
- Limits shared with the suite: 2 CPUs, 1024 MB memory, 256 processes, 900 s wall. Leave the suite room. A trial still running at 900 s scores 0 on every column.
- Do not bind port 8080. The suite listens there. Use `listen(0)` and read `server.address().port`.
- Size caps: at most 16 MiB for the whole tree (`max_tree_bytes: 16777216`) and 8 MiB for any one file.
- Do not commit provider API keys, tokens, or secrets.
- Model calls are optional and not required. If you call a model, use only the trial gateway (`ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` and the stand-in keys already in the environment). Allowed models and a tiny budget are in RULES.txt; a scripted forge with no model calls is the intended design.

## Lifecycle (keep `main.ts` as the starter has it)

The starter already implements the lifecycle correctly. Keep that contract in `main.ts` and grow `app.ts` (and modules it imports). Copy the starter lifecycle as a starting point, then edit `main.ts` yourself in this session (at least one authored line; revise it or `app.ts` again after a local grader run). Do not change the contract:

1. If `MAKEFUN_ENV_URL` is unset, exit 1 (trial always sets it).
2. `createApp()`, then `server.listen(0)`. Never bind 8080.
3. `POST $MAKEFUN_ENV_URL/register` with `{"port": <port>}` as JSON. Accept HTTP 200 or 409 (first register wins). Retry other network/start-up failures with short exponential backoff. HTTP 400/405 are contract errors and should exit.
4. Poll `GET $MAKEFUN_ENV_URL/done` until the body is `{"done": true}`. The env answers **immediately** (`{"done": false}` while the suite runs). This is a poll, not a long-held request. Never exit before `done: true`.
5. Close the server and exit 0.

`GET /healthz` must answer `200 {"ok": true}` with no Authorization header. The suite's boot probe calls it twice. A local-dev helper that starts the same `createApp()` without registering is allowed (for `sdk/run.ts`); do not break the trial lifecycle in `main.ts`.

## Auth and common HTTP rules

- Every API call except `/healthz` arrives with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` (same value is in the process environment).
- Missing or wrong token: **401**. Error body is JSON with a string `message`.
- A repository belongs to the **login the token names**. The same repo name under another owner is **404, never 403**. Implement `GET /api/v3/user` so MCP setup can read that login; the suite also uses it.
- Every REST reply is `application/json`, never HTML, and carries a non-empty `X-GitHub-Request-Id` header. `X-RateLimit-*` is not required.
- Unknown route: 404 JSON `{"message":...}`, never HTML.
- Malformed JSON body: 400.
- A push cannot invent a repository. Create repos only through `POST /api/v3/user/repos` (and the MCP `create_repo` tool, which must hit the same store).

## REST v3 (under `/api/v3`)

Implement exactly the statuses and shapes the suite requires. The contract comment at the top of `$KIT/program/env/src/checks-rest.ts` is the checklist:

- `GET /api/v3/` → 200 JSON plus `X-OAuth-Scopes` (starter uses `repo, read:org`; keep a non-empty scopes header).
- `GET /api/v3/meta` → 200 with a non-empty `installed_version` string.
- `GET /api/v3/user` → 200 with the token owner's `login` (needed by MCP setup).
- `POST /api/v3/user/repos` → 201 `{name, private, default_branch, ...}`.
- `GET /api/v3/user/repos?per_page=10` → 200 list; more than one page uses a `Link` header with `rel="next"`. The pagination check creates 30 repos and pages at `per_page=10`.
- `GET /api/v3/repos/{o}/{r}` → 200 `{name, private, default_branch, description}` or 404 JSON.
- `PATCH /api/v3/repos/{o}/{r}` → 200, description updated.
- `DELETE /api/v3/repos/{o}/{r}` → 204; a later GET is 404.
- `POST /api/v3/repos/{o}/{r}/git/refs` → 201 `{ref, object:{sha}}` (branch create).
- `GET /api/v3/repos/{o}/{r}/branches` → 200 `[{name, commit:{sha}}]`.
- `GET /api/v3/repos/{o}/{r}/commits[?sha=<ref>]` → 200 `[{sha, commit:{message}}]`.
- `PUT /api/v3/repos/{o}/{r}/contents/{path}` → 201 on create (this is how an unborn repo gets its first commit), 200 on update, 409 on a stale sha. GET a file returns `{encoding:"base64", content, sha, size}`. GET a directory returns an array of `{type, name, path, ...}`.
- `DELETE /api/v3/repos/{o}/{r}/contents/{path}` → 200 `{commit:{sha}}`, then GET is 404.
- `POST /api/v3/repos/{o}/{r}/labels` → 201 `{name, color}`. A label must exist before an issue may carry it.
- `POST /api/v3/repos/{o}/{r}/issues` → 201 open issue `{number, title, body, state, labels}`; 422 on an unknown label. Issue numbers increment by one.
- `GET /api/v3/repos/{o}/{r}/issues?state=&sort=created&direction=` → 200, filtered and ordered (`state=open` omits closed; `sort=created` with `asc`/`desc`).
- `GET /api/v3/repos/{o}/{r}/issues/{n}` → 200 one issue.
- `PATCH /api/v3/repos/{o}/{r}/issues/{n}` → 200; `{state:"closed"}` closes it and the close persists.
- `POST /api/v3/repos/{o}/{r}/issues/{n}/comments` → 201 `{id, body}`; `GET` lists them.
- `POST /api/v3/repos/{o}/{r}/pulls` → 201 `{number, state:"open", head, base}` with head/base shas equal to the branch tips. Body/title echoed.
- `GET /api/v3/repos/{o}/{r}/pulls` defaults to open; `?state=all` lists open and merged.
- `GET /api/v3/repos/{o}/{r}/pulls/{n}` → 200 or 404.
- `GET /api/v3/repos/{o}/{r}/pulls/{n}/files` → 200 `[{filename, status, additions, ...}]` for every changed path.
- `PUT /api/v3/repos/{o}/{r}/pulls/{n}/merge` with `merge_method=squash` → 200 `{sha, merged:true, message}`. The pull becomes closed and merged, carries `merged_at` and `merge_commit_sha` equal to the merge sha, and the squash commit (and its content) is on the default branch. Unknown PR: 404. Conflicted PR: 405.

Read each check in `checks-rest.ts` for the exact JSON shapes (`shapes.object(...)`). Match those shapes, not a full GitHub clone.

## Git smart HTTP at `/{owner}/{repo}.git`

Implement the protocol in-process (see `$KIT/program/env/src/git-client.ts` and `checks-git.ts`). Summary the rules require:

- upload-pack speaks **protocol v2** when the client sends `Git-Protocol: version=2`. The v2 capability advertisement and both `ls-refs` checks require it. The v2 advertisement opens with `version 2`. A v0 answer loses those three checks; some others may fall back to v0.
- receive-pack is **v0 only**, and must advertise `report-status`, `side-band-64k`, and `delete-refs`. The v0 upload-pack and receive-pack advertisements open with `# service=git-upload-pack` / `# service=git-receive-pack`. A v0 upload-pack that does not advertise `side-band-64k` cannot be framed.
- Missing repository: 404 on both services' `GET /info/refs`, and the server stays up.
- Every update must fast-forward. A non-fast-forward push is refused per ref with `ng <ref> non-fast-forward` and the ref does not move. The suite never force-pushes. A push with one fast-forward and one refused ref applies the fast-forward and answers one report line each (`ok` and `ng non-fast-forward`).
- Object identity is SHA-1 of the git object. Cloned blobs, trees, commits, and annotated tags must round-trip byte-exactly. Packs must be structurally valid and checksummed. Understand OFS/REF deltas (the suite has a delta check: "when the server sends them" — if you never send deltas you must still accept and resolve packs that contain them on push, and produce valid packs on fetch/clone).
- Support: first push to an unborn branch, empty clone, full clone, incremental fetch, fetch with nothing new, depth-1 shallow clone, deepen 2, shallow fetch of a new tip, branch create/delete, tag delete, 100-file single-commit push, sideband framing on fetch and on the push report, and staying healthy through 404s, refusals, and a malformed request.
- Repositories used by git checks are created first through REST, with deterministic names under the viewer's login.

## GraphQL at `POST /api/graphql`

Body: `{"query": ..., "variables": {...}}` with the bearer token. Unauthenticated: exactly 401. The suite sends the documents `gh` and `github-mcp-server` send. Implement those documents and the introspection they need, not a full GitHub schema.

From RULES.txt and `checks-graphql.ts`:

- `__type(name: "Issue")` lists `id, number, title, state, body`.
- `__type(name: "PullRequest")` lists `id, number, headRefName, baseRefName, mergeable, state, merged, files`.
- `__type(name: "Repository")` lists `id, databaseId, name, nameWithOwner, owner, defaultBranchRef, viewerPermission, hasIssuesEnabled, issue, issues, pullRequests, pullRequest`.
- `SearchType` enumValues include `ISSUE`.
- `query { viewer { login } }` answers the token owner.
- `repository(owner:, name:)` answers the fields above. Unknown repository: `data.repository = null` with no errors.
- `repository.issues` with `orderBy: {field: CREATED_AT, direction: DESC}` returns issue numbers descending (ties by number DESC); `states: [OPEN]` omits closed. Cursor pagination (`first`, `after`) pages to the end.
- `createIssue` / `closeIssue` / `addComment` / `createPullRequest` / `mergePullRequest` (SQUASH) must be visible on the REST view as the same objects.
- `createPullRequest` takes a repository id and a head branch already pushed over git; REST reads back the same head and base refs.
- `pullRequest(number:).files` lists changed paths.
- Two operation documents in one body answer two results in order.
- A query naming an unknown field is HTTP 200 with an `errors` array naming it.
- `node(id:)` round-trips a repository. Global ids are opaque and equal on repeated reads.

## MCP at `POST /mcp`

JSON-RPC 2.0 over Streamable HTTP. Answer `application/json` or `text/event-stream`. Each request must answer inside 3 seconds.

- Speak revision **2026-07-28** (every request declares it; `server/discover` answers a result whose `supportedVersions` is a non-empty list of `YYYY-MM-DD` revisions), or a handshake revision (2025-11-25 or earlier) through `initialize` and `notifications/initialized` with an optional `Mcp-Session-Id`.
- `tools/list` offers at least six tools, including `create_repo`, `get_repo`, `create_issue`, `list_issues`, `create_pull_request`, `merge_pull_request`. A server prefix is allowed (`github__create_issue`), matched as a suffix and case-insensitively. Every tool has a non-empty description and an `inputSchema` of type `object`. Two `tools/list` calls must answer the **same list, byte for byte**.
- `tools/call` arguments follow github-mcp-server: `create_repo {name, description?, private?}`, `get_repo {owner, repo}`, `create_issue {owner, repo, title, body?}`, `list_issues {owner, repo, state?}`, `create_pull_request {owner, repo, title, head, base}`, `merge_pull_request {owner, repo, pull_number}`. Map each argument onto the nearest property name in your schema; fill a `full_name` property with `owner/repo` if present. A required property the suite cannot fill fails the check rather than being called with made-up arguments.
- Two in-flight `tools/call` requests keep their own JSON-RPC ids and answer for their own arguments.
- Unknown tool: JSON-RPC `-32602` or `-32601`, never HTTP 500. Missing required argument: `isError: true` or `-32602`. Body that is not JSON: HTTP 400 with `-32700` (or 200 with `-32700` from a pre-2026-07-28 server). No Authorization: 401.
- Tools act on the **same store** as REST: ids unchanged across reads (`mcp.rest-parity`).

## Scoring (what to maximize)

Score = feature + agent + perf, integer 0..1000.

- **feature** (600) = `round(600 * passed / 120)` over all seven groups: git 30, repos 15, issues 15, pulls 20, graphql 20, mcp 15, protocol 5.
- **agent** (200) = `round(200 * passedMcp / 15)` — the MCP group's 15 checks, counted again.
- **perf** (200) = sum of four workloads, each 0..50: `round(clamp(25 - 12.5 * log2(t / T_ref), 0, 50))`. `t` is the median of 3 timed rounds after 5 untimed warm-ups, with inputs built before the clock starts. A time equal to T_ref scores 25; half of T_ref scores 38; double scores 13; a quarter or less scores 50. A workload that cannot finish in its 60 s budget scores 0 and stops early.

House T_ref values (from `$KIT/program/env/src/calibration.json`, measured 2026-11-14, medians of 3 production-shaped runs), in milliseconds:

- push: **1640.82** — 200 sequential 1 KiB pushes to one repository
- clone: **266.941** — 50 concurrent shallow clones (depth 1) of a 200-commit repository
- read: **2759.715** — 2,000 sequential `GET /api/v3/repos/{o}/{r}/issues/{n}` on one keep-alive connection
- write: **348.965** — 200 sequential `POST /api/v3/repos/{o}/{r}/issues`

The house reference forge passed 120/120 on those calibration runs. A prize requires a final score **strictly above** that reference (a statistical tie does not beat it). Passing every check and then beating T_ref on the workloads is the path to a paid place.

`working` is 1 when the forge came up and answered (`/healthz`); 0 otherwise. State export holds only pass/fail bits and counts, never a time, a port, or a date. Timings go in a separate perf export.

## Determinism

Final contender trials run twice (live, then replay). The env's `state.json` must match byte for byte or the run is listed and unranked. Practice / dev grades run once, so a practice score does not prove replay safety — `grade-local` can run the replay for you.

Implications for the forge:

- Do not let wall-clock or `Math.random()` change any pass/fail bit. Git object bytes must be the bytes the client pushed (author/committer timestamps come from the pack, not `Date.now()`). Issue numbers, ids, and order must be deterministic given the same request sequence.
- Presence of fields like `merged_at` is required; do not put unstable values into anything a check compares for equality (for example `merge_commit_sha` must be the real squash commit sha).
- HTTP keep-alive, buffers, and map iteration order must not change results. Prefer insertion-ordered maps and sorted iterations where you emit collections the suite might compare.
- The suite itself is seeded (`scenario_seed` in practice is `6a09e667f3bcc909`); two runs send the same requests in the same order if the forge is well-behaved.

## Architecture (recommended)

Keep `main.ts` as the lifecycle entrypoint (edit it yourself; do not leave it as an untouched starter copy). Grow the server:

- `app.ts` — HTTP router and header helpers (`Content-Type`, `X-GitHub-Request-Id`, 401/404/400/422)
- `auth.ts` — bearer token, current login, 401/404 ownership
- `store.ts` — in-memory source of truth (repos, refs, objects, issues, pulls, labels, comments). Persist under `os.tmpdir()` only if useful; a fresh process each trial can stay in memory. One store shared by git, REST, GraphQL, and MCP.
- `git/` — pkt-line, sideband, objects (blob/tree/commit/tag), pack read/write (zlib, deltas), receive-pack v0, upload-pack v2 (`ls-refs`, fetch, shallow/deepen)
- `rest.ts` — the `/api/v3` routes above, mutating the same store (contents PUT must write real git objects and refs)
- `graphql.ts` — the documents and introspection above, same store
- `mcp.ts` — streamable HTTP JSON-RPC, same store

Implement git objects correctly first: REST contents, pulls, merge, clone, and GraphQL files all depend on them.

## Local commands

Faster inner loop (you start the server yourself, no Docker, no models). Add a small `local-run.ts` that calls `createApp()`, listens (e.g. `listen(0)` or `process.env.PORT`), prints the port, and does not talk to `MAKEFUN_ENV_URL`. Use two terminals; set `PATH`, `$KIT`, and `$REPO` in each.

Window 1 (server; note the printed PORT):

```bash
export PATH=/workspace/makefun/node22/bin:$PATH
REPO=/workspace/makefun/repo
export MAKEFUN_SHOP_TOKEN=devtoken
cd "$REPO"
node --experimental-strip-types local-run.ts
```

Window 2 (suite; replace PORT with the number from window 1):

```bash
export PATH=/workspace/makefun/node22/bin:$PATH
KIT=/workspace/makefun/github-core-clone-01-kit
cd "$KIT"
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token devtoken --json
```

After conformance is green, add `--perf` (one line):

```bash
export PATH=/workspace/makefun/node22/bin:$PATH
KIT=/workspace/makefun/github-core-clone-01-kit
cd "$KIT"
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token devtoken --perf
```

`--token` can be any non-empty string locally; use the same value the server treats as `MAKEFUN_SHOP_TOKEN` (`$MAKEFUN_SHOP_TOKEN` in the server process). Exit status of `sdk/run.ts` is 0 if `/healthz` answered, even when checks fail — read the table or JSON (`counts.passed`, `columns`). The starter, as shipped, scores feature 15 / agent 0 / perf 0 / working 1 (3/120: `repos.root`, `repos.not-found`, `protocol.unknown-route-json-404`).

Official trial simulation (from `$KIT`). This box has no Docker; the CLI default is `docker`, so always pass `--driver process`:

```bash
export PATH=/workspace/makefun/node22/bin:$PATH
KIT=/workspace/makefun/github-core-clone-01-kit
REPO=/workspace/makefun/repo
cd "$KIT"
node cli/makefun.mjs grade-local --kit . --bot "$REPO" --driver process --worktree
```

More practice trials, or a replay after a live cassette exists:

```bash
export PATH=/workspace/makefun/node22/bin:$PATH
KIT=/workspace/makefun/github-core-clone-01-kit
REPO=/workspace/makefun/repo
cd "$KIT"
node cli/makefun.mjs grade-local --kit . --bot "$REPO" --driver process --instances 3
node cli/makefun.mjs grade-local --kit . --bot "$REPO" --driver process --replay CASSETTE
```

Use the Node 22 already on `PATH` after the export above (the official grader runtime is Node 22.18+). Confirm `node -v` starts with `v22` before grading. Without `--worktree`, the committed HEAD is graded.

After first meaningful edits, also run (from `$KIT`) once transcripts exist:

```bash
export PATH=/workspace/makefun/node22/bin:$PATH
KIT=/workspace/makefun/github-core-clone-01-kit
REPO=/workspace/makefun/repo
cd "$KIT"
node cli/makefun.mjs bundle --repo "$REPO" --dry-run
```

Codex session logs on this machine live under `~/.codex/sessions` (and `archived_sessions`), files `rollout-*.jsonl` or `rollout-*.jsonl.zst`.

Do not submit, register, or sign in unless the user explicitly asks.

## Phased build order

Work one phase at a time. After each phase, run `sdk/run.ts` and do not move on until that phase's checks pass.

0. **Bootstrap.** Copy the starter into this repo if it is not already here. Then edit `main.ts` yourself (keep the lifecycle contract; add a hook to your modules or the local-dev helper so it is not an untouched copy). Add `local-run.ts` with a tool call. Confirm `node -v` is major 22. Confirm `GET /healthz` and the three starter passes still work. After that first grader run, edit `main.ts` or `app.ts` again so at least one of those files is written in two assistant tool-call turns.
1. **Protocol + repo root.** `GET /api/v3/meta`, `GET /api/v3/user`, `POST /api/v3/user/repos`, request-id on every REST reply, 400 on malformed JSON, 401 without a token, 404 JSON with a string `message`. Target: protocol group (5) and early repos checks.
2. **REST repos and contents.** List/patch/delete, pagination (`Link: rel="next"`), foreign-owner 404, `git/refs`, branches, commits, contents create/update/stale-sha/delete/directory listing. Contents must write real git objects.
3. **Git smart HTTP.** Advertise v2 / receive-pack v0, 404 missing repo, ls-refs, first push, clone hashes/bytes, fetch, shallow/deepen, ff / non-ff / mixed report, branch and tag delete, 100-file push, pack integrity, sideband. Read failures from `sdk/run.ts` and fix against `checks-git.ts`.
4. **REST issues.** Labels first, create/list/get/patch, incrementing numbers, sort, comments, 422 unknown label.
5. **REST pulls and merge.** Create, files list/shape, squash merge onto default branch, 404/405, list filters.
6. **GraphQL.** Introspection, viewer, repository, issues order/filter/pagination, mutations with REST parity, batched operations, errors, node ids. Check `$KIT/program/env/src/checks-graphql.ts` and `graphql-client.ts` for the exact documents.
7. **MCP.** initialize / discover / tools/list (byte-identical), schemas, all six tools, errors, concurrency, auth, REST parity. Check `checks-mcp.ts` and `mcp-client.ts`.
8. **Performance and determinism.** Run `sdk/run.ts --perf`. Keep HTTP connections alive, serve issue GET from memory, make receive-pack and shallow clone fast. Then `grade-local --kit . --bot $REPO --driver process --worktree` (always `--driver process`; Docker is not available on this box) and a replay. Compare scores to T_ref. Tune until checks stay at 120/120 and workloads approach or beat T_ref.
9. **Stop for packaging.** Leave LICENSE, README template, and About to the user unless asked. Do not invent transcripts, do not write a `handed_over` file, and do not rewrite session logs.

When a check fails, read its `detail` and the corresponding function in the checks file. Fix the store or the wire, then re-run the suite. Do not disable a check or hard-code a scenario name unless the protocol truly requires that shape for all inputs — names come from a seed and will differ on the final instances.

## Coding style for this contest

- One process, no worker farm that can exhaust the 256-pid limit.
- Prefer correctness and identical bytes over clever git extensions the suite never sends.
- Keep responses JSON for REST; do not serve HTML error pages.
- Avoid shared mutable request state. Support concurrent clone (50 at once) and two concurrent MCP calls.
- Comment only where the suite contract is subtle (pkt-line, shallow, squash).
- Platform-neutral forge code: `path.join`, `os.tmpdir()`, no hardcoded Windows or Unix paths, no shell, no `git` executable. Git object and tree paths use `/`.
- Commands you run while developing are bash, as in the Local commands section. Always prepend `/workspace/makefun/node22/bin` to `PATH` first.
