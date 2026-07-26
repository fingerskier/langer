package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/testutil"
	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

var supEpoch = time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

// fakeResolver stands in for the real SPEC §9 resolver in supervision tests,
// which are about crash handling rather than about which binary runs.
type fakeResolver struct {
	path string
	err  error
}

func (f fakeResolver) Resolve(command, _ string, _ bool) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.path != "" {
		return f.path, nil
	}
	return filepath.Join("/fake/bin", filepath.Base(command)), nil
}

// harness bundles everything a supervision test drives.
type harness struct {
	sup    Supervisor
	runner *fakeRunner
	clock  *clock.Fake
	root   string
}

func newHarness(t *testing.T, setup func(n int, s *scriptedServer), tune func(o *Options)) *harness {
	t.Helper()

	root := t.TempDir()
	f := clock.NewFake(supEpoch)
	runner := newFakeRunner(t, setup)

	opts := Options{
		Root: root,
		Servers: []config.LanguageServer{{
			Name:           "typescript",
			Command:        "typescript-language-server",
			Args:           []string{"--stdio"},
			FileExtensions: []string{".ts", ".tsx"},
		}},
		Resolver: fakeResolver{},
		Runner:   runner,
		Clock:    f,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tune != nil {
		tune(&opts)
	}

	sup, err := NewSupervisor(opts)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.Shutdown(ctx)
	})

	return &harness{sup: sup, runner: runner, clock: f, root: root}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func structuredCode(t *testing.T, err error) protocol.ErrorCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected a structured error, got nil")
	}
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *protocol.Error", err)
	}
	return pe.Code
}

// SPEC §3.3: servers start on demand, not at construction.
func TestSupervisorStartsNothingUntilAcquire(t *testing.T) {
	h := newHarness(t, nil, nil)
	if got := h.runner.startCount(); got != 0 {
		t.Fatalf("%d processes started before Acquire", got)
	}
	if got := h.sup.Status(); len(got) != 0 {
		t.Fatalf("Status = %+v, want empty before Acquire", got)
	}
}

func TestSupervisorAcquireStartsAndInitializes(t *testing.T) {
	h := newHarness(t, nil, nil)

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := h.runner.startCount(); got != 1 {
		t.Fatalf("%d processes started, want 1", got)
	}
	if !h.runner.server(1).sawMethod("initialize") {
		t.Fatal("the server never received initialize")
	}
	if !h.runner.server(1).sawMethod("initialized") {
		t.Fatal("the server never received the initialized notification")
	}
	if got := srv.Generation(); got != 1 {
		t.Fatalf("Generation = %d, want 1", got)
	}
	if !srv.Supports(CapDefinition) {
		t.Fatal("capabilities were not captured from the initialize result")
	}
}

// Only one instance of a given language server is kept per workspace
// (SPEC §3.3).
func TestSupervisorKeepsOneProcessPerLanguage(t *testing.T) {
	h := newHarness(t, nil, nil)
	for i := 0; i < 5; i++ {
		if _, err := h.sup.Acquire(testContext(t), "typescript"); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	if got := h.runner.startCount(); got != 1 {
		t.Fatalf("%d processes started for repeated Acquire, want 1", got)
	}
}

func TestSupervisorConcurrentAcquireStartsOneProcess(t *testing.T) {
	h := newHarness(t, nil, nil)

	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := h.sup.Acquire(testContext(t), "typescript")
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Acquire: %v", err)
		}
	}
	if got := h.runner.startCount(); got != 1 {
		t.Fatalf("%d processes started under concurrent Acquire, want 1", got)
	}
}

func TestSupervisorUnknownLanguageIsUnsupported(t *testing.T) {
	h := newHarness(t, nil, nil)
	_, err := h.sup.Acquire(testContext(t), "cobol")
	if code := structuredCode(t, err); code != protocol.ErrUnsupported {
		t.Fatalf("code = %s, want UNSUPPORTED", code)
	}
	if got := h.runner.startCount(); got != 0 {
		t.Fatalf("%d processes started for an unknown language", got)
	}
}

// A registry entry named something else still claims the language through its
// file extensions.
func TestSupervisorMatchesByFileExtension(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) {
		o.Servers = []config.LanguageServer{{
			Name:           "ts-ls",
			Command:        "typescript-language-server",
			FileExtensions: []string{".ts"},
		}}
	})
	if _, err := h.sup.Acquire(testContext(t), "typescript"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
}

