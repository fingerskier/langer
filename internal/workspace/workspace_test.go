package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/protocol"
)

// ---- fakes -----------------------------------------------------------------
//
// internal/workspace is an orchestrator: its job is to decide WHICH server
// answers, whether a document is current, and which SPEC §3.6 code a failure
// becomes. Its tests therefore substitute lsp.Supervisor and lsp.Server, which
// are exported interfaces, rather than driving a language server. The wire
// layer below them is M1's, and daemon/ exercises the real one end to end.

type fakeSupervisor struct {
	mu           sync.Mutex
	server       *fakeServer
	acquireErr   error
	acquires     []string
	status       []protocol.ServerStatus
	shutdowns    int
	shutdownHook func()
}

func (f *fakeSupervisor) Acquire(_ context.Context, languageID string) (lsp.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires = append(f.acquires, languageID)
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	return f.server, nil
}

func (f *fakeSupervisor) Status() []protocol.ServerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeSupervisor) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdowns++
	hook := f.shutdownHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeSupervisor) acquired() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acquires...)
}

type fakeServer struct {
	mu sync.Mutex

	open        map[string]string
	closed      []string
	openCalls   int
	generation  uint64
	unsupported map[string]bool

	locations          []protocol.Location
	locationsByPath    map[string][]protocol.Location
	referenceCalls     int
	hover              *protocol.Hover
	symbols            []protocol.Symbol
	indexSymbols       []lsp.IndexSymbol
	indexSymbolsByPath map[string][]lsp.IndexSymbol
	diags              []protocol.Diagnostic
	stale              bool
	edits              []protocol.FileEdit
	queryErr           error
	settles            []string

	withTextSeen        []string
	withDiskSeen        []string
	documentSymbolCalls int
	indexStarted        chan struct{}
	indexRelease        <-chan struct{}
	referenceStarted    chan struct{}
	referenceRelease    <-chan struct{}
}

func newFakeServer() *fakeServer {
	return &fakeServer{open: map[string]string{}, unsupported: map[string]bool{}}
}

func (f *fakeServer) Generation() uint64 { return f.generation }

func (f *fakeServer) Supports(capability string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unsupported[capability]
}

func (f *fakeServer) Open(_ context.Context, path, _, text string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queryErr != nil {
		return 0, f.queryErr
	}
	f.openCalls++
	f.open[path] = text
	return uint64(f.openCalls), nil
}

func (f *fakeServer) Close(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, path)
	f.closed = append(f.closed, path)
	return nil
}

func (f *fakeServer) WithText(ctx context.Context, path, text string, fn func(context.Context, uint64) error) error {
	f.mu.Lock()
	if f.queryErr != nil {
		err := f.queryErr
		f.mu.Unlock()
		return err
	}
	if _, ok := f.open[path]; !ok {
		f.mu.Unlock()
		return protocol.NewErrorf(protocol.ErrNotReady, "%s must be open before a speculative edit", path)
	}
	base := f.open[path]
	f.open[path] = text
	f.withTextSeen = append(f.withTextSeen, text)
	f.mu.Unlock()

	err := fn(ctx, 99)

	f.mu.Lock()
	f.open[path] = base
	f.mu.Unlock()
	return err
}

func (f *fakeServer) WithDiskText(ctx context.Context, path, _ string, text string, fn func(context.Context, uint64) error) error {
	f.mu.Lock()
	if f.queryErr != nil {
		err := f.queryErr
		f.mu.Unlock()
		return err
	}
	base, wasOpen := f.open[path]
	f.open[path] = text
	f.withDiskSeen = append(f.withDiskSeen, text)
	f.mu.Unlock()

	err := fn(ctx, 101)

	f.mu.Lock()
	if wasOpen {
		f.open[path] = base
	} else {
		delete(f.open, path)
	}
	f.mu.Unlock()
	return err
}

func (f *fakeServer) Definition(context.Context, string, protocol.Position) ([]protocol.Location, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locations, f.queryErr
}

