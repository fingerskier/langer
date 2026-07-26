package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

var storeTestNow = time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)

func TestOpenCreatesSecureWALDatabaseWithSingleWriter(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "index.db")
	store := openTestStore(t, path, clock.NewFake(storeTestNow))
	sqlStore := store.(*sqliteStore)

	if got := sqlStore.writer.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer MaxOpenConnections = %d, want exactly 1", got)
	}

	var journalMode string
	if err := sqlStore.read.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var foreignKeys, busyTimeout int
	if err := sqlStore.read.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := sqlStore.read.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 {
		t.Fatalf("connection pragmas = foreign_keys:%d busy_timeout:%d, want 1 and 5000",
			foreignKeys, busyTimeout)
	}

	var version int
	if err := sqlStore.read.QueryRowContext(context.Background(),
		"SELECT MAX(version) FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}

	if runtime.GOOS != "windows" {
		assertPrivateFile(t, path)
		for _, suffix := range []string{"-wal", "-shm"} {
			sibling := path + suffix
			if _, err := os.Stat(sibling); err == nil {
				assertPrivateFile(t, sibling)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
}

func TestEnsureWorkspaceIsRootScopedAndRefreshesNamespace(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	ctx := context.Background()

	first, err := store.EnsureWorkspace(ctx, firstRoot, "org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if want := protocol.WorkspaceIDForRoot(firstRoot); first != want {
		t.Fatalf("workspace id = %q, want %q", first, want)
	}
	again, err := store.EnsureWorkspace(ctx, firstRoot, "new-org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("same root changed id from %q to %q", first, again)
	}
	second, err := store.EnsureWorkspace(ctx, secondRoot, "org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("different roots shared workspace id %q", first)
	}

	sqlStore := store.(*sqliteStore)
	var namespace string
	if err := sqlStore.read.QueryRowContext(ctx,
		"SELECT repo_namespace FROM workspaces WHERE id = ?", first).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if namespace != "new-org/repo" {
		t.Fatalf("repo namespace = %q, want refreshed namespace", namespace)
	}
}

func TestEnsureWorkspaceNamespaceChangeForcesFullReindex(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	root := t.TempDir()
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, root, "old-org/repo")
	record := testFileRecord("old-org/repo", "source.go", "Source")
	if err := store.PutFile(ctx, ws, record); err != nil {
		t.Fatal(err)
	}
	oldKey := record.Symbols[0].SymbolKey
	replaceTestReferences(t, store, ws, oldKey, []Reference{{
		Path: "source.go", Range: record.Symbols[0].SelectionRange, IsDefinition: true,
	}})

	if got, err := store.EnsureWorkspace(ctx, root, "new-org/repo"); err != nil || got != ws {
		t.Fatalf("EnsureWorkspace after namespace change = %q, %v; want %q", got, err, ws)
	}
	hash, found, err := store.FileState(ctx, ws, "source.go")
	if err != nil {
		t.Fatal(err)
	}
	if !found || hash != "" {
		t.Fatalf("namespace-change file state = (%q, %t), want invalidated row", hash, found)
	}
	if _, err := store.DocumentSymbols(ctx, ws, "source.go"); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("namespace-change symbols error = %v, want NOT_READY", err)
	}
	if _, err := store.ReferencesBySymbolKey(ctx, ws, oldKey); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("old namespace reference error = %v, want NOT_READY", err)
	}

	reindexed := testFileRecord("new-org/repo", "source.go", "Source")
	if err := store.PutFile(ctx, ws, reindexed); err != nil {
		t.Fatalf("reindex under new namespace: %v", err)
	}
	if key, unique, err := store.SymbolKeyAt(
		ctx, ws, "source.go", protocol.Position{Line: 1, Character: 1},
	); err != nil || !unique || key != reindexed.Symbols[0].SymbolKey {
		t.Fatalf("new namespace SymbolKeyAt = (%q, %t, %v), want %q true nil",
			key, unique, err, reindexed.Symbols[0].SymbolKey)
	}
}

