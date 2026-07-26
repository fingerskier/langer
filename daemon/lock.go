package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/filelock"
	"github.com/fingerskier/langer/protocol"
)

// SocketPath derives the per-workspace socket path from the configured socket
// path's DIRECTORY plus a short hash of the absolute root (SPEC §3.1: one
// daemon per workspace).
func SocketPath(cfg *config.Config, root string) (string, error) {
	path, err := cfg.WorkspaceSocketPath(root)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrInternal, "%v", err)
	}
	return path, nil
}

// LockPaths returns the two lock files of one workspace: the liveness lock a
// running daemon holds for its whole lifetime, and the spawn lock a client
// holds while it attempts a spawn (docs/ARCHITECTURE.md §6.8).
func LockPaths(cfg *config.Config, root string) (liveness, spawn string, err error) {
	return cfg.WorkspaceLivenessLockPath(root), cfg.WorkspaceSpawnLockPath(root), nil
}

// LogPath is where an auto-started daemon writes its log.
func LogPath(cfg *config.Config, root string) string { return cfg.WorkspaceLogPath(root) }

// Bounds on waiting for the outgoing daemon's liveness lock.
const (
	// DefaultLivenessLockWait is how long a starting daemon waits for a
	// PREDECESSOR to let go of the workspace's liveness lock.
	//
	// Waiting, rather than failing on the first EWOULDBLOCK, is what makes
	// SPEC §3.1's drain-and-restart work at all. The client asks the old daemon
	// to drain, gets its ack, and spawns the replacement immediately — but the
	// ack only means "I have started standing down". The old process still has
	// to finish in-flight work (DrainGrace), let the answers land (replyGrace),
	// and tear down its language servers (shutdownGrace, up to 10 s of
	// `shutdown` request plus process exit on a busy tsserver) before its lock
	// drops. A replacement that gives up in that window dies, and — since
	// daemonctl spawns exactly once — the workspace is left with NO daemon at
	// all until an agent happens to retry. This bound comfortably covers that
	// teardown, which is itself bounded by the constants in server.go.
	DefaultLivenessLockWait = 25 * time.Second
	// livenessLockPoll is the interval between attempts. A blocking OS lock
	// cannot be interrupted by a context, and a shutdown that cannot be
	// interrupted is how a daemon becomes unkillable, so this polls instead.
	livenessLockPoll = 25 * time.Millisecond
)

// livenessLock is an exclusive OS file lock held for a daemon's whole
// lifetime. It — not the presence of a socket file — is what "a daemon is
// running for this workspace" means. A killed daemon leaves its socket behind
// but cannot leave its lock behind: the OS drops it when the process dies.
type livenessLock struct {
	path string
	file *os.File
}

// acquireLiveness takes the lock, waiting up to wait for a predecessor to
// release it.
//
// Contention is reported as NOT_READY with a retry hint, never INTERNAL:
// SPEC §3.6 defines INTERNAL as "bug in the bridge", and "the daemon I am
// replacing has not finished standing down yet" is neither a bug nor
// permanent. A wait of zero or less means a single attempt.
func acquireLiveness(ctx context.Context, path string, wait time.Duration) (*livenessLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "opening lock %s: %v", path, err)
	}
	// A lock file left behind by an older, looser build must not stay readable
	// by other users (SPEC §9).
	if err := RestrictUserOnlyFile(path); err != nil {
		_ = file.Close()
		return nil, err
	}

	if err := flockWait(ctx, file, wait); err != nil {
		_ = file.Close()
		return nil, err
	}

	// The pid is for humans reading the directory; nothing depends on it, which
	// matters because a pid file can be stale and a held flock cannot.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &livenessLock{path: path, file: file}, nil
}

// flockWait polls for an exclusive, non-blocking file lock until it wins, the
// context is done, or the deadline passes.
func flockWait(ctx context.Context, file *os.File, wait time.Duration) error {
	path := file.Name()
	deadline := time.Now().Add(wait)
	for {
		err := filelock.Try(file)
		if err == nil {
			return nil
		}
		if !errors.Is(err, filelock.ErrContended) {
			return protocol.NewErrorf(protocol.ErrInternal, "locking %s: %v", path, err)
		}
		// Checked AFTER the attempt above, so a wait of zero still means one
		// honest try rather than none.
		if !time.Now().Before(deadline) {
			return protocol.NewErrorf(protocol.ErrNotReady,
				"another langer daemon still holds this workspace's lock after %s (lock %s)", wait, path).
				WithRetryAfterMS(500)
		}
		select {
		case <-ctx.Done():
			return protocol.NewErrorf(protocol.ErrNotReady,
				"gave up waiting for this workspace's daemon lock: %v", ctx.Err()).WithRetryAfterMS(500)
		case <-time.After(livenessLockPoll):
		}
	}
}

// release drops the lock.
//
// The file is deliberately NOT unlinked. Unlinking after unlocking races: a
// second daemon can take the lock on the same inode, after which our unlink
// leaves it holding a lock on a file nobody else will ever see — and a third
// daemon then creates a fresh file at the same path and locks that. Two live
// daemons, no error anywhere. An empty lock file left on disk costs nothing.
func (l *livenessLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := filelock.Unlock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "unlocking %s: %v", l.path, err)
	}
	if closeErr != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "closing %s: %v", l.path, closeErr)
	}
	return nil
}

// listenUnix binds the workspace socket with user-only permissions.
//
// The caller must already hold the liveness lock, which is what makes the
// unlink safe: any socket file present is stale by definition, because no live
// daemon for this root can exist while we hold the lock.
func listenUnix(socketPath string) (net.Listener, error) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "removing stale socket %s: %v", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "listening on %s: %v", socketPath, err)
	}
	// bind() honours the umask, so RestrictUserOnlyFile is what makes 0600 true
	// (SPEC §9). The window before it is closed by the 0700 runtime directory:
	// nobody else can traverse it to reach the socket at all.
	if err := RestrictUserOnlyFile(socketPath); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, err
	}
	return listener, nil
}
