# LSP-MCP Bridge — Technical Specification

**Version:** 0.1  
**Status:** Draft  
**Date:** 2026-07-23
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
│  Language Servers (gopls, rust-analyzer, pyright, tsserver…) │
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

- Started once (user-level preferred, optional system service later).
- Survives individual MCP client disconnects.
- On startup:
  1. Open/create the SQLite database.
  2. Load language server registry.
  3. Restore known workspaces from the database.
  4. Start file watchers for active workspaces.
  5. Lazily start language servers only when needed.

### 3.2 Multi-Repo Model

The daemon supports **multiple workspaces** concurrently.

- Each workspace is identified by its absolute root path.
- Index data for all workspaces lives in a single system/user-level SQLite database (default).
- Optional per-project databases are supported but not required for v0.1.

### 3.3 Language Server Management

- Language servers are started on demand when a file of the corresponding language is opened.
- Configuration is declarative (name, command, args, file extensions, root markers).
- The daemon monitors language server health and restarts on crash with exponential backoff.
- Only one instance of a given language server is kept per workspace (or shared when safe).

### 3.4 Indexing Behavior

- On first open of a workspace: full index (or resume from previous state).
- Subsequent changes: incremental via file watcher + content-hash comparison.
- Index stores: files, symbols, references, diagnostics, and basic hierarchy edges.
- Language servers remain the authoritative source for live queries when the index is stale or incomplete.

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

Exact wire format will be defined during implementation (prefer a small typed RPC layer).

---

## 4. MCP Server Specification

### 4.1 Transport

- **Primary:** stdio (compatible with Claude Code, GrokBuild, Codex, etc.)
- **Secondary:** Streamable HTTP (optional, for remote or multi-client scenarios)

### 4.2 Tool Surface (Base Set)

#### Navigation & Intelligence

| Tool                    | Purpose                                      |
|-------------------------|----------------------------------------------|
| `get_definition`        | Go to definition                             |
| `get_type_definition`   | Go to type definition                        |
| `get_references`        | Find all references                          |
| `get_implementations`   | Find implementations                         |
| `get_hover`             | Type + documentation at position             |
| `document_symbols`      | Hierarchical outline of a file               |
| `workspace_symbols`     | Fuzzy search across workspace                |
| `get_diagnostics`       | Current errors/warnings                      |
| `call_hierarchy`        | Incoming / outgoing calls                    |
| `type_hierarchy`        | Super/sub types                              |

#### Editing

| Tool                    | Purpose                                      |
|-------------------------|----------------------------------------------|
| `rename_symbol`         | Rename (dry-run by default)                  |
| `code_actions`          | Available quick fixes / actions              |
| `format_document`       | Format file or range                         |
| `apply_edit`            | Apply a previously computed edit             |

#### Session & Index

| Tool                    | Purpose                                      |
|-------------------------|----------------------------------------------|
| `open_document`         | Notify daemon a file is active               |
| `close_document`        | Notify daemon a file is closed               |
| `index_status`          | Report indexing progress / coverage          |
| `reindex`               | Force re-index of path or workspace          |
| `list_language_servers` | Show running language servers                |

#### Speculative (Recommended for v0.1)

| Tool                    | Purpose                                      |
|-------------------------|----------------------------------------------|
| `simulate_edit`         | Apply edit in memory, return new diagnostics |
| `simulate_rename`       | Dry-run rename with impact analysis          |

All tools must return concise, structured results optimized for low token usage.

### 4.3 Tool Design Rules

- Prefer absolute or workspace-relative paths consistently.
- Positions use 0-based or 1-based line/character according to LSP convention (document clearly).
- Dry-run is the default for any mutating operation.
- Tools that can be expensive (`workspace_symbols`, full reindex) should support limits and progress where possible.

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
  container_name, documentation
)

references (
  id, symbol_id, file_id,
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
- On file change: remove old symbols/references for that file, re-query language server, insert new data.
- Diagnostics are refreshed more aggressively (on open / after edit).

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
claude mcp add lsp-mcp -- /path/to/lsp-mcp --stdio
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
- The daemon only executes language server binaries that are explicitly configured.
- Mutating tools default to dry-run / require explicit apply.

---

## 10. Implementation Notes (Guidance)

Recommended implementation language: **Rust** or **Go** for the daemon + MCP server (single binary preferred).

Suggested structure:

```
lsp-mcp/
├── daemon/          # long-running process
├── mcp/             # MCP server frontend
├── index/           # SQLite schema + queries
├── lsp/             # language server client wrapper
└── protocol/        # shared IPC types
```

The same binary can support both `daemon` and `mcp` subcommands.

---

## 11. Success Criteria (v0.1)

- Can open a TypeScript or Python project and answer `get_definition`, `get_references`, and `get_hover` correctly via MCP.
- Index survives daemon restart.
- File changes are reflected incrementally without full re-index.
- Claude Code and GrokBuild can both use the same MCP server entry.
- Speculative edit returns accurate diagnostics without writing to disk.

---

**End of Specification**