// SPEC §8: a language server crash must not terminate the daemon. Here that
// means every method keeps answering with a structured error.
func TestSupervisorCrashIsStructuredNotFatal(t *testing.T) {
	h := newHarness(t, nil, nil)

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	h.runner.process(1).crash()

	waitFor(t, "the crash to be observed", func() bool {
		_, err := srv.Definition(testContext(t), "a.ts", protocol.Position{})
		return err != nil
	})

	ctx := testContext(t)
	checks := map[string]func() error{
		"Definition":       func() error { _, e := srv.Definition(ctx, "a.ts", protocol.Position{}); return e },
		"References":       func() error { _, e := srv.References(ctx, "a.ts", protocol.Position{}, true); return e },
		"Hover":            func() error { _, e := srv.Hover(ctx, "a.ts", protocol.Position{}); return e },
		"DocumentSymbols":  func() error { _, e := srv.DocumentSymbols(ctx, "a.ts"); return e },
		"WorkspaceSymbols": func() error { _, e := srv.WorkspaceSymbols(ctx, "q", 0); return e },
		"Rename":           func() error { _, e := srv.Rename(ctx, "a.ts", protocol.Position{}, "x"); return e },
		"Diagnostics":      func() error { _, _, e := srv.Diagnostics(ctx, "a.ts", 0); return e },
		"Open":             func() error { _, e := srv.Open(ctx, "a.ts", "typescript", "x"); return e },
	}
	for name, call := range checks {
		if code := structuredCode(t, call()); code != protocol.ErrServerCrashed {
			t.Errorf("%s after a crash returned %s, want SERVER_CRASHED", name, code)
		}
	}
}

// SPEC §3.6: a server whose symbol index is still building answers
// workspace/symbol with an EMPTY ARRAY, which is indistinguishable from "no
// such symbol" — the exact silent wrongness this bridge exists to prevent.
//
// pyright advertises workspaceSymbolProvider as {"workDoneProgress":true} and
// only fills its index when a pass ends; typescript-language-server advertises
// a bare `true` and is queryable at once. That advertised shape, not a guess
// about timing, is what decides whether an empty answer is trustworthy.
func TestSupervisorEmptyWorkspaceSymbolsBeforeIndexingIsNotReady(t *testing.T) {
	scripts := make(chan *scriptedServer, 1)
	h := newHarness(t, func(_ int, s *scriptedServer) {
		s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
			caps := defaultCapabilities()
			caps["workspaceSymbolProvider"] = map[string]any{"workDoneProgress": true}
			return map[string]any{"capabilities": caps}, nil
		})
		s.handle("workspace/symbol", func(json.RawMessage) (any, *wire.RPCError) {
			return []any{}, nil
		})
		scripts <- s
	}, nil)

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	_, err = srv.WorkspaceSymbols(testContext(t), "anything", 0)
	if code := structuredCode(t, err); code != protocol.ErrNotReady {
		t.Fatalf("empty result while indexing returned %s, want NOT_READY", code)
	}

	// Once a pass completes, an empty answer means what it says.
	(<-scripts).push("pyright/endProgress", []any{nil})
	waitFor(t, "the index to be reported ready", func() bool {
		_, err := srv.WorkspaceSymbols(testContext(t), "anything", 0)
		return err == nil
	})

	got, err := srv.WorkspaceSymbols(testContext(t), "anything", 0)
	if err != nil {
		t.Fatalf("WorkspaceSymbols after indexing: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d symbols, want an empty (but trustworthy) result", len(got))
	}
}

// A server that does not report progress is queryable immediately: an empty
// answer from typescript-language-server is a real answer, not a "wait".
func TestSupervisorEmptyWorkspaceSymbolsWithoutProgressIsAnAnswer(t *testing.T) {
	h := newHarness(t, func(_ int, s *scriptedServer) {
		s.handle("workspace/symbol", func(json.RawMessage) (any, *wire.RPCError) {
			return []any{}, nil
		})
	}, nil)

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	got, err := srv.WorkspaceSymbols(testContext(t), "anything", 0)
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d symbols, want 0", len(got))
	}
}

