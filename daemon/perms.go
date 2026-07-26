package daemon

import (
	"os"
	"runtime"

	"github.com/fingerskier/langer/protocol"
)

// RestrictUserOnlyFile forces user-only access on a regular file (SPEC §9).
// Unix uses mode 0600. Windows ACLs are process-user by default for files
// created under the user profile; we still call Chmod for best-effort
// attribute alignment and so a single code path owns the policy.
func RestrictUserOnlyFile(path string) error {
	if path == "" {
		return protocol.NewError(protocol.ErrInternal, "restricting file permissions: path is required")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "securing %s: %v", path, err)
	}
	return nil
}

// RestrictUserOnlyDir forces user-only access on a directory (mode 0700).
func RestrictUserOnlyDir(path string) error {
	if path == "" {
		return protocol.NewError(protocol.ErrInternal, "restricting directory permissions: path is required")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "securing directory %s: %v", path, err)
	}
	return nil
}

// FileIsUserOnly reports whether info has the SPEC §9 user-only mode on
// platforms where mode bits are authoritative. On Windows the check is
// best-effort: CreateFile isolation is the real boundary, so this returns
// true when the file exists (caller already Stat'd it).
func FileIsUserOnly(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm() == 0o600
}

// DirIsUserOnly reports whether info has mode 0700 on Unix. Windows always
// returns true for an existing directory (see FileIsUserOnly).
func DirIsUserOnly(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm() == 0o700
}
