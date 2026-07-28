# Managed language servers — primary picks & alternates

**Date:** 2026-07-28  
**Parent plan:** `plan/PLAN.md`  
**Rule:** Exactly **one primary** per profile for ensure/manifest. Alternates are recorded for future swaps, not installed by default.

Checksums and pinned versions live in the **release tools manifest**, not here.

---

## Primary registry

| Profile ID | Languages / extensions (indicative) | Primary LS | Install shape (typical) | Notes |
| --- | --- | --- | --- | --- |
| `markdown` | `.md`, `.mdx` | **marksman** | GitHub release binary | Outline, links, wiki-style; MDX best-effort |
| `typescript` | `.ts`, `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs` | **typescript-language-server** | npm into tools prefix | Needs `tsserver.path`; managed tsserver default, user toml can override |
| `python` | `.py`, `.pyi` | **pyright** | npm (`pyright`) into tools prefix | Covers CPython; MicroPython via same profile + project config/types |
| `go` | `.go` | **gopls** | Go module / official release into tools prefix | Root markers: `go.mod` |
| `rust` | `.rs` | **rust-analyzer** | GitHub release binary | Root markers: `Cargo.toml` |
| `html` | `.html`, `.htm` | **vscode-html-language-server** | npm (`vscode-langservers-extracted`) | HTMX: same server; project tags/attrs are editor/data, not a separate LS |
| `css` | `.css`, `.scss` | **vscode-css-language-server** | same package as HTML stack | SCSS via CSS LS where supported |
| `xml` | `.xml` | **lemminx** | GitHub / Red Hat binary or package | Schema-aware XML |
| `json` | `.json`, `.jsonc` | **vscode-json-language-server** | `vscode-langservers-extracted` | JSONC included |
| `csharp` | `.cs` | **csharp-ls** | dotnet tool / release into tools prefix | Lighter than full OmniSharp for agent use |
| `cpp` | `.c`, `.h`, `.cpp`, `.cc`, `.cxx`, `.hpp`, … | **clangd** | GitHub / LLVM release binary | **One profile** for C and C++ (same LS); `compile_commands.json` discovery |
| `gdscript` | `.gd`, `.tscn`? | **Godot language server** (Godot 4 LSP) | Documented Godot/editor headless or community binary — **packaging TBD** | Include if ensure path is reliable; else ship profile when fetch is solid |
| `csv` | `.csv` | **Best-effort / deferred primary** | See alternates | No strong mainstream LS; enable when a pin-able server exists |
| `tsv` | `.tsv` | **Same as csv** (shared implementation) | — | Prefer one `csv` profile serving both extensions if one LS covers both |

### Folded (not separate managed profiles)

| Surface | Handling |
| --- | --- |
| **MicroPython** | **`python`** profile; optional user/project config for typesheds/stubs — not a second ensure |
| **HTMX** | **`html`** profile (not a separate language server) |
| **MDX** | **`markdown`** extensions list (marksman primary; quality may vary) |
| **SCSS** | **`css`** extensions list |
| **JSONC** | **`json`** extensions list |

---

## Alternates (not default)

| Profile | Alternates | When to consider |
| --- | --- | --- |
| `markdown` | markdown-oxide, unified-language-server, manual remark stack | Performance, different feature set, pure Rust preference |
| `typescript` | vtsls (`@vtsls/language-server`) | Better TS performance / inlay parity with VS Code |
| `python` | basedpyright, pylsp, Jedi language server | Stricter typing fork; lighter / different plugins |
| `go` | (none strong) | — |
| `rust` | (none strong) | — |
| `html` | Tailwind LS (complement), emmet-ls (complement) | Complements, not full HTML replacements |
| `css` | tailwindcss-language-server | Utility-class projects |
| `xml` | (editor-embedded XML servers) | If lemminx packaging is painful |
| `json` | (same langservers-extracted only, practically) | — |
| `csharp` | OmniSharp, Roslyn Language Server (`Microsoft.CodeAnalysis.LanguageServer`) | Heavier, fuller IDE fidelity |
| `cpp` | ccls | Alternate C/C++ indexer |
| `gdscript` | godot-gdscript-toolkit / community LSP forks | If official Godot LSP packaging fails |
| `csv` / `tsv` | Custom thin LS, redhat.vscode-xml-style data tools, “no LS” (syntax only via other means) | Only if a maintained stdio LSP + checksummable asset appears |

---

## Profile granularity rule

**Depends on the language server** (`plan/PLAN.md` decision):

- **One profile, multiple extensions** when a single LS is the industry default (e.g. `typescript` for JS/TS, `cpp` for C and C++, `python` for MicroPython).  
- **Split profiles** only when primaries differ or ensure artifacts differ (e.g. `html` vs `css` may share an npm package but remain separate profile IDs if spawn/init options differ — implementer choice; prefer fewer profile IDs when one process can serve both).

Shared npm package (`vscode-langservers-extracted`) may install **once** and satisfy `html` + `css` + `json` ensures (implementation detail; single-flight per profile still applies).

---

## Packaging notes for implementers

- Prefer **checksummed** primary URL + **backup URLs** in the release manifest.  
- Node LSs: install into `~/.langer/tools/<profile>/` (or shared `node/` tree), never resolve via project `npx`.  
- Native LSs: platform triple assets (`windows-amd64`, etc.).  
- `gdscript` / `csv`: do not block Phases A–C; add when primary is pin-able end-to-end.