// SPEC §8 + §3.6: a crash that lands while a caller is already waiting inside
// the diagnostics settle window must fail that caller with SERVER_CRASHED.
//
// settle could only be woken by a push, by its budget timer, or by the caller's
// own context. A crash is none of those, so the caller was stranded until its
// deadline expired and then got a raw context error instead of a structured
// one. Under load that is what made TestSupervisorCrashIsStructuredNotFatal
// fail: whichever method the randomised map order reached while the crash was
// still propagating blocked for the full 15s test context.
func TestSupervisorDiagnosticsFailFastWhenTheServerCrashesMidSettle(t *testing.T) {
	h := newHarness(t, nil, nil)

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// An epoch no push can ever satisfy, so settle is guaranteed to still be
	// waiting when the crash lands.
	const unreachableEpoch = ^uint64(0)

	errs := make(chan error, 1)
	go func() {
		_, _, err := srv.Diagnostics(testContext(t), "a.ts", unreachableEpoch)
		errs <- err
	}()

	// Let the settle wait register its budget timer before crashing, so this
	// exercises the mid-settle path rather than the already-crashed path.
	h.clock.BlockUntil(1)
	h.runner.process(1).crash()

	select {
	case err := <-errs:
		if code := structuredCode(t, err); code != protocol.ErrServerCrashed {
			t.Errorf("Diagnostics interrupted by a crash returned %s, want SERVER_CRASHED", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a crash mid-settle stranded Diagnostics instead of failing it")
	}
}

// SPEC §3.3: restart on crash with exponential backoff.
func TestSupervisorRestartsAfterBackoff(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) {
		o.BackoffInitial = time.Second
		o.BackoffMax = time.Minute
	})

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	h.runner.process(1).crash()

	waitFor(t, "the supervisor to enter backoff", func() bool {
		for _, s := range h.sup.Status() {
			if s.State == protocol.ServerBackoff {
				return true
			}
		}
		return false
	})

	// During backoff, Acquire refuses fast and says when to come back.
	_, err = h.sup.Acquire(testContext(t), "typescript")
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.ErrServerCrashed {
		t.Fatalf("Acquire during backoff = %v, want SERVER_CRASHED", err)
	}
	if pe.RetryAfterMS <= 0 {
		t.Fatalf("retry_after_ms = %d, want the remaining backoff", pe.RetryAfterMS)
	}

	// Nothing restarts before the backoff elapses.
	h.clock.BlockUntil(1)
	if got := h.runner.startCount(); got != 1 {
		t.Fatalf("%d processes started before the backoff elapsed", got)
	}

	h.clock.Advance(time.Second)
	waitFor(t, "the restart", func() bool { return h.runner.startCount() == 2 })

	waitFor(t, "the new generation to be ready", func() bool { return srv.Generation() == 2 })
	if _, err := h.sup.Acquire(testContext(t), "typescript"); err != nil {
		t.Fatalf("Acquire after restart: %v", err)
	}
}

// Repeated crashes double the delay rather than hot-looping on a broken server.
func TestSupervisorBackoffIsExponential(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) {
		o.BackoffInitial = time.Second
		o.BackoffMax = 8 * time.Second
	})

	if _, err := h.sup.Acquire(testContext(t), "typescript"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		generation := i + 1
		waitFor(t, "generation to be running", func() bool { return h.runner.startCount() == generation })
		h.runner.process(generation).crash()

		waitFor(t, "backoff to be scheduled", func() bool {
			for _, s := range h.sup.Status() {
				if s.State == protocol.ServerBackoff {
					return true
				}
			}
			return false
		})
		h.clock.BlockUntil(1)

		// Just short of the delay: still nothing.
		h.clock.Advance(want - time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		if got := h.runner.startCount(); got != generation {
			t.Fatalf("restart %d fired after %v, want %v", i+1, want-time.Millisecond, want)
		}
		h.clock.Advance(time.Millisecond)
		waitFor(t, "restart", func() bool { return h.runner.startCount() == generation+1 })
	}
}

