package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

func TestGCPolicyDefaults(t *testing.T) {
	t.Parallel()

	if DefaultGCAttemptInterval != time.Hour {
		t.Fatalf("GC attempt interval = %v, want 1h", DefaultGCAttemptInterval)
	}
	if DefaultGCLeaseDuration != 60*time.Second {
		t.Fatalf("GC lease duration = %v, want 60s", DefaultGCLeaseDuration)
	}
	if DefaultGCLeaseRenewal != 20*time.Second {
		t.Fatalf("GC lease renewal = %v, want 20s", DefaultGCLeaseRenewal)
	}
	if DefaultDiagnosticRetention != 7*24*time.Hour {
		t.Fatalf("diagnostic retention = %v, want 7d", DefaultDiagnosticRetention)
	}
	if DefaultMissingWorkspaceRetention != 30*24*time.Hour {
		t.Fatalf("missing workspace retention = %v, want 30d", DefaultMissingWorkspaceRetention)
	}
}

func TestGCExpiresDiagnosticSnapshotsByInvalidatingFileCaches(t *testing.T) {
	t.Parallel()

	fakeClock := clock.NewFake(storeTestNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), fakeClock)
	root := t.TempDir()
	ws := ensureTestWorkspace(t, store, root, "org/repo")
	oldRecord := testFileRecord("org/repo", "old.go", "Old")
	oldRecord.Diagnostics = []protocol.Diagnostic{{
		Path:     "old.go",
		Severity: protocol.SeverityWarning,
		Message:  "old warning",
		Range:    protocol.Range{End: protocol.Position{Character: 1}},
	}}
	cleanRecord := testFileRecord("org/repo", "clean.go", "Clean")
	for _, record := range []FileRecord{oldRecord, cleanRecord} {
		if err := store.PutFile(context.Background(), ws, record); err != nil {
			t.Fatal(err)
		}
	}
	generation, err := store.ReferenceGeneration(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := store.ReplaceReferencesBySymbolKey(
		context.Background(),
		ws,
		oldRecord.Symbols[0].SymbolKey,
		generation,
		[]Reference{{
			Path:         "old.go",
			Range:        oldRecord.Symbols[0].SelectionRange,
			IsDefinition: true,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("initial reference replacement lost its generation")
	}

	fakeClock.Advance(DefaultDiagnosticRetention - time.Millisecond)
	ran, stats, err := store.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ran || stats.DiagnosticsPruned != 0 {
		t.Fatalf("pre-retention GC = ran:%t stats:%#v, want ran with no prune", ran, stats)
	}
	if diagnostics, err := store.Diagnostics(context.Background(), ws, "old.go"); err != nil || len(diagnostics) != 1 {
		t.Fatalf("pre-retention diagnostics = %#v, %v; want one", diagnostics, err)
	}
	if diagnostics, err := store.Diagnostics(context.Background(), ws, "clean.go"); err != nil ||
		diagnostics == nil || len(diagnostics) != 0 {
		t.Fatalf("pre-retention clean diagnostics = %#v, %v; want non-nil empty", diagnostics, err)
	}

	fakeClock.Advance(time.Millisecond)
	ran, stats, err = store.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ran || stats.DiagnosticsPruned != 1 || stats.SymbolsPruned != 2 {
		t.Fatalf("at-retention GC = ran:%t stats:%#v, want one diagnostic and two symbol caches pruned", ran, stats)
	}

	for _, path := range []string{"old.go", "clean.go"} {
		hash, found, err := store.FileState(context.Background(), ws, path)
		if err != nil {
			t.Fatal(err)
		}
		if !found || hash != "" {
			t.Errorf("expired %s state = (%q, %t), want blank-hash invalidation", path, hash, found)
		}
		if _, err := store.Diagnostics(context.Background(), ws, path); !hasProtocolCode(err, protocol.ErrNotReady) {
			t.Errorf("expired %s diagnostics error = %v, want NOT_READY", path, err)
		}
		if _, err := store.DocumentSymbols(context.Background(), ws, path); !hasProtocolCode(err, protocol.ErrNotReady) {
			t.Errorf("expired %s symbols error = %v, want NOT_READY", path, err)
		}
	}
	if _, err := store.Diagnostics(context.Background(), ws, ""); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Errorf("workspace diagnostics after expiry error = %v, want NOT_READY", err)
	}
	if _, err := store.SearchSymbols(context.Background(), ws, "", 10); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Errorf("workspace symbols after expiry error = %v, want NOT_READY", err)
	}
	if _, _, err := store.SymbolKeyAt(
		context.Background(),
		ws,
		"old.go",
		protocol.Position{Line: 1, Character: 1},
	); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Errorf("symbol identity after expiry error = %v, want NOT_READY", err)
	}
	if _, err := store.ReferencesBySymbolKey(
		context.Background(), ws, oldRecord.Symbols[0].SymbolKey,
	); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Errorf("references after expiry error = %v, want NOT_READY", err)
	}
	status, err := store.Status(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != protocol.IndexIndexing || status.FilesIndexed != 0 || status.FilesTotal != 2 {
		t.Fatalf("status after diagnostic expiry = %#v, want indexing 0/2", status)
	}

	// A fresh semantic pass restores a usable diagnostic snapshot, including
	// the explicit empty-clean result.
	for _, record := range []FileRecord{oldRecord, cleanRecord} {
		if err := store.PutFile(context.Background(), ws, record); err != nil {
			t.Fatal(err)
		}
	}
	if diagnostics, err := store.Diagnostics(context.Background(), ws, "old.go"); err != nil || len(diagnostics) != 1 {
		t.Fatalf("refreshed diagnostics = %#v, %v; want one", diagnostics, err)
	}
	if diagnostics, err := store.Diagnostics(context.Background(), ws, "clean.go"); err != nil ||
		diagnostics == nil || len(diagnostics) != 0 {
		t.Fatalf("refreshed clean diagnostics = %#v, %v; want non-nil empty", diagnostics, err)
	}
}

