package tools

import (
	"archive/zip"
	"compress/gzip"
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
	case "dotnet_tool":
		return d.installDotnetTool(ctx, dir, req.Profile)
	case "none", "":
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

func (d DefaultInstaller) installDotnetTool(ctx context.Context, dir string, prof ProfileManifest) error {
	dotnet, err := d.lookPath("dotnet")
	if err != nil {
		return fmt.Errorf("dotnet not found: %w", err)
	}
	pkg := prof.Package
	if pkg == "" {
		pkg = prof.Bin
	}
	if pkg == "" {
		return fmt.Errorf("dotnet_tool profile missing package")
	}
	toolPath := filepath.Join(dir, "bin")
	if err := os.MkdirAll(toolPath, 0o700); err != nil {
		return err
	}
	args := []string{"tool", "install", pkg, "--tool-path", toolPath}
	if prof.Version != "" {
		args = append(args, "--version", prof.Version)
	}
	cmd := d.command(ctx, dotnet, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Already installed at this tool-path with same version is OK.
		msg := string(out)
		if strings.Contains(msg, "already installed") {
			return nil
		}
		return fmt.Errorf("dotnet tool install: %w: %s", err, truncate(msg, 400))
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
	lower := strings.ToLower(asset.Name)
	isZip := strings.HasSuffix(lower, ".zip")
	isGz := strings.HasSuffix(lower, ".gz") && !strings.HasSuffix(lower, ".tar.gz")

	// Download archive (or plain binary) into the profile dir.
	downloadName := asset.Name
	if !isZip && !isGz && asset.ExtractBin != "" {
		downloadName = asset.ExtractBin
	}
	downloadPath := filepath.Join(dir, downloadName)

	var last error
	for _, u := range asset.URLs {
		last = d.downloadFile(ctx, u, downloadPath, asset.SHA256)
		if last == nil {
			break
		}
	}
	if last != nil {
		return last
	}

	switch {
	case isZip:
		if err := extractZip(downloadPath, dir); err != nil {
			return err
		}
		_ = os.Remove(downloadPath)
	case isGz:
		outName := asset.ExtractBin
		if outName == "" {
			outName = strings.TrimSuffix(asset.Name, ".gz")
			outName = strings.TrimSuffix(outName, ".GZ")
		}
		outPath := filepath.Join(dir, "bin", outName)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
			return err
		}
		if err := extractGzipFile(downloadPath, outPath); err != nil {
			return err
		}
		_ = os.Remove(downloadPath)
		if runtime.GOOS != "windows" {
			_ = os.Chmod(outPath, 0o755)
		}
	default:
		// Plain binary (e.g. marksman.exe).
		if runtime.GOOS != "windows" {
			_ = os.Chmod(downloadPath, 0o755)
		}
	}
	return nil
}

func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	destDir = filepath.Clean(destDir)
	for _, f := range r.File {
		// Zip-slip guard.
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, destDir+string(os.PathSeparator)) && target != destDir {
			return fmt.Errorf("zip entry escapes dest: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := writeZipFile(f, target); err != nil {
			return err
		}
		if runtime.GOOS != "windows" && f.Mode()&0o111 != 0 {
			_ = os.Chmod(target, 0o755)
		}
	}
	return nil
}

func writeZipFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func extractGzipFile(gzPath, dest string) error {
	in, err := os.Open(gzPath)
	if err != nil {
		return err
	}
	defer in.Close()
	gr, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gr.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, gr)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
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
