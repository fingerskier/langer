package index

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

func TestOpenMigratesSharedDatabaseConcurrentlyAndIdempotently(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "index.db")
	realClock := clock.New()
	const openers = 8
	stores := make(chan Store, openers)
	errs := make(chan error, openers)
	var wg sync.WaitGroup
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := Open(context.Background(), path, realClock)
			if err != nil {
				errs <- err
				return
			}
			stores <- store
		}()
	}
	wg.Wait()
	close(stores)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Open: %v", err)
	}
	for store := range stores {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	if t.Failed() {
		return
	}

	store, err := Open(context.Background(), path, realClock)
	if err != nil {
		t.Fatalf("idempotent Open after concurrent migrations: %v", err)
	}
	defer store.Close()
	var count int
	if err := store.(*sqliteStore).read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_version WHERE version = ?`,
		currentSchemaVersion).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("current schema version rows = %d, want exactly 1", count)
	}
}

func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "index.db")
	store, err := Open(context.Background(), path, clock.NewFake(storeTestNow))
	if err != nil {
		t.Fatal(err)
	}
	sqlStore := store.(*sqliteStore)
	if _, err := sqlStore.writer.ExecContext(context.Background(),
		`INSERT INTO schema_version(version) VALUES (?)`,
		currentSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(context.Background(), path, clock.NewFake(storeTestNow)); !hasProtocolCode(err, protocol.ErrInternal) {
		t.Fatalf("Open newer schema error = %v, want INTERNAL", err)
	}
}

func TestSQLiteBusyRetryUsesInjectedClock(t *testing.T) {
	t.Parallel()

	fakeClock := clock.NewFake(storeTestNow)
	attempts := 0
	done := make(chan error, 1)
	go func() {
		done <- retrySQLiteBusy(context.Background(), fakeClock, func() error {
			attempts++
			if attempts == 1 {
				return errors.New("database is locked (5) (SQLITE_BUSY)")
			}
			return nil
		})
	}()

	fakeClock.BlockUntil(1)
	fakeClock.Advance(10 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
