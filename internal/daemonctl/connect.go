package daemonctl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/filelock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/protocol"
)

// Spawn tuning. None of it is spec'd behaviour: these are the bounds that stop
// a cold start hanging forever.
const (
	// spawnLockWait is how long a client waits for another client's spawn
	// attempt to finish before giving up.
	spawnLockWait = 20 * time.Second
	// spawnPoll is the interval between dial attempts while a freshly spawned
	// daemon initialises.
	spawnPoll = 25 * time.Millisecond
	// spawnBudget bounds the whole spawn-and-wait sequence.
	spawnBudget = 20 * time.Second
	// dialTimeout bounds one connect attempt.
	dialTimeout = 2 * time.Second
	// handshakeTimeout bounds the version exchange.
	handshakeTimeout = 5 * time.Second
	// EnvSpawned marks a daemon that was auto-started by a client, so it logs
	// to a file instead of the inherited stderr pipe.
	EnvSpawned = "LANGER_DAEMON_LOG"
)

// Options are Connect's optional knobs. Zero values are sensible.
type Options struct {
	Logger *slog.Logger
	// Session identifies the caller during the handshake.
	Session protocol.SessionID
	// Executable overrides the binary spawned for `langer daemon <root>`.
	// Empty means os.Executable.
	Executable string
}

// Connect returns a live client for root, auto-starting the daemon if none is
// listening (SPEC §3.1).
//
// It is safe for N processes to call this concurrently for the same root and
// yield exactly ONE daemon. Every step below is load-bearing
// (docs/ARCHITECTURE.md §5.9):
//
//  1. dial the socket; on success, handshake and compare protocol versions;
//  2. on version mismatch, ask the old daemon to DRAIN — not to die mid-request
//     — then continue at 3;
//  3. on dial failure, take the SPAWN lock, a file distinct from the daemon's
//     own liveness lock: one lock cannot be handed from parent to child without
//     an unclosable race window;
//  4. RE-DIAL under the lock, because another process may have completed a
//     spawn while we blocked. Omitting this is the single most common way this
//     pattern produces duplicate daemons on a cold start;
//  5. spawn the daemon DETACHED so Ctrl-C in the agent's terminal cannot kill
//     the daemon SPEC §8 requires to survive client disconnects;
//  6. poll-dial until it answers, then release the spawn lock.
func Connect(ctx context.Context, cfg *config.Config, root string, ck clock.Clock, runner procx.Runner, opts Options) (*Client, error) {
	if cfg == nil {
		return nil, protocol.NewError(protocol.ErrInternal, "daemonctl: a config is required")
	}
	if ck == nil {
		ck = clock.New()
	}
	if runner == nil {
		runner = procx.NewRunner()
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	session := opts.Session
	if session == "" {
		session = "daemonctl"
	}

	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	if _, err := cfg.EnsureRuntimeDir(); err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "%v", err)
	}
	socket, err := cfg.WorkspaceSocketPath(canonical)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "%v", err)
	}

	// 1 & 2.
	client, err := dialAndHandshake(ctx, socket, canonical, session, log)
	if err == nil {
		return client, nil
	}
	if drained, ok := err.(*mismatchError); ok {
		log.Info("daemon protocol version mismatch; draining it",
			"daemon_version", drained.daemonVersion, "client_version", protocol.Version)
	}

	// 3. Take the spawn lock.
	lock, err := acquireSpawnLock(ctx, cfg.WorkspaceSpawnLockPath(canonical), ck)
	if err != nil {
		return nil, err
	}
	defer lock.release()

	// 4. Re-dial under the lock: another process may have finished a spawn
	// while we were blocked on it.
	if client, err := dialAndHandshake(ctx, socket, canonical, session, log); err == nil {
		return client, nil
	}

	// 5. Spawn, detached.
	if err := spawnDaemon(cfg, canonical, runner, opts, log); err != nil {
		return nil, err
	}

	// 6. Poll until it answers.
	deadline := time.Now().Add(spawnBudget)
	var lastErr error
	for {
		client, err := dialAndHandshake(ctx, socket, canonical, session, log)
		if err == nil {
			return client, nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			break
		}
		if err := ck.Sleep(ctx, spawnPoll); err != nil {
			return nil, protocol.NewErrorf(protocol.ErrNotReady,
				"gave up waiting for the daemon for %s: %v", canonical, err).WithRetryAfterMS(200)
		}
	}
	return nil, protocol.NewErrorf(protocol.ErrNotReady,
		"the daemon for %s did not start within %s: %v", canonical, spawnBudget, lastErr).
		WithRetryAfterMS(500)
}

// mismatchError reports that the daemon we reached speaks a different protocol
// version. It has already been asked to drain by the time this is returned.
type mismatchError struct{ daemonVersion int }

func (e *mismatchError) Error() string { return "daemon protocol version mismatch" }