func TestPutFileAndReadQueries(t *testing.T) {
	t.Parallel()

	fakeClock := clock.NewFake(storeTestNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), fakeClock)
	root := t.TempDir()
	ws := ensureTestWorkspace(t, store, root, "org/repo")
	ctx := context.Background()

	record := testFileRecord("org/repo", "src/server.go", "Serve")
	record.Symbols = append(record.Symbols, testSymbolRecord(
		"org/repo", "src/server.go", "Root", "package",
		protocol.Range{Start: protocol.Position{Line: 0}, End: protocol.Position{Line: 10}},
	))
	record.Diagnostics = []protocol.Diagnostic{{
		Path:     "src/server.go",
		Severity: protocol.SeverityWarning,
		Code:     "W001",
		Source:   "gopls",
		Message:  "consider a smaller interface",
		Range:    protocol.Range{Start: protocol.Position{Line: 7, Character: 2}, End: protocol.Position{Line: 7, Character: 11}},
	}}
	if err := store.PutFile(ctx, ws, record); err != nil {
		t.Fatal(err)
	}

	hash, found, err := store.FileState(ctx, ws, "src/server.go")
	if err != nil {
		t.Fatal(err)
	}
	if !found || hash != record.ContentHash {
		t.Fatalf("FileState = (%q, %t), want (%q, true)", hash, found, record.ContentHash)
	}

	gotSymbols, err := store.DocumentSymbols(ctx, ws, `src\server.go`)
	if err != nil {
		t.Fatal(err)
	}
	wantSymbols := []protocol.Symbol{record.Symbols[1].Symbol, record.Symbols[0].Symbol}
	for i := range wantSymbols {
		wantSymbols[i].Path = "src/server.go"
	}
	if diff := cmp.Diff(wantSymbols, gotSymbols); diff != "" {
		t.Fatalf("DocumentSymbols mismatch (-want +got):\n%s", diff)
	}

	gotSearch, err := store.SearchSymbols(ctx, ws, "serve", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSearch) != 1 || gotSearch[0].Name != "Serve" {
		t.Fatalf("SearchSymbols = %#v, want Serve", gotSearch)
	}

	gotDiagnostics, err := store.Diagnostics(ctx, ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(record.Diagnostics, gotDiagnostics); diff != "" {
		t.Fatalf("Diagnostics mismatch (-want +got):\n%s", diff)
	}

	status, err := store.Status(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if status.Root != root || status.State != protocol.IndexReady ||
		status.FilesIndexed != 1 || status.FilesTotal != 1 ||
		status.LastIndexedUnixMS != storeTestNow.UnixMilli() {
		t.Fatalf("Status = %#v, want ready 1/1 at fake-clock time", status)
	}
}

func TestSearchSymbolsUsesDeterministicUnicodeFuzzyRanking(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")
	for i, name := range []string{
		"UsageError",
		"GetUserByID",
		"UserService",
		"Configuration",
		"ÜberService",
		"GetUser",
		"User",
	} {
		record := testFileRecord("org/repo", filepath.ToSlash(filepath.Join("symbols", name+".go")), name)
		record.ContentHash = hashByte(byte(i + 1))
		if err := store.PutFile(ctx, ws, record); err != nil {
			t.Fatal(err)
		}
	}

	searchNames := func(query string, limit int) []string {
		t.Helper()
		symbols, err := store.SearchSymbols(ctx, ws, query, limit)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(symbols))
		for i := range symbols {
			names[i] = symbols[i].Name
		}
		return names
	}

	if diff := cmp.Diff(
		[]string{"User", "UserService", "GetUser", "GetUserByID", "UsageError"},
		searchNames("user", 20),
	); diff != "" {
		t.Fatalf("fuzzy rank mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(
		[]string{"User", "UserService"},
		searchNames("user", 2),
	); diff != "" {
		t.Fatalf("post-ranking limit mismatch (-want +got):\n%s", diff)
	}
	for _, test := range []struct {
		query string
		want  string
	}{
		{query: "gubi", want: "GetUserByID"},
		{query: "cfg", want: "Configuration"},
		{query: "über", want: "ÜberService"},
	} {
		if got := searchNames(test.query, 10); len(got) != 1 || got[0] != test.want {
			t.Errorf("SearchSymbols(%q) = %v, want [%s]", test.query, got, test.want)
		}
	}
}

func TestPutFileIsAtomicAndWorkspaceScoped(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	wsOne := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")
	wsTwo := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")

	original := testFileRecord("org/repo", "same.go", "Original")
	if err := store.PutFile(ctx, wsOne, original); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DocumentSymbols(ctx, wsTwo, "same.go"); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("other workspace missing-file error = %v, want NOT_READY", err)
	}

	invalid := testFileRecord("org/repo", "same.go", "Replacement")
	invalid.Symbols[0].SymbolKey = "wrong-key"
	if err := store.PutFile(ctx, wsOne, invalid); err == nil {
		t.Fatal("PutFile accepted a SymbolKey that does not match repository/path/stable key")
	}
	got, err := store.DocumentSymbols(ctx, wsOne, "same.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Original" {
		t.Fatalf("failed replacement changed cached data: %#v", got)
	}
}

func TestMissingFileNeverMasqueradesAsACompleteEmptySnapshot(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")

	if _, err := store.DocumentSymbols(ctx, ws, "never-indexed.go"); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("DocumentSymbols missing-file error = %v, want NOT_READY", err)
	}
	if _, err := store.Diagnostics(ctx, ws, "never-indexed.go"); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("Diagnostics missing-file error = %v, want NOT_READY", err)
	}
	if _, _, err := store.SymbolKeyAt(
		ctx,
		ws,
		"never-indexed.go",
		protocol.Position{},
	); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("SymbolKeyAt missing-file error = %v, want NOT_READY", err)
	}
}

