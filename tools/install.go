package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// InstallRequest describes one ensure install.
type InstallRequest struct {
	ToolsRoot string
	ProfileID string
	Profile   ProfileManifest
}

// Installer installs a profile into the tools prefix.
type Installer interface {
	Install(ctx context.Context, req InstallRequest) error
}

// DefaultInstaller performs npm / go / HTTP installs.
type DefaultInstaller struct {
	// HTTPClient optional; defaults to a client with a long timeout for downloads.
	HTTPClient *http.Client
	// LookPath optional; defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// Command optional factory for external commands (tests).
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func (d DefaultInstaller) client() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (d DefaultInstaller) lookPath(name string) (string, error) {
	if d.LookPath != nil {
		return d.LookPath(name)
	}
	return exec.LookPath(name)
}

func (d DefaultInstaller) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if d.Command != nil {
		return d.Command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// Install dispatches on profile kind.
func (d DefaultInstaller) Install(ctx context.Context, req InstallRequest) error {
	dir := ProfileDir(req.ToolsRoot, req.ProfileID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	switch strings.ToLower(req.Profile.Kind) {
	case "npm":
		return d.installNPM(ctx, dir, req.Profile)
	case "go_install":
		return d.installGo(ctx, dir, req.Profile)
	case "github_release":
		return d.installGitHub(ctx, dir, req.Profile)
	case "dotnet_tool", "none", "":
		return fmt.Errorf("kind %q not installable", req.Profile.Kind)
	default:
		return fmt.Errorf("unknown install kind %q", req.Profile.Kind)
	}
}

func (d DefaultInstaller) installNPM(ctx context.Context, dir string, prof ProfileManifest) error {
	npm, err := d.lookPath("npm")
	if err != nil {
		// Windows often exposes npm.cmd only via PATHEXT; try npm.cmd.
		if runtime.GOOS == "windows" {
			npm, err = d.lookPath("npm.cmd")
		}
		if err != nil {
			return fmt.Errorf("npm not found: %w", err)
		}
	}
	if len(prof.Packages) == 0 {
		return fmt.Errorf("npm profile has no packages")
	}
	args := []string{"install", "--prefix", dir, "--no-fund", "--no-audit"}
	args = append(args, prof.Packages...)
	cmd := d.command(ctx, npm, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("npm install: %w: %s", err, truncate(string(out), 400))
	}
	return nil
}

func (d DefaultInstaller) installGo(ctx context.Context, dir string, prof ProfileManifest) error {
	goBin, err := d.lookPath("go")
	if err != nil {
		return fmt.Errorf("go not found: %w", err)
	}
	if prof.Package == "" {
		return fmt.Errorf("go_install profile missing package")
	}
	cmd := d.command(ctx, goBin, "install", prof.Package)
	cmd.Env = append(os.Environ(), "GOBIN="+filepath.Join(dir, "bin"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go install: %w: %s", err, truncate(string(out), 400))
	}
	return nil
}

func (d DefaultInstaller) installGitHub(ctx context.Context, dir string, prof ProfileManifest) error {
	asset, ok := prof.AssetForPlatform()
	if !ok {
		return fmt.Errorf("no asset for %s", PlatformKey())
	}
	if len(asset.URLs) == 0 {
		return fmt.Errorf("asset has no urls")
	}
	destName := asset.Name
	if asset.ExtractBin != "" && !strings.HasSuffix(strings.ToLower(asset.Name), ".zip") &&
		!strings.HasSuffix(strings.ToLower(asset.Name), ".gz") {
		destName = asset.ExtractBin
	}
	dest := filepath.Join(dir, destName)

	var last error
	for _, u := range asset.URLs {
		last = d.downloadFile(ctx, u, dest, asset.SHA256)
		if last == nil {
			break
		}
	}
	if last != nil {
		return last
	}

	// Plain binary downloads need +x on Unix.
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dest, 0o755)
	}

	// Minimal handling: if the asset is the binary itself (common for marksman.exe), done.
	// Zip/gz extraction can be added when checksums are pinned; until then require non-archive assets
	// or pre-extracted names.
	lower := strings.ToLower(asset.Name)
	if strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".gz") {
		return fmt.Errorf("archive assets need extract support (asset %s)", asset.Name)
	}
	return nil
}

func (d DefaultInstaller) downloadFile(ctx context.Context, url, dest, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, res.StatusCode)
	}

	tmp := dest + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	_, copyErr := io.Copy(w, res.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(wantSHA, sum) {
		_ = os.Remove(tmp)
		return fmt.Errorf("checksum mismatch for %s", filepath.Base(dest))
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