func TestGCMissingWorkspaceNeedsContinuousRetention(t *testing.T) {
	t.Parallel()

	fakeClock := clock.NewFake(storeTestNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), fakeClock)
	root := t.TempDir()
	ws := ensureTestWorkspace(t, store, root, "org/repo")
	if err := store.PutFile(context.Background(), ws, testFileRecord("org/repo", "gone.go", "Gone")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	ran, stats, err := store.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ran || stats.WorkspacesPruned != 0 {
		t.Fatalf("first missing pass = ran:%t stats:%#v, want mark only", ran, stats)
	}

	fakeClock.Advance(DefaultMissingWorkspaceRetention - time.Millisecond)
	if _, stats, err = store.GC(context.Background()); err != nil {
		t.Fatal(err)
	} else if stats.WorkspacesPruned != 0 || stats.SymbolsPruned != 1 {
		t.Fatalf("pre-workspace-retention GC stats = %#v, want semantic snapshot only", stats)
	}

	// Reappearance clears the missing interval rather than preserving its age.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GC(context.Background()); err != nil {
		t.Fatal(err)
	}

	fakeClock.Advance(DefaultMissingWorkspaceRetention)
	ran, stats, err = store.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ran || stats.WorkspacesPruned != 1 || stats.FilesPruned != 1 || stats.SymbolsPruned != 0 {
		t.Fatalf("expired missing workspace GC = ran:%t stats:%#v, want remaining workspace/file rows pruned", ran, stats)
	}
	if _, err := store.Status(context.Background(), ws); !hasProtocolCode(err, protocol.ErrWorkspaceUnknown) {
		t.Fatalf("pruned workspace status error = %v, want WORKSPACE_UNKNOWN", err)
	}
}

func TestGCNeverPrunesMerelyInactiveWorkspace(t *testing.T) {
	t.Parallel()

	fakeClock := clock.NewFake(storeTestNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), fakeClock)
	root := t.TempDir()
	ws := ensureTestWorkspace(t, store, root, "org/repo")
	if err := store.PutFile(context.Background(), ws, testFileRecord("org/repo", "present.go", "Present")); err != nil {
		t.Fatal(err)
	}

	fakeClock.Advance(10 * DefaultMissingWorkspaceRetention)
	if _, stats, err := store.GC(context.Background()); err != nil {
		t.Fatal(err)
	} else if stats.WorkspacesPruned != 0 {
		t.Fatalf("present workspace was pruned: %#v", stats)
	}
	if _, err := store.Status(context.Background(), ws); err != nil {
		t.Fatalf("present inactive workspace disappeared: %v", err)
	}
}

func TestGCTransientStatFailureNeverAgesWorkspaceTowardDeletion(t *testing.T) {
	t.Parallel()

	fakeClock := clock.NewFake(storeTestNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), fakeClock).(*sqliteStore)
	root := t.TempDir()
	ws := ensureTestWorkspace(t, store, root, "org/repo")
	if err := store.PutFile(context.Background(), ws, testFileRecord("org/repo", "present.go", "Present")); err != nil {
		t.Fatal(err)
	}
	store.stat = func(path string) (os.FileInfo, error) {
		if path == root {
			return nil, errors.New("transient filesystem failure")
		}
		return os.Stat(path)
	}

	if _, _, err := store.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	fakeClock.Advance(2 * DefaultMissingWorkspaceRetention)
	if _, stats, err := store.GC(context.Background()); err != nil {
		t.Fatal(err)
	} else if stats.WorkspacesPruned != 0 {
		t.Fatalf("transient stat error aged workspace into deletion: %#v", stats)
	}

	var missing sql.NullInt64
	if err := store.read.QueryRowContext(context.Background(),
		`SELECT missing_since_unix_ms FROM workspaces WHERE id = ?`, ws).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing.Valid {
		t.Fatalf("transient stat error set missing_since to %d", missing.Int64)
	}
}

