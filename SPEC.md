# LSP-MCP Bridge — Technical Specification

**Version:** 0.3  
**Status:** Ready for implementation  
**Date:** 2026-07-24  
**Companion:** `PLAN.md` (milestone execution plan)
**Related systems:** Claude Code, GrokBuild, Codex, OpenClaw

---

## 1. Purpose

This document specifies the architecture, interfaces, data model, and behavior of the **LSP-MCP Bridge**.

The system provides AI coding agents with compiler-accurate semantic code intelligence through the Model Context Protocol (MCP).

Core design:

> A long-running **daemon** owns language servers and a persistent index/cache.  
> A thin **MCP server** provides the agent-facing tool interface and talks to the daemon on demand.

---

## 2. High-Level Architecture

```
┌──────────────────────────────────────────────────────────────┐
│  AI Clients                                                  │
│  (Claude Code / GrokBuild / Codex / OpenClaw / others)       │
└────────────────────────────┬─────────────────────────────────┘
                             │ MCP (stdio or HTTP)
                             ▼
┌──────────────────────────────────────────────────────────────┐
│  MCP Server Process                                          │
│  - Tool discovery & validation                               │
│  - Request translation                                       │
│  - Session / speculative edit management                     │
└────────────────────────────┬─────────────────────────────────┘
                             │ Local IPC (Unix socket / named pipe)
                             ▼
┌──────────────────────────────────────────────────────────────┐
│  Daemon (long-running)                                       │
│  - Language Server Manager                                   │
│  - File Watcher                                              │
│  - Indexer                                                   │
│  - SQLite Index (persistent cache)                           │
└────────────────────────────┬─────────────────────────────────┘
                             │
                             ▼
┌──────────────────────────────────────────────────────────────┐
│  Language Servers (gopls, rust-analyzer, pyright,            │
│  typescript-language-server, …)                              │
└──────────────────────────────────────────────────────────────┘
```

### 2.1 Component Responsibilities

| Component       | Owns                                      | Does not own                          |
|-----------------|-------------------------------------------|---------------------------------------|
| **Daemon**      | Language servers, file watching, SQLite index, workspace state | MCP protocol, agent sessions         |
| **MCP Server**  | MCP tool surface, request routing, speculative sessions | Language servers, persistent index   |

The daemon is the source of truth for all semantic data. The MCP server is a pure access layer.

---

## 3. Daemon Specification

### 3.1 Lifecycle

- **One daemon per repo/workspace.** The MCP server auto-starts the daemon for
  a workspace if none is running (spawn on first connect; a lockfile or
  socket-liveness check enforces single instance per workspace).
- **Sunset timer:** the daemon shuts itself down after a configurable idle
  period (no connected clients and no file activity; default e.g. 30 min).
  Index state persists in SQLite, so restart is cheap.
- Survives individual MCP client disconnects.
- If the MCP server detects a daemon/binary version mismatch, it asks the old
  daemon to drain and restarts it.
- On startup:
  1. Open/create the SQLite database.
  2. Load language server registry.
  3. Restore known workspaces from the database.
  4. Start file watchers for active workspaces.
  5. Lazily start language servers only when needed.

### 3.2 Multi-Repo Model

**v0.1 topology: one daemon per repo, one language server per language per
repo, one MCP server process per agent/client.** Multiple MCP clients may
still attach to the same workspace daemon.

- Each workspace is identified by its absolute root path.
- Index data for all workspaces lives in a single user-level SQLite database.
  **Concurrency model (decided): no dedicated DB daemon.** Each per-repo
  daemon opens the database directly in WAL mode with a `busy_timeout`;
  readers never block, writers queue briefly at the file level. Write
  transactions are kept short (batched per file re-index). GC / `VACUUM` /
  schema migrations are run by whichever daemon notices they are due,
  guarded by an advisory lock in the `meta` table. All access goes through
  a single shared driver module — the seam where a write-broker process
  could be inserted later if measured contention ever demands it.
- Optional per-project databases are supported but not required for v0.1.

### 3.3 Language Server Management

- Language servers are started on demand when a file of the corresponding language is opened.
- Configuration is declarative (name, command, args, file extensions, root markers).
- The daemon monitors language server health and restarts on crash with exponential backoff.
- Only one instance of a given language server is kept per workspace (or shared when safe).

### 3.4 Indexing Behavior

