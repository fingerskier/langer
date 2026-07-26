// Package index owns langer's persistent SQLite cache and every identifier
// used only by that cache.
package index

import (
	"path/filepath"
	"strings"

	"github.com/fingerskier/langer/protocol"
)

// keySeparator cannot appear in a Go, Python, TypeScript, or JavaScript
// identifier. Keeping one shared spelling avoids ambiguous concatenations such
// as ("ab", "c") and ("a", "bc").
const keySeparator = "\x1f"

// StableKey is the descriptive, deliberately non-unique identity specified by
// SPEC §5.1. It survives a file re-index, but it is not sufficient to identify
// cached references on its own.
func StableKey(s protocol.Symbol) string {
	return s.Name + keySeparator + s.Container + keySeparator + string(s.Kind)
}

// SymbolKey qualifies a stable key with its repository namespace and
// workspace-relative definition path. Queries remain workspace-scoped in SQL,
// so clones and worktrees with the same repository namespace never share rows.
func SymbolKey(repoNamespace, definitionPath, stableKey string) string {
	path := filepath.ToSlash(filepath.Clean(definitionPath))
	// Accept persisted/configured paths produced on the other supported OS.
	// filepath.ToSlash only replaces the current platform's separator.
	path = strings.ReplaceAll(path, `\`, "/")
	path = strings.TrimPrefix(path, "./")
	return repoNamespace + keySeparator + path + keySeparator + stableKey
}
