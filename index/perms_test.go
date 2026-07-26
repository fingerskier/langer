package index

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fingerskier/langer/internal/clock"
)

// TestDatabaseFilesAreUserOnly is SPEC §9: the index DB and its WAL/SHM
// siblings must be user-only on platforms where mode bits are meaningful.
func TestDatabaseFilesAreUserOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	store, err := Open(context.Background(), path, clock.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Force WAL siblings to exist when possible.
	if err := store.Checkpoint(context.Background()); err != nil {
		t.Logf("Checkpoint: %v", err)
	}

	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if err != nil {
			if os.IsNotExist(err) && (name == path+"-wal" || name == path+"-shm") {
				continue
			}
			t.Fatalf("stat %s: %v", name, err)
		}
		if runtime.GOOS == "windows" {
			continue
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 0600", name, got)
		}
	}
}
