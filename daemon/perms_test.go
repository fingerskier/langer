package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRestrictUserOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoint")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestrictUserOnlyFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !FileIsUserOnly(info) {
		t.Errorf("mode = %o, want user-only", info.Mode().Perm())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestRestrictUserOnlyDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RestrictUserOnlyDir(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !DirIsUserOnly(info) {
		t.Errorf("mode = %o, want user-only dir", info.Mode().Perm())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestRestrictUserOnlyFileEmptyPath(t *testing.T) {
	if err := RestrictUserOnlyFile(""); err == nil {
		t.Fatal("empty path must fail")
	}
}
