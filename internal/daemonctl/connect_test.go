package daemonctl

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/protocol"
)

// ---- a fake daemon ---------------------------------------------------------
//
// internal/daemonctl may not import daemon/ (docs/ARCHITECTURE.md §2.3 rule 4
// keeps the process boundary honest), so its tests speak to a hand-rolled peer.
// The daemon package's own tests drive the real thing through this client.

type fakeDaemon struct {
	t        *testing.T
	listener net.Listener
	socket   string
	root     string
	version  int

	drains     atomic.Int32
	handshakes atomic.Int32
	refuse     atomic.Bool // answer the handshake with "draining"
	done       chan struct{}
}

func startFakeDaemon(t *testing.T, socket, root string, version int) *fakeDaemon {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("binding %s: %v", socket, err)
	}
	d := &fakeDaemon{t: t, listener: listener, socket: socket, root: root, version: version, done: make(chan struct{})}
	go d.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
		<-d.done
	})
	return d
}

func (d *fakeDaemon) serve() {
	defer close(d.done)
	var conns sync.WaitGroup
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			conns.Wait()
			return
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			d.handle(conn)
		}()
	}
}

func (d *fakeDaemon) handle(conn net.Conn) {
	defer conn.Close()
	codec := protocol.NewCodec(conn)
	for {
		req, err := codec.ReadRequest()
		if err != nil {
			return
		}
		switch req.Method {
		case protocol.MethodHandshake:
			d.handshakes.Add(1)
			if d.refuse.Load() {
				// A daemon that is already draining. It lets the socket go
				// BEFORE answering, exactly as a real sunset does — otherwise
				// the client's replacement would find the address still taken.
				_ = d.listener.Close()
				_ = os.Remove(d.socket)
				_ = codec.WriteResponse(protocol.NewErrorResponse(req.ID,
					protocol.NewError(protocol.ErrNotReady, "daemon is draining; start a new one")))
				return
			}
			resp, _ := protocol.NewResponse(req.ID, protocol.HandshakeResult{
				ProtocolVersion: d.version, Root: d.root, PID: os.Getpid(),
			})
			_ = codec.WriteResponse(resp)
		case protocol.MethodDrain:
			d.drains.Add(1)
			resp, _ := protocol.NewResponse(req.ID, protocol.DrainResult{Draining: true})
			_ = codec.WriteResponse(resp)
			_ = d.listener.Close()
			_ = os.Remove(d.socket)
			return
		case protocol.MethodIndexStatus:
			resp, _ := protocol.NewResponse(req.ID, protocol.IndexStatusResult{
				Root: d.root, State: protocol.IndexIdle,
			})
			_ = codec.WriteResponse(resp)
		default:
			_ = codec.WriteResponse(protocol.NewErrorResponse(req.ID,
				protocol.NewErrorf(protocol.ErrInternal, "fake daemon: unknown method %q", req.Method)))
		}
	}
}

// ---- fake runners ----------------------------------------------------------

// spawningRunner stands in for exec'ing `langer daemon <root>`: it binds the
// socket before returning, exactly as a daemon that has finished starting up
// would. Everything the client does around it — the spawn lock, the re-dial
// under it, the readiness poll — is exercised for real.
type spawningRunner struct {
	t       *testing.T
	socket  string
	root    string
	version int

	mu     sync.Mutex
	starts int
	delay  time.Duration
}

func (r *spawningRunner) Start(context.Context, procx.Spec) (procx.Process, error) {
	r.mu.Lock()
	r.starts++
	delay := r.delay
	r.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	startFakeDaemon(r.t, r.socket, r.root, r.version)
	return exitedProcess{}, nil
}

func (r *spawningRunner) Output(context.Context, procx.Spec) ([]byte, error) {
	return nil, protocol.NewError(protocol.ErrInternal, "unused")
}

