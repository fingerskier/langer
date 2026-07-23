# LSP-MCP Bridge — Requirements Document

## 1. Overview

### 1.1 Purpose
Provide AI coding agents (Claude Code, GrokBuild, Codex, OpenClaw, etc.) with **compiler-grade semantic understanding** of source code via the Model Context Protocol (MCP).  

Instead of relying on text search / grep / full-file reads, agents gain precise, language-aware tools for:
- Go-to-definition / type definition
- Find all references / callers / implementations
- Hover (types + docs)
- Document & workspace symbols
- Diagnostics (errors, warnings)
- Rename / code actions (with dry-run)
- Call hierarchy & type hierarchy
- Speculative / simulated edits

The system is designed as a **standalone MCP server + long-running daemon** that can be plugged into any MCP-compatible client.

### 1.2 Design Principles
- **Agent-first** — tools expose high-level, token-efficient operations, not raw LSP JSON-RPC.
- **Persistent** — warm index survives process restarts.
- **Language-agnostic** — any LSP-compatible language server can be registered.
- **Local-first / privacy-preserving** — no network required for core operation; all data stays on disk.
- **Composable** — works as a pure MCP tool provider; optional deeper integration with Augment / OpenClaw later.
- **Zero-config for common languages** — sensible defaults for TypeScript, Python, Rust, Go, etc.

### 1.3 Non-Goals (v0.1)
- Full IDE replacement
- Cloud / multi-user shared index
- Building custom language servers
- Heavy AI / embedding features (can be added later as optional layer)

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  AI Client (Claude Code / GrokBuild / Codex / OpenClaw)     │
│                                                             │
│  MCP Client  ────────────────────────────────────────────┐  │
└──────────────────────────────────────────────────────────│──┘
                                                           │
                                                           │ stdio / HTTP
                                                           ▼
┌─────────────────────────────────────────────────────────────┐
│  LSP-MCP Bridge (MCP Server process)                        │
│  - Tool registry & schema                                   │
│  - Request routing                                          │
│  - Speculative edit session manager                         │
└────────────────────────────┬────────────────────────────────┘
                             │
                             │ IPC / local socket
                             ▼
