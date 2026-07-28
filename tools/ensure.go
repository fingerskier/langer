package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/protocol"
)

// Manager resolves managed language-server profiles with lazy ensure.
type Manager struct {
	ToolsRoot string
	Manifest  *Manifest
	Installer Installer

	mu    sync.Mutex
	inflight map[string]*ensureCall
}

type ensureCall struct {
	done chan struct{}
	entry config.LanguageServer
	err   error
}

// NewManager builds a Manager using the embedded (or env-overridden) manifest.
func NewManager() (*Manager, error) {
	root, err := DefaultToolsDir()
	if err != nil {
		return nil, err
	}
	man, err := LoadManifest()
	if err != nil {
		return nil, err
	}
	return &Manager{
		ToolsRoot:  root,
		Manifest:   man,
		Installer:  DefaultInstaller{},
		inflight:   map[string]*ensureCall{},
	}, nil
}

// CoversExtension reports whether a managed (enabled) profile claims ext without installing.
func (m *Manager) CoversExtension(ext string) bool {
	if m == nil || m.Manifest == nil {
		return false
	}
	_, _, ok := m.Manifest.ProfileForExtension(ext)
	return ok
}

// ResolveEntry returns the language server for path.
//
// Precedence (plan A11): user/local config entries win when they claim the
// extension. Otherwise a managed profile is ensured and returned.
func (m *Manager) ResolveEntry(ctx context.Context, path string, user *config.Config) (config.LanguageServer, error) {
	if user != nil {
		if entry, ok := user.LanguageServerForFile(path); ok {
			return entry, nil
		}
	}
	if m == nil || m.Manifest == nil {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported,
			"no language server for %s", filepath.Ext(path))
	}
	id, prof, ok := m.Manifest.ProfileForExtension(filepath.Ext(path))
	if !ok {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported,
			"no language server for %s", filepath.Ext(path))
	}
	if prof.Disabled {
		reason := prof.DisabledReason
		if reason == "" {
			reason = "profile disabled"
		}
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported,
			"profile %s unavailable: %s", id, reason)
	}
	return m.Ensure(ctx, id)
}

// HasUserOverride reports whether user config claims the same extension as path.
func HasUserOverride(user *config.Config, path string) bool {
	if user == nil {
		return false
	}
	_, ok := user.LanguageServerForFile(path)
	return ok
}

// Ensure installs (if needed) and returns a LanguageServer with an absolute command.
// Single-flight per profile ID. Never replaces a binary mid-session for an
// already-returned path in this process beyond first ensure (install only if missing).
func (m *Manager) Ensure(ctx context.Context, profileID string) (config.LanguageServer, error) {
	if m == nil {
		return config.LanguageServer{}, protocol.NewError(protocol.ErrInternal, "tools manager is nil")
	}
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return config.LanguageServer{}, protocol.NewError(protocol.ErrUnsupported, "empty profile id")
	}
	prof, ok := m.Manifest.Profile(profileID)
	if !ok {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported, "unknown profile %s", profileID)
	}
	if prof.Disabled {
		reason := prof.DisabledReason
		if reason == "" {
			reason = "disabled"
		}
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported,
			"profile %s unavailable: %s", profileID, reason)
	}

	// Shared npm installs (html/css/json) install once under shared_install id.
	installID := profileID
	if prof.SharedInstall != "" {
		installID = prof.SharedInstall
	}

	m.mu.Lock()
	if call, running := m.inflight[installID]; running {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrNotReady,
				"ensure %s cancelled", profileID).WithRetryAfterMS(200)
		case <-call.done:
			if call.err != nil {
				return config.LanguageServer{}, call.err
			}
			return m.entryFromInstall(profileID, prof, call.entry)
		}
	}
	call := &ensureCall{done: make(chan struct{})}
	m.inflight[installID] = call
	m.mu.Unlock()

	entry, err := m.ensureLocked(ctx, installID, profileID, prof)
	call.entry = entry
	call.err = err
	close(call.done)

	m.mu.Lock()
	delete(m.inflight, installID)
	m.mu.Unlock()

	if err != nil {
		return config.LanguageServer{}, err
	}
	return m.entryFromInstall(profileID, prof, entry)
}

func (m *Manager) entryFromInstall(profileID string, prof ProfileManifest, installed config.LanguageServer) (config.LanguageServer, error) {
	// When css/json share html's install dir, rebuild entry with this profile's bin/name.
	if prof.SharedInstall == "" {
		return installed, nil
	}
	dir := ProfileDir(m.ToolsRoot, prof.SharedInstall)
	bin, err := findBin(dir, prof.Bin)
	if err != nil {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported, "ensure %s: %v", profileID, err)
	}
	return config.LanguageServer{
		Name:                  profileID,
		Command:               bin,
		Args:                  append([]string(nil), prof.Args...),
		FileExtensions:        append([]string(nil), prof.FileExtensions...),
		RootMarkers:           append([]string(nil), prof.RootMarkers...),
		InitializationOptions: installed.InitializationOptions,
	}, nil
}

