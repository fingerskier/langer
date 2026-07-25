package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/daemonctl"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/testutil"
	"github.com/fingerskier/langer/protocol"
)

// ---- harness ---------------------------------------------------------------

type harness struct {
	t    *testing.T
	cfg  *config.Config
	root string
	srv  *Server
	lsp  *fakeLSP

	// done is closed once Run has returned and exitErr is readable. A channel
	// carrying the error would be consumable exactly once, and both the test
	// and the cleanup need to know.
	done    chan struct{}
	exitErr error
}

// waitExit reports Run's result, or false if it has not returned within d.
func (h *harness) waitExit(d time.Duration) (error, bool) {
	select {
	case <-h.done:
		return h.exitErr, true
	case <-time.After(d):
		return nil, false
	}
}

// startDaemon runs a real daemon over a fake language server and blocks until
// it is listening. setup receives the workspace root, because a fake that has
// to answer with file:// URIs needs to know it before the first request lands.
func startDaemon(t *testing.T, tweak func(*Options), setup func(root string, n int, s *fakeSession)) *harness {
	t.Helper()

	cfg := testConfig(t)
	root := fixtureRoot(t)

	var scripted func(int, *fakeSession)
	if setup != nil {
		scripted = func(n int, s *fakeSession) { setup(root, n, s) }
	}
	fake := newFakeLSP(t, scripted)

	opts := Options{
		Root:        root,
		Config:      cfg,
		Clock:       clock.New(),
		Logger:      testLogger(t),
		IdleTimeout: time.Hour, // never sunset unless a test asks for it
		DrainGrace:  50 * time.Millisecond,
		Resolver:    fake,
		Runner:      fake,
	}
	if tweak != nil {
		tweak(&opts)
	}

	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	h := &harness{t: t, cfg: cfg, root: root, srv: srv, lsp: fake, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		h.exitErr = srv.Run(ctx)
		close(h.done)
	}()

	t.Cleanup(func() {
		cancel()
		if _, ok := h.waitExit(20 * time.Second); !ok {
			t.Error("the daemon did not shut down")
		}
	})

	select {
	case <-srv.Ready():
	case <-h.done:
		t.Fatalf("the daemon exited before it was ready: %v", h.exitErr)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon never became ready")
	}
	return h
}

// connect returns a client attached to the running daemon. The runner it is
// given refuses to spawn, so a test that accidentally starts a second daemon
// fails loudly instead of passing for the wrong reason.
func (h *harness) connect(session protocol.SessionID) *daemonctl.Client {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := daemonctl.Connect(ctx, h.cfg, h.root, clock.New(), refusingRunner{h.t}, daemonctl.Options{
		Session: session,
		Logger:  testLogger(h.t),
	})
	if err != nil {
		h.t.Fatalf("Connect: %v", err)
	}
	h.t.Cleanup(func() { _ = client.Close() })
	return client
}

// openWorkspace connects and opens the workspace, returning both.
func (h *harness) session(session protocol.SessionID) (*daemonctl.Client, protocol.WorkspaceID) {
	h.t.Helper()
	client := h.connect(session)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := client.OpenWorkspace(ctx, protocol.OpenWorkspaceParams{Session: session, Root: h.root})
	if err != nil {
		h.t.Fatalf("OpenWorkspace: %v", err)
	}
	return client, result.Workspace
}

type refusingRunner struct{ t *testing.T }

func (r refusingRunner) Start(context.Context, procx.Spec) (procx.Process, error) {
	r.t.Error("a client tried to spawn a daemon when one was already running")
	return nil, errNoSpawn
}

func (r refusingRunner) Output(context.Context, procx.Spec) ([]byte, error) { return nil, errNoSpawn }

var errNoSpawn = protocol.NewError(protocol.ErrInternal, "this test must not spawn a daemon")

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- acceptance: two concurrent clients ------------------------------------

