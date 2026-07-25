package daemonctl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
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

	// lock is the workspace's liveness flock, which every real daemon holds for
	// its whole lifetime (docs/ARCHITECTURE.md §6.8). teardown is how long it
	// keeps that lock after agreeing to stand down: the drain ack is the START
	// of the §6.5 shutdown ordering, not the end of it, and a replacement
	// spawned in the meantime meets a workspace that is still occupied.
	lock     *heldLock
	teardown time.Duration

	drains     atomic.Int32
	handshakes atomic.Int32
	refuse     atomic.Bool // answer the handshake with "draining"
	standDown  sync.Once
	done       chan struct{}
}

func startFakeDaemon(t *testing.T, cfg *config.Config, socket, root string, version int) *fakeDaemon {
	t.Helper()
	lock := takeLivenessLock(t, cfg, root)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		lock.release()
		t.Fatalf("binding %s: %v", socket, err)
	}
	d := &fakeDaemon{
		t: t, listener: listener, socket: socket, root: root, version: version,
		lock: lock, done: make(chan struct{}),
	}
	go d.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
		<-d.done
		d.lock.release()
	})
	return d
}

// beginTeardown models docs §6.5: stop accepting now, let go of the liveness
// lock only once the (simulated) shutdown ordering has run.
//
// The socket file is deliberately NOT unlinked. A leftover socket never blocked
// a replacement — daemon.listenUnix removes one before binding, which is safe
// precisely because the LOCK is what says a daemon is alive.
func (d *fakeDaemon) beginTeardown() {
	d.standDown.Do(func() {
		_ = d.listener.Close()
		lock := d.lock
		wait := d.teardown
		go func() {
			time.Sleep(wait)
			lock.release()
		}()
	})
}

// heldLock is a flock this test process owns. Release is idempotent: a
// draining daemon lets go of its lock on a timer, and the test cleanup lets go
// of it again, and closing the same descriptor twice is a data race.
type heldLock struct {
	file *os.File
	once sync.Once
}

func (h *heldLock) release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		_ = syscall.Flock(int(h.file.Fd()), syscall.LOCK_UN)
		_ = h.file.Close()
	})
}

// takeLivenessLock holds the workspace's liveness flock the way daemon.Run
// does. internal/daemonctl may not import daemon/ (docs §2.3 rule 4), so the
// two syscalls are repeated here rather than shared.
func takeLivenessLock(t *testing.T, cfg *config.Config, root string) *heldLock {
	t.Helper()
	path := cfg.WorkspaceLivenessLockPath(root)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the liveness lock %s: %v", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatalf("locking %s: %v", path, err)
	}
	return &heldLock{file: file}
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
				// A daemon that is already draining refuses the handshake so
				// the client starts a fresh one (docs §6.7). The refusal is
				// answered on a connection that is still open, exactly as the
				// real daemon does — it stops accepting and only then works
				// through its teardown.
				_ = codec.WriteResponse(protocol.NewErrorResponse(req.ID,
					protocol.NewError(protocol.ErrNotReady, "daemon is draining; start a new one")))
				d.beginTeardown()
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
			d.beginTeardown()
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

// spawningRunner stands in for exec'ing `langer daemon <root>`. It does what
// daemon.Run does on the way up, in the same order: take the workspace's
// liveness lock — WAITING for a predecessor that is still standing down — then
// unlink any stale socket and bind. Everything the client does around it — the
// spawn lock, the re-dial under it, the readiness poll — is exercised for real.
//
// The wait is the load-bearing part. A runner that binds immediately can never
// reproduce the window every real replacement is spawned into, which is why the
// suite used to pass while `langer daemon <root>` was exiting 1 in exactly this
// situation.
type spawningRunner struct {
	t       *testing.T
	cfg     *config.Config
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
	if err := r.waitForLiveness(); err != nil {
		return nil, err
	}
	// daemon.listenUnix: a socket file present now is stale by definition,
	// because no live daemon for this root can exist while the lock is free.
	_ = os.Remove(r.socket)
	startFakeDaemon(r.t, r.cfg, r.socket, r.root, r.version)
	return exitedProcess{}, nil
}

// waitForLiveness is daemon.acquireLiveness's bounded wait, repeated here
// because internal/daemonctl may not import daemon/ (docs §2.3 rule 4).
//
// The lock is released again immediately: startFakeDaemon takes it for real a
// moment later. What is being modelled here is the WAIT — the thing a
// replacement daemon must do while its predecessor finishes standing down —
// not the ownership handoff.
func (r *spawningRunner) waitForLiveness() error {
	path := r.cfg.WorkspaceLivenessLockPath(r.root)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	held := &heldLock{file: file}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			held.release()
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			_ = file.Close()
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	daemon := startFakeDaemon(t, cfg, socket, root, protocol.Version)

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
	runner := &spawningRunner{t: t, cfg: cfg, socket: socket, root: root, version: protocol.Version}

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
		t: t, cfg: cfg, socket: socket, root: root, version: protocol.Version,
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
	stale := startFakeDaemon(t, cfg, socket, root, protocol.Version+1)
	runner := &spawningRunner{t: t, cfg: cfg, socket: socket, root: root, version: protocol.Version}

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
	draining := startFakeDaemon(t, cfg, socket, root, protocol.Version)
	draining.refuse.Store(true)

	runner := &spawningRunner{t: t, cfg: cfg, socket: socket, root: root, version: protocol.Version}

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

// TestConnectReplacesADaemonThatIsSlowToLetGo is the window every real
// replacement is spawned into.
//
// A drain ack — or a refused handshake — means the old daemon has STARTED
// standing down. Its liveness lock, which is what "a daemon is running for this
// workspace" actually means (docs §6.8), drops only at the END of the §6.5
// ordering: after in-flight work finishes, after the answers land, after the
// language servers are torn down. On a real project that is seconds, not
// microseconds.
//
// Connect spawns exactly ONCE (docs §5.9 step 5) and then only re-dials, so the
// whole sequence hinges on the replacement WAITING for that lock instead of
// giving up on it. Every fake in this suite used to hold no lock at all, which
// is why the suite stayed green while `langer daemon <root>` was exiting 1 in
// exactly this situation and leaving the workspace with no daemon.
func TestConnectReplacesADaemonThatIsSlowToLetGo(t *testing.T) {
	cfg, root, socket := testEnv(t)

	draining := startFakeDaemon(t, cfg, socket, root, protocol.Version)
	// Comfortably longer than a "the fake vanishes instantly" test can notice,
	// and well inside what a real tsserver teardown costs.
	draining.teardown = 1500 * time.Millisecond
	draining.refuse.Store(true)

	runner := &spawningRunner{t: t, cfg: cfg, socket: socket, root: root, version: protocol.Version}

	start := time.Now()
	client, err := Connect(testContext(t), cfg, root, clock.New(), runner, Options{
		Session: "alice", Logger: testLogger(t), Executable: "/nonexistent/langer",
	})
	if err != nil {
		t.Fatalf("Connect gave up while the outgoing daemon was still standing down: %v", err)
	}
	defer client.Close()

	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("the replacement came up in %v; it cannot have waited for the "+
			"outgoing daemon's lock, so this test is not exercising the window", elapsed)
	}
	if runner.count() != 1 {
		t.Errorf("spawned %d replacements, want exactly 1", runner.count())
	}
	if _, err := client.IndexStatus(testContext(t), protocol.IndexStatusParams{Session: "alice", Workspace: "ws"}); err != nil {
		t.Errorf("the replacement daemon does not answer: %v", err)
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