func (m *Manager) ensureLocked(ctx context.Context, installID, profileID string, prof ProfileManifest) (config.LanguageServer, error) {
	if err := ctx.Err(); err != nil {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrNotReady,
			"ensure %s: %v", profileID, err).WithRetryAfterMS(200)
	}
	if err := EnsureToolsRoot(m.ToolsRoot); err != nil {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrInternal, "ensure %s: %v", profileID, err)
	}

	dir := ProfileDir(m.ToolsRoot, installID)
	marker := filepath.Join(dir, ".langer-installed")
	if st, err := os.Stat(marker); err == nil && !st.IsDir() {
		bin, err := findBin(dir, effectiveBin(prof))
		if err == nil {
			return m.buildEntry(profileID, prof, dir, bin), nil
		}
		// incomplete install — reinstall
	}

	if m.Installer == nil {
		m.Installer = DefaultInstaller{}
	}
	if err := m.Installer.Install(ctx, InstallRequest{
		ToolsRoot: m.ToolsRoot,
		ProfileID: installID,
		Profile:   prof,
	}); err != nil {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported,
			"ensure %s failed: %v", profileID, err)
	}

	bin, err := findBin(dir, effectiveBin(prof))
	if err != nil {
		return config.LanguageServer{}, protocol.NewErrorf(protocol.ErrUnsupported,
			"ensure %s: %v", profileID, err)
	}
	_ = os.WriteFile(marker, []byte(prof.Kind+"\n"), 0o600)
	return m.buildEntry(profileID, prof, dir, bin), nil
}

func effectiveBin(prof ProfileManifest) string {
	if prof.Bin != "" {
		return prof.Bin
	}
	if a, ok := prof.AssetForPlatform(); ok {
		if a.ExtractBin != "" {
			return a.ExtractBin
		}
		return a.Name
	}
	return ""
}

func (m *Manager) buildEntry(profileID string, prof ProfileManifest, dir, bin string) config.LanguageServer {
	entry := config.LanguageServer{
		Name:           profileID,
		Command:        bin,
		Args:           append([]string(nil), prof.Args...),
		FileExtensions: append([]string(nil), prof.FileExtensions...),
		RootMarkers:    append([]string(nil), prof.RootMarkers...),
	}
	if prof.TSServerRel != "" {
		ts := filepath.Join(dir, filepath.FromSlash(prof.TSServerRel))
		entry.InitializationOptions = map[string]any{
			"tsserver": map[string]any{"path": ts},
		}
	}
	return entry
}

func findBin(dir, binName string) (string, error) {
	if binName == "" {
		return "", fmt.Errorf("no bin name")
	}
	names := []string{binName}
	if runtime.GOOS == "windows" {
		base := strings.TrimSuffix(strings.TrimSuffix(binName, ".exe"), ".cmd")
		names = []string{binName, base + ".exe", base + ".cmd", base + ".ps1", base}
	}
	// Shallow fixed paths first (npm, go install, plain binary).
	for _, name := range names {
		for _, c := range []string{
			filepath.Join(dir, name),
			filepath.Join(dir, "bin", name),
			filepath.Join(dir, "node_modules", ".bin", name),
		} {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return absPath(c), nil
			}
		}
	}
	// Archive layouts (clangd_*/bin/clangd, lemminx-*).
	var found string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := d.Name()
		for _, name := range names {
			if strings.EqualFold(base, name) || strings.EqualFold(base, filepath.Base(name)) {
				found = path
				return filepath.SkipAll
			}
		}
		// lemminx-win32.exe matches bin "lemminx"
		if runtime.GOOS == "windows" {
			for _, name := range names {
				stem := strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".cmd")
				if strings.HasPrefix(strings.ToLower(base), strings.ToLower(stem)) &&
					(strings.HasSuffix(strings.ToLower(base), ".exe") || !strings.Contains(base, ".")) {
					found = path
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if found != "" {
		return absPath(found), nil
	}
	return "", fmt.Errorf("bin %q not found under %s", binName, dir)
}

func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// ClaimsExtension is true if user config or an enabled managed profile covers path.
func ClaimsExtension(user *config.Config, m *Manager, path string) bool {
	if user != nil {
		if _, ok := user.LanguageServerForFile(path); ok {
			return true
		}
	}
	if m != nil && m.CoversExtension(filepath.Ext(path)) {
		return true
	}
	return false
}