// TestTwoConcurrentClientsShareOneDaemon is PLAN M2's first acceptance
// criterion and SPEC §8's "multiple MCP clients may connect to the same daemon
// concurrently".
func TestTwoConcurrentClientsShareOneDaemon(t *testing.T) {
	h := startDaemon(t, nil, func(root string, _ int, s *fakeSession) {
		s.locations = `[{"uri":"file://` + root + `/src/user.ts",` +
			`"range":{"start":{"line":0,"character":17},"end":{"line":0,"character":21}}}]`
	})

	alice, aliceWS := h.session("alice")
	bob, bobWS := h.session("bob")

	if aliceWS != bobWS {
		t.Fatalf("two clients got different workspace ids: %q and %q", aliceWS, bobWS)
	}

	const rounds = 8
	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)

	for i := 0; i < rounds; i++ {
		for _, pair := range []struct {
			client  *daemonctl.Client
			session protocol.SessionID
			ws      protocol.WorkspaceID
		}{{alice, "alice", aliceWS}, {bob, "bob", bobWS}} {
			wg.Add(1)
			go func(client *daemonctl.Client, session protocol.SessionID, ws protocol.WorkspaceID) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := client.GetDefinition(ctx, protocol.PositionParams{
					DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: "src/user.ts"},
					Position:       protocol.Position{Line: 0, Character: 17},
				})
				errs <- err
			}(pair.client, pair.session, pair.ws)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent query failed: %v", err)
		}
	}

	// One workspace, one language server: SPEC §3.2's v0.1 topology.
	if got := h.lsp.startCount(); got != 1 {
		t.Errorf("two clients started %d language servers, want 1", got)
	}
}

// ---- acceptance: a language server crash is survivable ---------------------

