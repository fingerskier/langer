# Releasing langer

How binary GitHub Releases and the `@fingerskier/langer` npm installer stay in
sync. End-user install steps live in the [root README](../README.md#install).

## Pieces

| Piece | Role |
|-------|------|
| Go module `github.com/fingerskier/langer` | Source of truth for `cmd/langer` |
| [`.github/workflows/release.yml`](../.github/workflows/release.yml) | On `v*` tag push: cross-build binaries, upload release assets |
| [`npm/`](../npm/) package `@fingerskier/langer` | Downloads those assets into `~/.langer/bin` and wires MCP hosts |

The **npm package version** selects the **GitHub release tag** unless the user
passes `--version`:

| `npm/package.json` `"version"` | Release tag / download URL |
|--------------------------------|----------------------------|
| `0.1.1` | `v0.1.1` → `…/releases/download/v0.1.1/langer_<os>_<arch>[.exe]` |

If the versioned URL 404s and the user did **not** pass `--version`, the
installer also tries
`…/releases/latest/download/langer_<os>_<arch>[.exe]` so a stale npm patch
can still grab the newest GH assets.

Keep those two numbers aligned whenever you publish.

## Release assets (GitHub Actions)

### Trigger

Push an annotated (or lightweight) tag matching `v*`:

```bash
git checkout main
git pull origin main
# working tree clean, tests green

git tag -a v0.1.1 -m "v0.1.1 — <one-line summary>"
git push origin v0.1.1
```

Do **not** expect an old tag (created before the workflow existed) to grow
assets. Cut a **new** tag on a commit that includes
`.github/workflows/release.yml`.

### What CI builds

`CGO_ENABLED=0` cross-compiles from `ubuntu-latest`:

| Asset | Platform |
|-------|----------|
| `langer_windows_amd64.exe` | Windows x64 |
| `langer_linux_amd64` | Linux x64 |
| `langer_linux_arm64` | Linux arm64 |
| `langer_darwin_amd64` | macOS Intel |
| `langer_darwin_arm64` | macOS Apple Silicon |

Also uploaded: per-asset `*.sha256` and a combined `SHA256SUMS`.

### Verify

1. Actions → workflow **release** for the tag is green.  
2. https://github.com/fingerskier/langer/releases shows the tag with all five
   binaries.  
3. Spot-check a download URL (example):

   ```text
   https://github.com/fingerskier/langer/releases/download/v0.1.1/langer_windows_amd64.exe
   ```

### Local binary without a release

```bash
go build -o langer.exe ./cmd/langer   # Windows
npx -y @fingerskier/langer install claude --scope user --binary ./langer.exe
```

Or:

```bash
go install github.com/fingerskier/langer/cmd/langer@v0.1.1
```

## npm package (`@fingerskier/langer`)

Published **manually** from `npm/` (not by the release workflow). CI only
publishes **binaries**.

### Preconditions

- Logged in: `npm whoami` (scope `@fingerskier` must allow publish)  
- `npm/package.json` `"version"` matches the release you want installers to
  download (e.g. `0.1.1` for tag `v0.1.1`)  
- Prefer publishing **after** the GitHub Release for that tag is green so
  `npx … ensure` can download immediately  

### Publish

```bash
cd npm
npm test
# bump version if needed (keep in lockstep with the git tag, without the "v")
# npm version 0.1.1 --no-git-tag-version   # optional; or edit package.json

npm publish --access public
```

The package is public (`publishConfig.access: "public"`). First publish of a
scoped package may require `--access public` explicitly (already set above).

### What the package does **not** do

- It does **not** compile Go.  
- It does **not** upload GitHub Release assets.  
- Its CLI bins are `langer-install` / `langer-npm` only — they intentionally do
  **not** shadow the Go `langer` binary on `PATH`.

Users run:

```bash
npx -y @fingerskier/langer install claude --scope user
```

`npx` executes the package CLI; remaining args are the subcommand.

## Recommended full release checklist

1. **Land code on `main`**  
   `go test ./...`, optional integration tags, `cd npm && npm test`.

2. **Choose version `X.Y.Z`**  
   - Git tag: `vX.Y.Z`  
   - `npm/package.json`: `X.Y.Z`  
   Commit the npm version bump on `main` if it is not already there.

3. **Tag and push**  
   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z — …"
   git push origin main          # if version bump commit is new
   git push origin vX.Y.Z
   ```

4. **Wait for GitHub Release**  
   Confirm assets under the tag.

5. **Publish npm**  
   ```bash
   cd npm && npm publish --access public
   ```

6. **Smoke-test install**  
   ```bash
   npx -y @fingerskier/langer@X.Y.Z ensure
   npx -y @fingerskier/langer@X.Y.Z install claude --scope user --dry-run
   ```

7. **Announce** (optional)  
   Point agents at `npx -y @fingerskier/langer install all --scope user`.

## Version skew and recovery

| Symptom | Cause | Fix |
|---------|--------|-----|
| `npx … ensure` → HTTP 404 | npm version points at a tag with no assets | Publish a new tag with the workflow, or `install --version A.B.C` / `--binary` |
| `npx` runs old installer | npm cache | `npx -y @fingerskier/langer@X.Y.Z …` or clear npx cache |
| Tag exists, no Actions run | Tag pushed before workflow on that commit | Retag a newer commit that has `release.yml`, or run a new `v*` tag |
| npm publish “version already exists” | That version was published | Bump patch (`X.Y.Z+1`), retag if you also need new binaries |

## Related paths

- Workflow: [`.github/workflows/release.yml`](../.github/workflows/release.yml)  
- Installer source: [`npm/`](../npm/)  
- Installer user docs: [`npm/README.md`](../npm/README.md)  
- End-user install: [README.md § Install](../README.md#install)  
