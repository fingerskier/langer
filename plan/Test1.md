# Test1 — plain JS repo probe (Windows)

**Probe context:** agent connected to langer MCP on a plain JS project
(`intuit_api` / `.js` files), no user `config.toml`.

## Resolution (langer main)

| Symptom in probe | Cause | Fix |
| --- | --- | --- |
| `UNSUPPORTED: …\.bin\typescript-language-server` not executable | `findBin` picked npm’s **extensionless** POSIX shim | **`054f59a`** — prefer `.cmd` / PATHEXT; reject extensionless on Windows |
| New terminal tabs / focus steal when LS starts | LS children lacked `CREATE_NO_WINDOW` | **`150061c`** — hide consoles for non-detached children |
| Separate “javascript” profile needed? | **No** — `typescript` profile already claims `.js`/`.jsx`/`.mjs`/`.cjs` + `jsconfig.json` | Same TLS + tsserver |

**Deploy:** rebuild/restart `langer.exe` so MCP daemons load those commits; re-open
the document (ensure re-resolves the `.cmd` path without reinstall).

---

## Original agent notes

❯ please connect to langer MCP and test the tools on this repo

I'll check langer's status and try running the core tools against this repo.

  Searched for 1 pattern, called langer

Good, root resolves correctly and indexing is running. Let me check what files exist here first.

  Searched for 1 pattern, called MCP

Plain JS, not TS. Let's try opening a document to see if a JS language server is configured.

  Called langer, ran 1 shell command

This matches a known issue: langer's TS/JS language server is configured to point at the bare .bin\typescript-language-server shim, which isn't directly executable on Windows — it needs the .cmd wrapper. Let me check the config.

  Searched for 1 pattern, read 1 file, listed 1 directory, ran 3 shell commands

Confirms the known bug: this is a Node shim (POSIX shell script), not a Windows-executable binary — nvoketypescript-language-server.cmd instead of the bare shim on Windows. This is a
langer-side bug, not somethig since there's no config.toml (langer now self-manages the profile). I can't patch langer's Go binary from here.
Let me confirm there's no ovt status.

  Ran 1 shell command

No user-level override existger's managed-profile launcher (a Go binary I don't have source access to from here). I've confirmed and can report
the bug precisely, but can'tanger source repo.

Result: langer MCP connects by a known Windows bug.

- index_status: connects, retly (31 files, was indexing).
- open_document on intuit_api/app.js: fails with UNSUPPORTED: language server "...\.bin\typescript-languagle file.
- Root cause confirmed: langer's managed typescript profile installed correctly via npm (tools list shows typescer points at the bare.bin\typescript-language-server file, which on Windows is a POSIX shell shim — not
directly executable. The reang right next to it aretypescript-language-server.cmd and .ps1.
- No ~/.config/lsp-mcp/confiis (langer now self-managesprofiles), and there's no env var to redirect just the binary path.

This matches a bug already logged in memory
(langer-typescript-server-biepo is also plain JS(intuit_api, intuit_app), not TS, but langer routes .js through the same typescript
profile, so it's blocked the
