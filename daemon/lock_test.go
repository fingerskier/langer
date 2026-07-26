package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/protocol"
)

// TestLivenessLockIsExclusive: the lock, not the presence of a socket file, is
// what "a daemon is running for this workspace" means.
func TestLivenessLockIsExclusive(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "daemon.lock")

	first, err := acquireLiveness(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("acquireLiveness: %v", err)
	}

	if _, err := acquireLiveness(context.Background(), path, 20*time.Millisecond); err == nil {
		t.Fatal("a second holder acquired the same liveness lock")
	} else if code := protocol.AsError(err).Code; code != protocol.ErrNotReady {
		// NOT_READY, not INTERNAL: SPEC §3.6 reserves INTERNAL for "bug in the
		// bridge", and a workspace whose previous daemon has not finished
		// standing down is a retryable condition, not a defect.
		t.Errorf("a contended lock reported %s, want %s", code, protocol.ErrNotReady)
	}

	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// After release the lock is available again — a daemon that exits cleanly
	// must not poison its workspace.
	second, err := acquireLiveness(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatal(err)
	}
}

// TestLivenessLockWaitsForTheOutgoingDaemon is SPEC §3.1's drain-and-restart,
// reduced to the one fact the whole sequence turns on.
//
// A drain ack means "I have STARTED standing down". The old daemon still has
// to finish in-flight work, let the answers land, and tear its language servers
// down — up to ~17 s by the bounds in server.go — before its lock drops. The
// replacement is spawned immediately, so it ALWAYS arrives inside that window.
// Failing on the first EWOULDBLOCK made it exit; daemonctl.Connect spawns
// exactly once, so the workspace was then left with no daemon at all until the
// agent happened to retry.
func TestLivenessLockWaitsForTheOutgoingDaemon(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "daemon.lock")

	outgoing, err := acquireLiveness(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}

	const teardown = 400 * time.Millisecond
	released := make(chan struct{})
	go func() {
		time.Sleep(teardown)
		close(released)
		_ = outgoing.release()
	}()

	start := time.Now()
	replacement, err := acquireLiveness(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("the replacement daemon gave up while its predecessor was still shutting down: %v", err)
	}
	defer replacement.release()

	select {
	case <-released:
	default:
		t.Fatal("the lock was acquired before the outgoing holder released it")
	}
	if elapsed := time.Since(start); elapsed < teardown {
		t.Errorf("waited %v for a lock released after %v", elapsed, teardown)
	}
}

// TestLivenessLockGivesUpOnAHealthyDaemon: the wait is bounded. A second
// daemon for a workspace that already has a live one must be told so, with a
// retryable code, rather than blocking forever.
func TestLivenessLockGivesUpOnAHealthyDaemon(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "daemon.lock")

	held, err := acquireLiveness(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()

	start := time.Now()
	_, err = acquireLiveness(context.Background(), path, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("two daemons hold the same workspace lock")
	}
	got := protocol.AsError(err)
	if got.Code != protocol.ErrNotReady {
		t.Errorf("code = %s (%s), want %s", got.Code, got.Message, protocol.ErrNotReady)
	}
	if got.RetryAfterMS <= 0 {
		t.Error("a NOT_READY without a retry hint tells the caller nothing about when to try again")
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("gave up after %v, before the %v wait elapsed", elapsed, 150*time.Millisecond)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the wait is not bounded: %v", elapsed)
	}
}

// TestLivenessLockWaitIsCancellable: a daemon parked waiting for a predecessor
// must still die on SIGTERM. A blocking flock cannot be interrupted by a
// context, which is why this polls.
func TestLivenessLockWaitIsCancellable(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "daemon.lock")

	held, err := acquireLiveness(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := acquireLiveness(ctx, path, time.Hour)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled wait acquired the lock")
		}
		if code := protocol.AsError(err).Code; code != protocol.ErrNotReady {
			t.Errorf("a cancelled wait reported %s, want %s", code, protocol.ErrNotReady)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context did not end the wait")
	}
}

// TestLivenessLockFileSurvivesRelease pins the deliberate decision not to
// unlink: unlinking after unlocking lets a second waiter hold a lock on an
// inode a third process can no longer see, and two daemons start.
func TestLivenessLockFileSurvivesRelease(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "daemon.lock")

	lock, err := acquireLiveness(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the lock file was unlinked on release: %v", err)
	}
}

// TestLivenessLockIsUserOnly, including when it was created loosely by an
// older build (SPEC §9).
func TestLivenessLockIsUserOnly(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "daemon.lock")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireLiveness(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("lock mode = %o, want 600", perm)
		}
	}
}

// TestListenUnixReplacesAStaleSocketAndSecuresTheNewOne.
func TestListenUnixReplacesAStaleSocketAndSecuresTheNewOne(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "daemon.sock")
	if err := os.WriteFile(socket, []byte("left behind by a killed daemon"), 0o644); err != nil {
		t.Fatal(err)
	}

	listener, err := listenUnix(socket)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer listener.Close()

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("socket mode = %o, want 600 (SPEC §9)", perm)
		}
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Error("the stale regular file was not replaced by a socket")
	}
}

// TestSocketPathIsRefusedWhenTooLong: sockaddr_un truncates silently, and two
// deeply nested roots that truncate to the same name would share one daemon.
func TestSocketPathIsRefusedWhenTooLong(t *testing.T) {
	cfg := &config.Config{SocketPath: filepath.Join("/"+strings.Repeat("nested/", 20), "daemon.sock")}

	_, err := SocketPath(cfg, "/repo")
	if err == nil {
		t.Fatal("an over-long socket path was accepted")
	}
	if code := protocol.AsError(err).Code; code != protocol.ErrInternal {
		t.Errorf("code = %s, want %s", code, protocol.ErrInternal)
	}
}

// TestLockPathsAreDistinct: one file cannot be handed from the spawning client
// to the spawned daemon without a window in which neither holds it.
func TestLockPathsAreDistinct(t *testing.T) {
	cfg := testConfig(t)

	liveness, spawn, err := LockPaths(cfg, "/repo/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if liveness == spawn {
		t.Fatalf("the liveness and spawn locks are the same file: %s", liveness)
	}

	otherLiveness, _, err := LockPaths(cfg, "/repo/beta")
	if err != nil {
		t.Fatal(err)
	}
	if liveness == otherLiveness {
		t.Errorf("two workspaces share the liveness lock %s", liveness)
	}
}
