// Package tools manages langer's user-level language-server installs.
//
// Layout (plan/PLAN.md):
//
//	~/.langer/tools/<profile>/…
//
// Managed binaries live outside the workspace. SPEC §9 still forbids executing
// workspace-local binaries without allow_workspace_local.
package tools

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvToolsDir overrides the tools install root (tests / custom layouts).
const EnvToolsDir = "LANGER_TOOLS_DIR"

// EnvManifestPath overrides the embedded tools manifest path.
const EnvManifestPath = "LANGER_TOOLS_MANIFEST"

// DefaultToolsDir returns ~/.langer/tools (or LANGER_TOOLS_DIR).
func DefaultToolsDir() (string, error) {
	if dir := os.Getenv(EnvToolsDir); dir != "" {
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("tools dir: home: %w", err)
	}
	return filepath.Join(home, ".langer", "tools"), nil
}

// ProfileDir is ~/.langer/tools/<profileID>.
func ProfileDir(toolsRoot, profileID string) string {
	return filepath.Join(toolsRoot, profileID)
}

// EnsureToolsRoot creates the tools root with user-only permissions.
func EnsureToolsRoot(toolsRoot string) error {
	if err := os.MkdirAll(toolsRoot, 0o700); err != nil {
		return fmt.Errorf("creating tools dir %s: %w", toolsRoot, err)
	}
	if err := os.Chmod(toolsRoot, 0o700); err != nil {
		return fmt.Errorf("securing tools dir %s: %w", toolsRoot, err)
	}
	return nil
}
