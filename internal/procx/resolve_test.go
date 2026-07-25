package procx_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/protocol"
)

// writeExe creates an executable file and returns its path.
func writeExe(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// realPath is the symlink-evaluated form; macOS temp dirs live under a symlink
// (/var → /private/var), and every assertion here must survive that.
func realPath(t *testing.T, p string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func wantCode(t *testing.T, err error, code protocol.ErrorCode) *protocol.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", code)
	}
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *protocol.Error", err)
	}
	if pe.Code != code {
		t.Fatalf("error code = %s (%v), want %s", pe.Code, pe, code)
	}
	return pe
}

func TestResolveAbsolutePathOutsideWorkspace(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := writeExe(t, filepath.Join(tmp, "tools"), "server")

	got, err := procx.NewResolver().Resolve(exe, root, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != realPath(t, exe) {
		t.Fatalf("Resolve = %q, want %q", got, realPath(t, exe))
	}
}

// SPEC §9: opening a workspace must never execute project-local binaries.
func TestResolveRejectsBinaryInsideWorkspace(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	exe := writeExe(t, filepath.Join(root, "node_modules", ".bin"), "typescript-language-server")

	_, err := procx.NewResolver().Resolve(exe, root, false)
	pe := wantCode(t, err, protocol.ErrInternal)
	if !strings.Contains(pe.Message, "typescript-language-server") {
		t.Fatalf("error message %q does not name the offending path", pe.Message)
	}
}

// SPEC §9 allows exactly one escape hatch: explicit opt-in. Nothing in the v0.1
// test suite or fixtures may ever set it.
func TestResolveAllowsWorkspaceLocalOnlyWithOptIn(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	exe := writeExe(t, filepath.Join(root, "node_modules", ".bin"), "server")

	got, err := procx.NewResolver().Resolve(exe, root, true)
	if err != nil {
		t.Fatalf("Resolve with opt-in: %v", err)
	}
	if got != realPath(t, exe) {
		t.Fatalf("Resolve = %q, want %q", got, realPath(t, exe))
	}
}

// A string-prefix containment check would treat /tmp/x/repo-evil as inside
// /tmp/x/repo. Comparison must be path-component-wise.
func TestResolveSiblingDirectoryIsNotInsideWorkspace(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := writeExe(t, filepath.Join(tmp, "repo-evil"), "server")

	if _, err := procx.NewResolver().Resolve(exe, root, false); err != nil {
		t.Fatalf("sibling directory rejected as workspace-local: %v", err)
	}
}

func TestResolveWorkspaceRootItselfIsInside(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	exe := writeExe(t, root, "server")

	if _, err := procx.NewResolver().Resolve(exe, root, false); err == nil {
		t.Fatal("a binary sitting directly in the workspace root was accepted")
	}
}

// Containment is decided after EvalSymlinks on BOTH sides: a path that merely
// *looks* outside but resolves inside must still be rejected.
func TestResolveSymlinkIntoWorkspaceIsRejected(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	writeExe(t, filepath.Join(root, "node_modules", ".bin"), "server")

	link := filepath.Join(tmp, "innocent-looking-server")
	if err := os.Symlink(filepath.Join(root, "node_modules", ".bin", "server"), link); err != nil {
		t.Fatal(err)
	}

	_, err := procx.NewResolver().Resolve(link, root, false)
	wantCode(t, err, protocol.ErrInternal)
}

func TestResolveMissingCommandIsUnsupported(t *testing.T) {
	tmp := t.TempDir()
	_, err := procx.NewResolver().Resolve(filepath.Join(tmp, "nope"), tmp, false)
	wantCode(t, err, protocol.ErrUnsupported)
}

func TestResolveEmptyCommandIsUnsupported(t *testing.T) {
	_, err := procx.NewResolver().Resolve("", t.TempDir(), false)
	wantCode(t, err, protocol.ErrUnsupported)
}

func TestResolveNonExecutableIsUnsupported(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "data.txt")
	if err := os.WriteFile(path, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := procx.NewResolver().Resolve(path, filepath.Join(tmp, "repo"), false)
	wantCode(t, err, protocol.ErrUnsupported)
}

func TestResolveDirectoryIsUnsupported(t *testing.T) {
	tmp := t.TempDir()
	_, err := procx.NewResolver().Resolve(tmp, filepath.Join(tmp, "repo"), false)
	wantCode(t, err, protocol.ErrUnsupported)
}

func TestResolveBareCommandFromPATH(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(tmp, "bin")
	exe := writeExe(t, binDir, "langer-test-server")
	t.Setenv("PATH", binDir)

	got, err := procx.NewResolver().Resolve("langer-test-server", root, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != realPath(t, exe) {
		t.Fatalf("Resolve = %q, want %q", got, realPath(t, exe))
	}
}

// testdata/README.md §3.3: the realistic attack is prepending
// <workspace>/node_modules/.bin to PATH. Scrubbing must make that entry
// invisible, so the lookup fails rather than silently running the repo's file.
func TestResolveScrubsWorkspacePATHEntries(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	binDir := filepath.Join(root, "node_modules", ".bin")
	writeExe(t, binDir, "typescript-language-server")
	t.Setenv("PATH", binDir)

	_, err := procx.NewResolver().Resolve("typescript-language-server", root, false)
	wantCode(t, err, protocol.ErrUnsupported)
}

// With the workspace entry scrubbed, a legitimate later entry must still win
// rather than the whole lookup failing.
func TestResolveSkipsWorkspacePATHEntryAndUsesTheNextOne(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	evil := filepath.Join(root, "node_modules", ".bin")
	writeExe(t, evil, "typescript-language-server")
	good := writeExe(t, filepath.Join(tmp, "devtools"), "typescript-language-server")
	t.Setenv("PATH", evil+string(os.PathListSeparator)+filepath.Dir(good))

	got, err := procx.NewResolver().Resolve("typescript-language-server", root, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != realPath(t, good) {
		t.Fatalf("Resolve = %q, want the out-of-tree %q", got, realPath(t, good))
	}
}

// "" and "." on PATH mean "the current directory" — which is very often the
// workspace root. Both must be dropped, along with every other relative entry.
func TestResolveDropsRelativePATHEntries(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExe(t, root, "sneaky")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	for _, entry := range []string{"", ".", "./", "node_modules/.bin"} {
		t.Run("PATH="+entry, func(t *testing.T) {
			t.Setenv("PATH", entry)
			if _, err := procx.NewResolver().Resolve("sneaky", root, false); err == nil {
				t.Fatalf("relative PATH entry %q was honoured", entry)
			}
		})
	}
}

func TestResolveSymlinkedPATHEntryIntoWorkspaceIsScrubbed(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	writeExe(t, filepath.Join(root, "node_modules", ".bin"), "server")

	link := filepath.Join(tmp, "linkbin")
	if err := os.Symlink(filepath.Join(root, "node_modules", ".bin"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", link)

	_, err := procx.NewResolver().Resolve("server", root, false)
	wantCode(t, err, protocol.ErrUnsupported)
}

// On macOS the filesystem is case-insensitive, so a case-shifted workspace path
// still names the same directory and must still be contained.
func TestResolveContainmentIsCaseInsensitiveOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("case-insensitive containment is a darwin rule")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Repo")
	exe := writeExe(t, filepath.Join(root, "node_modules", ".bin"), "server")

	shifted := filepath.Join(tmp, "repo", "node_modules", ".bin", "server")
	if _, err := os.Stat(shifted); err != nil {
		t.Skipf("filesystem at %s is case-sensitive: %v", tmp, err)
	}
	if _, err := procx.NewResolver().Resolve(shifted, root, false); err == nil {
		t.Fatalf("case-shifted workspace path %q was accepted (real path %q)", shifted, exe)
	}
}

// Resolve is pure: it must never execute anything. The tripwire fixture proves
// it — running it writes a sentinel file.
func TestResolveNeverExecutes(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "sentinel")
	t.Setenv("LANGER_TRIPWIRE_SENTINEL", sentinel)

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", "testdata", "ts-project"))
	if err != nil {
		t.Fatal(err)
	}
	tripwire := filepath.Join(repoRoot, "node_modules", ".bin", "typescript-language-server")
	if _, err := os.Stat(tripwire); err != nil {
		t.Fatalf("tripwire fixture missing: %v", err)
	}

	if _, err := procx.NewResolver().Resolve(tripwire, repoRoot, false); err == nil {
		t.Fatal("the tripwire binary resolved successfully")
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("Resolve executed the tripwire: sentinel exists (%v)", err)
	}
}
