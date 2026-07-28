# Langer — Managed language servers & onboarding

**Date:** 2026-07-28 (ambiguities resolved same day)  
**Status:** Decisions complete; Phase A + core wiring landed (2026-07-28); shipped as **v0.7.0**; remaining = pins, smoke, disabled profiles  
**Context:** Follow-up from `plan/Augment_test.md` and managed-tools design.  
**Companion:** root `PLAN.md` (M0–M6 v0.1 done). **LS picks:** `plan/LANGUAGE_SERVERS.md`.

---

## Goal

Agent-transparent language intelligence without a hand-written empty registry:

1. **Detect** language for a path (extension + root markers).  
2. If the profile’s LS is **missing** under langer’s **global tools dir**, **retrieve** it (network; not baked into `langer.exe`).  
3. **Run** that managed binary **against** the workspace (`cwd` / `rootUri` = project).  
4. **Use** it for the existing MCP tool surface.

**Security:** never execute workspace-local binaries without `allow_workspace_local`. Managed tools under e.g. `~/.langer/tools/…`.

---

## Decisions

### Core (2026-07-28)

| # | Topic | Decision |
| --- | --- | --- |
| 1 | Who installs | **Agent-transparent** ensure on first need |
| 2 | Network vs runtime | **Install from network**; afterward **local LSP only** |
| 3 | Versions | **Manifest pins** (known-good versions/assets) |
| 4 | Fetch | npm and/or release assets into tools prefix; **absolute** command paths |
| 5 | Profiles | See [Language profile set](#language-profile-set); primaries in `LANGUAGE_SERVERS.md` |
| 6 | Failure UX | **Useful, non-prose** messages (structured SPEC §3.6 + short `message`) |
| 7 | tsserver | **Reasonable managed defaults** + **user toml** overrides where prudent; no silent workspace `.bin` |
| 8 | Concurrent ensure | **Single-flight** per profile |
| 9 | Updates | Manifest ships **in the langer release**; refreshed by **`npx @fingerskier/langer install…`** (and tools update CLI). **Never mid-session** binary replace |
| 10 | Offline / ensure fail | **Fail cleanly**, terse useful message |

### Ambiguities resolved (2026-07-28, follow-up)

| # | Topic | Decision |
| --- | --- | --- |
| A1 | CSV / TSV / GDScript / MicroPython | **Include as many as we can** — see profile set + `LANGUAGE_SERVERS.md` (MicroPython folds into Python; CSV/TSV/GDScript best-effort) |
| A2 | Manifest updates | Manifest is **part of the release**; gets updates when the user runs **`npx @fingerskier/langer install…`** (release/npm version ↔ new manifest). Not a separate free-floating “latest LS” channel mid-flight |
| A3 | Upgrade timing | **Never mid-session** — apply new tools/manifest only on new ensure after release refresh, daemon restart boundary, or explicit `tools update` **before** spawning; never replace a running LS binary in-session |
| A4 | Which LS | **One primary per profile**; **alternates** documented in `plan/LANGUAGE_SERVERS.md` only |
| A5 | Extra web formats | **SCSS, MDX, JSONC, HTMX** in scope (extensions / fold into HTML/CSS/JSON/Markdown as appropriate) |
| A6 | tsserver / defaults | **User toml** for overrides; **prudent managed defaults** (e.g. managed tsserver path under tools prefix) |
| A7 | Error style | **Useful but avoid prose** — codes + short messages, not essays |
| A8 | Profile granularity | **Depends on LS** — one profile when one LS covers the family (e.g. C+C++ → `cpp`); split when ensure/spawn differs |
| A9 | Manifest integrity | **Checksums required**; **backup URLs** allowed in manifest |
| A10 | Disk / install scope | **Lazy ensure only** — install profiles when first needed, not all on first langer use |
| A11 | Config precedence | **Langer-global managed profiles by default**; **local/user `[[language_servers]]` overrides** when present |

### Policy paragraph (normative intent)

When intelligence is needed, langer maps the file to a **known profile**. If the primary server is missing under the user tools prefix, langer **ensures** a **manifest-pinned** install (network; checksum verified; backup URLs if primary fails). Spawn uses the **managed** absolute path with **workspace** as LSP root. **User/local config overrides** managed defaults. Ensure is **agent-transparent**, **single-flight**, **lazy**, and **fails cleanly** with short structured errors. The **tools manifest ships with each langer release** and is refreshed via **`npx @fingerskier/langer install…`** (or `tools update`); tool binary changes are **never applied mid-session** to a running language server.

---

## Language profile set

Primaries and alternates: **`plan/LANGUAGE_SERVERS.md`**.

| Profile ID | In scope | Notes |
| --- | --- | --- |
| `markdown` | yes | `.md`, `.mdx`; primary marksman |
| `typescript` | yes | TS/JS family; TLS + managed/default tsserver, user toml override |
| `python` | yes | pyright; **MicroPython** uses this profile |
| `go` | yes | gopls |
| `rust` | yes | rust-analyzer |
| `html` | yes | HTML + **HTMX** (same LS) |
| `css` | yes | `.css`, **`.scss`** |
| `xml` | yes | lemminx |
| `json` | yes | `.json`, **`.jsonc`** |
| `csharp` | yes | csharp-ls primary |
| `cpp` | yes | **C and C++** via clangd |
| `gdscript` | yes (best-effort) | Include when pin-able; do not block core phases |
| `csv` / `tsv` | yes (best-effort) | Shared or paired; enable when a pin-able LS exists |

### Explicit non-goals

- LS binaries inside `langer.exe`  
- Default workspace `node_modules/.bin` execution  
- Mid-session tool binary replacement  
- Installing every profile on first use  
- Multiple primaries fighting in one profile (alternates are docs-only until swapped)

---

## Implementation work

### Phase A — Foundation

- [x] Tools prefix: `~/.langer/tools/…` (`LANGER_TOOLS_DIR`, package `tools`)  
- [x] Embedded release manifest `tools/manifest.json` (pins, backup URL slots, sha256 fields)  
- [x] `ensure(profile)`: single-flight; checksum when sha256 set; short errors  
- [x] Lazy ensure on first need (`workspace` → `tools.Manager`)  
- [x] User `[[language_servers]]` overrides managed profiles by extension  
- [x] No mid-session binary replace for live servers (`Offer` + install-if-missing marker)  
- [x] `Load()` still has zero built-in registry entries (SPEC §9)  
- [x] Unit tests green (`go test ./...`); daemon harness sets `DisableManagedTools`

### Phase B — First profiles

- [x] Manifest + install path: `markdown`, `typescript`, `python`  
- [ ] Live smoke with network (augment TS, plan MD)  
- [ ] Fill real sha256 for marksman (and other github assets)

### Phase C — Systems

- [x] Manifest: `go` (`go install`), `rust` (github; **archive extract still TODO**)  
- [ ] `cpp` / clangd — disabled until pins

### Phase D — Web / data / other

- [x] Manifest: `html`, `css`, `json` (shared npm install)  
- [ ] `xml`, `csharp`, `gdscript`, `csv`/`tsv` — disabled until pin-able

### Phase E — CLI & updates

- [x] `langer tools list|ensure|update`  
- [x] npm proxy: `npx @fingerskier/langer tools …` → ensure binary + exec  
- [ ] Docs: network once per profile / after pin bump

### Phase F — UX

- [x] Ensure failures → short `protocol.Error`  
- [ ] `langer status` shows tools present/missing  
- [ ] MCP empty-symbols vs ensure-error polish

---

## Out of date / completed (do not re-drive)

- ~~Manual-only config.toml as sole long-term path~~ → managed ensure + profiles  
- ~~Markdown low leverage~~ → first-class  
- ~~Empty symbols = no symbols in repo~~ → explicit ensure/unsupported  
- ~~Open ambiguity list~~ → resolved above (2026-07-28)

v0.1 M0–M6: root `PLAN.md` only.

---

## References

- `plan/Augment_test.md`  
- `plan/LANGUAGE_SERVERS.md`  
- Root `PLAN.md`, `SPEC.md` §9, §3.6  
