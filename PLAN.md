# Langer (LSP-MCP Bridge) — Implementation Plan

**Companion:** `SPEC.md` v0.3 — the authoritative specification.
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

---

## Status (2026-07-25)

| Milestone | State | Commit |
|-----------|-------|--------|
| M0 — scaffold, CLI, config, fixtures | **done** | `2d07acb` |
| M1 — LSP client wrapper | **done** | `a400e7c` |
| M2 — daemon process | **done**, reviewed + fixed | `da65ee8` + fixes |
| M3 — SQLite index | not started | |
| M4 — MCP frontend | not started | |
| M5 — edits & speculative overlays | not started | |
| M6 — security test, GC, v0.1 sign-off | not started | |

Verification gates all green at this point: `gofmt`, `go build`, `go vet`
(plain and `-tags=integration`), `go test ./...`, `go test ./... -race`, and
`go test -tags=integration ./...` driving the real typescript-language-server
5.3.0 and pyright 1.1.411.

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
- **Windows does not build.** `internal/procx/run.go` uses `Setsid`,
  `Setpgid`, and `syscall.Kill` with no `_unix.go`/`_windows.go` split, and
  the daemon transport is Unix-socket-only (SPEC's architecture diagram says
  "Unix socket / named pipe"). All M0–M2 verification ran on Linux. Shipping
  to claude-cli users includes Windows; either add a Windows port milestone
  (build-tag procx, named-pipe or loopback-TCP transport, Windows CI) or
  declare v0.1 Unix-only in SPEC §11 and README. Decide before M6 sign-off.

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

- Schema per SPEC §5.1 including `symbols.stable_key` and stable-key-based
  references; WAL mode + busy_timeout driver module (SPEC §3.2) — all access
  through this one module.
- Full index on first workspace open; file watcher; content-hash staleness;
  cache-kill-on-change with live LSP fallback (SPEC §3.4); project-files-only
  scope (deps and .gitignored paths excluded).
- GC pass (deleted files, dead workspaces, expired diagnostics) guarded by an
  advisory lock in `meta`.
- Index survives daemon restart (SPEC §11).

**Accept:** unit tests for staleness/invalidation, stable_key reference
survival across re-index, GC; integration test proving a file edit is
reflected without full re-index and the index answers after daemon restart.

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
- Run the full SPEC §11 checklist; record results in `PLAN.md` under a
  "v0.1 verification" heading.

**Accept:** all §11 criteria checked off with test evidence; full suite green.

---

## Out of Scope (v0.1)

HTTP transport; per-project databases; the unmarked tools in SPEC §4.2
(type_definition, implementations, call/type hierarchy, code_actions,
format_document, simulate_rename); write-broker process; non-TS/Python
language server configs beyond registry entries.