func TestSymbolKeyAtRejectsResidualAmbiguity(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")

	firstRange := protocol.Range{
		Start: protocol.Position{Line: 2, Character: 5},
		End:   protocol.Position{Line: 2, Character: 9},
	}
	secondRange := protocol.Range{
		Start: protocol.Position{Line: 8, Character: 5},
		End:   protocol.Position{Line: 8, Character: 9},
	}
	record := testFileRecord("org/repo", "overloads.go", "Find")
	record.Symbols[0].Symbol.Range = firstRange
	record.Symbols[0].SelectionRange = firstRange
	record.Symbols = append(record.Symbols, record.Symbols[0])
	record.Symbols[1].Symbol.Range = secondRange
	record.Symbols[1].SelectionRange = secondRange
	if err := store.PutFile(ctx, ws, record); err != nil {
		t.Fatal(err)
	}

	if key, unique, err := store.SymbolKeyAt(ctx, ws, "overloads.go", protocol.Position{Line: 2, Character: 6}); err != nil {
		t.Fatal(err)
	} else if key != "" || unique {
		t.Fatalf("ambiguous SymbolKeyAt = (%q, %t), want (empty, false)", key, unique)
	}
	if key, unique, err := store.SymbolKeyAt(ctx, ws, "overloads.go", protocol.Position{Line: 50}); err != nil {
		t.Fatal(err)
	} else if key != "" || unique {
		t.Fatalf("missing SymbolKeyAt = (%q, %t), want (empty, false)", key, unique)
	}
}

func TestSymbolKeyAtUsesOneSnapshotForCandidateAndUniqueness(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow)).(*sqliteStore)
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")

	atCursor := protocol.Range{
		Start: protocol.Position{Line: 2, Character: 5},
		End:   protocol.Position{Line: 2, Character: 9},
	}
	elsewhere := protocol.Range{
		Start: protocol.Position{Line: 8, Character: 5},
		End:   protocol.Position{Line: 8, Character: 9},
	}
	old := testFileRecord("org/repo", "snapshot.go", "Find")
	old.Symbols[0].Symbol.Range = atCursor
	old.Symbols[0].SelectionRange = atCursor
	old.Symbols = append(old.Symbols, old.Symbols[0])
	old.Symbols[1].Symbol.Range = elsewhere
	old.Symbols[1].SelectionRange = elsewhere
	if err := store.PutFile(ctx, ws, old); err != nil {
		t.Fatal(err)
	}

	replacement := testFileRecord("org/repo", "snapshot.go", "Find")
	replacement.Symbols[0].Symbol.Range = elsewhere
	replacement.Symbols[0].SelectionRange = elsewhere
	var replaceErr error
	key, unique, err := store.symbolKeyAtWithHook(
		ctx,
		ws,
		"snapshot.go",
		protocol.Position{Line: 2, Character: 6},
		func() {
			replaceErr = store.PutFile(ctx, ws, replacement)
		},
	)
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if key != "" || unique {
		t.Fatalf("mixed-snapshot SymbolKeyAt = (%q, %t), want ambiguous old snapshot", key, unique)
	}
	if key, unique, err := store.SymbolKeyAt(
		ctx,
		ws,
		"snapshot.go",
		protocol.Position{Line: 2, Character: 6},
	); err != nil || key != "" || unique {
		t.Fatalf("post-replacement SymbolKeyAt = (%q, %t, %v), want no candidate", key, unique, err)
	}
}