┌─────────────────────────────────────────────────────────────┐
│  Daemon (long-running)                                      │
│  - Language Server lifecycle manager                        │
│  - File watcher (inotify / FSEvents / etc.)                 │
│  - Incremental indexer                                      │
│  - SQLite index (system-level or per-user)                  │
└─────────────────────────────────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────┐
│  Language Servers (gopls, rust-analyzer, pyright, tsserver…)│
└─────────────────────────────────────────────────────────────┘
```

### 2.1 Components

| Component              | Responsibility                                                                 | Lifetime          |
|------------------------|--------------------------------------------------------------------------------|-------------------|
| **Daemon**             | Starts/stops language servers, watches filesystem, maintains warm SQLite index | System / user session |
| **MCP Server**         | Exposes tools over MCP (stdio or HTTP), translates to daemon / LSP calls       | Per client connection |
| **Index (SQLite)**     | Persistent store of files, symbols, references, diagnostics, hierarchies       | Durable           |
| **Client Plugins**     | Thin adapters / config helpers for Claude Code, GrokBuild, Codex, etc.         | Optional          |

---

## 3. Core Requirements

### 3.1 Daemon
- Runs as a background process (user-level by default; optional system service).
- On start:
  - Opens / creates the SQLite database.
  - Loads registered language server configurations.
  - Re-indexes or verifies existing workspace(s) if needed.
  - Starts a file watcher on registered project roots.
- Supports multiple concurrent workspaces / projects.
- Graceful shutdown: flushes pending index writes, stops language servers cleanly.
- Health endpoint / status command for clients.

### 3.2 Index Persistence
- **Must** use SQLite as the primary store.
- Database location:
  - Default: `$XDG_DATA_HOME/lsp-mcp/index.db` (or platform equivalent).
  - Override via env `LSP_MCP_DB` or config.
  - Optional per-project DBs under `<project>/.lsp-mcp/index.db`.
- Supports incremental updates (file change → re-parse only affected files + dependents).
- Schema must be versioned and migrateable.

### 3.3 Language Server Management
- Pluggable registry of language servers (command + args + file extensions + root markers).
- Auto-detect common servers if binaries are on `$PATH`.
- Lazy start: only launch a language server when a file of that language is opened.
- Restart on crash with backoff.
- Support for multi-root workspaces.

### 3.4 MCP Interface
- Transport: stdio (primary) + optional Streamable HTTP.
- Full `tools/list` discovery with rich descriptions and JSON Schema.
- Tools must be designed for low token usage (return structured, concise results).
- Support for progress / partial results where useful (e.g. long-running workspace symbol search).

---

## 4. Base Suggested Tools (v0.1)

These form the **minimum viable** tool surface. Higher-level workflow tools can be layered later.

### 4.1 Core Navigation & Intelligence

| Tool Name              | Description                                                                 | Key Parameters                          | Notes |
|------------------------|-----------------------------------------------------------------------------|-----------------------------------------|-------|
| `get_definition`       | Jump to definition of a symbol                                              | `file`, `line`, `character`             | Returns location(s) |
| `get_type_definition`  | Jump to the type definition                                                 | `file`, `line`, `character`             |       |
| `get_references`       | Find all references to a symbol                                             | `file`, `line`, `character`, `includeDeclaration?` |       |
| `get_implementations`  | Find implementations of an interface / trait / abstract class               | `file`, `line`, `character`             |       |
| `get_hover`            | Get type information + documentation at position                            | `file`, `line`, `character`             |       |
| `document_symbols`     | Hierarchical outline of a single file                                       | `file`                                  |       |
| `workspace_symbols`    | Search symbols across the workspace by name                                 | `query`, `limit?`                       | Fuzzy / subsequence |
| `get_diagnostics`      | Get current diagnostics (errors/warnings) for a file or workspace           | `file?`                                 |       |
| `call_hierarchy`       | Incoming / outgoing call hierarchy                                          | `file`, `line`, `character`, `direction` |       |
| `type_hierarchy`       | Supertypes / subtypes                                                       | `file`, `line`, `character`             |       |

### 4.2 Editing & Refactoring

| Tool Name              | Description                                                                 | Key Parameters                          | Notes |
|------------------------|-----------------------------------------------------------------------------|-----------------------------------------|-------|
| `rename_symbol`        | Rename a symbol across the workspace (dry-run by default)                   | `file`, `line`, `character`, `newName`, `apply?` | Returns WorkspaceEdit |
| `code_actions`         | Get available code actions / quick fixes at position                        | `file`, `line`, `character` (or range)  |       |
| `format_document`      | Format a whole document or range                                            | `file`, `range?`                        |       |
| `apply_edit`           | Apply a previously computed WorkspaceEdit                                   | `edit`                                  | Safety: confirm first |

### 4.3 Session / Index Management

| Tool Name              | Description                                                                 | Key Parameters                          | Notes |
|------------------------|-----------------------------------------------------------------------------|-----------------------------------------|-------|
| `open_document`        | Notify the daemon that a file is open (triggers indexing if needed)         | `file`, `languageId?`                   |       |
| `close_document`       | Notify that a file is closed                                                | `file`                                  |       |
| `index_status`         | Report indexing progress / coverage for a workspace                         | `workspaceRoot?`                        |       |
| `reindex`              | Force re-index of a file or entire workspace                                | `path?`                                 |       |
| `list_language_servers`| Show currently running language servers and their status                    | —                                       |       |

### 4.4 Speculative / Simulation (Nice-to-have for v0.1)

| Tool Name              | Description                                                                 | Key Parameters                          | Notes |
|------------------------|-----------------------------------------------------------------------------|-----------------------------------------|-------|
| `simulate_edit`        | Apply an edit in-memory, return new diagnostics without touching disk       | `file`, `edits`                         | Critical for safe agent refactors |
| `simulate_rename`      | Dry-run rename + impact analysis                                            | same as `rename_symbol`                 |       |

---

## 5. SQLite Schema (v0.1)

```sql
-- Schema version for migrations
CREATE TABLE schema_version (
  version INTEGER NOT NULL PRIMARY KEY
);

-- Workspaces / projects
CREATE TABLE workspaces (
  id            INTEGER PRIMARY KEY,
  root_path     TEXT NOT NULL UNIQUE,
  name          TEXT,
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  last_indexed  TEXT
);

-- Source files
CREATE TABLE files (
  id            INTEGER PRIMARY KEY,
  workspace_id  INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  path          TEXT NOT NULL,                  -- relative to workspace root
  absolute_path TEXT NOT NULL,
  language_id   TEXT,
  content_hash  TEXT,                           -- for change detection
  size_bytes    INTEGER,
  mtime         REAL,                           -- filesystem mtime
  last_indexed  TEXT,
  UNIQUE(workspace_id, path)
);

CREATE INDEX idx_files_workspace ON files(workspace_id);
CREATE INDEX idx_files_hash ON files(content_hash);

-- Symbols (functions, classes, variables, etc.)
CREATE TABLE symbols (
  id            INTEGER PRIMARY KEY,
  file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  kind          INTEGER NOT NULL,               -- LSP SymbolKind
  detail        TEXT,                           -- signature / type string
  start_line    INTEGER NOT NULL,
  start_col     INTEGER NOT NULL,
  end_line      INTEGER NOT NULL,
  end_col       INTEGER NOT NULL,
  container_name TEXT,                          -- enclosing class/module
  documentation TEXT
);

CREATE INDEX idx_symbols_name ON symbols(name);
CREATE INDEX idx_symbols_file ON symbols(file_id);
CREATE INDEX idx_symbols_kind ON symbols(kind);

-- References / usages
CREATE TABLE references (
  id            INTEGER PRIMARY KEY,
  symbol_id     INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  start_line    INTEGER NOT NULL,
  start_col     INTEGER NOT NULL,
  end_line      INTEGER NOT NULL,
  end_col       INTEGER NOT NULL,
  is_definition INTEGER NOT NULL DEFAULT 0      -- 1 if this is the defining occurrence
);

