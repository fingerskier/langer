# Langer (LSP-MCP Bridge) — Implementation Plan

**Companion:** `SPEC.md` v0.4 — the authoritative specification.
**Language:** Go (single binary, `daemon` / `mcp` / `status` subcommands).
**Target:** v0.1 success criteria in SPEC §11.

---

## Ground Rules

1. **SPEC.md is authoritative.** Do not invent fields, tools, error codes, or
   behaviors not in the spec. If the spec is ambiguous or wrong, stop and ask —
   do not guess and proceed.
2. **TDD, red/green.** Write the failing test first for every feature. Unit
   tests for the index/driver/protocol layers; integration tests against real
   language servers (typescript-language-server, pyright) on the fixture
   codebase (M0).
3. **One commit per milestone**, message `M<n>: <summary>`. All tests green
   before commit. Do not start milestone N+1 until N's acceptance criteria
   pass.
4. **Lean scope.** Only the ✅ v0.1 tools in SPEC §4.2. Only the three CLI
   subcommands in SPEC §10. No HTTP transport, no per-project DBs, no deferred
   tools.
5. **Security invariant** (SPEC §9): opening a workspace never executes
   project-local binaries. The test for this (M6) is non-negotiable and must
   never be weakened to pass.
6. Errors are structured per SPEC §3.6 everywhere — no free-text error strings
   across IPC or MCP boundaries.
7. **Platform gate.** Through M5, run every build/vet/test gate in the pinned
   Go 1.26.5 Linux Docker environment while the pre-existing Windows port is
   incomplete. M6 must add Windows process and local-IPC support, then run the
   full gate on both Linux and Windows; v0.1 may not be declared Unix-only.

---

## Status (2026-07-26)

| Milestone | State | Commit |
|-----------|-------|--------|
| M0 — scaffold, CLI, config, fixtures | **done** | `2d07acb` |
| M1 — LSP client wrapper | **done** | `a400e7c` |
| M2 — daemon process | **done**, reviewed + fixed | `da65ee8` + fixes |
| M3 — SQLite index | **done**, reviewed + race-hardened | `(this commit)` |
| M4 — MCP frontend | not started | |
| M5 — edits & speculative overlays | not started | |
| M6 — security, Windows, v0.1 sign-off | not started | |

Verification gates are green through M3 in the pinned Go 1.26.5 Linux Docker
runtime: changed-file `gofmt`, `go build`, `go vet` (plain and
`-tags=integration`), `go test ./...`, `go test ./... -race`, and
`go test -tags=integration ./...` driving the real typescript-language-server
5.3.0 and pyright 1.1.411. The M3 race gate found and fixed a supervisor
shutdown race: restart/process workers now reserve their wait-group slots under
the same lifecycle lock that begins shutdown, so no worker can be added after
shutdown starts waiting.

M0–M2 were each adversarially reviewed after implementation. That found real
defects the passing test suite did not, and the pattern is worth keeping: in
every milestone so far the serious bugs were **answers that were wrong in a way
an agent could not detect**, not crashes. Examples: `workspace/symbol`
returning an empty list while the server was still indexing (indistinguishable
from "no such symbol"); a references query racing project load and returning
only the declaration; speculative diagnostics surviving a `simulate_edit` and
being reported against a clean file.

### Carried into later milestones

Known and deliberately deferred rather than forgotten:

- **`apply_edit` lives in `internal/workspace/` already** (M2 shipped it). SPEC
  §3.5 does not list it among daemon methods and it belongs to M5. Fold it into
  M5 rather than writing a second implementation.
- **`internal/testutil.NoGoroutineLeaks` compares goroutine *counts***, so a
  real leak is masked whenever an unrelated goroutine exits concurrently. Every
  "no goroutine leaks" assertion in M1 and M2 is weaker than it reads. Fix
  before M6 signs off on process hygiene.
- **SPEC §3.1's "configurable idle period" is not configurable** — no config
  key, and `cmd/langer` never sets one. Needs a `config` key.
- **Windows does not build yet.** `internal/procx/run.go` uses `Setsid`,
  `Setpgid`, and `syscall.Kill` with no `_unix.go`/`_windows.go` split, and
  the daemon transport is Unix-socket-only. The approved resolution is binding:
  M3–M5 verify in Linux Docker; M6 adds Windows process-tree cleanup, a secure
  local transport, and Windows build/test coverage. Declaring v0.1 Unix-only is
  not an alternative.

---

## Milestones

### M0 — Scaffold, CLI, config, fixtures

- Go module `langer`; package layout per SPEC §10 (`daemon/`, `mcp/`,
  `index/`, `lsp/`, `protocol/`).
- Cobra-or-stdlib CLI: `langer mcp --stdio`, `langer daemon <root>`,
  `langer status` (stubs OK beyond flag parsing).
- Config loading from `~/.config/lsp-mcp/config.toml` with env overrides
  (SPEC §6); XDG paths for DB and socket.
- Fixture codebases checked into `testdata/`: a tiny TypeScript project and a
  tiny Python project, each with a known symbol graph (a definition, ≥2
  references across files, one deliberate type error). Include a
  `node_modules/.bin/fake-server` executable tripwire for the M6 security test.

**Accept:** binary builds; `langer status` runs; config parses; fixtures exist
with a README documenting their expected symbols/diagnostics.

### M1 — LSP client wrapper (`lsp/`)

- Spawn/initialize/shutdown one language server from a declarative registry
  entry (SPEC §3.3); capability capture from `initialize` result.
- `textDocument/didOpen`/`didChange`/`didClose`; definition, references,
  hover, documentSymbol, workspace/symbol requests; `publishDiagnostics`
  collection with the debounce/settle rule (SPEC §4.3).