func (r *spawningRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

type refusingRunner struct{ t *testing.T }

func (r refusingRunner) Start(context.Context, procx.Spec) (procx.Process, error) {
	r.t.Error("Connect tried to spawn a daemon when one was already listening")
	return nil, protocol.NewError(protocol.ErrInternal, "must not spawn")
}

func (r refusingRunner) Output(context.Context, procx.Spec) ([]byte, error) {
	return nil, protocol.NewError(protocol.ErrInternal, "unused")
}

type exitedProcess struct{}

func (exitedProcess) Stdin() io.WriteCloser { return nopWriteCloser{} }
func (exitedProcess) Stdout() io.Reader     { return eofReader{} }
func (exitedProcess) Stderr() io.Reader     { return eofReader{} }
func (exitedProcess) Wait() error           { return nil }
func (exitedProcess) Kill() error           { return nil }
func (exitedProcess) PID() int              { return 4242 }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// ---- fixtures --------------------------------------------------------------

func testEnv(t *testing.T) (*config.Config, string, string) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "langer")
	if err != nil {
		t.Skipf("cannot create a short temporary directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	cfg := &config.Config{SocketPath: filepath.Join(dir, "daemon.sock"), LogLevel: "error"}
	if _, err := cfg.EnsureRuntimeDir(); err != nil {
		t.Fatal(err)
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socket, err := cfg.WorkspaceSocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, root, socket
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- tests -----------------------------------------------------------------

// TestConnectUsesARunningDaemon: the common case must not spawn anything.
func TestConnectUsesARunningDaemon(t *testing.T) {
	cfg, root, socket := testEnv(t)
	daemon := startFakeDaemon(t, socket, root, protocol.Version)

	client, err := Connect(testContext(t), cfg, root, clock.New(), refusingRunner{t}, Options{
		Session: "alice", Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if daemon.handshakes.Load() == 0 {
		t.Error("Connect did not handshake; it cannot have checked the protocol version")
	}
	if _, err := client.IndexStatus(testContext(t), protocol.IndexStatusParams{Session: "alice", Workspace: "ws"}); err != nil {
		t.Errorf("IndexStatus over the socket: %v", err)
	}
}

// TestConnectAutoStartsADaemon is SPEC §3.1's "spawn on first connect".
func TestConnectAutoStartsADaemon(t *testing.T) {
	cfg, root, socket := testEnv(t)
	runner := &spawningRunner{t: t, socket: socket, root: root, version: protocol.Version}

	client, err := Connect(testContext(t), cfg, root, clock.New(), runner, Options{
		Session: "alice", Logger: testLogger(t), Executable: "/nonexistent/langer",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if runner.count() != 1 {
		t.Errorf("Connect spawned %d daemons, want 1", runner.count())
	}
}

// TestConcurrentConnectsSpawnExactlyOneDaemon is PLAN M2's concurrent-start
// race: N callers racing from cold must yield ONE daemon, not N.
//
// The spawn lock alone is not enough — every waiter would spawn in turn. What
// makes this pass is the RE-DIAL under the lock (docs/ARCHITECTURE.md §5.9
// step 4).
func TestConcurrentConnectsSpawnExactlyOneDaemon(t *testing.T) {
	cfg, root, socket := testEnv(t)
	runner := &spawningRunner{
		t: t, socket: socket, root: root, version: protocol.Version,
		// A slow start widens the window every waiter would otherwise spawn in.
		delay: 40 * time.Millisecond,
	}

	const racers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, racers)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			client, err := Connect(testContext(t), cfg, root, clock.New(), runner, Options{
				Session:    protocol.SessionID("racer"),
				Logger:     testLogger(t),
				Executable: "/nonexistent/langer",
			})
			if err != nil {
				errs <- err
				return
			}
			t.Cleanup(func() { _ = client.Close() })
			errs <- nil
		}(i)
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("a racer failed to connect: %v", err)
		}
	}
	if got := runner.count(); got != 1 {
		t.Errorf("%d concurrent Connects spawned %d daemons, want exactly 1", racers, got)
	}
}

// TestVersionMismatchDrainsThenSpawns is SPEC §3.1: the old daemon is asked to
// drain, not killed mid-request, and a fresh one takes its place.
func TestVersionMismatchDrainsThenSpawns(t *testing.T) {
	cfg, root, socket := testEnv(t)
	stale := startFakeDaemon(t, socket, root, protocol.Version+1)
	runner := &spawningRunner{t: t, socket: socket, root: root, version: protocol.Version}

	client, err := Connect(testContext(t), cfg, root, clock.New(), runner, Options{
		Session: "alice", Logger: testLogger(t), Executable: "/nonexistent/langer",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if stale.drains.Load() == 0 {
		t.Error("the stale daemon was never asked to drain")
	}
	if runner.count() != 1 {
		t.Errorf("spawned %d replacements, want 1", runner.count())
	}
}

// TestConnectReplacesADrainingDaemon: a daemon on its way out refuses the
// handshake, and the client must start a fresh one rather than surfacing a
// transport error to the agent (docs/ARCHITECTURE.md §6.7).
func TestConnectReplacesADrainingDaemon(t *testing.T) {
	cfg, root, socket := testEnv(t)
	draining := startFakeDaemon(t, socket, root, protocol.Version)
	draining.refuse.Store(true)

	runner := &spawningRunner{t: t, socket: socket, root: root, version: protocol.Version}

	client, err := Connect(testContext(t), cfg, root, clock.New(), runner, Options{
		Session: "alice", Logger: testLogger(t), Executable: "/nonexistent/langer",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if runner.count() == 0 {
		t.Error("the client attached to a draining daemon instead of starting a fresh one")
	}
}

// TestConnectRejectsAnUnknownRoot.
func TestConnectRejectsAnUnknownRoot(t *testing.T) {
	cfg, root, _ := testEnv(t)

	_, err := Connect(testContext(t), cfg, filepath.Join(root, "nope"), clock.New(), refusingRunner{t}, Options{
		Session: "alice", Logger: testLogger(t),
	})
	if err == nil {
		t.Fatal("Connect accepted a root that does not exist")
	}
	if code := protocol.AsError(err).Code; code != protocol.ErrWorkspaceUnknown {
		t.Errorf("code = %s, want %s", code, protocol.ErrWorkspaceUnknown)
	}
}

// TestSpawnLockSerialisesSpawnAttempts.
func TestSpawnLockSerialisesSpawnAttempts(t *testing.T) {
	cfg, root, _ := testEnv(t)
	path := cfg.WorkspaceSpawnLockPath(root)

	first, err := acquireSpawnLock(context.Background(), path, clock.New())
	if err != nil {
		t.Fatalf("acquireSpawnLock: %v", err)
	}

	// A second attempt must not succeed while the first holds it. The wait is
	// cancellable, which is the point: a blocking flock could not be.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := acquireSpawnLock(ctx, path, clock.New()); err == nil {
		t.Fatal("two callers held the spawn lock at once")
	}

	first.release()

	second, err := acquireSpawnLock(context.Background(), path, clock.New())
	if err != nil {
		t.Fatalf("the spawn lock was not released: %v", err)
	}
	second.release()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("spawn lock mode = %o, want 600 (SPEC §9)", perm)
	}
}