func TestReferencesAreCompleteAtomicWorkspaceScopedSets(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	wsOne := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")
	wsTwo := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")
	definition := testFileRecord("org/repo", "definition.go", "Find")
	if err := store.PutFile(ctx, wsOne, definition); err != nil {
		t.Fatal(err)
	}
	key := definition.Symbols[0].SymbolKey

	refs := []Reference{
		{
			Path:         "definition.go",
			Range:        protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 1, Character: 4}},
			IsDefinition: true,
		},
		{
			Path:  "use.go",
			Range: protocol.Range{Start: protocol.Position{Line: 3, Character: 2}, End: protocol.Position{Line: 3, Character: 6}},
		},
	}
	replaceTestReferences(t, store, wsOne, key, refs)

	got, err := store.ReferencesBySymbolKey(ctx, wsOne, key)
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Location{
		{Path: refs[0].Path, Range: refs[0].Range, IsDefinition: true},
		{Path: refs[1].Path, Range: refs[1].Range},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("references mismatch (-want +got):\n%s", diff)
	}
	if _, err := store.ReferencesBySymbolKey(ctx, wsTwo, key); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("other workspace error = %v, want NOT_READY", err)
	}

	replaceTestReferences(t, store, wsOne, key, nil)
	got, err = store.ReferencesBySymbolKey(ctx, wsOne, key)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("cached empty reference set = %#v, want non-nil empty", got)
	}
}