// A server that ran healthily for a long time and then died is not
// crash-looping, so it gets a fresh budget.
func TestSupervisorHealthyUptimeResetsTheBackoff(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) {
		o.BackoffInitial = time.Second
		o.BackoffMax = time.Minute
		o.HealthyResetAfter = 30 * time.Second
	})

	if _, err := h.sup.Acquire(testContext(t), "typescript"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// First crash: 1s.
	h.runner.process(1).crash()
	waitFor(t, "backoff 1", func() bool { return h.sup.Status()[0].State == protocol.ServerBackoff })
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	waitFor(t, "restart 1", func() bool { return h.runner.startCount() == 2 })
	waitFor(t, "ready 1", func() bool { return h.sup.Status()[0].State == protocol.ServerReady })

	// Then a long healthy run before the next crash.
	h.clock.Advance(30 * time.Second)
	h.runner.process(2).crash()
	waitFor(t, "backoff 2", func() bool { return h.sup.Status()[0].State == protocol.ServerBackoff })
	h.clock.BlockUntil(1)

	// The doubling was reset, so 1s is enough again.
	h.clock.Advance(time.Second)
	waitFor(t, "restart 2", func() bool { return h.runner.startCount() == 3 })
}

// After a restart the server has no memory of our open documents. Ours is the
// only copy left, and it must be replayed.
func TestSupervisorResyncsOpenDocumentsAfterRestart(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := srv.Open(testContext(t), "src/user.ts", "typescript", "export const x = 1;"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	waitFor(t, "the first didOpen", func() bool { return h.runner.server(1).sawMethod("textDocument/didOpen") })

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool { return h.sup.Status()[0].State == protocol.ServerBackoff })
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)

	waitFor(t, "restart", func() bool { return h.runner.startCount() == 2 })
	waitFor(t, "the document to be resynced", func() bool {
		return h.runner.server(2).sawMethod("textDocument/didOpen")
	})
}

