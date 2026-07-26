# langer

LSP–MCP bridge for coding agents: compiler-accurate definitions, references,
hover, diagnostics, renames, and speculative edits — scoped per workspace.

- **[SPEC.md](SPEC.md)** — technical specification  
- **[PLAN.md](PLAN.md)** — milestone plan and v0.1 verification  

## Why

- **Correctness grep can't provide** — real call sites, re-exports, safe renames  
- **Project compilation context** — `tsconfig`, virtualenv, `go.mod`, not generic text  
- **Ground truth after edits** — `simulate_edit` type-checks without writing disk  
- **Token economics** — structured tools instead of stuffing whole files into context  

## Install

### One-liner (recommended)

Downloads the GitHub Release binary for your OS into `~/.langer/bin` and
registers MCP with your agent(s):

```powershell
npx -y @fingerskier/langer install claude --scope user
npx -y @fingerskier/langer install grok --scope user
npx -y @fingerskier/langer install codex --scope user
# or everything:
npx -y @fingerskier/langer install all --scope user
```

```bash
npx -y @fingerskier/langer install all --scope user
```

Options: `--scope repo`, `--dry-run`, `--force`, `--binary PATH`, `--version 0.1.0`.  
Details: [`npm/README.md`](npm/README.md).

Restart Claude/Codex after install. For Grok, restart or press `r` in `/mcps`.

> **Note:** `npx` needs a published release asset for your platform. Tag pushes
> build those via [`.github/workflows/release.yml`](.github/workflows/release.yml).
> Until assets exist for a tag, use `go install` below or `--binary` pointing at
> a local build.

### From source / Go toolchain

```bash
go install github.com/fingerskier/langer/cmd/langer@v0.1.0
```

Put `$(go env GOPATH)/bin` on your `PATH`, then register MCP yourself:

```bash
claude mcp add langer -- langer mcp --stdio
```

**Grok** (`~/.grok/config.toml` or repo `.grok/config.toml`):

```toml
[mcp_servers.langer]
command = "langer"
args = ["mcp", "--stdio"]
enabled = true
startup_timeout_sec = 60
```

**Claude** (`.mcp.json` or `~/.claude.json`):

```json
{
  "mcpServers": {
    "langer": {
      "command": "langer",
      "args": ["mcp", "--stdio"]
    }
  }
}
```

### Release binaries

On every `v*` tag, CI publishes multi-OS assets:

| Asset | Platform |
|-------|----------|
| `langer_windows_amd64.exe` | Windows x64 |
| `langer_linux_amd64` | Linux x64 |
| `langer_linux_arm64` | Linux arm64 |
| `langer_darwin_amd64` | macOS Intel |
| `langer_darwin_arm64` | macOS Apple Silicon |

https://github.com/fingerskier/langer/releases

## CLI

```text
langer mcp --stdio        # MCP frontend (auto-starts workspace daemon)
langer daemon <root>      # run daemon explicitly
langer status             # index / daemon status for cwd
```

Config defaults follow XDG (`~/.config/lsp-mcp/config.toml`). Language servers
are declarative registry entries — no built-in commands (SPEC §9: never run
workspace-local binaries without opt-in).

Optional:

```toml
idle_timeout = "30m"
```

## Development

```bash
go test ./...
go test -tags=integration ./...
```

Primary gate: native host OS. Integration tests need
`typescript-language-server` and `pyright` on `PATH`.

## License

See [LICENSE](LICENSE).