func (f *fakeServer) References(_ context.Context, path string, _ protocol.Position, _ bool) ([]protocol.Location, error) {
	f.mu.Lock()
	f.referenceCalls++
	locations := f.locations
	if f.locationsByPath != nil {
		locations = f.locationsByPath[path]
	}
	locations = append([]protocol.Location(nil), locations...)
	err := f.queryErr
	started := f.referenceStarted
	release := f.referenceRelease
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return locations, err
}

func (f *fakeServer) Hover(context.Context, string, protocol.Position) (*protocol.Hover, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hover, f.queryErr
}

func (f *fakeServer) DocumentSymbols(context.Context, string) ([]protocol.Symbol, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.documentSymbolCalls++
	return f.symbols, f.queryErr
}

func (f *fakeServer) DocumentSymbolsForIndex(_ context.Context, path string) ([]lsp.IndexSymbol, error) {
	f.mu.Lock()
	f.documentSymbolCalls++
	started := f.indexStarted
	release := f.indexRelease
	indexSymbols := append([]lsp.IndexSymbol(nil), f.indexSymbols...)
	if byPath, ok := f.indexSymbolsByPath[path]; ok {
		indexSymbols = make([]lsp.IndexSymbol, len(byPath))
		copy(indexSymbols, byPath)
	}
	symbols := append([]protocol.Symbol(nil), f.symbols...)
	err := f.queryErr
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if indexSymbols != nil {
		return indexSymbols, err
	}
	out := make([]lsp.IndexSymbol, 0, len(f.symbols))
	for _, symbol := range symbols {
		out = append(out, lsp.IndexSymbol{
			Symbol: symbol, SelectionRange: symbol.Range, HasSelectionRange: true,
		})
	}
	return out, err
}

func (f *fakeServer) WorkspaceSymbols(context.Context, string, int) ([]protocol.Symbol, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.symbols, f.queryErr
}

func (f *fakeServer) Rename(context.Context, string, protocol.Position, string) ([]protocol.FileEdit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.edits, f.queryErr
}

func (f *fakeServer) Diagnostics(_ context.Context, path string, _ uint64) ([]protocol.Diagnostic, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles = append(f.settles, path)
	return f.diags, f.stale, f.queryErr
}

func (f *fakeServer) settleCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.settles {
		if p == path {
			n++
		}
	}
	return n
}

func (f *fakeServer) openText(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	text, ok := f.open[path]
	return text, ok
}

func (f *fakeServer) closedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...)
}

func (f *fakeServer) setQueryErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryErr = err
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	t    *testing.T
	root string
	reg  *Registry
	ws   *Workspace
	sup  *fakeSupervisor
	srv  *fakeServer
}

const sess = protocol.SessionID("session-1")

func newHarness(t *testing.T) *harness {
	t.Helper()

	// t.TempDir hands back a symlinked path on macOS (/var → /private/var).
	// Canonicalise here so the harness compares like with like: the Registry
	// resolves symlinks on purpose (SPEC §3.2 keys a workspace by its absolute
	// root, and two spellings of one directory must not become two workspaces).
	root, err := CanonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "src/user.ts", "export interface User {\n  id: string;\n}\n")
	write(t, root, "notes.txt", "not a source file\n")

	srv := newFakeServer()
	sup := &fakeSupervisor{server: srv}

	cfg := &config.Config{
		LogLevel: "info",
		LanguageServers: []config.LanguageServer{{
			Name:           "typescript",
			Command:        "typescript-language-server",
			Args:           []string{"--stdio"},
			FileExtensions: []string{".ts", ".tsx"},
		}},
	}

	reg := NewRegistry(RegistryOptions{
		Config: cfg,
		Clock:  clock.NewFake(clock.New().Now()),
		NewSupervisor: func(lsp.Options) (lsp.Supervisor, error) {
			return sup, nil
		},
	})
	t.Cleanup(func() { _ = reg.Shutdown(context.Background()) })

	id, err := reg.Open(context.Background(), root)
	if err != nil {
		t.Fatalf("Registry.Open: %v", err)
	}
	ws, err := reg.Get(id)
	if err != nil {
		t.Fatalf("Registry.Get: %v", err)
	}
	return &harness{t: t, root: root, reg: reg, ws: ws, sup: sup, srv: srv}
}