func TestSupervisorRestartWaitsForDiskTextRestoreBeforeResync(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	const base = "export const value = 'base';"
	const disk = "export const value = 'disk';"
	if _, err := srv.Open(testContext(t), "src/user.ts", "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}
	waitFor(t, "the first didOpen", func() bool {
		return h.runner.server(1).sawMethod("textDocument/didOpen")
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	diskDone := make(chan error, 1)
	ctx := testContext(t)
	go func() {
		diskDone <- srv.WithDiskText(ctx, "src/user.ts", "typescript", disk,
			func(context.Context, uint64) error {
				close(entered)
				<-release
				return nil
			})
	}()
	<-entered

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	waitFor(t, "replacement process initialized", func() bool { return h.runner.startCount() == 2 })

	waitFor(t, "restart to wait on the document mutation gate", func() bool {
		_, _, waitingWriters, _ := mutationGateState(srv)
		return waitingWriters > 0
	})
	if h.runner.server(2).sawMethod("textDocument/didOpen") {
		t.Fatal("restart resynced the temporary disk view before WithDiskText restored it")
	}
	if got := h.sup.Status()[0].State; got != protocol.ServerStarting {
		t.Fatalf("server state while disk view is held = %s, want starting", got)
	}

	close(release)
	if err := <-diskDone; err != nil {
		t.Fatalf("WithDiskText: %v", err)
	}
	waitFor(t, "the restored document to be resynced", func() bool {
		return h.runner.server(2).sawMethod("textDocument/didOpen")
	})
	waitFor(t, "replacement server ready", func() bool {
		return h.sup.Status()[0].State == protocol.ServerReady
	})
	if got := srv.Generation(); got != 2 {
		t.Fatalf("generation after resync = %d, want 2", got)
	}

	replacement := h.runner.server(2)
	if got := notificationDocumentTexts(t, replacement, "textDocument/didOpen"); len(got) != 1 || got[0] != base {
		t.Fatalf("replacement didOpen texts = %q, want restored base [%q]", got, base)
	}
	if got := replacement.countMethod("textDocument/didChange"); got != 0 {
		t.Fatalf("replacement received %d didChange notifications before/after resync, want 0", got)
	}
}

func TestSupervisorRestartWaitsForSpeculativeTextRestoreBeforeResync(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	const base = "export const value = 'base';"
	const overlay = "export const value = 'overlay';"
	if _, err := srv.Open(testContext(t), "src/user.ts", "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}
	waitFor(t, "the first didOpen", func() bool {
		return h.runner.server(1).sawMethod("textDocument/didOpen")
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	editDone := make(chan error, 1)
	ctx := testContext(t)
	go func() {
		editDone <- srv.WithText(ctx, "src/user.ts", overlay,
			func(context.Context, uint64) error {
				close(entered)
				<-release
				return nil
			})
	}()
	<-entered

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	waitFor(t, "replacement process initialized", func() bool { return h.runner.startCount() == 2 })

	waitFor(t, "restart to wait on the document mutation gate", func() bool {
		_, _, waitingWriters, _ := mutationGateState(srv)
		return waitingWriters > 0
	})
	if h.runner.server(2).sawMethod("textDocument/didOpen") {
		t.Fatal("restart resynced speculative text before WithText restored it")
	}
	if got := h.sup.Status()[0].State; got != protocol.ServerStarting {
		t.Fatalf("server state while speculative text is held = %s, want starting", got)
	}

	close(release)
	if err := <-editDone; err != nil {
		t.Fatalf("WithText: %v", err)
	}
	waitFor(t, "the restored document to be resynced", func() bool {
		return h.runner.server(2).sawMethod("textDocument/didOpen")
	})
	waitFor(t, "replacement server ready", func() bool {
		return h.sup.Status()[0].State == protocol.ServerReady
	})

	replacement := h.runner.server(2)
	if got := notificationDocumentTexts(t, replacement, "textDocument/didOpen"); len(got) != 1 || got[0] != base {
		t.Fatalf("replacement didOpen texts = %q, want restored base [%q]", got, base)
	}
	text, _, _, open := h.docsFor(srv).state("src/user.ts")
	if !open || text != base {
		t.Fatalf("local state after restart = (%q, open=%v), want base %q", text, open, base)
	}
}

func TestSupervisorCloseWaitsThroughResyncPublication(t *testing.T) {
	releaseSecondOpen := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseSecondOpen)
		}
	}()
	pauseConfigured := make(chan (<-chan struct{}), 1)
	h := newHarness(t, func(n int, s *scriptedServer) {
		if n == 2 {
			pauseConfigured <- s.pauseAfter("textDocument/didOpen", releaseSecondOpen)
		}
	}, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, path := range []string{"src/a.ts", "src/b.ts"} {
		if _, err := srv.Open(testContext(t), path, "typescript", "export const value = 1;"); err != nil {
			t.Fatalf("Open(%s): %v", path, err)
		}
	}

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	reached := <-pauseConfigured
	<-reached

	replacement := h.runner.server(2)
	opened := notificationDocumentPaths(t, h.root, replacement, "textDocument/didOpen")
	if len(opened) != 1 {
		t.Fatalf("replacement had replayed %d documents at pause, want 1: %v", len(opened), opened)
	}
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- srv.Close(testContext(t), opened[0])
	}()

	waitFor(t, "Close to wait behind resync publication", func() bool {
		_, waitingReaders, _, writer := mutationGateState(srv)
		return writer && waitingReaders > 0
	})
	select {
	case closeErr := <-closeDone:
		t.Fatalf("Close completed after its path replay but before replacement publication: %v", closeErr)
	default:
	}

	close(releaseSecondOpen)
	released = true
	waitFor(t, "replacement server ready", func() bool {
		return h.sup.Status()[0].State == protocol.ServerReady
	})
	var closeErr error
	select {
	case closeErr = <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close remained blocked after resync publication")
	}
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	waitFor(t, "didClose on the published replacement", func() bool {
		return replacement.countMethod("textDocument/didClose") == 1
	})
	if got := notificationDocumentPaths(t, h.root, replacement, "textDocument/didClose"); len(got) != 1 || got[0] != opened[0] {
		t.Fatalf("replacement didClose paths = %v, want [%s]", got, opened[0])
	}
}

func TestSupervisorProgressCompletedDuringResyncSurvivesPublication(t *testing.T) {
	releaseSecondOpen := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseSecondOpen)
		}
	}()
	pauseConfigured := make(chan (<-chan struct{}), 1)
	h := newHarness(t, func(n int, s *scriptedServer) {
		s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
			caps := defaultCapabilities()
			caps["workspaceSymbolProvider"] = map[string]any{"workDoneProgress": true}
			return map[string]any{"capabilities": caps}, nil
		})
		s.handle("workspace/symbol", func(json.RawMessage) (any, *wire.RPCError) {
			return []any{}, nil
		})
		if n == 2 {
			pauseConfigured <- s.pauseAfter("textDocument/didOpen", releaseSecondOpen)
		}
	}, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, path := range []string{"src/a.ts", "src/b.ts"} {
		if _, err := srv.Open(testContext(t), path, "typescript", "export const value = 1;"); err != nil {
			t.Fatalf("Open(%s): %v", path, err)
		}
	}

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	reached := <-pauseConfigured
	<-reached

	h.runner.server(2).push("pyright/endProgress", []any{nil})
	waitFor(t, "replacement analysis completion", func() bool {
		analysis := candidateAnalysisState(h)
		return analysis != nil && analysis.indexed.Load()
	})

	close(releaseSecondOpen)
	released = true
	waitFor(t, "replacement server ready", func() bool {
		return h.sup.Status()[0].State == protocol.ServerReady
	})
	if _, err := srv.WorkspaceSymbols(testContext(t), "absent", 10); err != nil {
		t.Fatalf("completed replacement analysis was forgotten at publication: %v", err)
	}
}