- On first open of a workspace: full index (or resume from previous state).
- Subsequent changes: incremental via file watcher + content-hash comparison.
- **Scope: project files only.** Dependency directories (`node_modules`,
  `vendor`, `target`, virtualenvs, etc.) and `.gitignore`d paths are excluded
  from indexing. Language servers may still resolve into them for live queries.
- Index stores: files, symbols, references, diagnostics, and basic hierarchy edges.
- **Staleness = file changed underneath.** A file's index entries are stale
  when its current content hash differs from `files.content_hash`. On any
  detected change, the daemon invalidates (deletes) that file's cached
  symbols/references/diagnostics immediately and re-queries the language
  server; queries touching a stale file are answered live from the language
  server, never from the stale cache.
- The single shared database may grow large; a periodic GC pass removes rows
  for deleted files, dead workspaces, and expired diagnostics.

### 3.5 Daemon API (Internal)

The MCP server communicates with the daemon over a local socket using a simple request/response protocol (JSON or MessagePack).

Core daemon methods (conceptual):

```
open_workspace(root_path) → workspace_id
close_workspace(workspace_id)

open_document(workspace_id, path, language_id?)
close_document(workspace_id, path)

get_definition(workspace_id, path, line, character) → Location[]
get_references(workspace_id, path, line, character) → Location[]
get_hover(workspace_id, path, line, character) → Hover
document_symbols(workspace_id, path) → Symbol[]
workspace_symbols(workspace_id, query) → Symbol[]
get_diagnostics(workspace_id, path?) → Diagnostic[]

rename_symbol(...) → WorkspaceEdit
simulate_edit(...) → Diagnostics + preview
index_status(workspace_id) → Status
```

Wire format: **JSON throughout** (newline-delimited or length-prefixed JSON
over the socket; a small typed RPC layer on top). Large payloads may be
spilled to temp files and passed by path if ever needed. The IPC protocol
carries a version field for daemon/MCP-server compatibility checks.

### 3.6 Error Model

Errors are codified, structured returns — never free text. Every daemon (and
tool) error carries a machine-readable code, at minimum:

| Code                  | Meaning / agent action                          |
|-----------------------|--------------------------------------------------|
| `NOT_READY`           | Language server starting or indexing — retry     |
| `UNSUPPORTED`         | Capability not provided by this language server  |
| `NO_RESULT`           | Query succeeded, nothing found (not an error)    |
| `STALE_EDIT`          | Apply rejected: file changed since dry-run       |
| `WORKSPACE_UNKNOWN`   | Path not in an open workspace                    |
| `SERVER_CRASHED`      | Language server died; restart in progress        |
| `INTERNAL`            | Bug in the bridge                                |

---

## 4. MCP Server Specification

### 4.1 Transport

- **Primary:** stdio (compatible with Claude Code, GrokBuild, Codex, etc.)
- **Secondary:** Streamable HTTP (optional, for remote or multi-client scenarios)

### 4.2 Tool Surface

**Keep the initial toolset lean.** v0.1 ships only the tools marked ✅; the
rest are specified here as the target surface but deferred.

#### Navigation & Intelligence

| Tool                    | Purpose                                      | v0.1 |
|-------------------------|----------------------------------------------|------|
| `get_definition`        | Go to definition                             | ✅   |
| `get_type_definition`   | Go to type definition                        |      |
| `get_references`        | Find all references                          | ✅   |
| `get_implementations`   | Find implementations                         |      |
| `get_hover`             | Type + documentation at position             | ✅   |
| `document_symbols`      | Hierarchical outline of a file               | ✅   |
| `workspace_symbols`     | Fuzzy search across workspace                | ✅   |
| `get_diagnostics`       | Current errors/warnings                      | ✅   |
| `call_hierarchy`        | Incoming / outgoing calls                    |      |
| `type_hierarchy`        | Super/sub types                              |      |

#### Editing

| Tool                    | Purpose                                      | v0.1 |
|-------------------------|----------------------------------------------|------|
| `rename_symbol`         | Rename (dry-run by default)                  | ✅   |
| `code_actions`          | Available quick fixes / actions              |      |
| `format_document`       | Format file or range                         |      |
| `apply_edit`            | Apply a previously computed edit             | ✅   |

`rename_symbol` (and any dry-run producing an edit) returns an `edit_token`
that embeds the content hashes of all affected files. `apply_edit` verifies
those hashes against disk and **rejects with `STALE_EDIT`** if any file
changed since the dry-run.

