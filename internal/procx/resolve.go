// Package procx owns process execution for langer.
//
// It is the ONLY package permitted to import os/exec (enforced by an import
// rule test in cmd/langer), for two reasons:
//
//   - SPEC §9: opening a workspace must never execute project-local binaries.
//     Resolver is the single enforcement point, and it is a pure function so the
//     invariant can be table-tested exhaustively.
//   - Process groups: language servers are node wrapper scripts whose children
//     survive a plain kill of the parent. Every child is started in its own
//     process group so teardown cannot leak a node process.
package procx

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fingerskier/langer/protocol"
)

// Resolver decides which binary a registry entry actually runs.
type Resolver interface {
	// Resolve returns the absolute path of the executable to run.
	//
	// It returns an UNSUPPORTED *protocol.Error when the command cannot be
	// found, and an INTERNAL *protocol.Error naming the offending path when the
	// candidate resolves inside workspaceRoot and allowWorkspaceLocal is false.
	//
	// PATH lookup uses a PATH scrubbed of every entry that resolves inside
	// workspaceRoot and of every relative entry (including "" and ".").
	Resolve(command, workspaceRoot string, allowWorkspaceLocal bool) (string, error)
}

// NewResolver returns the production Resolver.
func NewResolver() Resolver { return resolver{} }

type resolver struct{}

func (r resolver) Resolve(command, workspaceRoot string, allowWorkspaceLocal bool) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", protocol.NewError(protocol.ErrUnsupported, "language server command is empty")
	}

	root := canonical(workspaceRoot)

	var candidate string
	if strings.ContainsRune(command, filepath.Separator) {
		abs, err := filepath.Abs(command)
		if err != nil {
			return "", protocol.NewErrorf(protocol.ErrUnsupported, "resolving %q: %v", command, err)
		}
		if !executable(abs) {
			return "", protocol.NewErrorf(protocol.ErrUnsupported, "language server %q is not an executable file", command)
		}
		candidate = canonical(abs)
	} else {
		found, ok := lookPath(command, root)
		if !ok {
			return "", protocol.NewErrorf(protocol.ErrUnsupported, "language server %q not found on PATH", command)
		}
		candidate = found
	}

	if !allowWorkspaceLocal && contains(root, candidate) {
		// SPEC §9. The message names the path so the operator can see exactly
		// which project-local binary was refused.
		return "", protocol.NewErrorf(protocol.ErrInternal,
			"refusing to execute workspace-local binary %s (SPEC §9); set allow_workspace_local to opt in", candidate)
	}
	return candidate, nil
}

// lookPath searches a scrubbed PATH. Entries that are empty, relative, or that
// resolve inside root are dropped before the search, so prepending
// <workspace>/node_modules/.bin to PATH — the standard Node convention and the
// attack testdata/README.md §3.3 describes — simply finds nothing.
func lookPath(command, root string) (string, bool) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		canonicalDir := canonical(dir)
		if contains(root, canonicalDir) {
			continue
		}
		for _, name := range executableNames(command) {
			candidate := filepath.Join(dir, name)
			if !executable(candidate) {
				continue
			}
			return canonical(candidate), true
		}
	}
	return "", false
}

// executable reports whether path is a regular executable file. Unix uses
// mode bits; Windows uses PATHEXT, matching normal command lookup there. It
// follows symlinks and never runs anything.
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		ext := strings.ToLower(filepath.Ext(path))
		for _, allowed := range windowsExecutableExtensions() {
			if ext == allowed {
				return true
			}
		}
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

func executableNames(command string) []string {
	if runtime.GOOS != "windows" || filepath.Ext(command) != "" {
		return []string{command}
	}
	exts := windowsExecutableExtensions()
	out := make([]string, 0, len(exts))
	for _, ext := range exts {
		out = append(out, command+ext)
	}
	return out
}

func windowsExecutableExtensions() []string {
	raw := os.Getenv("PATHEXT")
	if raw == "" {
		raw = ".COM;.EXE;.BAT;.CMD"
	}
	parts := strings.Split(raw, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// canonical returns path with symlinks evaluated where possible, absolute and
// cleaned. Containment is decided on canonical paths on BOTH sides: a candidate
// that merely looks outside the workspace but symlinks into it must still be
// caught.
func canonical(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// contains reports whether candidate lies inside root, comparing
// path-component-wise. A string-prefix test would let "/repo-evil" pass a
// "/repo" check; on macOS the comparison is case-insensitive because the
// filesystem is.
func contains(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	rootParts := components(root)
	candidateParts := components(candidate)
	if len(rootParts) == 0 || len(candidateParts) < len(rootParts) {
		return false
	}
	for i, part := range rootParts {
		if !sameComponent(part, candidateParts[i]) {
			return false
		}
	}
	return true
}

func components(path string) []string {
	cleaned := filepath.Clean(path)
	parts := strings.Split(cleaned, string(filepath.Separator))
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// caseInsensitiveFS is true on platforms whose default filesystem folds case.
var caseInsensitiveFS = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

func sameComponent(a, b string) bool {
	if caseInsensitiveFS {
		return strings.EqualFold(a, b)
	}
	return a == b
}
