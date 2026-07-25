# langer
LSP MCP service for search and indexing

## Why per-project LSP for AI agents

- **Correctness grep can't provide** — references are the real call sites, not
  string matches in comments, tests, or shadowed imports; definition-jumping
  resolves through re-exports, aliases, and inheritance; renames are safe
  across files.
- **Answers in the right compilation context** — a language server's results
  depend on the repo's own `tsconfig.json` paths, virtualenv, `go.mod`, flags,
  and dependency versions; per-repo scoping means the type of `user.profile`
  is *this* project's truth, and one repo's heavy type-check never degrades
  another's.
- **Ground truth after edits** — diagnostics show immediately, without running
  anything, whether an edit type-checks; `simulate_edit` gives that feedback
  before touching disk, turning the agent loop into propose → check → apply.
- **Token economics** — one structured tool call replaces reading whole files
  to reconstruct a call graph; hover returns signature + docs without opening
  the defining file; the persistent index keeps `workspace_symbols` and
  references fast on repos far too large for context.

In short: the LSP gives the agent the same semantic ground truth the compiler
has, and per-project scoping is what makes that truth correct — in real
codebases, meaning is a function of the project's config, not just the text.

- **[SPEC.md](SPEC.md)** — authoritative technical specification (v0.3)
- **[PLAN.md](PLAN.md)** — milestone implementation plan for the coding agent
