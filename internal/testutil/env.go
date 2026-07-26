// Package testutil holds test-only helpers shared across langer's packages:
// hermetic XDG isolation, fixture paths, the integration-server locator, and
// the goroutine-leak assertion.
//
// No non-test file anywhere may import this package.
package testutil

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TempXDG points HOME and the XDG variables at a fresh temporary directory and
// returns it. t.Setenv restores the previous values automatically, and it fails
// the test if called from a parallel test — which is what we want, because
// process-wide environment state cannot be shared safely.
func TempXDG(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	// An inherited override from the developer's shell would silently defeat
	// the isolation these tests depend on.
	t.Setenv("LANGER_CONFIG_PATH", "")
	t.Setenv("LANGER_DB_PATH", "")
	t.Setenv("LANGER_LOG_LEVEL", "")
	return home
}

// RepoRoot returns the absolute path of the repository checkout.
//
// It is derived from this file's own compile-time path rather than from the
// working directory, so it is correct no matter which package's tests call it.
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repository root")
	}
	// <root>/internal/testutil/env.go
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	return root
}

// FixtureRoot returns the absolute path of testdata/<name>.
func FixtureRoot(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(RepoRoot(t), "testdata", name)
}
