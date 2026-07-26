//go:build integration

package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/daemonctl"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/testutil"
	"github.com/fingerskier/langer/protocol"
)

// These tests drive a real daemon over a real typescript-language-server on the
// checked-in fixture (SPEC §11.1). They are build-tagged so the unit suite
// stays fast and hermetic:
//
//	go test -tags=integration ./daemon/
//
// Every expected value below is quoted from testdata/README.md, which was
// measured against the real server rather than guessed.

// recordingRunner wraps the production runner and keeps the process handles, so
// a test can kill a REAL language server's process group.
type recordingRunner struct {
	inner procx.Runner

	mu    sync.Mutex
	procs []procx.Process
}

func (r *recordingRunner) Start(ctx context.Context, spec procx.Spec) (procx.Process, error) {
	proc, err := r.inner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.procs = append(r.procs, proc)
	r.mu.Unlock()
	return proc, nil
}

func (r *recordingRunner) Output(ctx context.Context, spec procx.Spec) ([]byte, error) {
	return r.inner.Output(ctx, spec)
}

func (r *recordingRunner) started() []procx.Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]procx.Process(nil), r.procs...)
}

func (r *recordingRunner) waitForStart(n int) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.started()) >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// startRealDaemon runs a daemon over the real typescript-language-server on the
// TypeScript fixture.
func startRealDaemon(t *testing.T) (*harness, *recordingRunner) {
	t.Helper()

	root, err := filepath.EvalSymlinks(testutil.FixtureRoot(t, "ts-project"))
	if err != nil {
		t.Fatal(err)
	}
	h, runner, _ := startRealDaemonForRoot(t, root, "")
	return h, runner
}

func startRealDaemonForRoot(
	t *testing.T,
	root, databasePath string,
) (*harness, *recordingRunner, func()) {
	t.Helper()

	entry := testutil.RequireLanguageServer(t, "typescript")

	cfg := &config.Config{
		SocketPath:      filepath.Join(shortTempDir(t), "daemon.sock"),
		DatabasePath:    databasePath,
		LogLevel:        "error",
		LanguageServers: []config.LanguageServer{entry},
	}

	runner := &recordingRunner{inner: procx.NewRunner()}
	srv, err := NewServer(Options{
		Root:           root,
		Config:         cfg,
		Clock:          clock.New(),
		Logger:         testLogger(t),
		IdleTimeout:    time.Hour,
		DrainGrace:     time.Second,
		RequestTimeout: 60 * time.Second,
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	h := &harness{t: t, cfg: cfg, root: root, srv: srv, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		h.exitErr = srv.Run(ctx)
		close(h.done)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if _, ok := h.waitExit(60 * time.Second); !ok {
				t.Error("the daemon did not shut down")
			}
		})
	}
	t.Cleanup(stop)

	select {
	case <-srv.Ready():
	case <-h.done:
		t.Fatalf("the daemon exited before it was ready: %v", h.exitErr)
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never became ready")
	}
	return h, runner, stop
}

func realContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitForRealIndexReady(
	t *testing.T,
	client *daemonctl.Client,
	session protocol.SessionID,
	workspace protocol.WorkspaceID,
) protocol.IndexStatusResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var last protocol.IndexStatusResult
	for ctx.Err() == nil {
		status, err := client.IndexStatus(ctx, protocol.IndexStatusParams{
			Session: session, Workspace: workspace,
		})
		if err != nil {
			t.Fatalf("index_status: %v", err)
		}
		last = status
		switch status.State {
		case protocol.IndexReady:
			return status
		case protocol.IndexFailed:
			t.Fatalf("index failed: %+v", status.Error)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("index did not become ready: %+v", last)
	return protocol.IndexStatusResult{}
}

func hasNamedSymbol(symbols []protocol.Symbol, name string) bool {
	for _, symbol := range symbols {
		if symbol.Name == name {
			return true
		}
	}
	return false
}

func copyRealFixture(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(shortTempDir(t), name)
	if err := os.CopyFS(root, os.DirFS(testutil.FixtureRoot(t, name))); err != nil {
		t.Fatalf("copying %s fixture: %v", name, err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// TestRealIndexReflectsEditAndSurvivesDaemonRestart is PLAN M3's persistence
// acceptance test against the real TypeScript language server. The second
// daemon must answer both symbol APIs from SQLite without starting a language
// server at all.
func TestRealIndexReflectsEditAndSurvivesDaemonRestart(t *testing.T) {
	const probe = "restartCacheProbe"
	root := copyRealFixture(t, "ts-project")
	databasePath := filepath.Join(shortTempDir(t), "index.db")

	first, firstRunner, stopFirst := startRealDaemonForRoot(t, root, databasePath)
	firstClient, firstWorkspace := first.session("before-restart")
	initial := waitForRealIndexReady(t, firstClient, "before-restart", firstWorkspace)
	if initial.FilesTotal == 0 || initial.FilesIndexed != initial.FilesTotal {
		t.Fatalf("initial index status = %+v, want a complete non-empty index", initial)
	}
	if len(firstRunner.started()) == 0 {
		t.Fatal("initial indexing never started the real language server")
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	indexedAt := func(path string) (int64, error) {
		var at int64
		err := db.QueryRowContext(
			context.Background(),
			`SELECT last_indexed_unix_ms
			 FROM files
			 WHERE workspace_id = ? AND path = ?`,
			firstWorkspace,
			path,
		).Scan(&at)
		return at, err
	}
	beforeUserIndexedAt, err := indexedAt("src/user.ts")
	if err != nil {
		t.Fatal(err)
	}
	beforeUnchangedIndexedAt, err := indexedAt("src/unicode.ts")
	if err != nil {
		t.Fatal(err)
	}

	userPath := filepath.Join(root, "src", "user.ts")
	before, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(userPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := append(append([]byte(nil), before...), []byte("\nexport const "+probe+" = 1;\n")...)
	if err := os.WriteFile(userPath, edited, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var (
		lastStatus        protocol.IndexStatusResult
		lastUserIndexedAt int64
		stableSince       time.Time
	)
	for ctx.Err() == nil {
		status, statusErr := firstClient.IndexStatus(ctx, protocol.IndexStatusParams{
			Session: "before-restart", Workspace: firstWorkspace,
		})
		if statusErr != nil {
			t.Fatalf("index_status after edit: %v", statusErr)
		}
		lastStatus = status
		userIndexedAt, err := indexedAt("src/user.ts")
		if err != nil {
			t.Fatalf("reading edited index timestamp: %v", err)
		}
		lastUserIndexedAt = userIndexedAt
		unchangedIndexedAt, err := indexedAt("src/unicode.ts")
		if err != nil {
			t.Fatalf("reading unchanged index timestamp: %v", err)
		}
		if unchangedIndexedAt != beforeUnchangedIndexedAt {
			t.Fatalf("edit triggered a full re-index: src/unicode.ts timestamp changed %d -> %d",
				beforeUnchangedIndexedAt, unchangedIndexedAt)
		}

		// No semantic file query occurs before this condition. The timestamp
		// transition therefore proves the watcher, rather than query-driven
		// hash healing, observed and incrementally reindexed the edit.
		if status.State == protocol.IndexReady && userIndexedAt > beforeUserIndexedAt {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= 500*time.Millisecond {
				break
			}
		} else {
			stableSince = time.Time{}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("edited file never reached a stable incremental index: status=%+v timestamp=%d (before %d)",
			lastStatus, lastUserIndexedAt, beforeUserIndexedAt)
	}
	editedDocument, err := firstClient.DocumentSymbols(realContext(t), protocol.DocumentParams{
		Session: "before-restart", Workspace: firstWorkspace, Path: "src/user.ts",
	})
	if err != nil {
		t.Fatalf("document_symbols after watcher reindex: %v", err)
	}
	if !hasNamedSymbol(editedDocument.Symbols, probe) {
		t.Fatalf("incremental index missed edited symbol %q: %+v", probe, editedDocument.Symbols)
	}

	_ = firstClient.Close()
	stopFirst()

	second, secondRunner, _ := startRealDaemonForRoot(t, root, databasePath)
	secondClient, secondWorkspace := second.session("after-restart")
	if secondWorkspace != firstWorkspace {
		t.Fatalf("workspace id changed across restart: %q -> %q", firstWorkspace, secondWorkspace)
	}
	restarted := waitForRealIndexReady(t, secondClient, "after-restart", secondWorkspace)
	if restarted.FilesIndexed != restarted.FilesTotal || restarted.FilesTotal != initial.FilesTotal {
		t.Fatalf("restarted index status = %+v, initial = %+v", restarted, initial)
	}
	if got := len(secondRunner.started()); got != 0 {
		t.Fatalf("restart validation started %d language servers before a query, want 0", got)
	}

	document, err := secondClient.DocumentSymbols(realContext(t), protocol.DocumentParams{
		Session: "after-restart", Workspace: secondWorkspace, Path: "src/user.ts",
	})
	if err != nil {
		t.Fatalf("cached document_symbols after restart: %v", err)
	}
	if !hasNamedSymbol(document.Symbols, probe) {
		t.Fatalf("cached document symbols after restart missed %q: %+v", probe, document.Symbols)
	}
	workspaceSymbols, err := secondClient.WorkspaceSymbols(realContext(t), protocol.WorkspaceSymbolsParams{
		Session: "after-restart", Workspace: secondWorkspace, Query: probe, Limit: 10,
	})
	if err != nil {
		t.Fatalf("cached workspace_symbols after restart: %v", err)
	}
	if !hasNamedSymbol(workspaceSymbols.Symbols, probe) {
		t.Fatalf("cached workspace symbols after restart missed %q: %+v", probe, workspaceSymbols.Symbols)
	}
	if got := len(secondRunner.started()); got != 0 {
		t.Fatalf("cached restart queries started %d language servers, want 0", got)
	}
}

// TestRealDaemonAnswersTwoConcurrentClients is PLAN M2's first acceptance
// criterion against a real language server: two clients, one daemon, one
// tsserver, correct answers from both.
func TestRealDaemonAnswersTwoConcurrentClients(t *testing.T) {
	h, runner := startRealDaemon(t)
	ctx := realContext(t)

	alice, aliceWS := h.session("alice")
	bob, bobWS := h.session("bob")
	if aliceWS != bobWS {
		t.Fatalf("two clients got different workspace ids: %q and %q", aliceWS, bobWS)
	}

	type check struct {
		name string
		run  func(client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) error
	}

	checks := []check{
		{
			// testdata/README.md §1.3: the call site at src/service.ts [6,21]
			// resolves to the definition at src/user.ts [5,16]-[5,27].
			name: "get_definition",
			run: func(client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) error {
				out, err := client.GetDefinition(ctx, protocol.PositionParams{
					DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: "src/service.ts"},
					Position:       protocol.Position{Line: 6, Character: 21},
				})
				if err != nil {
					return err
				}
				if len(out.Locations) != 1 {
					t.Errorf("get_definition returned %d locations, want 1: %+v", len(out.Locations), out.Locations)
					return nil
				}
				got := out.Locations[0]
				if got.Path != "src/user.ts" {
					t.Errorf("definition path = %q, want src/user.ts", got.Path)
				}
				want := protocol.Range{
					Start: protocol.Position{Line: 5, Character: 16},
					End:   protocol.Position{Line: 5, Character: 27},
				}
				if got.Range != want {
					t.Errorf("definition range = %+v, want %+v", got.Range, want)
				}
				return nil
			},
		},
		{
			// testdata/README.md §1.3: six references with the declaration
			// included, and the lookalike in src/lookalike.ts is NOT one of
			// them — that is the whole point of a semantic bridge.
			name: "get_references",
			run: func(client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) error {
				out, err := client.GetReferences(ctx, protocol.PositionParams{
					DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: "src/user.ts"},
					Position:       protocol.Position{Line: 5, Character: 16},
				})
				if err != nil {
					return err
				}
				if len(out.Locations) != 6 {
					t.Errorf("get_references returned %d locations, want 6: %+v", len(out.Locations), out.Locations)
				}
				for _, loc := range out.Locations {
					if loc.Path == "src/lookalike.ts" {
						t.Errorf("get_references returned the textual lookalike at %s", loc.Path)
					}
				}
				return nil
			},
		},
		{
			name: "get_hover",
			run: func(client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) error {
				out, err := client.GetHover(ctx, protocol.PositionParams{
					DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: "src/user.ts"},
					Position:       protocol.Position{Line: 5, Character: 16},
				})
				if err != nil {
					return err
				}
				if out.Hover == nil || out.Hover.Contents == "" {
					t.Errorf("get_hover returned nothing: %+v", out)
				}
				return nil
			},
		},
		{
			name: "document_symbols",
			run: func(client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) error {
				out, err := client.DocumentSymbols(ctx, protocol.DocumentParams{
					Session: session, Workspace: ws, Path: "src/user.ts",
				})
				if err != nil {
					return err
				}
				var sawUser, sawGetUserById bool
				for _, s := range out.Symbols {
					switch s.Name {
					case "User":
						sawUser = true
					case "getUserById":
						sawGetUserById = true
					}
				}
				if !sawUser || !sawGetUserById {
					t.Errorf("document_symbols missed User (%v) or getUserById (%v): %+v", sawUser, sawGetUserById, out.Symbols)
				}
				return nil
			},
		},
		{
			// testdata/README.md §1.7: src/broken.ts holds the only deliberate
			// error in the fixture, and its code travels verbatim as "2339".
			name: "get_diagnostics",
			run: func(client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) error {
				out, err := client.GetDiagnostics(ctx, protocol.DiagnosticsParams{
					Session: session, Workspace: ws, Path: "src/broken.ts",
				})
				if err != nil {
					return err
				}
				if len(out.Diagnostics) == 0 {
					t.Errorf("get_diagnostics found no error in the deliberately broken file (possibly_stale=%v)", out.PossiblyStale)
					return nil
				}
				got := out.Diagnostics[0]
				if got.Path != "src/broken.ts" {
					t.Errorf("diagnostic path = %q, want src/broken.ts", got.Path)
				}
				if got.Severity != protocol.SeverityError {
					t.Errorf("severity = %q, want error", got.Severity)
				}
				if got.Code != "2339" {
					t.Errorf("code = %q, want %q (verbatim, no synthesised TS prefix)", got.Code, "2339")
				}
				return nil
			},
		},
	}

	var wg sync.WaitGroup
	for _, c := range checks {
		for _, peer := range []struct {
			client  *daemonctl.Client
			session protocol.SessionID
			ws      protocol.WorkspaceID
		}{{alice, "alice", aliceWS}, {bob, "bob", bobWS}} {
			wg.Add(1)
			go func(c check, client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) {
				defer wg.Done()
				if err := c.run(client, session, ws); err != nil {
					t.Errorf("%s for %s: %v", c.name, session, err)
				}
			}(c, peer.client, peer.session, peer.ws)
		}
	}
	wg.Wait()

	// SPEC §3.2's v0.1 topology: one language server per language per repo, no
	// matter how many clients attach.
	if got := len(runner.started()); got != 1 {
		t.Errorf("two clients started %d language servers, want 1", got)
	}
}

// TestKillingARealLanguageServerDoesNotKillTheDaemon is SPEC §8 against a real
// process: the whole tsserver process group is killed, and the daemon must keep
// answering — structurally — and recover on its own.
func TestKillingARealLanguageServerDoesNotKillTheDaemon(t *testing.T) {
	h, runner := startRealDaemon(t)
	ctx := realContext(t)

	client, ws := h.session("alice")

	if _, err := client.GetDefinition(ctx, protocol.PositionParams{
		DocumentParams: protocol.DocumentParams{
			Session: "alice", Workspace: ws, Path: "src/service.ts",
		},
		Position: protocol.Position{Line: 6, Character: 21},
	}); err != nil {
		t.Fatalf("the first query failed: %v", err)
	}
	if !runner.waitForStart(1) {
		t.Fatal("no language server was started")
	}

	// Kill the process GROUP: typescript-language-server is a node wrapper
	// whose children survive a plain kill of the parent.
	if err := runner.started()[0].Kill(); err != nil {
		t.Fatalf("killing the language server: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	var (
		sawStructured bool
		recovered     bool
		lastErr       error
	)
	for time.Now().Before(deadline) {
		_, err := client.GetDefinition(ctx, protocol.PositionParams{
			DocumentParams: protocol.DocumentParams{
				Session: "alice", Workspace: ws, Path: "src/service.ts",
			},
			Position: protocol.Position{Line: 6, Character: 21},
		})
		if err == nil {
			recovered = true
			break
		}
		lastErr = err
		switch code := protocol.AsError(err).Code; code {
		case protocol.ErrServerCrashed, protocol.ErrNotReady:
			sawStructured = true
		default:
			t.Fatalf("after the language server was killed the daemon answered %s: %v", code, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !sawStructured {
		t.Error("the daemon never reported the crash structurally; a caller could not tell what happened")
	}
	if !recovered {
		t.Fatalf("the daemon never recovered from a real language server crash; last error: %v", lastErr)
	}
	if got := len(runner.started()); got < 2 {
		t.Errorf("the language server was never restarted (%d starts)", got)
	}

	// The daemon itself is untouched: a brand new client can still attach.
	other, _ := h.session("bob")
	if _, err := other.IndexStatus(ctx, protocol.IndexStatusParams{Session: "bob", Workspace: ws}); err != nil {
		t.Errorf("the daemon stopped serving after a language server crash: %v", err)
	}
}

// TestRealDaemonSimulateEditNeverTouchesDisk is SPEC §4.2's speculative edit
// against a real type checker: the error it reports must be the one the edited
// text would produce, with nothing written.
func TestRealDaemonSimulateEditNeverTouchesDisk(t *testing.T) {
	h, _ := startRealDaemon(t)
	ctx := realContext(t)

	client, ws := h.session("alice")
	path := filepath.Join(h.root, "src", "user.ts")

	before, err := readFileBytes(path)
	if err != nil {
		t.Fatal(err)
	}

	// Reference a property that does not exist on User.
	broken := "export interface User {\n  id: string;\n  name: string;\n}\n\n" +
		"export function getUserById(id: string): User {\n" +
		"  return { id, name: \"user-\" + id.missingProp };\n}\n"

	out, err := client.SimulateEdit(ctx, protocol.SimulateEditParams{
		DocumentParams: protocol.DocumentParams{Session: "alice", Workspace: ws, Path: "src/user.ts"},
		NewText:        broken,
	})
	if err != nil {
		t.Fatalf("simulate_edit: %v", err)
	}
	if len(out.Diagnostics) == 0 {
		t.Errorf("simulate_edit found no error in text that does not compile (possibly_stale=%v)", out.PossiblyStale)
	}

	after, err := readFileBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("simulate_edit wrote to disk")
	}

	// And the real file still type-checks: the speculative text was restored.
	clean, err := client.GetDiagnostics(ctx, protocol.DiagnosticsParams{
		Session: "alice", Workspace: ws, Path: "src/user.ts",
	})
	if err != nil {
		t.Fatalf("get_diagnostics after simulate_edit: %v", err)
	}
	if len(clean.Diagnostics) != 0 {
		t.Errorf("the speculative text was left in place: %+v", clean.Diagnostics)
	}
}
