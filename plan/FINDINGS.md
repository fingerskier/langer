# Indexing findings (2026-07-28)

Review of the 0.7.7 diet failure (`index` stuck 0/N, `get_references` /
`workspace_symbols` `NOT_READY`) and the follow-up soft-skip / stale-diag fix.

## Done in tree (hardening items 1–5)

1. **`AbandonFile`** — soft-skip removes blank-hash / missing rows without
   advancing `reference_generation` or marking every reference set incomplete.
2. **Stale diagnostics** — index still commits symbols + diags; **skips**
   reference collection so partial LS refs are not published as `complete=1`.
3. **`maxIndexNotReadyRetries` (12)** — one stuck path soft-skips instead of
   pinning the single index worker forever.
4. **`index_status`** — `files_skipped` + `skipped[]` (`path` / `code` /
   `message`); `langer status` prints them. Dead `failed` map removed in favor
   of session `skipped`.
5. **Tests** — blank-hash abandon, no refs when diags stale, store
   `AbandonFile` vs invalidate poison, soft-skip status surface.

Primary root-cause fix remains: do not treat possibly-stale diags as infinite
`NOT_READY`; do not `InvalidateFile` on per-file soft-skip.

## Remaining / deferred

### Product / observability

- Soft-skipped paths are **not re-healed** until watcher admit or daemon
  restart (healer walks `known` only). Transient LS blips can leave permanent
  session holes without a file change.
- Managed **css / json / html** (vscode-langservers-extracted) advertise
  **pull** diagnostics. Indexing used to call push `Diagnostics()` whenever
  CapPull was true → `UNSUPPORTED: … does not provide get_diagnostics
  (publishDiagnostics)` and soft-skip. Fixed in 0.7.8 by indexing without
  diags for pull-only servers. Pull diagnostic capture still TODO.
- `index_status` caps reported skip rows (32); full count still in
  `files_skipped`.

### Store / concurrency design

- Normal **`InvalidateFile` / `DeleteFile`** still mark **all** reference sets
  incomplete and bump `reference_generation` (intentional for content change).
  Heavy edit bursts create long `NOT_READY` windows while the single worker
  re-completes sets.
- Soft-skip after a successful **`PutFile`** leaves the hashed row (usable
  per-file cache) but does not roll back partial `ReplaceReferences` mid-loop
  if that ever returns a hard error after some keys committed.
- **`requireWorkspaceCacheReady`** is still “any blank hash in `files`” —
  AbandonFile closes the soft-skip hole; other writers must not leave blank
  rows for paths outside `known`.

### Live query path (unchanged by design)

- Live **`awaitAnalysis`** still returns `NOT_READY` on stale settle (agent
  completeness for definition/references on open). Index path deliberately
  diverges for progress + incomplete-ref safety.
- Index path still has **no explicit awaitAnalysis** before documentSymbol;
  only the stale-diag gate suppresses refs.

### Status math

- When `scanComplete`, `files_indexed` local merge uses `known − pending`, not
  a direct store hash recount. Soft-skip keeps the invariant today; re-check if
  known/pending semantics change.

### Docs / API

- `docs/ARCHITECTURE.md` `IndexStatusResult` snippet should eventually list
  `files_skipped` / `skipped` (protocol is source of truth now).
- MCP tool description for `index_status` should mention skipped coverage when
  the frontend is next touched.

### Release

- **0.7.8** cut over **0.7.7**. Diet smoke (local binary): `ready (35/35 files)`
  after pull-diag skip fix; no soft-skips on css/json/html. Prior intermediate
  build showed 14 skips due to push Diagnostics on pull-only servers.

## Related Augment memories

- `ISSUE/index-stuck-zero-and-ref-poison-on-per-file-fail`
- `WORK/fix-index-stale-diags-and-soft-skip-per-file-fail`
- `ISSUE/index-soft-skip-review-gaps-2026-07-28`
- `TODO/index-harden-soft-skip-and-stale-refs` (items 1–5 addressed in tree)
- `TODO/css-json-html-index-soft-skip-followup`