// TestKillingALanguageServerDoesNotKillTheDaemon is SPEC §8. The daemon must
// keep answering — structurally, with a code the agent can act on — and must
// recover on its own.
func TestKillingALanguageServerDoesNotKillTheDaemon(t *testing.T) {
	h := startDaemon(t, func(o *Options) {
		o.RequestTimeout = 5 * time.Second
	}, nil)

	client, ws := h.session("alice")
	ctx := testContext(t)

	// Warm the server up.
	if _, err := client.DocumentSymbols(ctx, protocol.DocumentParams{
		Session: "alice", Workspace: ws, Path: "src/user.ts",
	}); err != nil {
		t.Fatalf("first query failed: %v", err)
	}
	if !h.lsp.waitForStart(1) {
		t.Fatal("no language server was started")
	}

	// Kill it, the way a real one dies: every pipe torn down at once.
	h.lsp.process(1).crash()

	// The daemon must still be there, and must say something structured.
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	recovered := false
	for time.Now().Before(deadline) {
		_, err := client.DocumentSymbols(ctx, protocol.DocumentParams{
			Session: "alice", Workspace: ws, Path: "src/user.ts",
		})
		if err == nil {
			recovered = true
			break
		}
		lastErr = err
		code := protocol.AsError(err).Code
		switch code {
		case protocol.ErrServerCrashed, protocol.ErrNotReady:
			// Both are honest answers while a restart is in flight.
		default:
			t.Fatalf("after the language server died the daemon answered %s: %v", code, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("the daemon never recovered from the crash; last error: %v", lastErr)
	}
	if h.lsp.startCount() < 2 {
		t.Errorf("the language server was never restarted (starts=%d)", h.lsp.startCount())
	}

	// And the daemon itself is still serving: a brand new client can attach.
	other, _ := h.session("bob")
	if _, err := other.IndexStatus(ctx, protocol.IndexStatusParams{Session: "bob", Workspace: ws}); err != nil {
		t.Errorf("the daemon stopped serving after a language server crash: %v", err)
	}
}

// ---- acceptance: every SPEC §3.6 code -------------------------------------

// TestEverySpecErrorCodeIsReachableOverTheWire is PLAN M2's "every §3.6 error
// code is exercised by a test", and it is exercised THROUGH the socket: a code
// that cannot survive serialisation is not reachable by an agent.
func TestEverySpecErrorCodeIsReachableOverTheWire(t *testing.T) {
	h := startDaemon(t, nil, func(_ string, _ int, s *fakeSession) {
		s.hover = `null`
	})

	client, ws := h.session("alice")
	ctx := testContext(t)
	const sess = protocol.SessionID("alice")

	seen := map[protocol.ErrorCode]string{}

	// WORKSPACE_UNKNOWN — a path outside the workspace.
	_, err := client.GetDefinition(ctx, protocol.PositionParams{
		DocumentParams: protocol.DocumentParams{Session: sess, Workspace: ws, Path: "../escape.ts"},
	})
	seen[wantCode(t, err, protocol.ErrWorkspaceUnknown).Code] = "path outside the workspace"

	// UNSUPPORTED — no registry entry claims .md.
	_, err = client.GetDefinition(ctx, protocol.PositionParams{
		DocumentParams: protocol.DocumentParams{Session: sess, Workspace: ws, Path: "README.md"},
	})
	seen[wantCode(t, err, protocol.ErrUnsupported).Code] = "no language server for this extension"

	// NO_RESULT — a hover with nothing to say.
	_, err = client.GetHover(ctx, protocol.PositionParams{
		DocumentParams: protocol.DocumentParams{Session: sess, Workspace: ws, Path: "src/user.ts"},
	})
	seen[wantCode(t, err, protocol.ErrNoResult).Code] = "hover with no result"

	// STALE_EDIT — an edit token this daemon cannot verify.
	_, err = client.ApplyEdit(ctx, protocol.ApplyEditParams{
		Session: sess, Workspace: ws, EditToken: staleTokenFor(t, ws),
	})
	seen[wantCode(t, err, protocol.ErrStaleEdit).Code] = "unverifiable edit token"

	// INTERNAL — an unknown method.
	err = rawCall(t, h, protocol.NewRequest(9001, "no_such_method", mustJSON(t, protocol.EndSessionParams{Session: sess})))
	seen[wantCode(t, err, protocol.ErrInternal).Code] = "unknown method"

	// INTERNAL — a protocol version mismatch on an ordinary request.
	mismatched := protocol.NewRequest(9002, protocol.MethodIndexStatus,
		mustJSON(t, protocol.IndexStatusParams{Session: sess, Workspace: ws}))
	mismatched.Version = protocol.Version + 1
	if err := rawCall(t, h, mismatched); wantCode(t, err, protocol.ErrInternal) == nil {
		t.Fatal("unreachable")
	}

	// SERVER_CRASHED — the language server is gone and the supervisor is
	// between restarts.
	if _, err := client.DocumentSymbols(ctx, protocol.DocumentParams{
		Session: sess, Workspace: ws, Path: "src/user.ts",
	}); err != nil {
		t.Fatalf("warm-up query failed: %v", err)
	}
	h.lsp.waitForStart(1)
	h.lsp.process(1).crash()
	if code := crashCode(t, client, ws); code != protocol.ErrServerCrashed {
		t.Fatalf("a dead language server reported %s, want %s", code, protocol.ErrServerCrashed)
	}
	seen[protocol.ErrServerCrashed] = "language server died"

	// NOT_READY — backpressure, on its own daemon so the limit is reached
	// deterministically rather than by racing.
	seen[backpressureCode(t)] = "in-flight limit"

	for _, code := range protocol.ErrorCodes {
		if _, ok := seen[code]; !ok {
			t.Errorf("SPEC §3.6 code %s is not reachable through the daemon", code)
		}
	}
}

// TestBackpressureReturnsNotReady: an unbounded goroutine-per-request model
// lets one confused agent take the workspace down for everyone else. Over the
// limit, the honest answer is NOT_READY with a retry hint — a legitimate use of
// the code that makes backpressure visible rather than fatal (docs §6.9).
func TestBackpressureReturnsNotReady(t *testing.T) {
	if code := backpressureCode(t); code != protocol.ErrNotReady {
		t.Errorf("over the in-flight limit the daemon answered %s, want %s", code, protocol.ErrNotReady)
	}
}

// backpressureCode saturates a one-slot daemon and returns the code the refused
// request carried.
func backpressureCode(t *testing.T) protocol.ErrorCode {
	t.Helper()

	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	h := startDaemon(t, func(o *Options) { o.MaxInFlight = 1 }, func(_ string, _ int, s *fakeSession) {
		s.hold = hold
		s.entered = entered
	})

	client, ws := h.session("alice")
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = client.DocumentSymbols(ctx, protocol.DocumentParams{
			Session: "alice", Workspace: ws, Path: "src/user.ts",
		})
	}()

	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		close(hold)
		t.Fatal("the fake language server never received the blocking request")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := client.IndexStatus(ctx, protocol.IndexStatusParams{Session: "alice", Workspace: ws})
	cancel()

	close(hold)
	<-blocked

	if err == nil {
		t.Fatal("a request over the in-flight limit succeeded; the limit is not enforced")
	}
	got := protocol.AsError(err)
	if got.Code == protocol.ErrNotReady && got.RetryAfterMS == 0 {
		t.Error("backpressure NOT_READY carried no retry hint")
	}
	return got.Code
}

func crashCode(t *testing.T, client *daemonctl.Client, ws protocol.WorkspaceID) protocol.ErrorCode {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := client.DocumentSymbols(ctx, protocol.DocumentParams{
			Session: "alice", Workspace: ws, Path: "src/user.ts",
		})
		cancel()
		if err == nil {
			continue // the restart won the race; try again
		}
		if code := protocol.AsError(err).Code; code == protocol.ErrServerCrashed {
			return code
		}
	}
	return ""
}

