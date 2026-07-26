package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTryIsExclusiveAndUnlockReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	first := openLockFile(t, path)
	second := openLockFile(t, path)

	if err := Try(first); err != nil {
		t.Fatalf("first Try: %v", err)
	}
	if err := Try(second); !errors.Is(err, ErrContended) {
		t.Fatalf("contending Try = %v, want ErrContended", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := Try(second); err != nil {
		t.Fatalf("Try after release: %v", err)
	}
	if err := Unlock(second); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
}

func TestUnlockIsIdempotentAtTheOSBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	file := openLockFile(t, path)
	if err := Try(file); err != nil {
		t.Fatal(err)
	}
	if err := Unlock(file); err != nil {
		t.Fatal(err)
	}
}

func openLockFile(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}
