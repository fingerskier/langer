package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/index"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/watch"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/protocol"
)

type operationLog struct {
	mu    sync.Mutex
	items []string
}

func (l *operationLog) add(item string) {
	l.mu.Lock()
	l.items = append(l.items, item)
	l.mu.Unlock()
}

func (l *operationLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.items...)
}

func operationIndex(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

type fakeIndexScanner struct {
	namespace string
	files     []string
	listErr   error
	order     *operationLog
}

func (s *fakeIndexScanner) RepositoryNamespace(context.Context, string) (string, error) {
	s.order.add("namespace")
	return s.namespace, nil
}

func (s *fakeIndexScanner) List(context.Context, string) ([]string, error) {
	s.order.add("list")
	return append([]string(nil), s.files...), s.listErr
}

func (*fakeIndexScanner) InScope(_ string, rel string) bool {
	return rel != "" && rel != ".." && !filepath.IsAbs(rel)
}

func (*fakeIndexScanner) Hash(abs string) (string, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type fakeIndexWatcher struct {
	ready   chan struct{}
	events  chan watch.Batch
	stopped chan struct{}
	order   *operationLog
	once    sync.Once
}

func newFakeIndexWatcher(order *operationLog) *fakeIndexWatcher {
	return &fakeIndexWatcher{
		ready:   make(chan struct{}),
		events:  make(chan watch.Batch, 8),
		stopped: make(chan struct{}),
		order:   order,
	}
}

func (w *fakeIndexWatcher) Ready() <-chan struct{}     { return w.ready }
func (w *fakeIndexWatcher) Events() <-chan watch.Batch { return w.events }

func (w *fakeIndexWatcher) Run(ctx context.Context) error {
	w.once.Do(func() {
		w.order.add("watcher-ready")
		close(w.ready)
	})
	<-ctx.Done()
	close(w.stopped)
	return nil
}

type fakeIndexStore struct {
	mu sync.Mutex

	order        *operationLog
	namespace    string
	root         string
	files        map[string]index.FileRecord
	refs         map[string][]protocol.Location
	referenceGen uint64
	symbolKey    string
	symbolUnique bool
	puts         chan index.FileRecord
	invalidates  chan string

	symbolReadStarted chan struct{}
	symbolReadRelease <-chan struct{}
	searchReadStarted chan struct{}
	searchReadRelease <-chan struct{}

	blockInvalidatePath string
	invalidateBlocked   chan struct{}
	invalidateRelease   <-chan struct{}

	documentSymbolsErr error
	searchSymbolsErr   error
	diagnosticsErr     error
}

func newFakeIndexStore(order *operationLog) *fakeIndexStore {
	return &fakeIndexStore{
		order:       order,
		files:       map[string]index.FileRecord{},
		refs:        map[string][]protocol.Location{},
		puts:        make(chan index.FileRecord, 16),
		invalidates: make(chan string, 16),
	}
}

func (s *fakeIndexStore) EnsureWorkspace(_ context.Context, root, namespace string) (protocol.WorkspaceID, error) {
	s.order.add("ensure")
	s.mu.Lock()
	s.root = root
	s.namespace = namespace
	s.mu.Unlock()
	return protocol.WorkspaceIDForRoot(root), nil
}

func (s *fakeIndexStore) FileState(_ context.Context, _ protocol.WorkspaceID, path string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, ok := s.files[path]
	return file.ContentHash, ok, nil
}

func (s *fakeIndexStore) PutFile(_ context.Context, _ protocol.WorkspaceID, file index.FileRecord) error {
	s.order.add("put:" + file.Path)
	s.mu.Lock()
	s.files[file.Path] = file
	s.documentSymbolsErr = nil
	s.searchSymbolsErr = nil
	s.diagnosticsErr = nil
	s.mu.Unlock()
	s.puts <- file
	return nil
}

func (s *fakeIndexStore) ReferenceGeneration(
	context.Context,
	protocol.WorkspaceID,
) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.referenceGen, nil
}

func (s *fakeIndexStore) ReplaceReferencesBySymbolKey(
	_ context.Context,
	_ protocol.WorkspaceID,
	key string,
	expectedGeneration uint64,
	refs []index.Reference,
) (bool, error) {
	locations := make([]protocol.Location, 0, len(refs))
	for _, ref := range refs {
		locations = append(locations, protocol.Location{
			Path: ref.Path, Range: ref.Range, IsDefinition: ref.IsDefinition,
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.referenceGen != expectedGeneration {
		return false, nil
	}
	s.refs[key] = locations
	return true, nil
}

func (s *fakeIndexStore) InvalidateFile(_ context.Context, _ protocol.WorkspaceID, path string) error {
	if path == s.blockInvalidatePath {
		select {
		case s.invalidateBlocked <- struct{}{}:
		default:
		}
		<-s.invalidateRelease
	}
	s.order.add("invalidate:" + path)
	s.mu.Lock()
	s.referenceGen++
	if file, ok := s.files[path]; ok {
		file.ContentHash = ""
		file.Symbols = nil
		file.Diagnostics = nil
		s.files[path] = file
	}
	for key := range s.refs {
		delete(s.refs, key)
	}
	s.mu.Unlock()
	s.invalidates <- path
	return nil
}

func (s *fakeIndexStore) DeleteFile(_ context.Context, _ protocol.WorkspaceID, path string) error {
	s.order.add("delete:" + path)
	s.mu.Lock()
	s.referenceGen++
	delete(s.files, path)
	for key := range s.refs {
		delete(s.refs, key)
	}
	s.mu.Unlock()
	return nil
}

func (s *fakeIndexStore) ReconcileWorkspace(_ context.Context, _ protocol.WorkspaceID, existing []string) (int, error) {
	keep := make(map[string]struct{}, len(existing))
	for _, path := range existing {
		keep[path] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := 0
	for path := range s.files {
		if _, ok := keep[path]; !ok {
			delete(s.files, path)
			pruned++
		}
	}
	if pruned > 0 {
		s.referenceGen++
		for key := range s.refs {
			delete(s.refs, key)
		}
	}
	return pruned, nil
}

func (s *fakeIndexStore) DocumentSymbols(_ context.Context, _ protocol.WorkspaceID, path string) ([]protocol.Symbol, error) {
	if s.symbolReadStarted != nil {
		select {
		case s.symbolReadStarted <- struct{}{}:
		default:
		}
	}
	if s.symbolReadRelease != nil {
		<-s.symbolReadRelease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.documentSymbolsErr != nil {
		return nil, s.documentSymbolsErr
	}
	file := s.files[path]
	out := make([]protocol.Symbol, 0, len(file.Symbols))
	for _, symbol := range file.Symbols {
		out = append(out, symbol.Symbol)
	}
	return out, nil
}

func (s *fakeIndexStore) SearchSymbols(_ context.Context, _ protocol.WorkspaceID, query string, limit int) ([]protocol.Symbol, error) {
	if s.searchReadStarted != nil {
		select {
		case s.searchReadStarted <- struct{}{}:
		default:
		}
	}
	if s.searchReadRelease != nil {
		<-s.searchReadRelease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.searchSymbolsErr != nil {
		return nil, s.searchSymbolsErr
	}
	var out []protocol.Symbol
	for _, file := range s.files {
		for _, symbol := range file.Symbols {
			if query == "" || symbol.Symbol.Name == query {
				out = append(out, symbol.Symbol)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeIndexStore) SymbolKeyAt(context.Context, protocol.WorkspaceID, string, protocol.Position) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.symbolKey, s.symbolUnique, nil
}

func (s *fakeIndexStore) ReferencesBySymbolKey(context.Context, protocol.WorkspaceID, string) ([]protocol.Location, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refs, ok := s.refs[s.symbolKey]
	if !ok {
		return nil, protocol.NewError(protocol.ErrNotReady, "reference set is unavailable")
	}
	return append([]protocol.Location(nil), refs...), nil
}

func (s *fakeIndexStore) Diagnostics(_ context.Context, _ protocol.WorkspaceID, path string) ([]protocol.Diagnostic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diagnosticsErr != nil {
		return nil, s.diagnosticsErr
	}
	var out []protocol.Diagnostic
	for filePath, file := range s.files {
		if path != "" && path != filePath {
			continue
		}
		out = append(out, file.Diagnostics...)
	}
	return out, nil
}

func (s *fakeIndexStore) Status(context.Context, protocol.WorkspaceID) (protocol.IndexStatusResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := protocol.IndexStatusResult{Root: s.root, State: protocol.IndexReady}
	for _, file := range s.files {
		result.FilesTotal++
		if file.ContentHash != "" {
			result.FilesIndexed++
		}
	}
	if result.FilesIndexed < result.FilesTotal {
		result.State = protocol.IndexIndexing
	}
	return result, nil
}

func (*fakeIndexStore) GC(context.Context) (bool, index.GCStats, error) {
	return false, index.GCStats{}, nil
}
func (*fakeIndexStore) Checkpoint(context.Context) error { return nil }
func (*fakeIndexStore) Close() error                     { return nil }

type indexedHarness struct {
	root     string
	reg      *Registry
	ws       *Workspace
	srv      *fakeServer
	sup      *fakeSupervisor
	scanner  *fakeIndexScanner
	watcher  *fakeIndexWatcher
	store    *fakeIndexStore
	order    *operationLog
	activity atomic.Int32
}

func newIndexedHarness(t *testing.T, configure func(*indexedHarness)) *indexedHarness {
	t.Helper()
	root, err := CanonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/user.ts", "export interface User {\n  id: string;\n}\n")

	order := &operationLog{}
	h := &indexedHarness{
		root:  root,
		srv:   newFakeServer(),
		order: order,
	}
	h.srv.symbols = []protocol.Symbol{{
		Name: "User", Kind: protocol.SymbolKindInterface, Path: "src/user.ts",
		Range: rng(0, 0, 2, 1),
	}}
	h.sup = &fakeSupervisor{server: h.srv}
	h.scanner = &fakeIndexScanner{
		namespace: "fingerskier/langer",
		files:     []string{"src/user.ts", "README.md"},
		order:     order,
	}
	h.watcher = newFakeIndexWatcher(order)
	h.store = newFakeIndexStore(order)
	if configure != nil {
		configure(h)
	}

	cfg := &config.Config{
		LogLevel: "info",
		LanguageServers: []config.LanguageServer{{
			Name: "typescript", Command: "typescript-language-server",
			Args: []string{"--stdio"}, FileExtensions: []string{".ts", ".tsx"},
		}},
	}
	h.reg = NewRegistry(RegistryOptions{
		Config: cfg,
		Clock:  clock.NewFake(clock.New().Now()),
		Store:  h.store,
		NewScanner: func(procx.Resolver, procx.Runner) watch.Scanner {
			return h.scanner
		},
		NewWatcher: func(string, watch.Scanner, clock.Clock, time.Duration) (watch.Watcher, error) {
			order.add("watcher-create")
			return h.watcher, nil
		},
		OnFileActivity: func() { h.activity.Add(1) },
		NewSupervisor: func(lsp.Options) (lsp.Supervisor, error) {
			return h.sup, nil
		},
	})
	t.Cleanup(func() { _ = h.reg.Shutdown(context.Background()) })

	id, err := h.reg.Open(context.Background(), root)
	if err != nil {
		t.Fatalf("Registry.Open: %v", err)
	}
	h.ws, err = h.reg.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func waitForPut(t *testing.T, puts <-chan index.FileRecord) index.FileRecord {
	t.Helper()
	select {
	case file := <-puts:
		return file
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for indexed file")
		return index.FileRecord{}
	}
}

func waitForReady(t *testing.T, ws *Workspace) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := ws.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.State == protocol.IndexReady {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for ready index")
}

func drainStrings(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestIndexedOpenIsWatcherFirstAndPersistsDiskOnlySymbols(t *testing.T) {
	selection := rng(0, 17, 0, 21)
	h := newIndexedHarness(t, func(h *indexedHarness) {
		h.srv.indexSymbols = []lsp.IndexSymbol{{
			Symbol: protocol.Symbol{
				Name: "User", Kind: protocol.SymbolKindInterface, Path: "src/user.ts",
				Range: rng(0, 0, 2, 1),
			},
			SelectionRange: selection,
		}}
	})

	file := waitForPut(t, h.store.puts)
	order := h.order.snapshot()
	for before, after := range map[string]string{
		"namespace":      "ensure",
		"ensure":         "watcher-create",
		"watcher-create": "watcher-ready",
		"watcher-ready":  "list",
	} {
		if operationIndex(order, before) < 0 || operationIndex(order, before) >= operationIndex(order, after) {
			t.Fatalf("startup order %q before %q not observed: %v", before, after, order)
		}
	}
	if file.Path != "src/user.ts" || file.LanguageID != "typescript" {
		t.Fatalf("indexed file = %+v", file)
	}
	if len(file.Symbols) != 1 {
		t.Fatalf("indexed symbols = %+v", file.Symbols)
	}
	symbol := file.Symbols[0]
	if symbol.SelectionRange != selection {
		t.Errorf("selection range = %+v, want %+v", symbol.SelectionRange, selection)
	}
	if want := index.StableKey(symbol.Symbol); symbol.StableKey != want {
		t.Errorf("stable key = %q, want %q", symbol.StableKey, want)
	}
	if want := index.SymbolKey("fingerskier/langer", file.Path, symbol.StableKey); symbol.SymbolKey != want {
		t.Errorf("symbol key = %q, want %q", symbol.SymbolKey, want)
	}
	h.srv.mu.Lock()
	seen := append([]string(nil), h.srv.withDiskSeen...)
	h.srv.mu.Unlock()
	if len(seen) != 1 || seen[0] != "export interface User {\n  id: string;\n}\n" {
		t.Errorf("WithDiskText saw %q", seen)
	}
	if _, open := h.srv.openText("src/user.ts"); open {
		t.Error("background indexing left a previously closed document open")
	}
}

func TestFreshDocumentSymbolsAndReferencesUseTheCompleteCache(t *testing.T) {
	h := newIndexedHarness(t, nil)
	file := waitForPut(t, h.store.puts)
	key := file.Symbols[0].SymbolKey

	h.store.mu.Lock()
	h.store.symbolKey = key
	h.store.symbolUnique = true
	h.store.refs[key] = []protocol.Location{{
		Path: "src/cached.ts", Range: rng(4, 2, 4, 6), IsDefinition: false,
	}}
	h.store.mu.Unlock()
	h.srv.mu.Lock()
	beforeCalls := h.srv.documentSymbolCalls
	h.srv.symbols = []protocol.Symbol{{Name: "LiveOnly", Path: "src/user.ts"}}
	h.srv.locations = []protocol.Location{{Path: "src/live.ts"}}
	h.srv.mu.Unlock()

	symbols, err := h.ws.DocumentSymbols(context.Background(), sess, "src/user.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 1 || symbols[0].Name != "User" {
		t.Fatalf("document symbols = %+v, want cached User", symbols)
	}
	refs, err := h.ws.References(context.Background(), sess, "src/user.ts", protocol.Position{Line: 0, Character: 18})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "src/cached.ts" {
		t.Fatalf("references = %+v, want complete cached set", refs)
	}
	h.srv.mu.Lock()
	afterCalls := h.srv.documentSymbolCalls
	h.srv.mu.Unlock()
	if afterCalls != beforeCalls {
		t.Errorf("fresh cache query called documentSymbol live: %d -> %d", beforeCalls, afterCalls)
	}
}

func TestReferencesBypassCacheWhileWorkspaceIndexIsNotReady(t *testing.T) {
	h := newIndexedHarness(t, nil)
	file := waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	key := file.Symbols[0].SymbolKey

	h.store.mu.Lock()
	h.store.symbolKey = key
	h.store.symbolUnique = true
	h.store.refs[key] = []protocol.Location{{Path: "src/stale-cached.ts"}}
	h.store.mu.Unlock()
	h.srv.mu.Lock()
	h.srv.locations = []protocol.Location{{Path: "src/live.ts"}}
	beforeCalls := h.srv.referenceCalls
	h.srv.mu.Unlock()

	// Model restart staging or a watcher failure after the on-disk cache was
	// previously complete. A per-file hash alone cannot authorize a
	// workspace-wide reference set while the workspace barrier is down.
	h.ws.indexMu.Lock()
	h.ws.indexState = protocol.IndexIndexing
	h.ws.indexMu.Unlock()

	refs, err := h.ws.References(
		context.Background(),
		sess,
		"src/user.ts",
		protocol.Position{Line: 0, Character: 18},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "src/live.ts" {
		t.Fatalf("references = %+v, want live fallback while workspace is not ready", refs)
	}
	h.srv.mu.Lock()
	afterCalls := h.srv.referenceCalls
	h.srv.mu.Unlock()
	if afterCalls != beforeCalls+1 {
		t.Fatalf("live reference calls = %d -> %d, want one call", beforeCalls, afterCalls)
	}
}

func TestQueryOutsideAuthoritativeProjectSetStaysLiveOnly(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	drainStrings(h.store.invalidates)
	write(t, h.root, "node_modules/unlisted.ts", "export const secret = 1\n")

	symbols, err := h.ws.DocumentSymbols(context.Background(), sess, "node_modules/unlisted.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) == 0 {
		t.Fatal("safe in-root dependency query did not reach the live language server")
	}
	select {
	case path := <-h.store.invalidates:
		t.Fatalf("live-only dependency query invalidated/queued %q for indexing", path)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case file := <-h.store.puts:
		t.Fatalf("live-only dependency query persisted %q", file.Path)
	default:
	}
	h.ws.indexMu.Lock()
	_, known := h.ws.known["node_modules/unlisted.ts"]
	h.ws.indexMu.Unlock()
	if known {
		t.Fatal("live-only dependency query contaminated the authoritative project set")
	}
}

func TestQueryRejectsKnownPathAfterParentBecomesSymlink(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)

	outside := t.TempDir()
	write(t, outside, "user.ts", "export const outsideSecret = 1\n")
	if err := os.Rename(filepath.Join(h.root, "src"), filepath.Join(h.root, "src-before-symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.root, "src")); err != nil {
		t.Skipf("creating directory symlink: %v", err)
	}

	_, err := h.ws.DocumentSymbols(context.Background(), sess, "src/user.ts")
	wantCode(t, err, protocol.ErrWorkspaceUnknown)
}

func TestMissingSelectionRangeNeverPublishesCachedReferences(t *testing.T) {
	h := newIndexedHarness(t, func(h *indexedHarness) {
		h.srv.indexSymbols = []lsp.IndexSymbol{{
			Symbol: protocol.Symbol{
				Name: "User", Kind: protocol.SymbolKindInterface, Path: "src/user.ts",
				Range: rng(0, 0, 2, 1),
			},
			SelectionRange:    rng(0, 0, 2, 1),
			HasSelectionRange: false,
		}}
		h.srv.locations = []protocol.Location{{
			Path: "src/user.ts", Range: rng(0, 17, 0, 21), IsDefinition: true,
		}}
	})
	file := waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	drainStrings(h.store.invalidates)
	key := file.Symbols[0].SymbolKey
	h.store.mu.Lock()
	h.store.symbolKey = key
	h.store.symbolUnique = true
	_, published := h.store.refs[key]
	h.store.mu.Unlock()
	if published {
		t.Fatal("a fallback symbol range was published as a complete cached reference set")
	}
	h.srv.mu.Lock()
	if h.srv.referenceCalls != 0 {
		t.Fatalf("background index queried references %d times without a valid selectionRange", h.srv.referenceCalls)
	}
	h.srv.mu.Unlock()

	for query := 0; query < 2; query++ {
		refs, err := h.ws.References(context.Background(), sess, "src/user.ts", protocol.Position{Line: 0, Character: 18})
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 1 || refs[0].Path != "src/user.ts" {
			t.Fatalf("missing cached reference set did not fall back live: %+v", refs)
		}
	}
	select {
	case path := <-h.store.invalidates:
		t.Fatalf("intentional reference cache miss invalidated fresh file %q", path)
	default:
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if h.srv.referenceCalls != 2 {
		t.Fatalf("live reference fallback calls = %d, want 2", h.srv.referenceCalls)
	}
}

func TestInitialScanPublishesReferencesOnlyAfterAllInvalidations(t *testing.T) {
	blocked := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newIndexedHarness(t, func(h *indexedHarness) {
		write(t, h.root, "a.ts", "export const target = 1\n")
		write(t, h.root, "z.ts", "import { target } from './a'\nconsole.log(target)\n")
		h.scanner.files = []string{"a.ts", "z.ts"}
		h.store.blockInvalidatePath = "z.ts"
		h.store.invalidateBlocked = blocked
		h.store.invalidateRelease = release
		h.srv.indexSymbolsByPath = map[string][]lsp.IndexSymbol{
			"a.ts": {{
				Symbol: protocol.Symbol{
					Name: "target", Kind: protocol.SymbolKindVariable, Path: "a.ts",
					Range: rng(0, 0, 0, 23),
				},
				SelectionRange: rng(0, 13, 0, 19), HasSelectionRange: true,
			}},
			"z.ts": {},
		}
		h.srv.locationsByPath = map[string][]protocol.Location{
			"a.ts": {
				{Path: "a.ts", Range: rng(0, 13, 0, 19), IsDefinition: true},
				{Path: "z.ts", Range: rng(1, 12, 1, 18)},
			},
		}
	})

	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("later initial invalidation never blocked")
	}
	select {
	case file := <-h.store.puts:
		close(release)
		t.Fatalf("committed %s before all initial invalidations completed", file.Path)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	first := waitForPut(t, h.store.puts)
	second := waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	var definition index.FileRecord
	for _, file := range []index.FileRecord{first, second} {
		if file.Path == "a.ts" {
			definition = file
		}
	}
	if len(definition.Symbols) != 1 {
		t.Fatalf("definition record = %+v", definition)
	}
	key := definition.Symbols[0].SymbolKey
	h.store.mu.Lock()
	h.store.symbolKey = key
	h.store.symbolUnique = true
	refs := append([]protocol.Location(nil), h.store.refs[key]...)
	h.store.mu.Unlock()
	if len(refs) != 2 {
		t.Fatalf("initial reference set = %+v, want definition and usage", refs)
	}
}

func TestReferenceGenerationRejectsSnapshotRacedByReferencedFileDeletion(t *testing.T) {
	referenceStarted := make(chan struct{}, 1)
	referenceRelease := make(chan struct{})
	h := newIndexedHarness(t, func(h *indexedHarness) {
		write(t, h.root, "a.ts", "export const target = 1\n")
		write(t, h.root, "b.ts", "import { target } from './a'\nconsole.log(target)\n")
		h.scanner.files = []string{"a.ts", "b.ts"}
		h.srv.indexSymbolsByPath = map[string][]lsp.IndexSymbol{
			"a.ts": {{
				Symbol: protocol.Symbol{
					Name: "target", Kind: protocol.SymbolKindVariable, Path: "a.ts",
					Range: rng(0, 0, 0, 23),
				},
				SelectionRange: rng(0, 13, 0, 19), HasSelectionRange: true,
			}},
			"b.ts": {},
		}
		h.srv.locationsByPath = map[string][]protocol.Location{
			"a.ts": {
				{Path: "a.ts", Range: rng(0, 13, 0, 19), IsDefinition: true},
				{Path: "b.ts", Range: rng(1, 12, 1, 18)},
			},
		}
		h.srv.referenceStarted = referenceStarted
		h.srv.referenceRelease = referenceRelease
	})

	select {
	case <-referenceStarted:
	case <-time.After(5 * time.Second):
		close(referenceRelease)
		t.Fatal("definition indexing never reached its cross-file reference query")
	}
	if err := os.Remove(filepath.Join(h.root, "b.ts")); err != nil {
		close(referenceRelease)
		t.Fatal(err)
	}
	h.watcher.events <- watch.Batch{Deleted: []string{"b.ts"}}
	deadline := time.Now().Add(5 * time.Second)
	for operationIndex(h.order.snapshot(), "delete:b.ts") < 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if operationIndex(h.order.snapshot(), "delete:b.ts") < 0 {
		close(referenceRelease)
		t.Fatal("referenced-file deletion did not reach the store")
	}
	close(referenceRelease)

	file := waitForPut(t, h.store.puts)
	if file.Path != "a.ts" || len(file.Symbols) != 1 {
		t.Fatalf("indexed definition = %+v", file)
	}
	waitForReady(t, h.ws)
	key := file.Symbols[0].SymbolKey
	h.store.mu.Lock()
	_, published := h.store.refs[key]
	h.store.mu.Unlock()
	if published {
		t.Fatal("pre-deletion cross-file reference snapshot was republished as complete")
	}
}

func TestStaleCacheIsInvalidatedBeforeLiveAnswerAndReindexed(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	h.order.mu.Lock()
	h.order.items = nil
	h.order.mu.Unlock()

	write(t, h.root, "src/user.ts", "export interface Renamed { id: string }\n")
	h.srv.mu.Lock()
	h.srv.symbols = []protocol.Symbol{{
		Name: "Renamed", Kind: protocol.SymbolKindInterface, Path: "src/user.ts",
		Range: rng(0, 0, 0, 39),
	}}
	h.srv.mu.Unlock()

	symbols, err := h.ws.DocumentSymbols(context.Background(), sess, "src/user.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 1 || symbols[0].Name != "Renamed" {
		t.Fatalf("stale cache returned %+v", symbols)
	}
	_ = waitForPut(t, h.store.puts)
	order := h.order.snapshot()
	if invalidated, put := operationIndex(order, "invalidate:src/user.ts"), operationIndex(order, "put:src/user.ts"); invalidated < 0 || put < 0 || invalidated >= put {
		t.Fatalf("stale replacement was not invalidate-before-put: %v", order)
	}
}

func TestPerFileCacheReadIsAtomicAgainstWatcherInvalidation(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	drainStrings(h.store.invalidates)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h.store.symbolReadStarted = started
	h.store.symbolReadRelease = release

	type result struct {
		symbols []protocol.Symbol
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		symbols, err := h.ws.DocumentSymbols(context.Background(), sess, "src/user.ts")
		resultCh <- result{symbols: symbols, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cached document-symbol read did not start")
	}

	write(t, h.root, "src/user.ts", "export interface Changed { id: string }\n")
	h.watcher.events <- watch.Batch{Changed: []string{"src/user.ts"}}
	select {
	case path := <-h.store.invalidates:
		t.Fatalf("watcher invalidated %s while the cache read was in flight", path)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	got := <-resultCh
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.symbols) != 1 || got.symbols[0].Name != "User" {
		t.Fatalf("atomic cached read = %+v", got.symbols)
	}
	select {
	case <-h.store.invalidates:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher invalidation did not proceed after cached read completed")
	}
}

func TestWorkspaceCacheReadIsAtomicAgainstReadinessInvalidation(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	drainStrings(h.store.invalidates)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h.store.searchReadStarted = started
	h.store.searchReadRelease = release

	type result struct {
		symbols []protocol.Symbol
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		symbols, err := h.ws.WorkspaceSymbols(context.Background(), sess, "User", 10)
		resultCh <- result{symbols: symbols, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace-symbol cache read did not start")
	}

	write(t, h.root, "src/user.ts", "export interface Changed { id: string }\n")
	h.watcher.events <- watch.Batch{Changed: []string{"src/user.ts"}}
	select {
	case path := <-h.store.invalidates:
		t.Fatalf("watcher invalidated %s while workspace cache read was in flight", path)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	got := <-resultCh
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.symbols) != 1 || got.symbols[0].Name != "User" {
		t.Fatalf("atomic workspace cache read = %+v", got.symbols)
	}
	select {
	case <-h.store.invalidates:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace invalidation did not proceed after cache read completed")
	}
}

func TestPerFileNotReadyCacheRaceFallsBackLiveAndQueuesHealing(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	drainStrings(h.store.invalidates)

	h.store.mu.Lock()
	h.store.documentSymbolsErr = protocol.NewError(protocol.ErrNotReady, "GC invalidated this file")
	h.store.mu.Unlock()
	h.srv.mu.Lock()
	h.srv.symbols = []protocol.Symbol{{
		Name: "LiveAfterGC", Kind: protocol.SymbolKindInterface, Path: "src/user.ts",
		Range: rng(0, 0, 2, 1),
	}}
	h.srv.mu.Unlock()

	symbols, err := h.ws.DocumentSymbols(context.Background(), sess, "src/user.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 1 || symbols[0].Name != "LiveAfterGC" {
		t.Fatalf("per-file NOT_READY did not fall back live: %+v", symbols)
	}
	select {
	case <-h.store.invalidates:
	case <-time.After(5 * time.Second):
		t.Fatal("per-file NOT_READY did not synchronously invalidate before healing")
	}
	_ = waitForPut(t, h.store.puts)
}

func TestWorkspaceNotReadyTriggersHealingAndRemainsNotReady(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	drainStrings(h.store.invalidates)

	h.store.mu.Lock()
	file := h.store.files["src/user.ts"]
	file.ContentHash = ""
	h.store.files["src/user.ts"] = file
	h.store.diagnosticsErr = protocol.NewError(protocol.ErrNotReady, "GC invalidated workspace diagnostics")
	h.store.mu.Unlock()

	_, _, err := h.ws.Diagnostics(context.Background(), sess, "")
	wantCode(t, err, protocol.ErrNotReady)
	select {
	case <-h.store.invalidates:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace NOT_READY did not start a healing pass")
	}
	_ = waitForPut(t, h.store.puts)
}

func TestStatusMergesStoreDegradationAndTriggersHealing(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	drainStrings(h.store.invalidates)

	h.store.mu.Lock()
	file := h.store.files["src/user.ts"]
	file.ContentHash = ""
	h.store.files["src/user.ts"] = file
	h.store.mu.Unlock()

	status, err := h.ws.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != protocol.IndexIndexing || status.FilesIndexed != 0 || status.FilesTotal != 1 {
		t.Fatalf("degraded status = %+v, want indexing 0/1", status)
	}
	select {
	case <-h.store.invalidates:
	case <-time.After(5 * time.Second):
		t.Fatal("degraded status did not trigger healing")
	}
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
}

func TestIndexerDiscardsAFileThatChangesDuringSemanticQueries(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newIndexedHarness(t, func(h *indexedHarness) {
		h.srv.indexStarted = started
		h.srv.indexRelease = release
	})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("index query never started")
	}
	write(t, h.root, "src/user.ts", "export interface User { id: string; changed: true }\n")
	close(release)

	file := waitForPut(t, h.store.puts)
	wantHash, err := h.scanner.Hash(filepath.Join(h.root, "src", "user.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if file.ContentHash != wantHash {
		t.Fatalf("committed hash = %q, want latest %q", file.ContentHash, wantHash)
	}
	select {
	case stale := <-h.store.puts:
		t.Fatalf("stale generation was also committed: %+v", stale)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCanceledQueryDoesNotWaitForOrStealIndexerPathLock(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newIndexedHarness(t, func(h *indexedHarness) {
		h.srv.indexStarted = started
		h.srv.indexRelease = release
	})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("background index never acquired the path")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	queryDone := make(chan error, 1)
	go func() {
		_, err := h.ws.DocumentSymbols(ctx, sess, "src/user.ts")
		queryDone <- err
	}()

	select {
	case err := <-queryDone:
		wantCode(t, err, protocol.ErrNotReady)
	case <-time.After(250 * time.Millisecond):
		close(release)
		t.Fatal("canceled query remained blocked behind background indexing")
	}

	close(release)
	_ = waitForPut(t, h.store.puts)
	waitForReady(t, h.ws)
	symbols, err := h.ws.DocumentSymbols(context.Background(), sess, "src/user.ts")
	if err != nil {
		t.Fatalf("later query after releasing index lock: %v", err)
	}
	if len(symbols) != 1 || symbols[0].Name != "User" {
		t.Fatalf("later query symbols = %+v", symbols)
	}
}

func TestWatcherBatchInvalidatesSynchronouslyAndRecordsActivity(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	h.order.mu.Lock()
	h.order.items = nil
	h.order.mu.Unlock()
	write(t, h.root, "src/user.ts", "export interface Changed { id: string }\n")
	h.watcher.events <- watch.Batch{Changed: []string{"src/user.ts"}}

	_ = waitForPut(t, h.store.puts)
	order := h.order.snapshot()
	if invalidated, put := operationIndex(order, "invalidate:src/user.ts"), operationIndex(order, "put:src/user.ts"); invalidated < 0 || put < 0 || invalidated >= put {
		t.Fatalf("watcher replacement was not invalidate-before-put: %v", order)
	}
	if got := h.activity.Load(); got != 1 {
		t.Fatalf("file activity calls = %d, want one per non-empty batch", got)
	}
}

func TestWatcherEditReindexesOnlyTheChangedFile(t *testing.T) {
	h := newIndexedHarness(t, func(h *indexedHarness) {
		write(t, h.root, "src/other.ts", "export const other = 1\n")
		h.scanner.files = []string{"src/other.ts", "src/user.ts"}
	})
	first := waitForPut(t, h.store.puts)
	second := waitForPut(t, h.store.puts)
	if first.Path == second.Path {
		t.Fatalf("initial scan indexed %s twice", first.Path)
	}
	waitForReady(t, h.ws)

	write(t, h.root, "src/user.ts", "export interface Changed { id: string }\n")
	h.watcher.events <- watch.Batch{Changed: []string{"src/user.ts"}}
	reindexed := waitForPut(t, h.store.puts)
	if reindexed.Path != "src/user.ts" {
		t.Fatalf("watcher edit reindexed %s, want only src/user.ts", reindexed.Path)
	}
	select {
	case extra := <-h.store.puts:
		t.Fatalf("single-file edit also reindexed %s", extra.Path)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFatalInitialScanErrorIsStructuredFailedStatus(t *testing.T) {
	h := newIndexedHarness(t, func(h *indexedHarness) {
		h.scanner.listErr = errors.New("scan exploded")
	})
	status, err := h.ws.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != protocol.IndexFailed || status.Error == nil {
		t.Fatalf("status = %+v, want structured failed state", status)
	}
	if status.Error.Code != protocol.ErrInternal {
		t.Errorf("failure code = %q, want INTERNAL", status.Error.Code)
	}
}

func TestIndexedShutdownJoinsWatcherBeforeSupervisor(t *testing.T) {
	h := newIndexedHarness(t, nil)
	_ = waitForPut(t, h.store.puts)
	h.sup.shutdownHook = func() {
		select {
		case <-h.watcher.stopped:
		default:
			t.Error("supervisor shut down before watcher/indexer joined")
		}
	}
	if err := h.reg.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

var _ watch.Scanner = (*fakeIndexScanner)(nil)
var _ watch.Watcher = (*fakeIndexWatcher)(nil)
var _ index.Store = (*fakeIndexStore)(nil)
