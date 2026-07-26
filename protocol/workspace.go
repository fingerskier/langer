package protocol

import (
	"crypto/sha256"
	"encoding/hex"
)

// WorkspaceIDForRoot returns the deterministic identifier of a canonical
// absolute workspace root. Repository slugs are portable symbol metadata; the
// root remains the isolation boundary so clones and worktrees never share
// cache rows.
func WorkspaceIDForRoot(root string) WorkspaceID {
	sum := sha256.Sum256([]byte(root))
	return WorkspaceID("ws-" + hex.EncodeToString(sum[:])[:12])
}
