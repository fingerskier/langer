package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/workspace"
	"github.com/fingerskier/langer/protocol"
)

// newTestCore builds a Core over a real registry backed by the fake language
// server, with no socket in the way.
func newTestCore(t *testing.T) (*Core, string) {
	t.Helper()

	root := fixtureRoot(t)
	cfg := testConfig(t)
	fake := newFakeLSP(t, func(_ int, s *fakeSession) {
		s.symbols = `[{"name":"User","kind":11,"range":{"start":{"line":0,"character":0},"end":{"line":3,"character":1}},` +
			`"selectionRange":{"start":{"line":0,"character":17},"end":{"line":0,"character":21}}}]`
	})

	reg := workspace.NewRegistry(workspace.RegistryOptions{
		Config:   cfg,
		Clock:    clock.New(),
		Logger:   testLogger(t),
		Resolver: fake,
		Runner:   fake,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = reg.Shutdown(ctx)
	})
	return NewCore(reg, clock.New()), root
}

// TestCoreImplementsService is compile-time in core.go; this pins the runtime
// behaviour that matters: an unopened workspace is WORKSPACE_UNKNOWN on every
// method, not a nil dereference.
func TestUnknownWorkspaceIsRefusedByEveryMethod(t *testing.T) {
	core, _ := newTestCore(t)
	ctx := testContext(t)
	const ws = protocol.WorkspaceID("ws-never-opened")

	calls := map[string]func() error{
		"open_document": func() error {
			_, err := core.OpenDocument(ctx, protocol.OpenDocumentParams{DocumentParams: protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"}})
			return err
		},
		"close_document": func() error {
			_, err := core.CloseDocument(ctx, protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"})
			return err
		},
		"get_definition": func() error {
			_, err := core.GetDefinition(ctx, protocol.PositionParams{DocumentParams: protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"}})
			return err
		},
		"get_references": func() error {
			_, err := core.GetReferences(ctx, protocol.PositionParams{DocumentParams: protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"}})
			return err
		},
		"get_hover": func() error {
			_, err := core.GetHover(ctx, protocol.PositionParams{DocumentParams: protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"}})
			return err
		},
		"document_symbols": func() error {
			_, err := core.DocumentSymbols(ctx, protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"})
			return err
		},
		"workspace_symbols": func() error {
			_, err := core.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolsParams{Session: "s", Workspace: ws, Query: "User"})
			return err
		},
		"get_diagnostics": func() error {
			_, err := core.GetDiagnostics(ctx, protocol.DiagnosticsParams{Session: "s", Workspace: ws})
			return err
		},
		"rename_symbol": func() error {
			_, err := core.RenameSymbol(ctx, protocol.RenameParams{PositionParams: protocol.PositionParams{DocumentParams: protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"}}, NewName: "X"})
			return err
		},
		"apply_edit": func() error {
			_, err := core.ApplyEdit(ctx, protocol.ApplyEditParams{Session: "s", Workspace: ws, EditToken: "tok"})
			return err
		},
		"simulate_edit": func() error {
			_, err := core.SimulateEdit(ctx, protocol.SimulateEditParams{DocumentParams: protocol.DocumentParams{Session: "s", Workspace: ws, Path: "src/user.ts"}})
			return err
		},
		"index_status": func() error {
			_, err := core.IndexStatus(ctx, protocol.IndexStatusParams{Session: "s", Workspace: ws})
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			wantCode(t, call(), protocol.ErrWorkspaceUnknown)
		})
	}
}

// TestEndSessionOnAnUnknownSessionSucceeds: it is the state the caller asked
// for. Making it an error would mean every disconnect logs a failure.
func TestEndSessionOnAnUnknownSessionSucceeds(t *testing.T) {
	core, _ := newTestCore(t)
	if _, err := core.EndSession(testContext(t), protocol.EndSessionParams{Session: "never-seen"}); err != nil {
		t.Errorf("EndSession on an unknown session = %v, want nil", err)
	}
}

// TestCloseWorkspaceIsIdempotent for the same reason.
func TestCloseWorkspaceIsIdempotent(t *testing.T) {
	core, root := newTestCore(t)
	ctx := testContext(t)

	opened, err := core.OpenWorkspace(ctx, protocol.OpenWorkspaceParams{Session: "s", Root: root})
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := core.CloseWorkspace(ctx, protocol.CloseWorkspaceParams{Session: "s", Workspace: opened.Workspace}); err != nil {
			t.Fatalf("CloseWorkspace #%d: %v", i+1, err)
		}
	}
}

// TestOpenWorkspaceIsIdempotentAndReportsTheCanonicalRoot.
func TestOpenWorkspaceIsIdempotentAndReportsTheCanonicalRoot(t *testing.T) {
	core, root := newTestCore(t)
	ctx := testContext(t)

	first, err := core.OpenWorkspace(ctx, protocol.OpenWorkspaceParams{Session: "alice", Root: root})
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	second, err := core.OpenWorkspace(ctx, protocol.OpenWorkspaceParams{Session: "bob", Root: root})
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	if first.Workspace != second.Workspace {
		t.Errorf("two sessions got different workspace ids: %q and %q", first.Workspace, second.Workspace)
	}
	if first.Root != root {
		t.Errorf("root = %q, want %q", first.Root, root)
	}
}

// TestDaemonRefusesAForeignRoot: one daemon per repo (SPEC §3.2). Hosting a
// second root here would give it language servers under another project's
// socket, and nothing downstream would notice.
func TestDaemonRefusesAForeignRoot(t *testing.T) {
	h := startDaemon(t, nil, nil)
	client, _ := h.session("alice")

	other := fixtureRoot(t)
	_, err := client.OpenWorkspace(testContext(t), protocol.OpenWorkspaceParams{Session: "alice", Root: other})
	wantCode(t, err, protocol.ErrWorkspaceUnknown)
}

// TestIndexStatusReportsTheDaemonsView.
func TestIndexStatusReportsTheDaemonsView(t *testing.T) {
	h := startDaemon(t, nil, nil)
	client, ws := h.session("alice")

	status, err := client.IndexStatus(testContext(t), protocol.IndexStatusParams{Session: "alice", Workspace: ws})
	if err != nil {
		t.Fatalf("IndexStatus: %v", err)
	}
	if status.Root != h.root {
		t.Errorf("root = %q, want %q", status.Root, h.root)
	}
	if status.State != protocol.IndexIdle {
		t.Errorf("state = %q, want %q — M2 has no index and must not claim otherwise", status.State, protocol.IndexIdle)
	}
	// SPEC §3.5: index_status must never have the side effect of spawning a
	// language server.
	if h.lsp.startCount() != 0 {
		t.Errorf("index_status started %d language servers", h.lsp.startCount())
	}
}