func TestGCLeaseIsExclusiveExpiringAndRenewable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "index.db")
	fakeClock := clock.NewFake(storeTestNow)
	first := openTestStore(t, path, fakeClock).(*sqliteStore)
	second := openTestStore(t, path, fakeClock).(*sqliteStore)
	ctx := context.Background()

	acquired, err := first.tryAcquireGCLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("first store did not acquire an empty GC lease")
	}
	if acquired, err := first.tryAcquireGCLease(ctx); err != nil {
		t.Fatal(err)
	} else if acquired {
		t.Fatal("same store re-acquired its own unexpired lease concurrently")
	}
	if ran, stats, err := first.GC(ctx); err != nil {
		t.Fatal(err)
	} else if ran || stats != (GCStats{}) {
		t.Fatalf("same-store reentrant GC = ran:%t stats:%#v, want skipped", ran, stats)
	}
	if acquired, err := second.tryAcquireGCLease(ctx); err != nil {
		t.Fatal(err)
	} else if acquired {
		t.Fatal("second store acquired an unexpired GC lease")
	}

	heartbeatCtx, cancel := context.WithCancel(ctx)
	done, renewed := first.startGCLeaseHeartbeat(heartbeatCtx)
	fakeClock.BlockUntil(1)
	fakeClock.Advance(DefaultGCLeaseRenewal)
	<-renewed

	fakeClock.Advance(DefaultGCLeaseDuration - time.Millisecond)
	if acquired, err := second.tryAcquireGCLease(ctx); err != nil {
		t.Fatal(err)
	} else if acquired {
		t.Fatal("second store stole a renewed, unexpired lease")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fakeClock.Advance(DefaultGCLeaseDuration)
	if acquired, err := second.tryAcquireGCLease(ctx); err != nil {
		t.Fatal(err)
	} else if !acquired {
		t.Fatal("second store could not acquire the expired lease")
	}
}

func TestGCSkipsWhenAnotherDaemonHoldsLease(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "index.db")
	fakeClock := clock.NewFake(storeTestNow)
	first := openTestStore(t, path, fakeClock).(*sqliteStore)
	second := openTestStore(t, path, fakeClock).(*sqliteStore)
	if acquired, err := first.tryAcquireGCLease(context.Background()); err != nil || !acquired {
		t.Fatalf("first lease = %t, %v", acquired, err)
	}

	ran, stats, err := second.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ran || stats != (GCStats{}) {
		t.Fatalf("contended GC = ran:%t stats:%#v, want false and zero", ran, stats)
	}
}

func TestCheckpointVacuumThreshold(t *testing.T) {
	t.Parallel()

	if shouldVacuum(25, 100) {
		t.Fatal("exactly 25% free pages must not VACUUM")
	}
	if !shouldVacuum(26, 100) {
		t.Fatal("more than 25% free pages must VACUUM")
	}
	if shouldVacuum(1, 0) {
		t.Fatal("zero-page database must not VACUUM")
	}
}

func TestCheckpointReclaimsDatabaseOnlyWhenEligible(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow)).(*sqliteStore)
	ctx := context.Background()
	payload := strings.Repeat("x", 8*1024)
	if err := store.withWrite(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 200; i++ {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)`,
				fmt.Sprintf("checkpoint-%04d", i), payload); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key LIKE 'checkpoint-%'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var beforeFree, beforePages int
	if err := store.writer.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&beforeFree); err != nil {
		t.Fatal(err)
	}
	if err := store.writer.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&beforePages); err != nil {
		t.Fatal(err)
	}
	if !shouldVacuum(beforeFree, beforePages) {
		t.Skipf("SQLite allocation produced only %d/%d free pages; threshold helper is covered separately",
			beforeFree, beforePages)
	}
	if err := store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	var afterFree int
	if err := store.writer.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&afterFree); err != nil {
		t.Fatal(err)
	}
	if afterFree != 0 {
		t.Fatalf("freelist after eligible Checkpoint = %d, want 0 after VACUUM", afterFree)
	}
}
