package daemon

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"strconv"
	"syscall"

	"github.com/fingerskier/langer/config"
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

// livenessLock is an exclusive, non-blocking flock held for a daemon's whole
// lifetime. It — not the presence of a socket file — is what "a daemon is
// running for this workspace" means. A killed daemon leaves its socket behind
// but cannot leave its lock behind: the kernel drops the flock when the process
// dies.
type livenessLock struct {
	path string
	file *os.File
}

// acquireLiveness takes the lock, or reports that somebody else holds it.
func acquireLiveness(path string) (*livenessLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "opening lock %s: %v", path, err)
	}
	// A lock file left behind by an older, looser build must not stay readable
	// by other users (SPEC §9).
	if err := os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, protocol.NewErrorf(protocol.ErrInternal, "securing lock %s: %v", path, err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, protocol.NewErrorf(protocol.ErrInternal,
				"another langer daemon is already running for this workspace (lock %s)", path)
		}
		return nil, protocol.NewErrorf(protocol.ErrInternal, "locking %s: %v", path, err)
	}

	// The pid is for humans reading the directory; nothing depends on it, which
	// matters because a pid file can be stale and a held flock cannot.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &livenessLock{path: path, file: file}, nil
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
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
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
	// bind() honours the umask, so an explicit chmod is what makes 0600 true
	// (SPEC §9). The window before it is closed by the 0700 runtime directory:
	// nobody else can traverse it to reach the socket at all.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, protocol.NewErrorf(protocol.ErrInternal, "securing socket %s: %v", socketPath, err)
	}
	return listener, nil
}