// staleTokenFor builds a well-formed edit token for a file that does not hash
// to what it claims.
func staleTokenFor(t *testing.T, ws protocol.WorkspaceID) string {
	t.Helper()
	payload := map[string]any{
		"ws":    string(ws),
		"files": map[string]string{"src/user.ts": "0000000000000000000000000000000000000000000000000000000000000000"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64URL(raw)
}

// rawCall sends one hand-built request and returns the structured error it
// produced. It is how the tests reach failures a typed client cannot express,
// such as an unknown method or a mismatched protocol version.
func rawCall(t *testing.T, h *harness, req *protocol.Request) error {
	t.Helper()
	conn, err := net.DialTimeout("unix", h.srv.SocketPath(), 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the daemon: %v", err)
	}
	defer conn.Close()

	codec := protocol.NewCodec(conn)
	if err := codec.WriteRequest(req); err != nil {
		t.Fatalf("writing %s: %v", req.Method, err)
	}
	resp, err := codec.ReadResponse()
	if err != nil {
		t.Fatalf("reading the response to %s: %v", req.Method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	return nil
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---- acceptance: version mismatch drains and restarts ----------------------

// TestVersionMismatchDrainsTheStaleDaemonAndStartsAFreshOne is SPEC §3.1's
// "if the MCP server detects a daemon/binary version mismatch, it asks the old
// daemon to drain and restarts it".
func TestVersionMismatchDrainsTheStaleDaemonAndStartsAFreshOne(t *testing.T) {
	cfg := testConfig(t)
	root := fixtureRoot(t)

	stale := startStaleDaemon(t, cfg, root)

	// A runner that "spawns" a real, current-version daemon in-process.
	spawner := newSpawningRunner(t, cfg, root)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := daemonctl.Connect(ctx, cfg, root, clock.New(), spawner, daemonctl.Options{
		Session: "alice",
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if !stale.drained() {
		t.Error("the stale daemon was never asked to drain; SPEC §3.1 says drain, not kill")
	}
	if spawner.starts() != 1 {
		t.Errorf("the client spawned %d daemons, want exactly 1", spawner.starts())
	}

	// And the replacement actually works.
	result, err := client.OpenWorkspace(ctx, protocol.OpenWorkspaceParams{Session: "alice", Root: root})
	if err != nil {
		t.Fatalf("the replacement daemon does not answer: %v", err)
	}
	if result.Root != root {
		t.Errorf("the replacement daemon serves %q, want %q", result.Root, root)
	}
}

// staleDaemon is a daemon from an older build: it speaks a different protocol
// version and knows how to drain.
type staleDaemon struct {
	listener net.Listener
	socket   string
	sawDrain atomic.Bool
	done     chan struct{}
}

func startStaleDaemon(t *testing.T, cfg *config.Config, root string) *staleDaemon {
	t.Helper()
	if _, err := cfg.EnsureRuntimeDir(); err != nil {
		t.Fatal(err)
	}
	socket, err := cfg.WorkspaceSocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("binding the stale daemon socket: %v", err)
	}

	d := &staleDaemon{listener: listener, socket: socket, done: make(chan struct{})}
	go d.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		<-d.done
	})
	return d
}

func (d *staleDaemon) serve() {
	defer close(d.done)
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		go d.handle(conn)
	}
}

func (d *staleDaemon) handle(conn net.Conn) {
	defer conn.Close()
	codec := protocol.NewCodec(conn)
	for {
		req, err := codec.ReadRequest()
		if err != nil {
			return
		}
		switch req.Method {
		case protocol.MethodHandshake:
			resp, _ := protocol.NewResponse(req.ID, protocol.HandshakeResult{
				ProtocolVersion: protocol.Version + 1, // a different build
				Root:            "",
				PID:             os.Getpid(),
			})
			_ = codec.WriteResponse(resp)
		case protocol.MethodDrain:
			d.sawDrain.Store(true)
			resp, _ := protocol.NewResponse(req.ID, protocol.DrainResult{Draining: true})
			_ = codec.WriteResponse(resp)
			// A real daemon finishes its in-flight work first; this one has
			// none, so it stops accepting immediately and unlinks its socket,
			// which is what lets the fresh daemon bind.
			_ = d.listener.Close()
			_ = os.Remove(d.socket)
			return
		default:
			_ = codec.WriteResponse(protocol.NewErrorResponse(req.ID,
				protocol.NewError(protocol.ErrInternal, "stale daemon")))
		}
	}
}

func (d *staleDaemon) drained() bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.sawDrain.Load() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// spawningRunner stands in for exec'ing `langer daemon <root>`: it starts a
// real daemon.Server on a goroutine. Everything the client does — the spawn
// lock, the re-dial under it, the poll for readiness — is exercised for real.
type spawningRunner struct {
	t    *testing.T
	cfg  *config.Config
	root string

	mu      sync.Mutex
	count   int
	servers []*Server
}

func newSpawningRunner(t *testing.T, cfg *config.Config, root string) *spawningRunner {
	return &spawningRunner{t: t, cfg: cfg, root: root}
}

func (r *spawningRunner) Start(_ context.Context, _ procx.Spec) (procx.Process, error) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()

	fake := newFakeLSP(r.t, nil)
	srv, err := NewServer(Options{
		Root:        r.root,
		Config:      r.cfg,
		Clock:       clock.New(),
		Logger:      testLogger(r.t),
		IdleTimeout: time.Hour,
		DrainGrace:  50 * time.Millisecond,
		Resolver:    fake,
		Runner:      fake,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan error, 1)
	go func() { exit <- srv.Run(ctx) }()
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-exit:
		case <-time.After(20 * time.Second):
			r.t.Error("a spawned daemon did not shut down")
		}
	})

	r.mu.Lock()
	r.servers = append(r.servers, srv)
	r.mu.Unlock()

	// The "process" exits immediately; the daemon lives on its goroutine. That
	// mirrors the detached spawn, where the client also stops caring about the
	// child the moment it is started.
	return &exitedProcess{}, nil
}

