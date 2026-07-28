# Langer evaluation against `@fingerskier/augment`

**Date:** 2026-07-26  
**Subject repo:** `C:\dev\fingerskier\agent\augment` (`@fingerskier/augment`, local RAG memory for coding agents)  
**Probe host:** Grok Build with langer MCP enabled  
**Purpose:** Peruse the langer MCP tool surface and evaluate fitness for real work on the augment TypeScript codebase.

---

## Executive summary

| Question | Answer |
| --- | --- |
| Right product for this repo? | **Yes** — pure TypeScript ESM package is a good langer customer |
| Ready to use today? | **No** — language-server registry is empty |
| Highest-ROI next step | Configure TypeScript language server in `~/.config/lsp-mcp/config.toml` and smoke-test nav tools |
| Expected payoff once fixed | Compiler-accurate defs/refs, hover, diagnostics, safe rename, `simulate_edit` without stuffing whole modules into context |

**Bottom line:** langer MCP is installed, daemon is healthy, and the tool surface matches this repo well — but every code-intelligence call fails or no-ops until at least one `[[language_servers]]` entry exists.

---

## Subject repo snapshot

- **What it is:** Local RAG memory system for agentic work (MCP server, daemon, CLI, dashboard, embeddings, sleep/decoction workflows).
- **Language / toolchain:** TypeScript strict, ES2022, `module`/`moduleResolution` NodeNext; `typescript` as devDependency; scripts: `build` (`tsc`), `test` (`tsx`), `check`.
- **Layout:** `src/` (mcp, daemon, memory, dashboard, embeddings, …), `test/`, `integrations/`, `docs/`, compiled `dist/`.
- **Scale (probe-time):** ~38 `src/**/*.ts` files, ~10k source lines, ~30 test TS files.
- **tsconfig note:** `"include": ["src/**/*.ts"]` only — tests under `test/` are outside the TypeScript project. A default TLS session may not typecheck tests the way `tsx` test runs do.

---

## Langer installation observed

**Grok MCP** (`~/.grok/config.toml`):

```toml
[mcp_servers.langer]
command = "C:/Users/finge/.langer/bin/langer.exe"
args = ["mcp", "--stdio"]
enabled = true
startup_timeout_sec = 60
```

**CLI status** (cwd = augment):

```text
workspace:  C:\dev\fingerskier\agent\augment
config:     C:\Users\finge\.config\lsp-mcp\config.toml  (not found — using defaults)
database:   C:\Users\finge\.local\share\lsp-mcp\index.db
socket:     C:\Users\finge\.local\share\lsp-mcp\daemon.sock
log level:  info
servers:    none configured
daemon:     connected
index:      ready (0/0 files)
```

**Design constraint (by product intent):** No built-in language servers (SPEC §9). Registry is entirely declarative / user-supplied. Missing config ⇒ no `.ts` support.

---

## MCP tool inventory (12 tools)

Evaluated surface as exposed to the agent:

| Tool | Role | Value for augment |
| --- | --- | --- |
| `index_status` | Index / LS readiness; honor `NOT_READY` + `retry_after_ms` | Operational gate for everything else |
| `open_document` | Mark file active; start configured LS | Required before deep nav on a file |
| `close_document` | Release session reference | Hygiene for long sessions |
| `document_symbols` | Semantic outline of one document | Strong for large modules (`src/mcp/server.ts`, service, daemon) |
| `workspace_symbols` | Fuzzy symbol search across workspace | Better than grep for APIs (`createMcpServer`, project/recall paths) |
| `get_definition` | Compiler-accurate definition(s) | High — ESM NodeNext, multi-package internal layout |
| `get_references` | Semantic references (incl. declaration) | High — real call sites vs string lookalikes |
| `get_hover` | Type signature + docs at position | High during refactors / API wiring |
| `get_diagnostics` | File or workspace compiler diagnostics | High — faster mid-edit feedback than full `npm run build` |
| `rename_symbol` | Dry-run rename → `edit_token` + content hashes | High for shared types / MCP handlers |
| `apply_edit` | Apply prior dry-run; reject `STALE_EDIT` | Safe multi-file write after user/agent approval |
| `simulate_edit` | Full-file in-memory overlay + diagnostics, no disk write | High for try/typecheck/discard agent loops |

**Error model (agent-friendly):** `NOT_READY`, `UNSUPPORTED`, `NO_RESULT`, `STALE_EDIT`, `WORKSPACE_UNKNOWN`, `SERVER_CRASHED`, `INTERNAL` — domain failures should not escape as raw protocol crashes.

**Position convention:** 0-based line; character is UTF-16 code unit offset (LSP default).

---

## Live probe results (augment workspace)