// dialAndHandshake connects and verifies the peer is a daemon we can talk to.
func dialAndHandshake(ctx context.Context, socket, root string, session protocol.SessionID, log *slog.Logger) (*Client, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}

	client := newClient(conn, root, log)

	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	result, err := client.handshake(hsCtx, session)
	if err != nil {
		// A draining daemon refuses the handshake; so does one that died
		// between our dial and our first write. Either way it is not usable.
		_ = client.Close()
		return nil, err
	}

	if result.ProtocolVersion != protocol.Version {
		// SPEC §3.1: ask the old daemon to drain rather than killing it
		// mid-request, then start a fresh one.
		if err := client.requestDrain(hsCtx, session, "protocol version mismatch"); err != nil {
			log.Debug("the stale daemon refused to drain", "error", err)
		}
		_ = client.Close()
		return nil, &mismatchError{daemonVersion: result.ProtocolVersion}
	}

	if root != "" && result.Root != "" && result.Root != root {
		// A hash collision or a hand-edited socket path. Refusing beats
		// answering with another project's compilation context.
		_ = client.Close()
		return nil, protocol.NewErrorf(protocol.ErrInternal,
			"the daemon on %s serves %s, not %s", socket, result.Root, root)
	}
	// Prefer the daemon's reported root (needed when dialing by socket alone).
	if result.Root != "" {
		client.root = result.Root
	}
	return client, nil
}

// spawnDaemon starts `langer daemon <root>` in a new session so it outlives
// this process (SPEC §8).
func spawnDaemon(cfg *config.Config, root string, runner procx.Runner, opts Options, log *slog.Logger) error {
	exe := opts.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return protocol.NewErrorf(protocol.ErrInternal, "locating the langer binary: %v", err)
		}
	}

	env := append(os.Environ(), EnvSpawned+"="+cfg.WorkspaceLogPath(root))

	// The spawn context is deliberately NOT the caller's: procx kills the child
	// when its context is done, and this child must outlive the call that
	// started it.
	proc, err := runner.Start(context.Background(), procx.Spec{
		Path:     exe,
		Args:     []string{"daemon", root},
		Dir:      root,
		Env:      env,
		Detached: true,
	})
	if err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "spawning the daemon for %s: %v", root, err)
	}
	log.Info("spawned a daemon", "root", root, "pid", proc.PID())

	// The child inherited pipes it will never use for protocol traffic. Close
	// our write end and drain the read ends so a full pipe can never block the
	// daemon; both goroutines end when the daemon exits.
	_ = proc.Stdin().Close()
	go func() { _, _ = io.Copy(io.Discard, proc.Stdout()) }()
	go func() { _, _ = io.Copy(io.Discard, proc.Stderr()) }()
	return nil
}

// canonicalRoot resolves a workspace root the same way the daemon does:
// absolute, with symlinks resolved (SPEC §3.2). The two must agree or a client
// would derive a different socket name and start a second daemon for one
// directory.
func canonicalRoot(root string) (string, error) {
	if root == "" {
		return "", protocol.NewError(protocol.ErrWorkspaceUnknown, "a workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "resolving %q: %v", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "workspace root %s: %v", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "workspace root %s is not a directory", resolved)
	}
	return resolved, nil
}

// spawnLock is the client-side lock of docs/ARCHITECTURE.md §6.8. It is a
// different file from the daemon's liveness lock on purpose.
type spawnLock struct{ file *os.File }

// acquireSpawnLock blocks — with a bound — until it owns the spawn attempt for
// this workspace.
//
// It polls a non-blocking OS file lock rather than using a blocking one so the
// wait stays cancellable: a blocking lock cannot be interrupted by a context, and a
// wedged spawn elsewhere would hang this process forever.
func acquireSpawnLock(ctx context.Context, path string, ck clock.Clock) (*spawnLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "opening spawn lock %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, protocol.NewErrorf(protocol.ErrInternal, "securing spawn lock %s: %v", path, err)
	}

	deadline := time.Now().Add(spawnLockWait)
	for {
		err := filelock.Try(file)
		if err == nil {
			return &spawnLock{file: file}, nil
		}
		if !errors.Is(err, filelock.ErrContended) {
			_ = file.Close()
			return nil, protocol.NewErrorf(protocol.ErrInternal, "locking %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, protocol.NewErrorf(protocol.ErrNotReady,
				"another process has been starting the daemon for %s longer than %s", path, spawnLockWait).
				WithRetryAfterMS(500)
		}
		if err := ck.Sleep(ctx, spawnPoll); err != nil {
			_ = file.Close()
			return nil, protocol.NewErrorf(protocol.ErrNotReady, "waiting for the spawn lock: %v", err)
		}
	}
}

// release drops the lock. The file is not unlinked: unlinking after unlocking
// lets a second waiter hold a lock on an inode a third process can no longer
// see (see daemon.livenessLock.release for the same reasoning).
func (l *spawnLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = filelock.Unlock(l.file)
	_ = l.file.Close()
	l.file = nil
}