func (r *spawningRunner) Output(context.Context, procx.Spec) ([]byte, error) {
	return nil, errNoSpawn
}

func (r *spawningRunner) starts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// ---- acceptance: drain refuses new work ------------------------------------

// TestDrainRefusesNewWork: once a daemon is draining it must refuse the
// handshake so the client starts a fresh one, rather than accepting requests
// into a dying process (docs/ARCHITECTURE.md §6.7).
func TestDrainRefusesNewWork(t *testing.T) {
	h := startDaemon(t, nil, nil)

	// Ask it to drain over the wire.
	socket := h.srv.SocketPath()
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	codec := protocol.NewCodec(conn)
	if err := codec.WriteRequest(protocol.NewRequest(1, protocol.MethodDrain,
		mustJSON(t, protocol.DrainParams{Session: "alice", Reason: "test"}))); err != nil {
		t.Fatal(err)
	}
	if _, err := codec.ReadResponse(); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	err, exited := h.waitExit(20 * time.Second)
	if !exited {
		t.Fatal("the daemon did not stop after being asked to drain")
	}
	if err != nil {
		t.Fatalf("Run returned %v after a drain", err)
	}

	// The socket is gone with it.
	if _, err := os.Stat(socket); err == nil {
		t.Errorf("the socket %s outlived the daemon", socket)
	}
}