func TestSupervisorDropsInitializeDiagnosticsBeforeReplay(t *testing.T) {
	const path = "src/user.ts"
	var h *harness
	h = newHarness(t, func(n int, s *scriptedServer) {
		if n != 2 {
			return
		}
		s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
			s.publishDiagnostics(uriIn(h.root, path), []any{map[string]any{
				"range": map[string]any{
					"start": map[string]any{"line": 0, "character": 0},
					"end":   map[string]any{"line": 0, "character": 1},
				},
				"message": "stale disk diagnostic",
			}})
			return map[string]any{"capabilities": defaultCapabilities()}, nil
		})
	}, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := srv.Open(testContext(t), path, "typescript", "export const value = 1;"); err != nil {
		t.Fatalf("Open: %v", err)
	}

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	waitFor(t, "replacement server ready", func() bool {
		return h.sup.Status()[0].State == protocol.ServerReady
	})

	impl := srv.(*server)
	if got, _, ok := impl.diags.snapshot(path); ok {
		t.Fatalf("initialize-time diagnostics survived authoritative replay: %+v", got)
	}

	h.runner.server(2).publishDiagnostics(uriIn(h.root, path), []any{map[string]any{
		"range": map[string]any{
			"start": map[string]any{"line": 0, "character": 0},
			"end":   map[string]any{"line": 0, "character": 1},
		},
		"message": "fresh replayed diagnostic",
	}})
	waitFor(t, "post-publication diagnostics", func() bool {
		got, _, ok := impl.diags.snapshot(path)
		return ok && len(got) == 1 && got[0].Message == "fresh replayed diagnostic"
	})
}

func TestSupervisorPreservesDiagnosticsPublishedDuringReplay(t *testing.T) {
	const path = "src/user.ts"
	var h *harness
	h = newHarness(t, func(n int, s *scriptedServer) {
		if n != 2 {
			return
		}
		s.handleNotification("textDocument/didOpen", func(json.RawMessage) {
			s.publishDiagnostics(uriIn(h.root, path), []any{map[string]any{
				"range": map[string]any{
					"start": map[string]any{"line": 0, "character": 0},
					"end":   map[string]any{"line": 0, "character": 1},
				},
				"message": "diagnostic caused by replay",
			}})
		})
	}, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := srv.Open(testContext(t), path, "typescript", "export const value = 1;"); err != nil {
		t.Fatalf("Open: %v", err)
	}

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	waitFor(t, "replacement server ready", func() bool {
		return h.sup.Status()[0].State == protocol.ServerReady
	})

	impl := srv.(*server)
	waitFor(t, "buffered replay diagnostics", func() bool {
		got, _, ok := impl.diags.snapshot(path)
		return ok && len(got) == 1 && got[0].Message == "diagnostic caused by replay"
	})
}

func TestSupervisorFailedCandidateProgressCannotBlessReplacement(t *testing.T) {
	failedProgressSent := make(chan struct{})
	h := newHarness(t, func(n int, s *scriptedServer) {
		s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
			if n == 2 {
				s.push("pyright/endProgress", []any{nil})
				close(failedProgressSent)
				return nil, &wire.RPCError{Code: -32603, Message: "candidate initialize failed"}
			}
			caps := defaultCapabilities()
			caps["workspaceSymbolProvider"] = map[string]any{"workDoneProgress": true}
			return map[string]any{"capabilities": caps}, nil
		})
		s.handle("workspace/symbol", func(json.RawMessage) (any, *wire.RPCError) {
			return []any{}, nil
		})
	}, func(o *Options) { o.BackoffInitial = time.Second })

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	select {
	case <-failedProgressSent:
	case <-time.After(5 * time.Second):
		t.Fatal("failed candidate never emitted progress")
	}
	waitFor(t, "failed candidate backoff", func() bool {
		return h.runner.startCount() == 2 &&
			h.sup.Status()[0].State == protocol.ServerBackoff
	})

	h.clock.BlockUntil(1)
	h.clock.Advance(2 * time.Second)
	waitFor(t, "replacement server ready", func() bool {
		return h.runner.startCount() == 3 &&
			h.sup.Status()[0].State == protocol.ServerReady
	})
	_, err = srv.WorkspaceSymbols(testContext(t), "absent", 10)
	if code := structuredCode(t, err); code != protocol.ErrNotReady {
		t.Fatalf("empty symbols after only failed-candidate progress returned %s, want NOT_READY", code)
	}
}