#### Session & Index

| Tool                    | Purpose                                      |
|-------------------------|----------------------------------------------|
| `open_document`         | Notify daemon a file is active               |
| `close_document`        | Notify daemon a file is closed               |
| `index_status`          | Report indexing progress / coverage          |
| `reindex`               | Force re-index of path or workspace          |
| `list_language_servers` | Show running language servers                |

#### Speculative (Recommended for v0.1)

| Tool                    | Purpose                                      | v0.1 |
|-------------------------|----------------------------------------------|------|
| `simulate_edit`         | Apply edit in memory, return new diagnostics | ✅   |
| `simulate_rename`       | Dry-run rename with impact analysis          |      |

**Overlay semantics:** speculative edits live in a per-caller (per MCP
session) in-memory overlay, keyed by session ID.

- Overlays are short-lived: TTL-expired (default e.g. 5 min, refreshed on
  use) and dropped when the session disconnects.
- Overlays never touch disk and never enter the SQLite index.
- Callers are isolated: two sessions simulating edits to the same file see
  only their own overlay.
- If the real file changes on disk while an overlay exists, the overlay is
  invalidated (consistent with the cache-kill-on-change rule) and the caller
  gets `STALE_EDIT` on next use.

All tools must return concise, structured results optimized for low token usage.
Errors follow the codified error model in §3.6.

### 4.3 Tool Design Rules

- Prefer absolute or workspace-relative paths consistently.
- **Positions are 0-based** lines and characters, matching LSP. Character
  offsets are **UTF-16 code units** (the LSP default); the bridge does not
  negotiate alternate encodings in v0.1.
- Dry-run is the default for any mutating operation.
- **Diagnostics freshness:** LSP diagnostics are server-pushed. `get_diagnostics`
  is correctness-leaning: after an edit/open it debounces and waits for the
  language server to settle (bounded, e.g. ≤2 s) before returning, rather than
  returning the last stale push. If the settle window elapses, it returns the
  latest known diagnostics flagged `possibly_stale: true`.
- Tools that can be expensive (`workspace_symbols`, full reindex) should support limits and progress where possible.

### 4.4 Result Shapes (Examples)

Canonical JSON shapes for tool results. All positions 0-based, UTF-16
characters; paths workspace-relative.

`Location` (returned by `get_definition`, `get_references`):

```json
{
  "path": "src/user/service.ts",
  "range": { "start": { "line": 41, "character": 9 },
             "end":   { "line": 41, "character": 20 } },
  "is_definition": true,
  "preview": "export function getUserById(id: string): User {"
}
```

`Hover` (returned by `get_hover`):

```json
{
  "contents": "function getUserById(id: string): User",
  "documentation": "Fetches a user by primary key. Throws NotFoundError.",
  "range": { "start": { "line": 12, "character": 6 },
             "end":   { "line": 12, "character": 17 } }
}
```

`Symbol` (returned by `document_symbols`, `workspace_symbols`):

```json
{
  "name": "getUserById",
  "kind": "function",
  "container": "UserService",
  "path": "src/user/service.ts",
  "range": { "start": { "line": 41, "character": 0 },
             "end":   { "line": 48, "character": 1 } },
  "detail": "(id: string) => User"
}
```

`Diagnostic` (returned by `get_diagnostics`):

```json
{
  "path": "src/user/service.ts",
  "severity": "error",
  "code": "TS2339",
  "source": "typescript",
  "message": "Property 'idd' does not exist on type 'User'.",
  "range": { "start": { "line": 44, "character": 14 },
             "end":   { "line": 44, "character": 17 } }
}
```

Error results (any tool) follow §3.6:

```json
{ "error": { "code": "NOT_READY", "message": "pyright is indexing", "retry_after_ms": 1500 } }
```

---

## 5. Data Model (SQLite)

The daemon maintains a single primary SQLite database.

### 5.1 Core Tables

```sql
schema_version (version)

workspaces (
  id, root_path, name, created_at, last_indexed
)

files (
  id, workspace_id, path, absolute_path,
  language_id, content_hash, size_bytes, mtime, last_indexed
)

symbols (
  id, file_id, name, kind, detail,
  start_line, start_col, end_line, end_col,
  container_name, documentation,
  stable_key  -- name + container_name + kind: survives re-index
)

references (
  id, symbol_stable_key, file_id,   -- keyed by stable_key, not symbol row id
  start_line, start_col, end_line, end_col,
  is_definition
)

diagnostics (
  id, file_id, severity, message, code, source,
  start_line, start_col, end_line, end_col, recorded_at
)

meta (key, value)

-- optional
call_edges (from_symbol, to_symbol)
```

