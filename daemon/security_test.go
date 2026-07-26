//go:build integration

package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/testutil"
	"github.com/fingerskier/langer/protocol"
)

// TestTripwireNeverExecutesWorkspaceLocalBinary is PLAN M6 / SPEC §9:
// opening the TS fixture (whose node_modules/.bin holds an executable that
// writes a sentinel if run) must never create that sentinel.
//
// The PATH is poisoned with the workspace .bin first — the standard Node
// attack — so a bridge that prepends project-local tools would fire the
// tripwire while looking correct in code review.
func TestTripwireNeverExecutesWorkspaceLocalBinary(t *testing.T) {
	root, err := filepath.EvalSymlinks(testutil.FixtureRoot(t, "ts-project"))
	if err != nil {
		t.Fatal(err)
	}

	sentinel := filepath.Join(t.TempDir(), "tripwire-sentinel")
	t.Setenv("LANGER_TRIPWIRE_SENTINEL", sentinel)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("sentinel already exists before the test")
	}

	poison := filepath.Join(root, "node_modules", ".bin")
	t.Setenv("PATH", poison+string(os.PathListSeparator)+os.Getenv("PATH"))

	h, _ := startRealDaemon(t)
	ctx := realContext(t)
	client := h.connect("tripwire")

	opened, err := client.OpenWorkspace(ctx, protocol.OpenWorkspaceParams{
		Session: "tripwire", Root: h.root,
	})
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	if _, err := client.OpenDocument(ctx, protocol.OpenDocumentParams{
		DocumentParams: protocol.DocumentParams{
			Session: "tripwire", Workspace: opened.Workspace, Path: "src/user.ts",
		},
		LanguageID: "typescript",
	}); err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}

	// Exercise a real semantic query so a language server is actually spawned.
	var defErr error
	for attempt := 0; attempt < 20; attempt++ {
		_, defErr = client.GetDefinition(ctx, protocol.PositionParams{
			DocumentParams: protocol.DocumentParams{
				Session: "tripwire", Workspace: opened.Workspace, Path: "src/user.ts",
			},
			Position: protocol.Position{Line: 5, Character: 16},
		})
		if defErr == nil {
			break
		}
		if protocol.AsError(defErr).Code != protocol.ErrNotReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if defErr != nil {
		t.Fatalf("GetDefinition: %v", defErr)
	}

	if data, err := os.ReadFile(sentinel); err == nil {
		t.Fatalf("SPEC §9 violation: workspace-local tripwire executed:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
	}
}

// TestAbsoluteTripwirePathIsRefusedWithoutOptIn covers the direct-path attack:
// a registry entry whose command is the fixture binary under the workspace.
func TestAbsoluteTripwirePathIsRefusedWithoutOptIn(t *testing.T) {
	root := testutil.FixtureRoot(t, "ts-project")
	sentinel := filepath.Join(t.TempDir(), "tripwire-sentinel")
	t.Setenv("LANGER_TRIPWIRE_SENTINEL", sentinel)

	tripwire := filepath.Join(root, "node_modules", ".bin", "typescript-language-server")
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(tripwire + ".cmd"); err == nil {
			tripwire += ".cmd"
		}
	}
	if _, err := os.Stat(tripwire); err != nil {
		t.Fatalf("tripwire missing: %v", err)
	}

	_, err := procx.NewResolver().Resolve(tripwire, root, false)
	if err == nil {
		t.Fatal("workspace-local tripwire path resolved without allow_workspace_local")
	}
	if protocol.AsError(err).Code != protocol.ErrInternal {
		t.Fatalf("code = %s, want INTERNAL (SPEC §9 refusal)", protocol.AsError(err).Code)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("Resolve executed the tripwire")
	}
}