| Call | Result |
| --- | --- |
| `index_status` | `{ "state": "ready", "files_indexed": 0, "files_total": 0, "root_path": "C:\\dev\\fingerskier\\agent\\augment" }` |
| `workspace_symbols({ query: "searchMemories", limit: 10 })` | `{ "symbols": [] }` — empty registry, not “no symbols in repo” |
| `open_document({ path: "src/mcp/server.ts", language_id: "typescript" })` | **`UNSUPPORTED`**: `no language server is configured for .ts` |
| `open_document({ path: "package.json", language_id: "json" })` | **`UNSUPPORTED`**: `no language server is configured for .json` (expected without a JSON LS) |
| `get_diagnostics` (workspace, no path) | `{ "diagnostics": [] }` — not meaningful without an LS |

### Binary / PATH notes from probe

- `langer.exe` present at `C:\Users\finge\.langer\bin\langer.exe`.
- `typescript-language-server` **not** found via `where.exe` on PATH during probe.
- Project-local tsserver **does** exist:  
  `C:\dev\fingerskier\agent\augment\node_modules\typescript\lib\tsserver.js`
- langer integration notes: real `typescript-language-server` often needs  
  `initialization_options.tsserver.path` or initialize fails.

---

## Fit assessment

### Why this repo is a good langer target

1. **Language match** — TypeScript + `tsconfig.json` + `package.json` root markers; first-class LS target.
2. **Cross-module structure** — `mcp/` ↔ `daemon/` ↔ `service/` ↔ `memory/` ↔ `dashboard/` benefits from semantic refs over grep.
3. **Agent edit loops** — `simulate_edit` + diagnostics complement `npm run check` without replacing tests/build.
4. **Orthogonal to Augment MCP** — Augment stores durable project memory; langer provides live code intelligence. No product conflict.

### Lower leverage / out of scope for langer

- Markdown under `plan/`, `docs/`, integration skills
- Generated `dist/`
- Pure packaging / JSON config without a JSON language server

### Still use non-langer tools for

- File read/write, git, packaging
- `npm test` / `npm run build` / `npm run check`
- Augment memory workflow (`init_project`, `search`, `upsert`, sleep/dream, …)

---

## Recommended config (not applied during evaluation)

Create `C:\Users\finge\.config\lsp-mcp\config.toml` (or set `LANGER_CONFIG_PATH`). Paths must match the machine; example shape:

```toml
# Optional
# idle_timeout = "30m"

[[language_servers]]
name = "typescript"
command = "typescript-language-server"   # or absolute path to .cmd on Windows
args = ["--stdio"]
file_extensions = [".ts", ".tsx", ".js", ".jsx"]
root_markers = ["tsconfig.json", "package.json"]

# Often required for typescript-language-server 5.x — point at real tsserver
# [language_servers.initialization_options]
# Use the correct TOML nesting per config schema; see langer config package
# and testdata notes for `tsserver.path`.
```

**Security:** Do not set `allow_workspace_local = true` unless the command deliberately resolves inside the workspace tree (SPEC §9). Prefer a user-level / PATH language server over workspace-local binaries.

**After config:**

1. Restart Grok MCP (or press `r` in `/mcps`).
2. `langer status` from augment cwd → servers listed, index not stuck at 0/0 forever without progress.
3. Smoke:
   - `open_document` → `src/mcp/server.ts`
   - `document_symbols` on same path
   - `get_definition` / `get_references` on an exported symbol (e.g. `createMcpServer`)
   - `get_diagnostics` after a deliberate type error (optional)
   - `simulate_edit` with a small invalid change (optional)

---

## Suggested high-leverage use once configured

1. Jump definition/refs across MCP tool handlers and daemon client APIs.
2. Workspace symbol search instead of broad greps for public surfaces.
3. `simulate_edit` before writing risky type/API changes.
4. Workspace diagnostics after multi-file edits.
5. Rename dry-run for shared types and tool names; apply only when hashes still match.

---

## Blockers checklist

- [ ] `~/.config/lsp-mcp/config.toml` exists with TypeScript `[[language_servers]]`
- [ ] `typescript-language-server` installable/resolvable on Windows PATH (or absolute command)
- [ ] `tsserver.path` (or equivalent) set if TLS initialize fails without it
- [ ] Smoke nav tools succeed on `src/mcp/server.ts`
- [ ] Decide whether tests under `test/` need a separate TS project / include for diagnostics

---

## Cross-links / memory

Augment project memories recorded from the same session:

- `fingerskier/augment/ISSUE/langer-no-language-servers-configured.md`
- `fingerskier/augment/TODO/wire-langer-typescript-ls.md` (DEPENDS_ON the ISSUE)

Related product context lives in the langer repo itself (`SPEC.md`, `PLAN.md`, `docs/ARCHITECTURE.md`, integration fixtures under `testdata/`).

---

## Conclusion

Against **augment**, langer’s tool design is appropriate and would materially improve agent navigation and edit verification on a mid-size TypeScript codebase. The evaluation did **not** fail on product fit; it failed on **empty language-server configuration**. Treat empty `workspace_symbols` / diagnostics as **configuration failure**, not as evidence that the repo has no symbols.