func TestSupervisorRestartWaitsForPriorStartAttemptToFinish(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) { o.BackoffInitial = time.Second })
	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	const (
		path    = "src/user.ts"
		base    = "export const value = 'base';"
		overlay = "export const value = 'overlay';"
	)
	if _, err := srv.Open(testContext(t), path, "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	editDone := make(chan error, 1)
	go func() {
		editDone <- srv.WithText(testContext(t), path, overlay, func(context.Context, uint64) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	h.runner.process(1).crash()
	waitFor(t, "first backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	waitFor(t, "second process waiting on mutation gate", func() bool {
		if h.runner.startCount() != 2 {
			return false
		}
		_, _, waitingWriters, _ := mutationGateState(srv)
		return waitingWriters > 0
	})

	acquireDone := make(chan error, 1)
	go func() {
		_, err := h.sup.Acquire(testContext(t), "typescript")
		acquireDone <- err
	}()

	h.runner.process(2).crash()
	waitFor(t, "second backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(2 * time.Second)
	if got := h.runner.startCount(); got != 2 {
		t.Fatalf("started generation 3 before generation 2 startAttempt returned: starts=%d", got)
	}

	close(release)
	released = true
	if err := <-editDone; err != nil {
		t.Fatalf("WithText: %v", err)
	}
	waitFor(t, "third process ready", func() bool {
		return h.runner.startCount() == 3 &&
			h.sup.Status()[0].State == protocol.ServerReady
	})
	select {
	case <-acquireDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire remained stranded on the prior startAttempt ready channel")
	}
}

func TestSupervisorDoesNotPublishCandidateClosedAfterReplay(t *testing.T) {
	const path = "src/user.ts"
	closedAtPublication := make(chan struct{})
	var h *harness
	h = newHarness(t, nil, func(o *Options) {
		o.BackoffInitial = time.Second
		o.beforePublish = func(generation uint64) {
			if generation != 2 {
				return
			}
			// Close at the exact post-replay/pre-publication boundary. A server
			// notification handler cannot establish this ordering: io.Pipe.Write
			// may return as soon as the frame is consumed, before that handler
			// runs, which made the former test race publication rather than test it.
			h.runner.process(2).crash()
			waitFor(t, "closed replacement backoff", func() bool {
				return h.sup.Status()[0].State == protocol.ServerBackoff
			})
			close(closedAtPublication)
		}
	})

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := srv.Open(testContext(t), path, "typescript", "export const value = 1;"); err != nil {
		t.Fatalf("Open: %v", err)
	}

	h.runner.process(1).crash()
	waitFor(t, "first backoff", func() bool {
		return h.sup.Status()[0].State == protocol.ServerBackoff
	})
	h.clock.BlockUntil(1)
	h.clock.Advance(time.Second)
	select {
	case <-closedAtPublication:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement never reached the publication boundary")
	}
	waitFor(t, "failed publication gate release", func() bool {
		_, _, _, writer := mutationGateState(srv)
		return !writer
	})
	if got := srv.Generation(); got != 1 {
		t.Fatalf("published closed candidate generation %d, want last live generation 1", got)
	}
	if _, err := srv.(*server).live(); structuredCode(t, err) != protocol.ErrServerCrashed {
		t.Fatalf("closed candidate remained live: %v", err)
	}
}

// index_status must never have the side effect of spawning a language server.
func TestSupervisorStatusNeverStartsAnything(t *testing.T) {
	h := newHarness(t, nil, nil)
	for i := 0; i < 3; i++ {
		h.sup.Status()
	}
	if got := h.runner.startCount(); got != 0 {
		t.Fatalf("Status started %d processes", got)
	}
}

func TestSupervisorStatusReportsState(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) { o.BackoffInitial = 5 * time.Second })

	if _, err := h.sup.Acquire(testContext(t), "typescript"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	status := h.sup.Status()
	if len(status) != 1 || status[0].Name != "typescript" || status[0].State != protocol.ServerReady {
		t.Fatalf("Status = %+v", status)
	}

	h.runner.process(1).crash()
	waitFor(t, "backoff", func() bool { return h.sup.Status()[0].State == protocol.ServerBackoff })

	status = h.sup.Status()
	if status[0].Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", status[0].Restarts)
	}
	if status[0].RetryAfterMS <= 0 || status[0].RetryAfterMS > 5000 {
		t.Fatalf("retry_after_ms = %d, want the remaining 5s backoff", status[0].RetryAfterMS)
	}
}