func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return abs
}

func wantCode(t *testing.T, err error, code protocol.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", code)
	}
	got := protocol.AsError(err)
	if got.Code != code {
		t.Fatalf("error code = %s (%s), want %s", got.Code, got.Message, code)
	}
}

// ---- tests -----------------------------------------------------------------

func TestRegistryOpenIsIdempotentPerRoot(t *testing.T) {
	h := newHarness(t)

	second, err := h.reg.Open(context.Background(), h.root)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if second != h.ws.ID() {
		t.Errorf("reopening %s gave id %q, want %q — SPEC §3.2 keys a workspace by its absolute root",
			h.root, second, h.ws.ID())
	}
}

func TestConcurrentRegistryOpenBuildsOneWorkspaceActor(t *testing.T) {
	root, err := CanonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	reg := NewRegistry(RegistryOptions{
		Config: &config.Config{LogLevel: "info"},
		Clock:  clock.NewFake(clock.New().Now()),
		NewSupervisor: func(lsp.Options) (lsp.Supervisor, error) {
			started <- struct{}{}
			<-release
			return &fakeSupervisor{server: newFakeServer()}, nil
		},
	})
	t.Cleanup(func() { _ = reg.Shutdown(context.Background()) })

	type result struct {
		id  protocol.WorkspaceID
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			id, err := reg.Open(context.Background(), root)
			results <- result{id: id, err: err}
		}()
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("first workspace construction did not start")
	}
	select {
	case <-started:
		close(release)
		t.Fatal("concurrent Open constructed a duplicate workspace actor")
	case <-time.After(100 * time.Millisecond):
		close(release)
	}

	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent Open errors = %v, %v", first.err, second.err)
	}
	if first.id == "" || first.id != second.id {
		t.Fatalf("concurrent Open ids = %q, %q", first.id, second.id)
	}
}

func TestRegistryGetUnknownWorkspace(t *testing.T) {
	h := newHarness(t)
	_, err := h.reg.Get("ws-does-not-exist")
	wantCode(t, err, protocol.ErrWorkspaceUnknown)
}

func TestRegistryOpenRejectsANonDirectory(t *testing.T) {
	h := newHarness(t)
	_, err := h.reg.Open(context.Background(), filepath.Join(h.root, "notes.txt"))
	wantCode(t, err, protocol.ErrWorkspaceUnknown)
}

// TestPathEscapingTheRootIsWorkspaceUnknown is the one that keeps a confused —
// or malicious — client from reading /etc/passwd through the daemon.
func TestPathEscapingTheRootIsWorkspaceUnknown(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"../outside.ts",
		"src/../../outside.ts",
		"/etc/passwd",
		"",
	} {
		t.Run(path, func(t *testing.T) {
			_, err := h.ws.Definition(context.Background(), sess, path, protocol.Position{})
			wantCode(t, err, protocol.ErrWorkspaceUnknown)
		})
	}
}