### 5.2 Indexing Strategy

- Content hash (SHA-256) used for change detection.
- On file change: immediately delete that file's cached symbols, references,
  and diagnostics (cache-kill-on-change), then re-query the language server
  and insert fresh data. Cross-file references survive because they are keyed
  by `stable_key` (name + container + kind), not by the deleted symbol rows.
- Diagnostics are refreshed more aggressively (on open / after edit).
- Periodic GC: prune rows for deleted files and stale workspaces; `VACUUM`
  opportunistically. Unbounded DB growth is acceptable between GC passes.

---

## 6. Configuration

Default locations follow XDG conventions:

- Config: `~/.config/lsp-mcp/config.toml`
- Database: `~/.local/share/lsp-mcp/index.db`
- Socket: `~/.local/share/lsp-mcp/daemon.sock`

Example language server entry:

```toml
[[language_servers]]
name = "typescript"
command = "typescript-language-server"
args = ["--stdio"]
file_extensions = [".ts", ".tsx", ".js", ".jsx"]
root_markers = ["tsconfig.json", "package.json"]
```

Environment overrides are supported for database path, config path, and log level.

---

## 7. Client Integration

### 7.1 Claude Code
```bash
claude mcp add lsp-mcp -- /path/to/lsp-mcp mcp --stdio
```

### 7.2 GrokBuild / Codex
Standard MCP configuration pointing at the same binary in stdio mode.

### 7.3 OpenClaw / Augment
The MCP server can be registered like any other tool provider. Deeper coupling (shared project registration, status surface) is deferred.

---

## 8. Process Model & Isolation

- The daemon runs independently of any MCP client.
- Multiple MCP clients may connect to the same daemon concurrently.
- Language server processes are children of the daemon and are cleaned up on daemon shutdown.
- A language server crash must not terminate the daemon or the MCP server.

---

## 9. Security Considerations

- No network access required for core operation.
- Database and socket files should have restrictive permissions (user-only).
- The daemon only executes language server binaries that are explicitly
  configured. **Opening a workspace must never execute project-local binaries**
  (e.g. a repo's own `node_modules/.bin/*` server) without explicit opt-in —
  this invariant is enforced by a dedicated test (see §11.1).
- Mutating tools default to dry-run / require explicit apply.

---

## 10. Implementation Notes (Guidance)

Implementation language (decided): **Go**, single binary. Rationale: mature
LSP/JSON-RPC and process-supervision libraries, gopls as a reference client,
trivial single-binary distribution.

Suggested structure:

```
lsp-mcp/
├── daemon/          # long-running process
├── mcp/             # MCP server frontend
├── index/           # SQLite schema + queries
├── lsp/             # language server client wrapper
└── protocol/        # shared IPC types
```

The same binary supports both `daemon` and `mcp` subcommands. **The v0.1 CLI
is constrained to necessities:**

```
lsp-mcp mcp --stdio        # MCP frontend (auto-starts workspace daemon)
lsp-mcp daemon <root>      # run daemon explicitly (normally auto-started)
lsp-mcp status             # daemon/index status for the current workspace
```

Staging plan: single DB driver first → one language server per repo → one MCP
server per agent. Defer HTTP transport, per-project DBs, and extra subcommands.

---

## 11. Success Criteria (v0.1)

- Can open a TypeScript or Python project and answer `get_definition`, `get_references`, and `get_hover` correctly via MCP.
- Index survives daemon restart.
- File changes are reflected incrementally without full re-index.
- Claude Code and GrokBuild can both use the same MCP server entry.
- Speculative edit returns accurate diagnostics without writing to disk.
- Daemon auto-starts on first MCP connect and sunsets after idle timeout.

### 11.1 Testing Strategy

- Integration tests run against **real language servers** (typescript-language-server
  and pyright) on a minimal fixture codebase checked into the repo.
- TDD for security invariants — in particular a test proving that opening a
  workspace containing a project-local language server binary
  (`node_modules/.bin/...`) never executes it.
- Unit tests for the index layer (staleness/invalidation, stable_key reference
  survival across re-index, GC) and the error model (each §3.6 code reachable).

---

**End of Specification**