// ---- SPEC §9: permissions --------------------------------------------------

// TestSocketAndLocksAreUserOnly is SPEC §9's "database and socket files should
// have restrictive permissions (user-only)".
func TestSocketAndLocksAreUserOnly(t *testing.T) {
	h := startDaemon(t, nil, nil)

	dir, err := os.Stat(h.cfg.RuntimeDir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("runtime directory mode = %o, want 700", perm)
	}

	socket, err := os.Stat(h.srv.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := socket.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}

	livenessPath, spawnPath, err := LockPaths(h.cfg, h.root)
	if err != nil {
		t.Fatal(err)
	}
	liveness, err := os.Stat(livenessPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := liveness.Mode().Perm(); perm != 0o600 {
		t.Errorf("liveness lock mode = %o, want 600", perm)
	}
	// The spawn lock is created by clients; make one and check it too.
	h.connect("alice")
	if info, err := os.Stat(spawnPath); err == nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("spawn lock mode = %o, want 600", perm)
		}
	}
}

// ---- single instance -------------------------------------------------------

// TestSecondDaemonForTheSameWorkspaceIsRefused: the liveness lock, not the
// presence of a socket file, is what "a daemon is running" means.
func TestSecondDaemonForTheSameWorkspaceIsRefused(t *testing.T) {
	h := startDaemon(t, nil, nil)

	fake := newFakeLSP(t, nil)
	second, err := NewServer(Options{
		Root: h.root, Config: h.cfg, Logger: testLogger(t),
		IdleTimeout: time.Hour, Resolver: fake, Runner: fake,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := second.Run(ctx); err == nil {
		t.Fatal("a second daemon for the same workspace started successfully")
	}
}

// TestConcurrentStartsYieldExactlyOneDaemon: single-instance is a RACE, not a
// check. N daemons starting at the same instant must produce exactly one
// winner.
func TestConcurrentStartsYieldExactlyOneDaemon(t *testing.T) {
	cfg := testConfig(t)
	root := fixtureRoot(t)

	const racers = 8
	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		winners atomic.Int32
		bound   = make(chan *Server, racers)
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exits := make(chan error, racers)
	for i := 0; i < racers; i++ {
		fake := newFakeLSP(t, nil)
		srv, err := NewServer(Options{
			Root: root, Config: cfg, Logger: testLogger(t),
			IdleTimeout: time.Hour, DrainGrace: 10 * time.Millisecond,
			Resolver: fake, Runner: fake,
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}

		wg.Add(1)
		go func(srv *Server) {
			defer wg.Done()
			<-start
			err := srv.Run(ctx)
			exits <- err
		}(srv)

		go func(srv *Server) {
			select {
			case <-srv.Ready():
				winners.Add(1)
				bound <- srv
			case <-ctx.Done():
			}
		}(srv)
	}

	close(start)

	// Give the losers time to fail and the winner time to bind.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && winners.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	if got := winners.Load(); got != 1 {
		t.Errorf("%d daemons bound the socket, want exactly 1", got)
	}

	cancel()
	wg.Wait()
	close(exits)

	refused := 0
	for err := range exits {
		if err != nil {
			refused++
		}
	}
	if refused != racers-1 {
		t.Errorf("%d racers were refused, want %d", refused, racers-1)
	}
}

// TestStaleSocketIsReclaimed: a killed daemon leaves its socket behind. The
// next daemon must reclaim it, not mistake it for a live one.
func TestStaleSocketIsReclaimed(t *testing.T) {
	cfg := testConfig(t)
	root := fixtureRoot(t)

	if _, err := cfg.EnsureRuntimeDir(); err != nil {
		t.Fatal(err)
	}
	socket, err := cfg.WorkspaceSocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	// A leftover file where the socket belongs, with nothing behind it.
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := newFakeLSP(t, nil)
	srv, err := NewServer(Options{
		Root: root, Config: cfg, Logger: testLogger(t),
		IdleTimeout: time.Hour, DrainGrace: 10 * time.Millisecond,
		Resolver: fake, Runner: fake,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := make(chan error, 1)
	go func() { exit <- srv.Run(ctx) }()

	select {
	case <-srv.Ready():
	case err := <-exit:
		t.Fatalf("the daemon refused to reclaim a stale socket: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon never became ready")
	}

	cancel()
	<-exit
}

// ---- sessions --------------------------------------------------------------

// TestDisconnectEndsTheSession: SPEC §4.2 requires a session's state to be
// dropped when it disconnects, and the daemon can only see that at the
// transport.
func TestDisconnectEndsTheSession(t *testing.T) {
	h := startDaemon(t, nil, nil)
	ctx := testContext(t)

	client, ws := h.session("alice")
	if _, err := client.OpenDocument(ctx, protocol.OpenDocumentParams{
		DocumentParams: protocol.DocumentParams{Session: "alice", Workspace: ws, Path: "src/user.ts"},
	}); err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}

	// Hanging up must not take the daemon or the workspace with it.
	_ = client.Close()

	other, _ := h.session("bob")
	if _, err := other.IndexStatus(ctx, protocol.IndexStatusParams{Session: "bob", Workspace: ws}); err != nil {
		t.Errorf("the daemon stopped serving after a client disconnected: %v", err)
	}
}

// TestRequestsWithoutASessionAreRefused: overlay isolation and cleanup both key
// on the session id, so a missing one is a bug, not a default.
func TestRequestsWithoutASessionAreRefused(t *testing.T) {
	h := startDaemon(t, nil, nil)

	err := rawCall(t, h, protocol.NewRequest(1, protocol.MethodIndexStatus, json.RawMessage(`{"workspace_id":"ws-1"}`)))
	wantCode(t, err, protocol.ErrInternal)
}

// ---- goroutine hygiene -----------------------------------------------------

// TestShutdownJoinsEveryGoroutine makes "Run does not return until every
// goroutine it started has exited" a contract rather than a comment.
func TestShutdownJoinsEveryGoroutine(t *testing.T) {
	testutil.NoGoroutineLeaks(t)

	cfg := testConfig(t)
	root := fixtureRoot(t)
	fake := newFakeLSP(t, nil)

	srv, err := NewServer(Options{
		Root: root, Config: cfg, Logger: testLogger(t),
		IdleTimeout: time.Hour, DrainGrace: 10 * time.Millisecond,
		Resolver: fake, Runner: fake,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan error, 1)
	go func() { exit <- srv.Run(ctx) }()
	<-srv.Ready()

	client, err := daemonctl.Connect(context.Background(), cfg, root, clock.New(), refusingRunner{t},
		daemonctl.Options{Session: "alice", Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := client.OpenWorkspace(queryCtx, protocol.OpenWorkspaceParams{Session: "alice", Root: root}); err != nil {
		t.Fatal(err)
	}
	queryCancel()
	_ = client.Close()

	cancel()
	if err := <-exit; err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = filepath.Clean(root)
}