func TestReferenceReadersSeeOldCompleteSetDuringReplacement(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow)).(*sqliteStore)
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")
	definition := testFileRecord("org/repo", "definition.go", "Find")
	if err := store.PutFile(ctx, ws, definition); err != nil {
		t.Fatal(err)
	}
	key := definition.Symbols[0].SymbolKey
	old := []Reference{{
		Path:  "old.go",
		Range: protocol.Range{End: protocol.Position{Character: 3}},
	}}
	replaceTestReferences(t, store, ws, key, old)

	tx, err := store.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE reference_sets SET complete = 0
		WHERE workspace_id = ? AND symbol_key = ?`, ws, key); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM "references"
		WHERE workspace_id = ? AND symbol_key = ?`, ws, key); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO "references"(
			workspace_id, symbol_key, ordinal, path,
			start_line, start_col, end_line, end_col, is_definition
		) VALUES (?, ?, 0, 'new.go', 0, 0, 0, 3, 0)`, ws, key); err != nil {
		t.Fatal(err)
	}

	// WAL readers take a stable snapshot of the committed state. They neither
	// block on the IMMEDIATE writer nor observe its incomplete replacement.
	got, err := store.ReferencesBySymbolKey(ctx, ws, key)
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.Location{{Path: "old.go", Range: old[0].Range}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("references during uncommitted replacement (-want +got):\n%s", diff)
	}
}

func TestInvalidationNeverExposesPartialReferenceSets(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")

	definition := testFileRecord("org/repo", "definition.go", "Find")
	use := testFileRecord("org/repo", "use.go", "Call")
	for _, record := range []FileRecord{definition, use} {
		if err := store.PutFile(ctx, ws, record); err != nil {
			t.Fatal(err)
		}
	}
	key := definition.Symbols[0].SymbolKey
	refs := []Reference{
		{Path: "definition.go", Range: definition.Symbols[0].SelectionRange, IsDefinition: true},
		{Path: "use.go", Range: use.Symbols[0].SelectionRange},
	}
	replaceTestReferences(t, store, ws, key, refs)

	if err := store.InvalidateFile(ctx, ws, "use.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReferencesBySymbolKey(ctx, ws, key); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("incomplete reference query error = %v, want NOT_READY", err)
	}
	hash, found, err := store.FileState(ctx, ws, "use.go")
	if err != nil {
		t.Fatal(err)
	}
	if !found || hash != "" {
		t.Fatalf("invalidated FileState = (%q, %t), want (empty, true)", hash, found)
	}
	if _, err := store.DocumentSymbols(ctx, ws, "use.go"); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("invalidated symbols error = %v, want NOT_READY", err)
	}

	replaceTestReferences(t, store, ws, key, refs)
	if _, err := store.ReferencesBySymbolKey(ctx, ws, key); err != nil {
		t.Fatalf("complete replacement did not restore readability: %v", err)
	}
	if err := store.InvalidateFile(ctx, ws, "definition.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReferencesBySymbolKey(ctx, ws, key); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("definition invalidation error = %v, want NOT_READY", err)
	}
}

func TestInvalidatingAnySourceFileMakesEveryReferenceSetUnavailable(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")

	definition := testFileRecord("org/repo", "definition.go", "Find")
	previouslyUnrelated := testFileRecord("org/repo", "unrelated.go", "Other")
	for _, record := range []FileRecord{definition, previouslyUnrelated} {
		if err := store.PutFile(ctx, ws, record); err != nil {
			t.Fatal(err)
		}
	}
	key := definition.Symbols[0].SymbolKey
	replaceTestReferences(t, store, ws, key, []Reference{{
		Path:         "definition.go",
		Range:        definition.Symbols[0].SelectionRange,
		IsDefinition: true,
	}})
	capturedGeneration, err := store.ReferenceGeneration(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}

	// unrelated.go can acquire a brand-new reference to Find. Its old
	// reference absence is not represented by a row, so invalidating only sets
	// that already name this path would silently preserve a stale "complete"
	// set.
	if err := store.InvalidateFile(ctx, ws, "unrelated.go"); err != nil {
		t.Fatal(err)
	}
	committed, err := store.ReplaceReferencesBySymbolKey(
		ctx,
		ws,
		key,
		capturedGeneration,
		[]Reference{{
			Path:         "definition.go",
			Range:        definition.Symbols[0].SelectionRange,
			IsDefinition: true,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("stale reference generation republished a complete snapshot")
	}
	if _, err := store.ReferencesBySymbolKey(ctx, ws, key); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("unrelated source invalidation left a potentially stale reference set readable: %v", err)
	}
}

func TestDefinitionInvalidationPrunesOrphanReferenceRows(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")
	definition := testFileRecord("org/repo", "definition.go", "Find")
	use := testFileRecord("org/repo", "use.go", "Use")
	for _, record := range []FileRecord{definition, use} {
		if err := store.PutFile(ctx, ws, record); err != nil {
			t.Fatal(err)
		}
	}
	key := definition.Symbols[0].SymbolKey
	replaceTestReferences(t, store, ws, key, []Reference{
		{
			Path:         "definition.go",
			Range:        definition.Symbols[0].SelectionRange,
			IsDefinition: true,
		},
		{Path: "use.go", Range: use.Symbols[0].SelectionRange},
	})

	if err := store.InvalidateFile(ctx, ws, "definition.go"); err != nil {
		t.Fatal(err)
	}
	sqlStore := store.(*sqliteStore)
	var sets, locations int
	if err := sqlStore.read.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM reference_sets WHERE workspace_id = ? AND symbol_key = ?),
			(SELECT COUNT(*) FROM "references" WHERE workspace_id = ? AND symbol_key = ?)`,
		ws, key, ws, key,
	).Scan(&sets, &locations); err != nil {
		t.Fatal(err)
	}
	if sets != 0 || locations != 0 {
		t.Fatalf("orphan reference rows = sets:%d locations:%d, want 0/0", sets, locations)
	}
}

