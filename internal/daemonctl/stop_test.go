package daemonctl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListDaemonKeys(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "daemon-abc123def456.sock"), []byte{}, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "daemon-abc123def456.lock"), []byte("42\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "daemon-zzz.lock"), []byte("9\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "other.txt"), []byte{}, 0o600)

	keys, err := listDaemonKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want 2", keys)
	}
	seen := map[string]bool{}
	for _, k := range keys {
		seen[k] = true
	}
	if !seen["abc123def456"] || !seen["zzz"] {
		t.Fatalf("keys = %v", keys)
	}
}

func TestReadPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	if err := os.WriteFile(path, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readPID(path); got != 12345 {
		t.Fatalf("readPID = %d", got)
	}
	if got := readPID(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("missing = %d", got)
	}
}