- Crash detection + restart with exponential backoff.
- UTF-16 position handling verified with a test containing non-BMP characters.

**Accept:** integration tests pass against real typescript-language-server and
pyright on the fixtures: correct definition, references, hover, symbols, and
the deliberate type error surfaces as a diagnostic.

### M2 — Daemon process (`daemon/`, `protocol/`)

- Unix socket server; newline-delimited JSON RPC with protocol version field
  (SPEC §3.5); full §3.6 error model.
- Per-workspace daemon lifecycle: lockfile + socket-liveness single-instance
  check, auto-start handshake, idle sunset timer, version-mismatch
  drain-and-restart (SPEC §3.1).
- Daemon methods from SPEC §3.5 wired to the M1 LSP layer (no index yet —
  live queries only).

**Accept:** two concurrent clients query one daemon; killing a language server
does not kill the daemon; daemon exits after idle timeout; stale-daemon
version mismatch triggers restart; every §3.6 error code is exercised by a
test.

### M3 — SQLite index (`index/`)

- First repair the Go 1.26 Linux baseline assertion that uses the removed
  `syscall.Getsid`, without weakening its process-group behavior check.
- Schema per SPEC §5.1. `stable_key` remains non-unique metadata.
  `symbol_key = repo_namespace + U+001F + relative definition path + U+001F
  + stable_key`; every operation remains workspace-ID scoped.
- Derive `repo_namespace` from the normalized system-Git `origin` slug
  (normally `<org>/<name>`, preserving deeper namespace paths) with the
  canonical workspace root supplied by the CLI — normally cwd — as fallback.
  Use no network lookup and never execute workspace-local Git.
- Retain LSP `selectionRange` in the internal indexing shape and query
  references at its start. If a `symbol_key` is still ambiguous in one
  workspace/file, bypass the cache and ask the live server.
- WAL mode + busy_timeout driver module (SPEC §3.2) — all access through this
  one module. Replace file metadata/symbols/diagnostics atomically per file,
  and replace each complete cross-file reference set independently and
  atomically by `(workspace_id, symbol_key)`.
- Start the watcher before the full/resumed scan. On a watcher batch, call the
  daemon activity callback, synchronously invalidate/delete affected paths,
  then queue indexing. Index disk text only under the per-document lock;
  re-hash before commit and discard/reschedule raced work.
- Content-hash staleness and cache-kill-on-change with live LSP fallback
  (SPEC §3.4); project-files-only scope excludes dependencies, ignored files,
  and denied dependency directories even when tracked. Workspace-wide
  incomplete results return `NOT_READY`, never partial success.
- `RegistryOptions` exposes `Store`, `NewScanner`, `NewWatcher`, and
  `OnFileActivity` seams. `index_status` exposes `failed` with a structured
  §3.6 error.
- GC attempt hourly: 60-second expiring lease renewed every 20 seconds,
  diagnostics retained seven days, missing workspaces retained 30 days.
  `VACUUM` is shutdown-checkpoint-only and requires >25% reclaimable pages.
- Index survives daemon restart (SPEC §11).

**Accept:** red/green unit tests cover origin URL normalization and canonical
root fallback; duplicate `stable_key` isolation and residual-ambiguity live
fallback; independent atomic reference replacement; watcher-before-scan and
scan/edit race discard; staleness/invalidation; structured failed status; and
fake-clock GC lease/renewal/retention. Integration tests prove an edit is
reflected without full re-index and the index answers after daemon restart.
The complete plain, race, vet, build, and integration gates pass in Linux
Docker.

### M4 — MCP frontend (`mcp/`)

- MCP server over stdio exposing exactly the nine ✅ tools (SPEC §4.2) with
  result shapes from SPEC §4.4; auto-starts the workspace daemon on demand.
- `open_document` / `close_document` / `index_status` plumbing.

**Accept:** SPEC §11 navigation criteria pass end-to-end via MCP on both
fixture projects; `claude mcp add langer -- langer mcp --stdio` works against
a local checkout (manual verification note in the milestone commit).

### M5 — Edits & speculative overlays

- `rename_symbol` dry-run returning `edit_token` (content hashes of affected
  files); `apply_edit` verifying hashes, rejecting `STALE_EDIT` (SPEC §4.2).
- `simulate_edit`: per-session in-memory overlays, TTL, isolation between
  sessions, invalidation on real file change, never indexed (SPEC §4.2).

**Accept:** rename round-trip on fixtures; apply after out-of-band file change
returns `STALE_EDIT`; two sessions' overlays are isolated; overlay diagnostics
are accurate with nothing written to disk (SPEC §11).

### M6 — Security test, hardening, v0.1 sign-off

- The tripwire test: open the TS fixture (whose `node_modules/.bin` contains
  an executable that writes a sentinel file if run) and assert the sentinel is
  never created.
- Restrictive permissions on socket + DB (user-only).
- Split process supervision into Unix and Windows implementations while
  preserving whole-process-tree cleanup; add a secure Windows local transport
  and endpoint/lock permissions; make `GOOS=windows go build ./...` and the
  Windows unit/integration suite green.
- Run the full SPEC §11 checklist; record results in `PLAN.md` under a
  "v0.1 verification" heading.

**Accept:** all §11 criteria checked off with test evidence; full suite green
on Linux and Windows, including the tripwire and process-tree cleanup tests.

---

## Out of Scope (v0.1)

HTTP transport; per-project databases; the unmarked tools in SPEC §4.2
(type_definition, implementations, call/type hierarchy, code_actions,
format_document, simulate_rename); write-broker process; non-TS/Python
language server configs beyond registry entries.