func TestReconcileWorkspacePrunesOfflineDeletions(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, filepath.Join(t.TempDir(), "index.db"), clock.NewFake(storeTestNow))
	ctx := context.Background()
	ws := ensureTestWorkspace(t, store, t.TempDir(), "org/repo")
	var deleted FileRecord
	for _, path := range []string{"keep.go", "deleted.go"} {
		record := testFileRecord("org/repo", path, path)
		if err := store.PutFile(ctx, ws, record); err != nil {
			t.Fatal(err)
		}
		if path == "deleted.go" {
			deleted = record
		}
	}
	deletedKey := deleted.Symbols[0].SymbolKey
	replaceTestReferences(t, store, ws, deletedKey, []Reference{{Path: "deleted.go"}})

	pruned, err := store.ReconcileWorkspace(ctx, ws, []string{"keep.go"})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("ReconcileWorkspace pruned %d files, want 1", pruned)
	}
	if _, found, err := store.FileState(ctx, ws, "deleted.go"); err != nil || found {
		t.Fatalf("deleted file state found=%t err=%v, want false nil", found, err)
	}
	if _, err := store.ReferencesBySymbolKey(ctx, ws, deletedKey); !hasProtocolCode(err, protocol.ErrNotReady) {
		t.Fatalf("deleted path reference set error = %v, want NOT_READY", err)
	}
}

func TestTwoStoreHandlesSerializeConcurrentWrites(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "index.db")
	fakeClock := clock.NewFake(storeTestNow)
	first := openTestStore(t, path, fakeClock)
	second := openTestStore(t, path, fakeClock)
	ctx := context.Background()
	root := t.TempDir()
	ws := ensureTestWorkspace(t, first, root, "org/repo")
	if got, err := second.EnsureWorkspace(ctx, root, "org/repo"); err != nil || got != ws {
		t.Fatalf("second EnsureWorkspace = %q, %v; want %q", got, err, ws)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := first
			if i%2 == 1 {
				target = second
			}
			record := testFileRecord("org/repo", "concurrent.go", "symbol")
			record.ContentHash = hashByte(byte(i + 1))
			errs <- target.PutFile(ctx, ws, record)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent PutFile: %v", err)
		}
	}
	if symbols, err := first.DocumentSymbols(ctx, ws, "concurrent.go"); err != nil || len(symbols) != 1 {
		t.Fatalf("final symbols = %#v, %v; want one complete record", symbols, err)
	}
}

func openTestStore(t *testing.T, path string, ck clock.Clock) Store {
	t.Helper()
	store, err := Open(context.Background(), path, ck)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func ensureTestWorkspace(t *testing.T, store Store, root, namespace string) protocol.WorkspaceID {
	t.Helper()
	ws, err := store.EnsureWorkspace(context.Background(), root, namespace)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func replaceTestReferences(
	t *testing.T,
	store Store,
	ws protocol.WorkspaceID,
	key string,
	refs []Reference,
) {
	t.Helper()
	generation, err := store.ReferenceGeneration(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := store.ReplaceReferencesBySymbolKey(
		context.Background(),
		ws,
		key,
		generation,
		refs,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("reference replacement unexpectedly lost its generation")
	}
}

func testFileRecord(namespace, path, name string) FileRecord {
	return FileRecord{
		Path:         path,
		AbsolutePath: filepath.Join("/workspace", filepath.FromSlash(path)),
		LanguageID:   "go",
		ContentHash:  hashByte('a'),
		SizeBytes:    128,
		ModTime:      storeTestNow.Add(-time.Minute),
		Symbols: []SymbolRecord{testSymbolRecord(namespace, path, name, "", protocol.Range{
			Start: protocol.Position{Line: 1, Character: 0},
			End:   protocol.Position{Line: 1, Character: 5},
		})},
	}
}

func testSymbolRecord(namespace, path, name, container string, selection protocol.Range) SymbolRecord {
	symbol := protocol.Symbol{
		Name:      name,
		Kind:      protocol.SymbolKindFunction,
		Container: container,
		Path:      path,
		Range:     selection,
		Detail:    "func " + name + "()",
	}
	stable := StableKey(symbol)
	return SymbolRecord{
		Symbol:         symbol,
		SelectionRange: selection,
		StableKey:      stable,
		SymbolKey:      SymbolKey(namespace, path, stable),
	}
}

func hashByte(b byte) string {
	const digits = "0123456789abcdef"
	c := digits[int(b)%len(digits)]
	return strings.Repeat(string(c), 64)
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %04o, want 0600", path, got)
	}
}

func hasProtocolCode(err error, code protocol.ErrorCode) bool {
	structured := protocol.AsError(err)
	return structured != nil && structured.Code == code
}
