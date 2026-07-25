package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{SocketPath: filepath.Join(shortTempDir(t), "daemon.sock"), LogLevel: DefaultLogLevel}
}

// shortTempDir returns a temporary directory short enough to hold a Unix socket
// path. t.TempDir embeds the test's name under macOS's deep per-user TMPDIR and
// routinely blows past sockaddr_un's ~104 byte limit — which is the very
// failure WorkspaceSocketPath refuses, so the test must not trip over it while
// testing something else.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "langer")
	if err != nil {
		t.Skipf("cannot create a short temporary directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestWorkspacePathsAreDistinctPerRoot: two roots sharing one socket would mean
// one daemon answering with the wrong project's compilation context.
func TestWorkspacePathsAreDistinctPerRoot(t *testing.T) {
	cfg := testConfig(t)

	a, err := cfg.WorkspaceSocketPath("/repo/alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := cfg.WorkspaceSocketPath("/repo/beta")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two roots share the socket %s", a)
	}
	if filepath.Dir(a) != cfg.RuntimeDir() {
		t.Errorf("socket %s is not in the configured runtime directory %s", a, cfg.RuntimeDir())
	}

	again, err := cfg.WorkspaceSocketPath("/repo/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if again != a {
		t.Errorf("the same root gave two socket paths: %s and %s", a, again)
	}
}

// TestSocketPathLengthIsRefusedNotTruncated: macOS truncates sun_path silently,
// which is how two workspaces end up sharing a daemon.
func TestSocketPathLengthIsRefusedNotTruncated(t *testing.T) {
	cfg := &Config{SocketPath: filepath.Join("/"+strings.Repeat("deep-directory/", 12), "daemon.sock")}

	_, err := cfg.WorkspaceSocketPath("/repo")
	if err == nil {
		t.Fatal("an over-long socket path was accepted; it would be truncated silently")
	}
	if !strings.Contains(err.Error(), "socket_path") {
		t.Errorf("the error does not tell the operator how to fix it: %v", err)
	}
}

// TestEnsureRuntimeDirIsUserOnly is the SPEC §9 permission requirement.
func TestEnsureRuntimeDirIsUserOnly(t *testing.T) {
	cfg := testConfig(t)

	// A pre-existing, world-readable directory must be tightened, not accepted:
	// MkdirAll is a no-op when the directory exists.
	if err := os.MkdirAll(cfg.RuntimeDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := cfg.EnsureRuntimeDir()
	if err != nil {
		t.Fatalf("EnsureRuntimeDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("runtime directory mode = %o, want 700 (SPEC §9)", perm)
	}
}

func TestLockAndLogPathsAreSiblingsOfTheSocket(t *testing.T) {
	cfg := testConfig(t)
	const root = "/repo/alpha"

	socket, err := cfg.WorkspaceSocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	liveness := cfg.WorkspaceLivenessLockPath(root)
	spawn := cfg.WorkspaceSpawnLockPath(root)
	log := cfg.WorkspaceLogPath(root)

	paths := map[string]string{"socket": socket, "liveness": liveness, "spawn": spawn, "log": log}
	seen := map[string]string{}
	for name, path := range paths {
		if filepath.Dir(path) != cfg.RuntimeDir() {
			t.Errorf("%s path %s is not in %s", name, path, cfg.RuntimeDir())
		}
		if other, dup := seen[path]; dup {
			t.Errorf("%s and %s are the same file %s", name, other, path)
		}
		seen[path] = name
	}
	// The spawn lock must not be the liveness lock: one file cannot be handed
	// from spawner to spawned without a race (docs/ARCHITECTURE.md §6.8).
	if liveness == spawn {
		t.Error("the spawn lock and the liveness lock are the same file")
	}
}
