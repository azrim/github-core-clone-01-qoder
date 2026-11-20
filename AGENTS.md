# Build brief: single-process GitHub-compatible forge

This repository is an entry in make.fun's `github-core-clone-01` bounty. The grader runs
`node /bot/main.ts` in a 2 CPU / 1024 MiB pod with no network and no git binary, beside a sealed
suite that is the only client. The suite, its checks and its scorer ship in the kit:

    D:\project\meta\github-core-clone-01\kit\github-core-clone-01-kit

Read `RULES.txt` there first, then `program/env/src/checks-git.ts`, `checks-rest.ts`,
`checks-graphql.ts`, `checks-mcp.ts`, `wire.ts`, `bench.ts` and `manifest.ts`. They are the exact
contract; nothing is hidden.

## Score

- feature 600 = 600 * passed / 120 conformance checks (git 30, repos 15, issues 15, pulls 20, graphql 20, mcp 15, protocol 5)
- agent 200 = 200 * passedMcp / 15 (the MCP group again)
- perf 200 = four workloads, each round(clamp(25 - 12.5*log2(t/T_ref), 0, 50)): push 200 pushes of
  1 KiB (T_ref 1640.82 ms), clone 50 concurrent depth-1 clones of a 200-commit repo (266.941 ms),
  read 2,000 sequential GET issue on keep-alive (2759.715 ms), write 200 issue creations (348.965 ms).

Target: 120/120 checks first (that is 800 points), then as much of the perf column as possible.
The top of the leaderboard is 970; 958+ is the paid-zone bar.

## Language and layout

Write the forge in Go (stdlib only), a statically linked Linux amd64 binary committed in the
repository, with a thin authored `main.ts` that spawns it and forwards its exit code. `main.ts`
must remain the entrypoint and must contain lines you wrote.

Go lives at `D:\opt\go\bin\go.exe` (also on PATH as `go` in a fresh shell). Build both:

    # grading binary: Linux, committed
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge ./cmd/forge
    git add --chmod=+x forge

    # local-test binary: Windows, never committed (in .gitignore)
    go build -trimpath -ldflags "-s -w" -o forge.exe ./cmd/forge

`main.ts` selects the binary by platform: on `win32` it spawns `./forge.exe`, otherwise `./forge`.
Both come from the same Go source, so the Windows build is a faithful test proxy. If the spawned
binary is missing it must fail loudly with the spawn error on stderr — never silently pass.

A pure Node 22 implementation is the fallback only if Go cannot build. Do not use any third-party
package, vendored forge, or code fetched from the internet: every line must be authored in this
session. The transcript checks compare the code against the session that wrote it.

## Local loop

From the kit directory, the process driver runs the real trial on practice/trial-0 in a few seconds
(this machine has no Docker, so `--driver process` is the local loop):

    node cli/makefun.mjs grade-local --kit . --bot D:/project/github-core-clone-01-v1-qoder --driver process

Build `forge.exe` before every run. The printed `scores` line is the score; `state.json` under the
kept trial lists every check and its failure detail. Fix checks in groups; re-run. Run the same
command with `--instances 2` for a second practice instance. The final grading run happens in a
Linux container, where `forge` runs; the replay determinism check needs both passes identical, so
never write wall-clock time, host names or random values into any response or into `state`.

## Engineering notes

- Store: one Map of objects; refs; commits; issues; pull requests. No persistence is needed across
  the process's life; determinism is required only within a run.
- Git: implement pkt-line framing, upload-pack v2 (`ls-refs`, `fetch` with `want`/`have`/`deepen`,
  side-band-64k) and receive-pack v0 (`report-status`, `delete-refs`, side-band-64k). Use
  `node:zlib` in Node or `compress/zlib` in Go; compute SHA-1 yourself or with the stdlib. Deltas
  are optional for correctness but packs must be valid and checksummed.
- REST: every reply JSON, `X-GitHub-Request-Id` non-empty on every reply, auth via
  `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`, 401 without it.
- GraphQL: introspection for Issue/PullRequest/Repository and SearchType; documents the suite sends
  are in `checks-graphql.ts`.
- MCP: JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28 (`server/discover`) and the legacy
  initialize handshake; tools create_repo/get_repo/create_issue/list_issues/create_pull_request/
  merge_pull_request, same store as REST.
- Perf: avoid per-request `JSON.parse`/`JSON.stringify` where possible; keep a warm keep-alive, no
  Nagle stalls (`setNoDelay(true)` in Node); cache serialized issue JSON; precompute pack data for
  clone; keep writes O(1) per issue.

## Process rules

- Commit after each green group, with a short message.
- Never commit anything from the kit directory: it is reference only, not part of the entry.
- Stay under 16 MiB total and 8 MiB per file, so the committed `forge` must stay well under 8 MiB
  (`-s -w` and stdlib only keep it near 5-6 MiB).
- `.gitignore` holds `forge.exe` and local run output; the committed tree carries `forge` only.
- Do not add network calls, telemetry, or a package manager. `go.mod` (module + go version only,
  no requires) is fine and expected.
- Use your file tools for every edit so the session records authorship.
