# @fingerskier/langer

Thin installer for the [langer](https://github.com/fingerskier/langer) LSP–MCP bridge.

It downloads the platform binary from GitHub Releases into `~/.langer/bin` and
registers the MCP server with Claude Code, Grok Build, and/or Codex.

## Quickstart

```powershell
npx -y @fingerskier/langer install claude --scope user
npx -y @fingerskier/langer install grok --scope user
npx -y @fingerskier/langer install all --scope user
```

```bash
npx -y @fingerskier/langer install claude --scope user
```

Restart the agent (or press `r` in Grok `/mcps`).

## Commands

| Command | Purpose |
|---------|---------|
| `install <claude\|grok\|codex\|all>` | Download binary + write MCP config |
| `ensure` | Download binary only |
| `path` | Print `~/.langer/bin/langer` path |
| `version` | npm package version |

Options: `--scope user|repo`, `--binary PATH`, `--version X.Y.Z`, `--force`, `--dry-run`.

The package version selects the GitHub release tag (`0.1.0` → `v0.1.0`) unless
you pass `--version`.

## What gets written

| Target | User scope | Repo scope |
|--------|------------|------------|
| Claude | `~/.claude.json` → `mcpServers.langer` | `<cwd>/.mcp.json` |
| Grok | `~/.grok/config.toml` → `[mcp_servers.langer]` | `<cwd>/.grok/config.toml` |
| Codex | `~/.codex/config.toml` → `[mcp_servers.langer]` | `<cwd>/.codex/config.toml` |

MCP entry shape:

```json
{
  "command": "C:/Users/you/.langer/bin/langer.exe",
  "args": ["mcp", "--stdio"]
}
```

## Without npm

```bash
go install github.com/fingerskier/langer/cmd/langer@v0.1.0
claude mcp add langer -- langer mcp --stdio
```

Or grab a release asset from
https://github.com/fingerskier/langer/releases
