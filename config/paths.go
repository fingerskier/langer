package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Per-workspace runtime paths.
//
// SPEC §6 names one socket, but SPEC §3.1 requires one daemon per workspace, so
// the actual socket is per-root: the configured socket path supplies the
// DIRECTORY and the workspace root supplies the name. This lives in config,
// beside the other XDG path derivations, because both the daemon (which binds
// the socket) and the client (which dials it) must agree on the answer and
// neither may import the other.
//
// The name is a hash rather than a mangled path on purpose: macOS caps
// sockaddr_un.sun_path at about 104 bytes and TRUNCATES silently, so two deeply
// nested roots can collapse to the same name and end up sharing one daemon —
// answering with the wrong project's compilation context
// (docs/ARCHITECTURE.md §5.8).

// maxSocketPathBytes is a conservative bound on sun_path. Darwin allows 104
// including the terminator; Linux allows 108. Refusing at 100 leaves room and
// fails loudly instead of silently truncating.
const maxSocketPathBytes = 100

// WorkspaceKey is the short, stable identifier of a workspace root in file
// names. Roots must be absolute and symlink-resolved before they get here.
func WorkspaceKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])[:12]
}

// RuntimeDir is the directory holding sockets, lock files and daemon logs. It
// is the directory of the configured socket path (SPEC §6).
func (c *Config) RuntimeDir() string {
	return filepath.Dir(c.SocketPath)
}

// EnsureRuntimeDir creates the runtime directory with user-only permissions and
// returns it.
//
// SPEC §9 requires restrictive permissions on socket and database files. The
// 0700 directory is the part that actually holds: it closes the window between
// bind() and chmod() on the socket itself, because nobody else can traverse the
// directory to reach it in the first place.
func (c *Config) EnsureRuntimeDir() (string, error) {
	dir := c.RuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating runtime directory %s: %w", dir, err)
	}
	// MkdirAll honours the umask and does nothing at all when the directory
	// already exists, so an explicit chmod is what makes 0700 true.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("securing runtime directory %s: %w", dir, err)
	}
	return dir, nil
}

// WorkspaceSocketPath returns the Unix socket for one workspace root.
func (c *Config) WorkspaceSocketPath(root string) (string, error) {
	path := filepath.Join(c.RuntimeDir(), "daemon-"+WorkspaceKey(root)+".sock")
	if len(path) > maxSocketPathBytes {
		return "", fmt.Errorf("socket path %s is %d bytes, over the %d byte limit for a Unix socket; "+
			"set socket_path in config.toml to a shorter directory", path, len(path), maxSocketPathBytes)
	}
	return path, nil
}

// WorkspaceLivenessLockPath returns the lock a running daemon holds for its
// whole lifetime.
func (c *Config) WorkspaceLivenessLockPath(root string) string {
	return filepath.Join(c.RuntimeDir(), "daemon-"+WorkspaceKey(root)+".lock")
}

// WorkspaceSpawnLockPath returns the lock a CLIENT holds while it attempts a
// spawn.
//
// It is deliberately a different file from the liveness lock: one lock cannot be
// handed from spawner to spawned without a window in which neither holds it —
// or, if released earlier, one in which two daemons start
// (docs/ARCHITECTURE.md §6.8).
func (c *Config) WorkspaceSpawnLockPath(root string) string {
	return filepath.Join(c.RuntimeDir(), "daemon-"+WorkspaceKey(root)+".spawn.lock")
}

// WorkspaceLogPath returns where an auto-started daemon writes its log.
//
// A daemon spawned by a client inherits that client's stderr pipe. Writing to it
// after the client exits raises SIGPIPE, which kills the process — and SPEC §8
// requires the daemon to survive client disconnects. Logging to a file instead
// is what makes that true.
func (c *Config) WorkspaceLogPath(root string) string {
	return filepath.Join(c.RuntimeDir(), "daemon-"+WorkspaceKey(root)+".log")
}