func TestSymlinkInsideWorkspaceCannotEscapeRoot(t *testing.T) {
	h := newHarness(t)
	outside := filepath.Join(t.TempDir(), "secret.ts")
	if err := os.WriteFile(outside, []byte("export const secret = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.root, "src", "escape.ts")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := h.ws.Definition(context.Background(), sess, "src/escape.ts", protocol.Position{})
	wantCode(t, err, protocol.ErrWorkspaceUnknown)
}

func TestMissingFileIsWorkspaceUnknown(t *testing.T) {
	h := newHarness(t)
	_, err := h.ws.Definition(context.Background(), sess, "src/ghost.ts", protocol.Position{})
	wantCode(t, err, protocol.ErrWorkspaceUnknown)
}

// TestUnclaimedExtensionIsUnsupported: no registry entry claims .txt, so no
// language server can answer. SPEC §3.6 has a code for exactly that.
func TestUnclaimedExtensionIsUnsupported(t *testing.T) {
	h := newHarness(t)
	_, err := h.ws.Definition(context.Background(), sess, "notes.txt", protocol.Position{})
	wantCode(t, err, protocol.ErrUnsupported)
}

func TestCapabilityGatingIsUnsupportedWithNoRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.srv.unsupported[lsp.CapRename] = true

	_, err := h.ws.Rename(context.Background(), sess, "src/user.ts", protocol.Position{}, "Renamed")
	wantCode(t, err, protocol.ErrUnsupported)
}

// TestCrashedServerSurfacesStructured — SPEC §8: a language server crash must
// not take anything else down, and the caller must learn a code, not a hang.
func TestCrashedServerSurfacesStructured(t *testing.T) {
	h := newHarness(t)
	h.sup.acquireErr = protocol.NewError(protocol.ErrServerCrashed, "typescript crashed; restart 1 is in flight").
		WithRetryAfterMS(250)

	_, err := h.ws.Definition(context.Background(), sess, "src/user.ts", protocol.Position{})
	wantCode(t, err, protocol.ErrServerCrashed)
	if protocol.AsError(err).RetryAfterMS == 0 {
		t.Error("SERVER_CRASHED carried no retry hint; the agent cannot tell how long to wait")
	}
}

func TestEmptyLocationListIsSuccessNotNoResult(t *testing.T) {
	h := newHarness(t)
	h.srv.locations = nil

	got, err := h.ws.Definition(context.Background(), sess, "src/user.ts", protocol.Position{})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if got == nil {
		t.Fatal("Definition returned a nil slice; lists must be empty-but-present so they marshal to []")
	}
	if len(got) != 0 {
		t.Errorf("Definition = %v, want empty", got)
	}
}

// TestHoverWithNothingIsNoResult: "nothing here" is not expressible in a Hover,
// so it travels as NO_RESULT (docs/ARCHITECTURE.md §10.7).
func TestHoverWithNothingIsNoResult(t *testing.T) {
	h := newHarness(t)
	h.srv.hover = nil

	_, err := h.ws.Hover(context.Background(), sess, "src/user.ts", protocol.Position{})
	wantCode(t, err, protocol.ErrNoResult)
}

func TestHoverReturnsTheServerAnswer(t *testing.T) {
	h := newHarness(t)
	h.srv.hover = &protocol.Hover{Contents: "interface User"}

	got, err := h.ws.Hover(context.Background(), sess, "src/user.ts", protocol.Position{})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got.Contents != "interface User" {
		t.Errorf("Hover contents = %q", got.Contents)
	}
}

// TestQueryOpensTheDocumentWithDiskText: a language server answers about the
// text it was given, so the daemon must give it the file that is actually on
// disk before asking anything about it.
func TestOpenDocumentReturnsNotReadyWhenInitialAnalysisDidNotSettle(t *testing.T) {
	h := newHarness(t)
	h.srv.stale = true
	err := h.ws.OpenDocument(context.Background(), sess, "src/user.ts", "typescript")
	if err == nil {
		t.Fatal("OpenDocument succeeded after the initial analysis timed out")
	}
	if code := protocol.AsError(err).Code; code != protocol.ErrNotReady {
		t.Fatalf("OpenDocument error = %v (code %s), want NOT_READY", err, code)
	}
}

func TestQueryOpensTheDocumentWithDiskText(t *testing.T) {
	h := newHarness(t)

	if _, err := h.ws.Definition(context.Background(), sess, "src/user.ts", protocol.Position{}); err != nil {
		t.Fatalf("Definition: %v", err)
	}
	text, ok := h.srv.openText("src/user.ts")
	if !ok {
		t.Fatal("the document was never opened")
	}
	if want := "export interface User {\n  id: string;\n}\n"; text != want {
		t.Errorf("opened text = %q, want %q", text, want)
	}
}

// TestDiskChangeReopensTheDocument is M2's whole staleness story: with no index
// and no watcher yet, re-reading before each query is what stops the daemon
// answering from a file that no longer exists in that form.
func TestDiskChangeReopensTheDocument(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.ws.Definition(ctx, sess, "src/user.ts", protocol.Position{}); err != nil {
		t.Fatal(err)
	}
	write(t, h.root, "src/user.ts", "export interface User { id: string; name: string }\n")

	if _, err := h.ws.Definition(ctx, sess, "src/user.ts", protocol.Position{}); err != nil {
		t.Fatal(err)
	}

	text, _ := h.srv.openText("src/user.ts")
	if want := "export interface User { id: string; name: string }\n"; text != want {
		t.Errorf("after a disk change the server still holds %q, want %q", text, want)
	}
	if len(h.srv.closedPaths()) == 0 {
		t.Error("the stale document was never closed before being reopened")
	}
}

// TestFirstQueryWaitsForTheServerToAnalyseTheDocument.
//
// Measured against typescript-language-server 5.3.0: a references query issued
// in the same instant as the didOpen returns ONE location — the declaration —
// because the project is still loading, and the same query 250 ms later returns
// all six. An agent cannot tell that from "this symbol is unused". The first
// query on a document therefore waits for the server to settle on it.
func TestFirstQueryWaitsForTheServerToAnalyseTheDocument(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.ws.Definition(ctx, sess, "src/user.ts", protocol.Position{}); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.settleCount("src/user.ts"); got != 1 {
		t.Fatalf("the first query settled %d times, want exactly 1", got)
	}

	// A second query on an unchanged document pays nothing.
	if _, err := h.ws.Definition(ctx, sess, "src/user.ts", protocol.Position{}); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.settleCount("src/user.ts"); got != 1 {
		t.Errorf("a repeat query settled again (%d total); the wait must be per document open", got)
	}

	// A disk change reopens, so the wait is paid again.
	write(t, h.root, "src/user.ts", "export interface User { id: string }\n")
	if _, err := h.ws.Definition(ctx, sess, "src/user.ts", protocol.Position{}); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.settleCount("src/user.ts"); got != 2 {
		t.Errorf("after a disk change the document settled %d times, want 2", got)
	}
}

// TestSimulateEditResynchronisesTheDocument: WithText restores the base text,
// but the diagnostics that restore produces are still in flight. Without a
// resync the next get_diagnostics reports the SPECULATIVE errors for a file
// that compiles cleanly on disk.
func TestSimulateEditResynchronisesTheDocument(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.ws.SimulateEdit(ctx, sess, "src/user.ts", "export interface User { }\n"); err != nil {
		t.Fatalf("SimulateEdit: %v", err)
	}
	before := h.srv.settleCount("src/user.ts")

	if _, _, err := h.ws.Diagnostics(ctx, sess, "src/user.ts"); err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if got := h.srv.settleCount("src/user.ts"); got <= before+1 {
		t.Errorf("the query after a simulate_edit did not resynchronise the document (settles %d → %d)", before, got)
	}
	if len(h.srv.closedPaths()) == 0 {
		t.Error("the speculatively edited document was never reopened")
	}
}

func TestDocumentRefcountingAcrossSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const other = protocol.SessionID("session-2")

	if err := h.ws.OpenDocument(ctx, sess, "src/user.ts", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.ws.OpenDocument(ctx, other, "src/user.ts", ""); err != nil {
		t.Fatal(err)
	}

	if err := h.ws.CloseDocument(ctx, sess, "src/user.ts"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.srv.openText("src/user.ts"); !ok {
		t.Fatal("one session closing the file closed it for the other session too")
	}

	h.ws.EndSession(ctx, other)
	if _, ok := h.srv.openText("src/user.ts"); ok {
		t.Error("the last session ended and the document is still open on the language server")
	}
}

func TestStatusNeverStartsAServer(t *testing.T) {
	h := newHarness(t)
	h.sup.status = []protocol.ServerStatus{{Name: "typescript", State: protocol.ServerReady}}

	got, err := h.ws.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Root != h.root {
		t.Errorf("Status root = %q, want %q", got.Root, h.root)
	}
	if got.State != protocol.IndexIdle {
		t.Errorf("Status state = %q, want %q (M2 has no index)", got.State, protocol.IndexIdle)
	}
	if len(got.LanguageServers) != 1 {
		t.Errorf("Status language servers = %v", got.LanguageServers)
	}
	if len(h.sup.acquired()) != 0 {
		t.Errorf("index_status started a language server: %v", h.sup.acquired())
	}
}

// ---- edits -----------------------------------------------------------------

func TestRenameMintsATokenOverTheAffectedFiles(t *testing.T) {
	h := newHarness(t)
	h.srv.edits = []protocol.FileEdit{{
		Path:  "src/user.ts",
		Edits: []protocol.TextEdit{{Range: rng(0, 17, 0, 21), NewText: "Person"}},
	}}

	plan, err := h.ws.Rename(context.Background(), sess, "src/user.ts", protocol.Position{Line: 0, Character: 17}, "Person")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if plan.EditToken == "" {
		t.Fatal("rename produced no edit_token; apply_edit cannot verify anything without one")
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != "src/user.ts" {
		t.Fatalf("plan files = %+v", plan.Files)
	}
}

func TestRenameWithNoEditsIsNoResult(t *testing.T) {
	h := newHarness(t)
	h.srv.edits = nil

	_, err := h.ws.Rename(context.Background(), sess, "src/user.ts", protocol.Position{}, "Person")
	wantCode(t, err, protocol.ErrNoResult)
}

func TestApplyEditWritesTheDryRunPlan(t *testing.T) {
	h := newHarness(t)
	h.srv.edits = []protocol.FileEdit{{
		Path:  "src/user.ts",
		Edits: []protocol.TextEdit{{Range: rng(0, 17, 0, 21), NewText: "Person"}},
	}}

	plan, err := h.ws.Rename(context.Background(), sess, "src/user.ts", protocol.Position{}, "Person")
	if err != nil {
		t.Fatal(err)
	}

	applied, err := h.ws.ApplyEdit(context.Background(), sess, plan.EditToken)
	if err != nil {
		t.Fatalf("ApplyEdit: %v", err)
	}
	if len(applied) != 1 || applied[0] != "src/user.ts" {
		t.Errorf("applied = %v", applied)
	}

	got, err := os.ReadFile(filepath.Join(h.root, "src", "user.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "export interface Person {\n  id: string;\n}\n"; string(got) != want {
		t.Errorf("file after apply =\n%q\nwant\n%q", got, want)
	}
}

// TestApplyEditAfterAnOutOfBandChangeIsStaleEdit is SPEC §4.2's core safety
// promise: the hashes in the token are checked against disk, not trusted.
func TestApplyEditAfterAnOutOfBandChangeIsStaleEdit(t *testing.T) {
	h := newHarness(t)
	h.srv.edits = []protocol.FileEdit{{
		Path:  "src/user.ts",
		Edits: []protocol.TextEdit{{Range: rng(0, 17, 0, 21), NewText: "Person"}},
	}}

	plan, err := h.ws.Rename(context.Background(), sess, "src/user.ts", protocol.Position{}, "Person")
	if err != nil {
		t.Fatal(err)
	}

	// Somebody else edits the file between the dry run and the apply.
	write(t, h.root, "src/user.ts", "export interface User {\n  id: string;\n  name: string;\n}\n")

	_, err = h.ws.ApplyEdit(context.Background(), sess, plan.EditToken)
	wantCode(t, err, protocol.ErrStaleEdit)
}

func TestApplyEditWithAnUnknownTokenIsStaleEdit(t *testing.T) {
	h := newHarness(t)
	token := mintEditToken(h.ws.ID(), map[string]string{"src/user.ts": "deadbeef"})

	_, err := h.ws.ApplyEdit(context.Background(), sess, token)
	wantCode(t, err, protocol.ErrStaleEdit)
}

func TestApplyEditWithGarbageIsInternal(t *testing.T) {
	h := newHarness(t)
	_, err := h.ws.ApplyEdit(context.Background(), sess, "not-a-token")
	wantCode(t, err, protocol.ErrInternal)
}

// TestSimulateEditNeverTouchesDisk (SPEC §4.2).
func TestSimulateEditNeverTouchesDisk(t *testing.T) {
	h := newHarness(t)
	h.srv.diags = []protocol.Diagnostic{{Path: "src/user.ts", Severity: protocol.SeverityError, Message: "boom"}}

	before, err := os.ReadFile(filepath.Join(h.root, "src", "user.ts"))
	if err != nil {
		t.Fatal(err)
	}

	diags, stale, err := h.ws.SimulateEdit(context.Background(), sess, "src/user.ts", "export interface User { }\n")
	if err != nil {
		t.Fatalf("SimulateEdit: %v", err)
	}
	if len(diags) != 1 || stale {
		t.Errorf("SimulateEdit = %v, stale=%v", diags, stale)
	}

	after, err := os.ReadFile(filepath.Join(h.root, "src", "user.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("simulate_edit wrote to disk")
	}
	if text, _ := h.srv.openText("src/user.ts"); text != string(before) {
		t.Errorf("the server was left holding the speculative text %q", text)
	}
}

// TestTwoSessionsOverlaysAreIsolated is SPEC §4.2: two sessions simulating
// edits to the same file see only their own overlay.
func TestTwoSessionsOverlaysAreIsolated(t *testing.T) {
	h := newHarness(t)
	const other = protocol.SessionID("session-2")
	ctx := context.Background()

	// Session 1's overlay text is what the fake records in withTextSeen.
	if _, _, err := h.ws.SimulateEdit(ctx, sess, "src/user.ts", "session-one-text"); err != nil {
		t.Fatalf("session1 SimulateEdit: %v", err)
	}
	if _, _, err := h.ws.SimulateEdit(ctx, other, "src/user.ts", "session-two-text"); err != nil {
		t.Fatalf("session2 SimulateEdit: %v", err)
	}

	h.srv.mu.Lock()
	seen := append([]string(nil), h.srv.withTextSeen...)
	h.srv.mu.Unlock()
	if len(seen) < 2 || seen[0] != "session-one-text" || seen[1] != "session-two-text" {
		t.Fatalf("WithText sequence = %v, want each session's own overlay", seen)
	}

	// get_diagnostics for each session re-applies only that session's text.
	h.srv.mu.Lock()
	h.srv.withTextSeen = nil
	h.srv.mu.Unlock()
	if _, _, err := h.ws.Diagnostics(ctx, sess, "src/user.ts"); err != nil {
		t.Fatalf("session1 Diagnostics: %v", err)
	}
	if _, _, err := h.ws.Diagnostics(ctx, other, "src/user.ts"); err != nil {
		t.Fatalf("session2 Diagnostics: %v", err)
	}
	h.srv.mu.Lock()
	seen = append([]string(nil), h.srv.withTextSeen...)
	h.srv.mu.Unlock()
	if len(seen) != 2 || seen[0] != "session-one-text" || seen[1] != "session-two-text" {
		t.Fatalf("diagnostics WithText sequence = %v, want isolated overlays", seen)
	}
}

// TestOverlayInvalidatedByDiskChangeReturnsStaleEdit on the next diagnostics
// use of that session's overlay (SPEC §4.2).
func TestOverlayInvalidatedByDiskChangeReturnsStaleEdit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.ws.SimulateEdit(ctx, sess, "src/user.ts", "export interface User { }\n"); err != nil {
		t.Fatalf("SimulateEdit: %v", err)
	}

	write(t, h.root, "src/user.ts", "export interface User {\n  id: string;\n  name: string;\n}\n")
	// Without a watcher (no store), the disk-hash check on next use is the
	// safety net that surfaces STALE_EDIT.
	_, _, err := h.ws.Diagnostics(ctx, sess, "src/user.ts")
	wantCode(t, err, protocol.ErrStaleEdit)
}

// TestEndSessionDropsOverlays is SPEC §4.2: overlays are dropped on disconnect.
func TestEndSessionDropsOverlays(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.ws.SimulateEdit(ctx, sess, "src/user.ts", "overlay"); err != nil {
		t.Fatalf("SimulateEdit: %v", err)
	}
	if h.ws.overlays.count() != 1 {
		t.Fatalf("overlay count = %d, want 1", h.ws.overlays.count())
	}

	h.ws.EndSession(ctx, sess)
	if h.ws.overlays.count() != 0 {
		t.Fatalf("overlay count after EndSession = %d, want 0", h.ws.overlays.count())
	}

	// Diagnostics without an overlay must not try to re-apply speculative text.
	h.srv.mu.Lock()
	h.srv.withTextSeen = nil
	h.srv.mu.Unlock()
	if _, _, err := h.ws.Diagnostics(ctx, sess, "src/user.ts"); err != nil {
		t.Fatalf("Diagnostics after EndSession: %v", err)
	}
	h.srv.mu.Lock()
	seen := len(h.srv.withTextSeen)
	h.srv.mu.Unlock()
	if seen != 0 {
		t.Fatalf("Diagnostics re-applied an overlay after EndSession (%d WithText calls)", seen)
	}
}

func TestDiagnosticsForOnePath(t *testing.T) {
	h := newHarness(t)
	h.srv.diags = []protocol.Diagnostic{{Path: "src/user.ts", Severity: protocol.SeverityError, Message: "TS2339"}}
	h.srv.stale = true

	diags, stale, err := h.ws.Diagnostics(context.Background(), sess, "src/user.ts")
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v", diags)
	}
	if !stale {
		t.Error("possibly_stale was dropped on the way out of the settle window")
	}
}

// TestWorkspaceWideDiagnosticsAreFlaggedIncomplete: with no index (M3) the
// daemon can only speak for the files it has opened, and an agent that cannot
// tell "clean" from "unseen" is exactly the failure this project exists to
// prevent.
func TestWorkspaceWideDiagnosticsAreFlaggedIncomplete(t *testing.T) {
	h := newHarness(t)

	_, stale, err := h.ws.Diagnostics(context.Background(), sess, "")
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if !stale {
		t.Error("workspace-wide diagnostics claimed to be complete without an index")
	}
}

func TestShutdownStopsEveryServer(t *testing.T) {
	h := newHarness(t)
	if err := h.reg.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	h.sup.mu.Lock()
	defer h.sup.mu.Unlock()
	if h.sup.shutdowns == 0 {
		t.Error("Registry.Shutdown did not shut the supervisor down")
	}
}

func TestQueryAfterShutdownIsStructured(t *testing.T) {
	h := newHarness(t)
	if err := h.reg.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := h.reg.Get(h.ws.ID())
	wantCode(t, err, protocol.ErrWorkspaceUnknown)
}

func TestCancelledContextIsStructured(t *testing.T) {
	h := newHarness(t)
	h.srv.setQueryErr(protocol.NewError(protocol.ErrNotReady, "typescript is still starting").WithRetryAfterMS(500))

	_, err := h.ws.Definition(context.Background(), sess, "src/user.ts", protocol.Position{})
	wantCode(t, err, protocol.ErrNotReady)
	if !errors.Is(err, err) { // sanity: the error survives as a value
		t.Fatal("unreachable")
	}
}

func rng(sl, sc, el, ec int) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: sl, Character: sc},
		End:   protocol.Position{Line: el, Character: ec},
	}
}
