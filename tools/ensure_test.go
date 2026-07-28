package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fingerskier/langer/config"
)

type fakeInstaller struct {
	n    atomic.Int32
	err  error
	hook func(InstallRequest)
}

func (f *fakeInstaller) Install(_ context.Context, req InstallRequest) error {
	f.n.Add(1)
	if f.hook != nil {
		f.hook(req)
	}
	if f.err != nil {
		return f.err
	}
	dir := ProfileDir(req.ToolsRoot, req.ProfileID)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		return err
	}
	bin := req.Profile.Bin
	if bin == "" {
		bin = "server"
	}
	path := filepath.Join(dir, "bin", bin)
	return os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)
}

func TestLoadEmbeddedManifest(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Version == "" {
		t.Fatal("manifest version empty")
	}
	if _, ok := m.Profile("typescript"); !ok {
		t.Fatal("missing typescript profile")
	}
	id, _, ok := m.ProfileForExtension(".ts")
	if !ok || id != "typescript" {
		t.Fatalf("ProfileForExtension(.ts) = %q ok=%v", id, ok)
	}
	id, _, ok = m.ProfileForExtension(".md")
	if !ok || id != "markdown" {
		t.Fatalf("ProfileForExtension(.md) = %q ok=%v", id, ok)
	}
}

func TestEnsureSingleFlight(t *testing.T) {
	root := t.TempDir()
	man, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	// Use a tiny synthetic profile for speed.
	man.Profiles = map[string]ProfileManifest{
		"demo": {
			Kind:           "npm",
			Bin:            "demo-ls",
			Args:           []string{"--stdio"},
			FileExtensions: []string{".demo"},
			Packages:       []string{"demo@1"},
		},
	}
	fi := &fakeInstaller{}
	mgr := &Manager{ToolsRoot: root, Manifest: man, Installer: fi, inflight: map[string]*ensureCall{}}

	var wg sync.WaitGroup
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mgr.Ensure(context.Background(), "demo")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}
	if got := fi.n.Load(); got != 1 {
		t.Fatalf("install calls = %d, want 1", got)
	}
	// Second ensure should not reinstall.
	if _, err := mgr.Ensure(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if got := fi.n.Load(); got != 1 {
		t.Fatalf("after re-ensure install calls = %d, want 1", got)
	}
}

func TestResolveEntryUserOverride(t *testing.T) {
	root := t.TempDir()
	man := &Manifest{Version: "1", Profiles: map[string]ProfileManifest{
		"typescript": {
			Kind:           "npm",
			Bin:            "tls",
			FileExtensions: []string{".ts"},
			Packages:       []string{"x@1"},
		},
	}}
	fi := &fakeInstaller{}
	mgr := &Manager{ToolsRoot: root, Manifest: man, Installer: fi, inflight: map[string]*ensureCall{}}
	user := &config.Config{LanguageServers: []config.LanguageServer{{
		Name:           "typescript",
		Command:        "/opt/custom/tls",
		FileExtensions: []string{".ts"},
	}}}
	entry, err := mgr.ResolveEntry(context.Background(), "src/a.ts", user)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Command != "/opt/custom/tls" {
		t.Fatalf("command = %q, want user override", entry.Command)
	}
	if fi.n.Load() != 0 {
		t.Fatalf("managed ensure should not run when user overrides")
	}
}

func TestDisabledProfile(t *testing.T) {
	mgr := &Manager{
		ToolsRoot: t.TempDir(),
		Manifest: &Manifest{Profiles: map[string]ProfileManifest{
			"csv": {Kind: "none", Disabled: true, DisabledReason: "no ls", FileExtensions: []string{".csv"}},
		}},
		Installer: &fakeInstaller{},
		inflight:  map[string]*ensureCall{},
	}
	_, err := mgr.Ensure(context.Background(), "csv")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEmbeddedCppCsharpXmlEnabled(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cpp", "csharp", "xml"} {
		p, ok := m.Profile(id)
		if !ok {
			t.Fatalf("missing profile %s", id)
		}
		if p.Disabled {
			t.Fatalf("profile %s still disabled: %s", id, p.DisabledReason)
		}
	}
	if _, _, ok := m.ProfileForExtension(".cpp"); !ok {
		t.Fatal("expected .cpp covered")
	}
	if _, _, ok := m.ProfileForExtension(".cs"); !ok {
		t.Fatal("expected .cs covered")
	}
	if _, _, ok := m.ProfileForExtension(".xml"); !ok {
		t.Fatal("expected .xml covered")
	}
	if p, _ := m.Profile("cpp"); p.Kind != "github_release" || p.Bin != "clangd" {
		t.Fatalf("cpp primary = %+v", p)
	}
	if p, _ := m.Profile("csharp"); p.Kind != "dotnet_tool" || p.Package != "csharp-ls" {
		t.Fatalf("csharp primary = %+v", p)
	}
	if p, _ := m.Profile("xml"); p.Kind != "github_release" || p.Bin != "lemminx" {
		t.Fatalf("xml primary = %+v", p)
	}
}