// A start that fails resolution is UNSUPPORTED, not a crash loop: no binary
// means no amount of retrying will help.
func TestSupervisorUnresolvableCommandIsUnsupported(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) {
		o.Resolver = fakeResolver{err: protocol.NewError(protocol.ErrUnsupported, "not found on PATH")}
	})
	_, err := h.sup.Acquire(testContext(t), "typescript")
	if code := structuredCode(t, err); code != protocol.ErrUnsupported {
		t.Fatalf("code = %s, want UNSUPPORTED", code)
	}
	if got := h.runner.startCount(); got != 0 {
		t.Fatalf("%d processes started despite an unresolvable command", got)
	}
}

// SPEC §9, enforced five milestones before M6's end-to-end tripwire: the real
// resolver refuses a binary inside the workspace tree.
func TestSupervisorRefusesWorkspaceLocalBinary(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "typescript-language-server")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	runner := newFakeRunner(t, nil)
	sup, err := NewSupervisor(Options{
		Root: root,
		Servers: []config.LanguageServer{{
			Name:           "typescript",
			Command:        exe,
			FileExtensions: []string{".ts"},
		}},
		Resolver: procx.NewResolver(), // the REAL one
		Runner:   runner,
		Clock:    clock.NewFake(supEpoch),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Shutdown(context.Background()) })

	_, acquireErr := sup.Acquire(testContext(t), "typescript")
	if acquireErr == nil {
		t.Fatal("the supervisor accepted a workspace-local language server binary")
	}
	if runner.startCount() != 0 {
		t.Fatal("the supervisor spawned a workspace-local binary")
	}
}

// Shutdown performs the SPEC §8 teardown and joins every goroutine it started.
func TestSupervisorShutdownIsClean(t *testing.T) {
	testutil.NoGoroutineLeaks(t)

	h := newHarness(t, nil, nil)
	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := srv.Open(testContext(t), "a.ts", "typescript", "x"); err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sup.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := h.sup.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}

	if _, err := h.sup.Acquire(testContext(t), "typescript"); err == nil {
		t.Fatal("Acquire succeeded after Shutdown")
	}
}

func TestSupervisorShutdownSendsShutdownThenExit(t *testing.T) {
	h := newHarness(t, nil, nil)
	if _, err := h.sup.Acquire(testContext(t), "typescript"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sup.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	srv := h.runner.server(1)
	if !srv.sawMethod("shutdown") {
		t.Error("the server never received the shutdown request")
	}
	if !srv.sawMethod("exit") {
		t.Error("the server never received the exit notification")
	}
}

// A crash while a request is in flight must fail it, not strand it.
func TestSupervisorCrashFailsInFlightRequest(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	h := newHarness(t, func(_ int, s *scriptedServer) {
		s.handle("textDocument/definition", func(json.RawMessage) (any, *wire.RPCError) {
			<-release
			return json.RawMessage(`[]`), nil
		})
	}, nil)

	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	errs := make(chan error, 1)
	go func() {
		_, err := srv.Definition(context.Background(), "a.ts", protocol.Position{Line: 1})
		errs <- err
	}()
	time.Sleep(100 * time.Millisecond)

	h.runner.process(1).crash()

	select {
	case err := <-errs:
		if code := structuredCode(t, err); code != protocol.ErrServerCrashed {
			t.Fatalf("in-flight request failed with %s, want SERVER_CRASHED", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an in-flight request was stranded by the crash")
	}
}