CREATE INDEX idx_refs_symbol ON references(symbol_id);
CREATE INDEX idx_refs_file ON references(file_id);

-- Diagnostics (errors, warnings, hints)
CREATE TABLE diagnostics (
  id            INTEGER PRIMARY KEY,
  file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  severity      INTEGER NOT NULL,               -- 1=Error, 2=Warning, 3=Info, 4=Hint
  message       TEXT NOT NULL,
  code          TEXT,
  source        TEXT,                           -- language server name
  start_line    INTEGER NOT NULL,
  start_col     INTEGER NOT NULL,
  end_line      INTEGER NOT NULL,
  end_col       INTEGER NOT NULL,
  recorded_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_diag_file ON diagnostics(file_id);

-- Simple key-value for daemon state / metadata
CREATE TABLE meta (
  key           TEXT PRIMARY KEY,
  value         TEXT
);

-- Optional: call hierarchy edges (can be computed on demand initially)
CREATE TABLE call_edges (
  id            INTEGER PRIMARY KEY,
  from_symbol   INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  to_symbol     INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
  UNIQUE(from_symbol, to_symbol)
);
```

**Notes on schema:**
- Keep it lean for v0.1. Full call graphs and type hierarchies can be materialised later or computed live from language servers.
- Use `content_hash` (SHA-256 of file contents) for cheap change detection.
- All timestamps are ISO-8601 UTC strings for simplicity.

---

## 6. Client / Plugin Integration

### 6.1 Claude Code
- Provide a one-liner install / register command:
  ```bash
  claude mcp add lsp-mcp -- <path-to-binary> --stdio
  ```
- Or a small helper script that writes the correct entry into `~/.claude.json` / project `.mcp.json`.
- Optional Claude Code plugin that auto-detects the daemon and surfaces status.

### 6.2 GrokBuild
- Compatible via standard MCP config (`~/.grok/mcp.json` or project equivalent).
- Provide a `grok mcp add lsp-mcp ...` style helper if the CLI supports it.

### 6.3 Codex / other MCP clients
- Document the standard MCP server launch command + environment variables.
- Supply a JSON snippet for common clients.

### 6.4 OpenClaw / Augment Integration (future)
- Optional deeper hooks: shared memory directory, automatic project registration, status surface inside OpenClaw UI.

---

## 7. Configuration

Primary config file: `~/.config/lsp-mcp/config.toml` (or XDG equivalent).

```toml
[daemon]
db_path = "~/.local/share/lsp-mcp/index.db"
log_level = "info"

[[language_servers]]
name = "typescript"
command = "typescript-language-server"
args = ["--stdio"]
file_extensions = [".ts", ".tsx", ".js", ".jsx"]
root_markers = ["tsconfig.json", "package.json"]

[[language_servers]]
name = "python"
command = "pyright-langserver"
args = ["--stdio"]
file_extensions = [".py"]
root_markers = ["pyproject.toml", "setup.py"]

# ... more servers
```

Environment overrides:
- `LSP_MCP_DB`
- `LSP_MCP_CONFIG`
- `LSP_MCP_LOG_LEVEL`

---

## 8. Non-Functional Requirements

| Area              | Requirement                                      |
|-------------------|--------------------------------------------------|
| **Performance**   | Index updates for a single file < 500 ms on typical hardware; workspace symbol search interactive |
| **Languages**     | v0.1: TypeScript/JS, Python, Rust, Go, Markdown (via Marksman). Expand rapidly. |
| **Reliability**   | Language server crashes must not take down the daemon |
| **Security**      | No network by default; SQLite file permissions; no execution of arbitrary code beyond configured LS binaries |
| **Footprint**     | Daemon idle memory < 100 MB when few projects open |
| **Portability**   | Linux (primary), macOS, Windows (later) |

---

## 9. Implementation Phases (Suggested)

1. **MVP**  
   - Daemon + SQLite + basic file indexer  
   - 6–8 core tools (definition, references, hover, symbols, diagnostics)  
   - TypeScript + Python support  
   - stdio MCP server  

2. **v0.2**  
   - Rename + code actions + simulate_edit  
   - More languages (Rust, Go)  
   - File watcher + incremental updates  

3. **v0.3**  
   - Call hierarchy, type hierarchy  
   - Claude / GrokBuild convenience plugins  
   - Speculative session support  

4. **Later**  
   - Optional embedding layer for semantic search  
   - Shared index with Augment / OpenClaw  
   - HTTP transport + remote access (opt-in)

---

## 10. Open Questions

- Exact binary name / packaging (`lsp-mcp`, `symbol-bridge`, `code-intel-mcp`…)?
- Should the daemon be user-level only, or also support a system service?
- How aggressively should we cache call graphs vs. query language servers live?
- Prefer Rust, Go, or TypeScript for the core implementation?

---

**Next Actions**
1. Decide on name + language.
2. Scaffold the SQLite schema + migration runner.
3. Implement a minimal daemon that can start one language server and answer `get_definition` / `get_hover`.
4. Expose those as MCP tools and test with Claude Code.

This document is intentionally lightweight and actionable. Refine as implementation begins.
