package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

// acquire spins up a harness with a scripted server and returns a ready Server.
func acquire(t *testing.T, setup func(s *scriptedServer), tune func(o *Options)) (*harness, Server) {
	t.Helper()
	h := newHarness(t, func(_ int, s *scriptedServer) {
		if setup != nil {
			setup(s)
		}
	}, tune)
	srv, err := h.sup.Acquire(testContext(t), "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return h, srv
}

func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// uriIn builds a file URI inside the harness root.
func uriIn(root, rel string) string { return wire.PathToURI(root, rel) }

// ---- capability gating ----

// SPEC §3.6 UNSUPPORTED: a capability the server never advertised must fail
// immediately, with NO round trip — not hang, not guess.
func TestCapabilityGatingReturnsUnsupportedWithoutARoundTrip(t *testing.T) {
	h, srv := acquire(t, func(s *scriptedServer) {
		s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
			// A server that provides hover and nothing else.
			return map[string]any{"capabilities": map[string]any{"hoverProvider": true}}, nil
		})
	}, nil)

	ctx := testContext(t)
	cases := []struct {
		name   string
		method string
		call   func() error
	}{
		{"definition", "textDocument/definition", func() error {
			_, err := srv.Definition(ctx, "a.ts", protocol.Position{})
			return err
		}},
		{"references", "textDocument/references", func() error {
			_, err := srv.References(ctx, "a.ts", protocol.Position{}, true)
			return err
		}},
		{"documentSymbol", "textDocument/documentSymbol", func() error {
			_, err := srv.DocumentSymbols(ctx, "a.ts")
			return err
		}},
		{"workspaceSymbol", "workspace/symbol", func() error {
			_, err := srv.WorkspaceSymbols(ctx, "q", 0)
			return err
		}},
		{"rename", "textDocument/rename", func() error {
			_, err := srv.Rename(ctx, "a.ts", protocol.Position{}, "x")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := structuredCode(t, tc.call()); code != protocol.ErrUnsupported {
				t.Fatalf("code = %s, want UNSUPPORTED", code)
			}
			if h.runner.server(1).sawMethod(tc.method) {
				t.Fatalf("%s was sent to a server that does not provide it", tc.method)
			}
		})
	}

	// The one capability it DOES advertise still works.
	if !srv.Supports(CapHover) {
		t.Fatal("hoverProvider was advertised but Supports says otherwise")
	}
}

// An options object counts as advertised; false and null do not.
func TestSupportsTreatsOptionsObjectsAsAdvertised(t *testing.T) {
	_, srv := acquire(t, func(s *scriptedServer) {
		s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
			return map[string]any{"capabilities": map[string]any{
				"renameProvider":         map[string]any{"prepareProvider": true},
				"definitionProvider":     false,
				"documentSymbolProvider": nil,
			}}, nil
		})
	}, nil)

	if !srv.Supports(CapRename) {
		t.Error("an options object was not treated as advertised")
	}
	if srv.Supports(CapDefinition) {
		t.Error("definitionProvider:false was treated as advertised")
	}
	if srv.Supports(CapDocumentSymbol) {
		t.Error("documentSymbolProvider:null was treated as advertised")
	}
	if srv.Supports("somethingNobodyAdvertised") {
		t.Error("an absent capability was treated as advertised")
	}
}

// docs/ARCHITECTURE.md §11 item 5: v0.1 implements the PUSH model only. A
// server advertising diagnosticProvider expects the 3.17 PULL model, and must
// get UNSUPPORTED rather than a silently empty result.
func TestPullModelServerGetsUnsupportedDiagnostics(t *testing.T) {
	_, srv := acquire(t, func(s *scriptedServer) {
		s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
			caps := defaultCapabilities()
			caps["diagnosticProvider"] = map[string]any{"interFileDependencies": true}
			return map[string]any{"capabilities": caps}, nil
		})
	}, nil)

	_, _, err := srv.Diagnostics(testContext(t), "a.ts", 0)
	if code := structuredCode(t, err); code != protocol.ErrUnsupported {
		t.Fatalf("code = %s, want UNSUPPORTED for a pull-model server", code)
	}
}

func TestPushModelServerSupportsDiagnostics(t *testing.T) {
	_, srv := acquire(t, nil, nil)
	if !srv.Supports(CapPushDiagnostics) {
		t.Fatal("a server with no diagnosticProvider must be treated as push-capable")
	}
}

// ---- result mapping ----

func TestDefinitionMapsLocationLinkAndMarksIsDefinition(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	writeFixture(t, h.root, "src/user.ts", "export interface User {}\n\nexport function getUserById(id: string): User {\n}\n")

	h.runner.server(1).handle("textDocument/definition", func(json.RawMessage) (any, *wire.RPCError) {
		return json.RawMessage(`[{
			"targetUri":"` + uriIn(h.root, "src/user.ts") + `",
			"targetRange":{"start":{"line":2,"character":0},"end":{"line":3,"character":1}},
			"targetSelectionRange":{"start":{"line":2,"character":16},"end":{"line":2,"character":27}}
		}]`), nil
	})

	got, err := srv.Definition(testContext(t), "src/service.ts", protocol.Position{Line: 6, Character: 21})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d locations, want 1", len(got))
	}
	want := protocol.Location{
		Path: "src/user.ts",
		Range: protocol.Range{
			Start: protocol.Position{Line: 2, Character: 16},
			End:   protocol.Position{Line: 2, Character: 27},
		},
		IsDefinition: true,
		Preview:      "export function getUserById(id: string): User {",
	}
	if got[0] != want {
		t.Fatalf("location =\n  %+v\nwant\n  %+v", got[0], want)
	}
}

// SPEC §3.4: a live query may resolve into a dependency. Those results are
// never ours to report, and must not surface as "../../.." paths.
func TestDefinitionDropsResultsOutsideTheWorkspace(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	writeFixture(t, h.root, "a.ts", "const x = 1;\n")

	h.runner.server(1).handle("textDocument/definition", func(json.RawMessage) (any, *wire.RPCError) {
		return json.RawMessage(`[
			{"uri":"file:///usr/lib/node_modules/typescript/lib/lib.es5.d.ts","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}},
			{"uri":"` + uriIn(h.root, "a.ts") + `","range":{"start":{"line":0,"character":6},"end":{"line":0,"character":7}}}
		]`), nil
	})

	got, err := srv.Definition(testContext(t), "a.ts", protocol.Position{})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(got) != 1 || got[0].Path != "a.ts" {
		t.Fatalf("locations = %+v, want only the in-workspace one", got)
	}
}

// LSP does not say which reference is the declaration; the bridge asks and
// marks it, so is_definition is a real field rather than a permanent false.
func TestReferencesMarksTheDeclaration(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	writeFixture(t, h.root, "src/user.ts", "\n\nexport function getUserById() {}\n")
	writeFixture(t, h.root, "src/service.ts", "import { getUserById } from './user';\n")

	userURI := uriIn(h.root, "src/user.ts")
	h.runner.server(1).handle("textDocument/references", func(json.RawMessage) (any, *wire.RPCError) {
		return json.RawMessage(`[
			{"uri":"` + userURI + `","range":{"start":{"line":2,"character":16},"end":{"line":2,"character":27}}},
			{"uri":"` + uriIn(h.root, "src/service.ts") + `","range":{"start":{"line":0,"character":9},"end":{"line":0,"character":20}}}
		]`), nil
	})
	h.runner.server(1).handle("textDocument/definition", func(json.RawMessage) (any, *wire.RPCError) {
		return json.RawMessage(`[{"uri":"` + userURI + `","range":{"start":{"line":2,"character":16},"end":{"line":2,"character":27}}}]`), nil
	})

	got, err := srv.References(testContext(t), "src/user.ts", protocol.Position{Line: 2, Character: 16}, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d references, want 2", len(got))
	}
	if !got[0].IsDefinition {
		t.Error("the declaration was not marked is_definition")
	}
	if got[1].IsDefinition {
		t.Error("a plain reference was marked is_definition")
	}
	if got[1].Preview != "import { getUserById } from './user';" {
		t.Errorf("preview = %q", got[1].Preview)
	}
}

func TestHoverSplitsContentsAndDocumentation(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	h.runner.server(1).handle("textDocument/hover", reply(`{
		"contents":{"kind":"markdown","value":"`+"```python\\n(function) def get_user_by_id(user_id: str) -> User\\n```\\n---\\nReturn a User for the given id."+`"},
		"range":{"start":{"line":9,"character":4},"end":{"line":9,"character":18}}
	}`))

	got, err := srv.Hover(testContext(t), "user.py", protocol.Position{Line: 9, Character: 4})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got == nil {
		t.Fatal("Hover returned nil for a real hover")
	}
	if got.Contents != "(function) def get_user_by_id(user_id: str) -> User" {
		t.Errorf("contents = %q", got.Contents)
	}
	if got.Documentation != "Return a User for the given id." {
		t.Errorf("documentation = %q", got.Documentation)
	}
	if got.Range == nil || got.Range.Start.Line != 9 || got.Range.End.Character != 18 {
		t.Errorf("range = %+v", got.Range)
	}
}

// Hovering a string literal returns null. That is "nothing here", not an error;
// the NO_RESULT decision belongs to the layer above (docs §10.7).
func TestHoverNullIsNilNotAnError(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	h.runner.server(1).handle("textDocument/hover", reply(`null`))

	got, err := srv.Hover(testContext(t), "a.ts", protocol.Position{})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got != nil {
		t.Fatalf("hover = %+v, want nil", got)
	}
}

func TestDocumentSymbolsFlattenWithContainer(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	h.runner.server(1).handle("textDocument/documentSymbol", reply(`[{
		"name":"User","kind":5,
		"range":{"start":{"line":3,"character":0},"end":{"line":6,"character":24}},
		"selectionRange":{"start":{"line":3,"character":6},"end":{"line":3,"character":10}},
		"children":[{
			"name":"__init__","kind":6,
			"range":{"start":{"line":4,"character":4},"end":{"line":6,"character":24}},
			"selectionRange":{"start":{"line":4,"character":8},"end":{"line":4,"character":16}}
		}]
	}]`))

	got, err := srv.DocumentSymbols(testContext(t), "user.py")
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d symbols, want a flat list of 2", len(got))
	}
	if got[0].Name != "User" || got[0].Kind != protocol.SymbolKindClass || got[0].Container != "" {
		t.Fatalf("parent = %+v", got[0])
	}
	if got[1].Name != "__init__" || got[1].Kind != protocol.SymbolKindMethod || got[1].Container != "User" {
		t.Fatalf("child = %+v", got[1])
	}
	if got[1].Path != "user.py" {
		t.Fatalf("path = %q", got[1].Path)
	}
}

func TestDocumentSymbolsForIndexRetainsSelectionRange(t *testing.T) {
	_, srv := acquire(t, func(s *scriptedServer) {
		s.handle("textDocument/documentSymbol", reply(`[{
			"name":"User","kind":5,
			"range":{"start":{"line":3,"character":0},"end":{"line":6,"character":24}},
			"selectionRange":{"start":{"line":3,"character":6},"end":{"line":3,"character":10}},
			"children":[{
				"name":"__init__","kind":6,
				"range":{"start":{"line":4,"character":4},"end":{"line":6,"character":24}},
				"selectionRange":{"start":{"line":4,"character":8},"end":{"line":4,"character":16}}
			}]
		}]`))
	}, nil)

	got, err := srv.DocumentSymbolsForIndex(testContext(t), "user.py")
	if err != nil {
		t.Fatalf("DocumentSymbolsForIndex: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d symbols, want 2", len(got))
	}
	if got[0].Name != "User" || got[0].Path != "user.py" {
		t.Fatalf("parent = %+v", got[0])
	}
	if got[0].SelectionRange != (protocol.Range{
		Start: protocol.Position{Line: 3, Character: 6},
		End:   protocol.Position{Line: 3, Character: 10},
	}) {
		t.Fatalf("parent selection range = %+v", got[0].SelectionRange)
	}
	if got[1].Name != "__init__" || got[1].Container != "User" {
		t.Fatalf("child = %+v", got[1])
	}
	if got[1].SelectionRange.Start != (protocol.Position{Line: 4, Character: 8}) {
		t.Fatalf("child selection start = %+v", got[1].SelectionRange.Start)
	}
}

// SymbolInformation is equally legal, and its own URI wins over the requested
// path — that is what workspace/symbol returns.
func TestDocumentSymbolsAcceptSymbolInformation(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	h.runner.server(1).handle("textDocument/documentSymbol", func(json.RawMessage) (any, *wire.RPCError) {
		return json.RawMessage(`[{
			"name":"describeUser","kind":12,"containerName":"",
			"location":{"uri":"` + uriIn(h.root, "src/service.ts") + `","range":{"start":{"line":5,"character":0},"end":{"line":8,"character":1}}}
		}]`), nil
	})

	got, err := srv.DocumentSymbols(testContext(t), "src/service.ts")
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(got) != 1 || got[0].Path != "src/service.ts" || got[0].Kind != protocol.SymbolKindFunction {
		t.Fatalf("symbols = %+v", got)
	}
}

func TestWorkspaceSymbolsRespectTheLimit(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	h.runner.server(1).handle("workspace/symbol", func(json.RawMessage) (any, *wire.RPCError) {
		items := make([]any, 0, 10)
		for i := 0; i < 10; i++ {
			items = append(items, map[string]any{
				"name": "sym", "kind": 12,
				"location": map[string]any{
					"uri":   uriIn(h.root, "a.ts"),
					"range": map[string]any{"start": map[string]any{"line": i, "character": 0}, "end": map[string]any{"line": i, "character": 3}},
				},
			})
		}
		return items, nil
	})

	got, err := srv.WorkspaceSymbols(testContext(t), "sym", 3)
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d symbols, want the limit of 3", len(got))
	}
}

func TestRenameMapsAWorkspaceEdit(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	h.runner.server(1).handle("textDocument/rename", func(json.RawMessage) (any, *wire.RPCError) {
		return json.RawMessage(`{"changes":{"` + uriIn(h.root, "src/user.ts") + `":[
			{"range":{"start":{"line":5,"character":16},"end":{"line":5,"character":27}},"newText":"findUserById"}
		]}}`), nil
	})

	got, err := srv.Rename(testContext(t), "src/user.ts", protocol.Position{Line: 5, Character: 16}, "findUserById")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if len(got) != 1 || got[0].Path != "src/user.ts" || len(got[0].Edits) != 1 {
		t.Fatalf("edits = %+v", got)
	}
	if got[0].Edits[0].NewText != "findUserById" {
		t.Fatalf("newText = %q", got[0].Edits[0].NewText)
	}
}

// ---- document synchronisation ----

func TestOpenIsIdempotent(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	for i := 0; i < 3; i++ {
		if _, err := srv.Open(ctx, "a.ts", "typescript", "const x = 1;"); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
	}
	waitFor(t, "didOpen", func() bool { return h.runner.server(1).sawMethod("textDocument/didOpen") })
	if got := h.runner.server(1).countMethod("textDocument/didOpen"); got != 1 {
		t.Fatalf("%d didOpen notifications for 3 Opens, want 1", got)
	}
}

func TestCloseSendsDidCloseOnceAndToleratesUnopened(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	// Closing a document that was never open is not an error.
	if err := srv.Close(ctx, "never-opened.ts"); err != nil {
		t.Fatalf("Close on an unopened document: %v", err)
	}
	if got := h.runner.server(1).countMethod("textDocument/didClose"); got != 0 {
		t.Fatalf("%d didClose notifications for an unopened document", got)
	}

	if _, err := srv.Open(ctx, "a.ts", "typescript", "x"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := srv.Close(ctx, "a.ts"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "didClose", func() bool { return h.runner.server(1).sawMethod("textDocument/didClose") })
	if err := srv.Close(ctx, "a.ts"); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := h.runner.server(1).countMethod("textDocument/didClose"); got != 1 {
		t.Fatalf("%d didClose notifications, want 1", got)
	}
}

// WithText is SPEC §4.2 overlay isolation. The base text MUST come back, or
// every later answer for the file is computed against somebody's speculation.
func TestWithTextRestoresTheBaseText(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	const base = "const x = 1;"
	if _, err := srv.Open(ctx, "a.ts", "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}

	var seen []string
	h.runner.server(1).handle("noop", reply(`null`))

	err := srv.WithText(ctx, "a.ts", "const x = 2;", func(context.Context, uint64) error {
		text, _, _, _ := h.docsFor(srv).state("a.ts")
		seen = append(seen, text)
		return nil
	})
	if err != nil {
		t.Fatalf("WithText: %v", err)
	}

	if len(seen) != 1 || seen[0] != "const x = 2;" {
		t.Fatalf("inside WithText the server's text was %v, want the overlay", seen)
	}
	text, _, _, _ := h.docsFor(srv).state("a.ts")
	if text != base {
		t.Fatalf("after WithText the text is %q, want the base %q", text, base)
	}
}

// The restore must happen even when the callback fails.
func TestWithTextRestoresAfterAFailedCallback(t *testing.T) {
	_, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	const base = "const x = 1;"
	if _, err := srv.Open(ctx, "a.ts", "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}

	wantErr := protocol.NewError(protocol.ErrInternal, "callback exploded")
	if err := srv.WithText(ctx, "a.ts", "overlay", func(context.Context, uint64) error { return wantErr }); err == nil {
		t.Fatal("WithText swallowed the callback error")
	}

	impl := srv.(*server)
	text, _, _, _ := impl.docs.state("a.ts")
	if text != base {
		t.Fatalf("text = %q after a failed callback, want the base %q", text, base)
	}
}

func TestWithTextRestoresWhenOverlayPushCrashes(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	const base = "const x = 1;"
	const overlay = "const x = 2;"
	releaseReads := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseReads)
		}
	}()
	paused := h.runner.server(1).pauseAfter("textDocument/didOpen", releaseReads)
	if _, err := srv.Open(ctx, "a.ts", "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}
	<-paused

	callbackCalled := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- srv.WithText(ctx, "a.ts", overlay, func(context.Context, uint64) error {
			callbackCalled <- struct{}{}
			return nil
		})
	}()

	impl := srv.(*server)
	waitFor(t, "the speculative local state before the blocked didChange", func() bool {
		text, _, _, _ := impl.docs.state("a.ts")
		return text == overlay
	})
	h.runner.process(1).crash()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WithText did not return after its notification connection crashed")
	}
	close(releaseReads)
	released = true

	if code := structuredCode(t, err); code != protocol.ErrServerCrashed {
		t.Fatalf("WithText push failure returned %s, want SERVER_CRASHED", code)
	}
	select {
	case <-callbackCalled:
		t.Fatal("WithText ran its callback after the overlay push failed")
	default:
	}
	text, _, _, open := impl.docs.state("a.ts")
	if !open || text != base {
		t.Fatalf("state after failed overlay push = (%q, open=%v), want restored base %q", text, open, base)
	}
}

// Versions must be strictly increasing for the life of the process: a server
// that sees a version go backwards may silently keep its old view of the file.
func TestDocumentVersionsAreStrictlyIncreasing(t *testing.T) {
	_, srv := acquire(t, nil, nil)
	ctx := testContext(t)
	impl := srv.(*server)

	if _, err := srv.Open(ctx, "a.ts", "typescript", "one"); err != nil {
		t.Fatal(err)
	}
	_, v1, _, _ := impl.docs.state("a.ts")

	if err := srv.WithText(ctx, "a.ts", "two", func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, v2, _, _ := impl.docs.state("a.ts")

	if v2 <= v1 {
		t.Fatalf("version went from %d to %d; it must strictly increase", v1, v2)
	}
}

// A speculative edit asks for diagnostics from INSIDE the document lock it
// already holds. Without the re-entrancy marker the two SPEC requirements
// deadlock against each other (docs §6.6).
func TestWithTextCallbackCanAskForDiagnostics(t *testing.T) {
	h, srv := acquire(t, nil, func(o *Options) {
		o.SettleQuiet = 100 * time.Millisecond
		o.SettleBudget = time.Second
	})
	ctx := testContext(t)

	if _, err := srv.Open(ctx, "a.ts", "typescript", "const x = 1;"); err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- srv.WithText(ctx, "a.ts", "const x: string = 1;", func(inner context.Context, epoch uint64) error {
			diags, _, err := srv.Diagnostics(inner, "a.ts", epoch)
			if err != nil {
				return err
			}
			if len(diags) != 1 {
				t.Errorf("got %d diagnostics inside the overlay, want 1", len(diags))
			}
			return nil
		})
	}()

	// Let the overlay's didChange land, then answer it.
	waitFor(t, "didChange", func() bool { return h.runner.server(1).sawMethod("textDocument/didChange") })
	h.runner.server(1).publishDiagnostics(uriIn(h.root, "a.ts"), []any{map[string]any{
		"range":    map[string]any{"start": map[string]any{"line": 0, "character": 6}, "end": map[string]any{"line": 0, "character": 7}},
		"severity": 1, "code": 2322, "source": "typescript",
		"message": "Type 'number' is not assignable to type 'string'.",
	}})
	advanceUntil(t, h.clock, time.Second, 25*time.Millisecond, func() bool { return len(done) > 0 })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithText: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("simulate_edit deadlocked against its own diagnostics query")
	}
}

func TestWithTextCallbackContextCannotBypassLocksAfterReturn(t *testing.T) {
	_, srv := acquire(t, nil, nil)
	ctx := testContext(t)
	const path = "a.ts"
	if _, err := srv.Open(ctx, path, "typescript", "const x = 1;"); err != nil {
		t.Fatalf("Open: %v", err)
	}

	var callbackCtx context.Context
	if err := srv.WithText(ctx, path, "const x = 2;", func(inner context.Context, _ uint64) error {
		callbackCtx = inner
		return nil
	}); err != nil {
		t.Fatalf("WithText: %v", err)
	}

	impl := srv.(*server)
	if err := impl.mutations.writeLock(ctx); err != nil {
		t.Fatalf("lock publication gate: %v", err)
	}
	heldDoc, err := impl.docs.acquire(ctx, path)
	if err != nil {
		impl.mutations.writeUnlock()
		t.Fatalf("lock document: %v", err)
	}
	gateUnlocked := false
	docUnlocked := false
	defer func() {
		if !docUnlocked {
			impl.docs.release(heldDoc)
		}
		if !gateUnlocked {
			impl.mutations.writeUnlock()
		}
	}()

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- srv.Close(callbackCtx, path)
	}()
	waitFor(t, "captured callback context to reacquire the publication gate", func() bool {
		_, waitingReaders, _, writer := mutationGateState(srv)
		return writer && waitingReaders > 0
	})
	select {
	case err := <-closeDone:
		t.Fatalf("captured callback context bypassed an active publication gate: %v", err)
	default:
	}

	impl.mutations.writeUnlock()
	gateUnlocked = true
	waitFor(t, "captured callback context to reacquire the document lock", func() bool {
		readers, _, _, _ := mutationGateState(srv)
		return readers > 0
	})
	select {
	case err := <-closeDone:
		t.Fatalf("captured callback context bypassed an active document lock: %v", err)
	default:
	}
	impl.docs.release(heldDoc)
	docUnlocked = true
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after publication gate released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not resume after publication gate released")
	}
}

func TestWithDiskTextOpensAndClosesAPreviouslyClosedDocument(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	const disk = "export const indexed = true;"
	var callbackEpoch uint64
	if err := srv.WithDiskText(ctx, "a.ts", "typescript", disk, func(inner context.Context, epoch uint64) error {
		callbackEpoch = epoch
		if !holdsDocLock(inner, "a.ts") {
			t.Fatal("WithDiskText callback context does not mark the document lock held")
		}
		text, _, languageID, open := h.docsFor(srv).state("a.ts")
		if !open || text != disk || languageID != "typescript" {
			t.Fatalf("inside WithDiskText state = (%q, %q, %v)", text, languageID, open)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithDiskText: %v", err)
	}
	if callbackEpoch == 0 {
		t.Fatal("WithDiskText returned a zero diagnostics epoch")
	}

	scripted := h.runner.server(1)
	waitFor(t, "didOpen and didClose", func() bool {
		return scripted.countMethod("textDocument/didOpen") == 1 &&
			scripted.countMethod("textDocument/didClose") == 1
	})
	if got := notificationDocumentTexts(t, scripted, "textDocument/didOpen"); len(got) != 1 || got[0] != disk {
		t.Fatalf("didOpen texts = %q, want [%q]", got, disk)
	}
	text, _, languageID, open := h.docsFor(srv).state("a.ts")
	if open || text != "" || languageID != "" {
		t.Fatalf("after WithDiskText state = (%q, %q, %v), want original closed state", text, languageID, open)
	}
}

func TestWithDiskTextRestoresAPreviouslyOpenDocument(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	const base = "export const value = 'base';"
	const disk = "export const value = 'disk';"
	if _, err := srv.Open(ctx, "a.ts", "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := srv.WithDiskText(ctx, "a.ts", "typescriptreact", disk, func(context.Context, uint64) error {
		text, _, languageID, open := h.docsFor(srv).state("a.ts")
		if !open || text != disk || languageID != "typescript" {
			t.Fatalf("inside WithDiskText state = (%q, %q, %v)", text, languageID, open)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithDiskText: %v", err)
	}

	scripted := h.runner.server(1)
	waitFor(t, "disk change and base restore", func() bool {
		return scripted.countMethod("textDocument/didChange") == 2
	})
	if got := notificationDocumentTexts(t, scripted, "textDocument/didChange"); len(got) != 2 || got[0] != disk || got[1] != base {
		t.Fatalf("didChange texts = %q, want [%q %q]", got, disk, base)
	}
	if got := scripted.countMethod("textDocument/didClose"); got != 0 {
		t.Fatalf("didClose count = %d for a document that was already open", got)
	}
	text, _, languageID, open := h.docsFor(srv).state("a.ts")
	if !open || text != base || languageID != "typescript" {
		t.Fatalf("after WithDiskText state = (%q, %q, %v), want original open state", text, languageID, open)
	}
}

func TestWithDiskTextSameOpenTextUsesCurrentEpochWithoutNotifications(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	const (
		path = "a.ts"
		text = "export const value = 'disk';"
	)
	if _, err := srv.Open(ctx, path, "typescript", text); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scripted := h.runner.server(1)
	waitFor(t, "initial didOpen", func() bool {
		return scripted.countMethod("textDocument/didOpen") == 1
	})

	impl := srv.(*server)
	_, versionBefore, _, _ := impl.docs.state(path)
	impl.diags.publishVersioned(path, versionBefore, nil)
	wantEpoch := impl.diags.current(path)
	didChangeBefore := scripted.countMethod("textDocument/didChange")
	didCloseBefore := scripted.countMethod("textDocument/didClose")

	var callbackEpoch uint64
	if err := srv.WithDiskText(ctx, path, "TypeScript", text, func(inner context.Context, epoch uint64) error {
		callbackEpoch = epoch
		if !holdsDocLock(inner, path) {
			t.Fatal("same-text callback context does not mark the document lock held")
		}
		if got := impl.diags.current(path); epoch != got {
			t.Fatalf("callback epoch = %d, want current epoch %d", epoch, got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithDiskText: %v", err)
	}

	if callbackEpoch != wantEpoch {
		t.Fatalf("callback epoch = %d, want pre-existing current epoch %d", callbackEpoch, wantEpoch)
	}
	_, versionAfter, languageID, open := impl.docs.state(path)
	if !open || languageID != "typescript" {
		t.Fatalf("document state after same-text indexing = (language=%q, open=%v)", languageID, open)
	}
	if versionAfter != versionBefore {
		t.Fatalf("document version after same-text indexing = %d, want unchanged %d", versionAfter, versionBefore)
	}
	if got := scripted.countMethod("textDocument/didChange"); got != didChangeBefore {
		t.Fatalf("didChange count after same-text indexing = %d, want unchanged %d", got, didChangeBefore)
	}
	if got := scripted.countMethod("textDocument/didClose"); got != didCloseBefore {
		t.Fatalf("didClose count after same-text indexing = %d, want unchanged %d", got, didCloseBefore)
	}
}

func TestWithDiskTextSameTextDoesNotReuseRestoredOverlayDiagnostics(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)
	const (
		path = "a.ts"
		base = "export const value = 'base';"
	)
	if _, err := srv.Open(ctx, path, "typescript", base); err != nil {
		t.Fatalf("Open: %v", err)
	}

	impl := srv.(*server)
	var (
		overlayEpoch   uint64
		overlayVersion int
	)
	if err := srv.WithText(ctx, path, "export const value: string = 1;", func(context.Context, uint64) error {
		impl.diags.publish(path, []protocol.Diagnostic{{Message: "overlay-only error"}})
		overlayEpoch = impl.diags.current(path)
		_, overlayVersion, _, _ = impl.docs.state(path)
		return nil
	}); err != nil {
		t.Fatalf("WithText: %v", err)
	}
	if _, _, ok := impl.diags.snapshot(path); ok {
		t.Fatal("overlay diagnostics remained current after restoring base text")
	}

	scripted := h.runner.server(1)
	waitFor(t, "overlay change and base restore", func() bool {
		return scripted.countMethod("textDocument/didChange") == 2
	})
	scripted.publishDiagnosticsVersion(uriIn(h.root, path), overlayVersion, []any{map[string]any{
		"range": map[string]any{
			"start": map[string]any{"line": 0, "character": 0},
			"end":   map[string]any{"line": 0, "character": 1},
		},
		"message": "late overlay-only error",
	}})
	if _, err := scripted.request("workspace/configuration", map[string]any{"items": []any{}}, 5*time.Second); err != nil {
		t.Fatalf("barrier request after late overlay diagnostics: %v", err)
	}
	if _, _, ok := impl.diags.snapshot(path); ok {
		t.Fatal("late versioned overlay diagnostics were accepted for restored base text")
	}

	var diskEpoch uint64
	if err := srv.WithDiskText(ctx, path, "typescript", base, func(_ context.Context, epoch uint64) error {
		diskEpoch = epoch
		return nil
	}); err != nil {
		t.Fatalf("WithDiskText: %v", err)
	}
	if diskEpoch <= overlayEpoch {
		t.Fatalf("same-text disk epoch = %d, want future epoch after invalidated overlay %d", diskEpoch, overlayEpoch)
	}
	if got := scripted.countMethod("textDocument/didChange"); got != 2 {
		t.Fatalf("same-text disk indexing sent didChange; total = %d, want 2 from overlay and restore", got)
	}
}

func TestWithDiskTextSameTextDifferentLanguageDoesNotReuseDiagnostics(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	const (
		path = "a.ts"
		text = "export const value = 'disk';"
	)
	if _, err := srv.Open(ctx, path, "typescript", text); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scripted := h.runner.server(1)
	waitFor(t, "initial didOpen", func() bool {
		return scripted.countMethod("textDocument/didOpen") == 1
	})

	impl := srv.(*server)
	impl.diags.publish(path, nil)
	staleEpoch := impl.diags.current(path)

	var callbackEpoch uint64
	if err := srv.WithDiskText(ctx, path, "typescriptreact", text, func(_ context.Context, epoch uint64) error {
		callbackEpoch = epoch
		return nil
	}); err != nil {
		t.Fatalf("WithDiskText: %v", err)
	}
	if callbackEpoch <= staleEpoch {
		t.Fatalf("callback epoch = %d, want a future epoch after incompatible-language diagnostics %d", callbackEpoch, staleEpoch)
	}
	waitFor(t, "change and restore for incompatible language", func() bool {
		return scripted.countMethod("textDocument/didChange") == 2
	})
}

func TestWithDiskTextRestoresAfterAFailedCallback(t *testing.T) {
	h, srv := acquire(t, nil, nil)
	ctx := testContext(t)

	wantErr := protocol.NewError(protocol.ErrInternal, "index callback exploded")
	err := srv.WithDiskText(ctx, "a.ts", "typescript", "disk", func(context.Context, uint64) error {
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("WithDiskText error = %v, want callback error %v", err, wantErr)
	}

	scripted := h.runner.server(1)
	waitFor(t, "didClose after failed callback", func() bool {
		return scripted.countMethod("textDocument/didClose") == 1
	})
	text, _, languageID, open := h.docsFor(srv).state("a.ts")
	if open || text != "" || languageID != "" {
		t.Fatalf("after failed callback state = (%q, %q, %v), want original closed state", text, languageID, open)
	}
}

func TestWithDiskTextCallbackCanAskForDiagnostics(t *testing.T) {
	h, srv := acquire(t, nil, func(o *Options) {
		o.SettleQuiet = 100 * time.Millisecond
		o.SettleBudget = time.Second
	})
	ctx := testContext(t)

	done := make(chan error, 1)
	go func() {
		done <- srv.WithDiskText(ctx, "a.ts", "typescript", "const x: string = 1;", func(inner context.Context, epoch uint64) error {
			diags, _, err := srv.Diagnostics(inner, "a.ts", epoch)
			if err != nil {
				return err
			}
			if len(diags) != 1 {
				t.Errorf("got %d diagnostics while indexing, want 1", len(diags))
			}
			return nil
		})
	}()

	scripted := h.runner.server(1)
	waitFor(t, "index didOpen", func() bool {
		return scripted.sawMethod("textDocument/didOpen")
	})
	scripted.publishDiagnostics(uriIn(h.root, "a.ts"), []any{map[string]any{
		"range":    map[string]any{"start": map[string]any{"line": 0, "character": 6}, "end": map[string]any{"line": 0, "character": 7}},
		"severity": 1, "code": 2322, "source": "typescript",
		"message": "Type 'number' is not assignable to type 'string'.",
	}})
	advanceUntil(t, h.clock, time.Second, 25*time.Millisecond, func() bool { return len(done) > 0 })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithDiskText: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("index diagnostics deadlocked against the document lock")
	}
	waitFor(t, "didClose after index diagnostics", func() bool {
		return scripted.countMethod("textDocument/didClose") == 1
	})
}

// ---- diagnostics through the Server surface ----

func TestDiagnosticsThroughTheServerSurface(t *testing.T) {
	h, srv := acquire(t, nil, func(o *Options) {
		o.SettleQuiet = 100 * time.Millisecond
		o.SettleBudget = time.Second
	})
	ctx := testContext(t)

	epoch, err := srv.Open(ctx, "src/broken.ts", "typescript", "const w = {}; export const v = w.missingProp;")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	type result struct {
		diags []protocol.Diagnostic
		stale bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		diags, stale, err := srv.Diagnostics(ctx, "src/broken.ts", epoch)
		done <- result{diags, stale, err}
	}()

	waitFor(t, "didOpen", func() bool { return h.runner.server(1).sawMethod("textDocument/didOpen") })
	h.runner.server(1).publishDiagnostics(uriIn(h.root, "src/broken.ts"), []any{map[string]any{
		"range":    map[string]any{"start": map[string]any{"line": 7, "character": 34}, "end": map[string]any{"line": 7, "character": 45}},
		"severity": 1, "code": 2339, "source": "typescript",
		"message": "Property 'missingProp' does not exist on type 'Widget'.",
	}})
	advanceUntil(t, h.clock, time.Second, 25*time.Millisecond, func() bool { return len(done) > 0 })

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Diagnostics: %v", r.err)
		}
		if r.stale {
			t.Fatal("possiblyStale set for a settled result")
		}
		if len(r.diags) != 1 {
			t.Fatalf("got %d diagnostics, want 1", len(r.diags))
		}
		want := protocol.Diagnostic{
			Path:     "src/broken.ts",
			Severity: protocol.SeverityError,
			Code:     "2339", // verbatim, no synthesised TS prefix (docs §10.5)
			Source:   "typescript",
			Message:  "Property 'missingProp' does not exist on type 'Widget'.",
			Range: protocol.Range{
				Start: protocol.Position{Line: 7, Character: 34},
				End:   protocol.Position{Line: 7, Character: 45},
			},
		}
		if r.diags[0] != want {
			t.Fatalf("diagnostic =\n  %+v\nwant\n  %+v", r.diags[0], want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Diagnostics never returned")
	}
}

func TestDiagnosticsForADependencyFileAreDropped(t *testing.T) {
	h, srv := acquire(t, nil, func(o *Options) {
		o.SettleQuiet = 100 * time.Millisecond
		o.SettleBudget = 500 * time.Millisecond
	})
	ctx := testContext(t)

	if _, err := srv.Open(ctx, "a.ts", "typescript", "x"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	waitFor(t, "didOpen", func() bool { return h.runner.server(1).sawMethod("textDocument/didOpen") })

	// A push for a file outside the workspace must not become a diagnostic.
	h.runner.server(1).publishDiagnostics("file:///usr/lib/node_modules/typescript/lib/lib.es5.d.ts", []any{
		map[string]any{"range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}}, "message": "not ours"},
	})

	impl := srv.(*server)
	time.Sleep(50 * time.Millisecond)
	if _, _, ok := impl.diags.snapshot("../../usr/lib/node_modules/typescript/lib/lib.es5.d.ts"); ok {
		t.Fatal("a dependency file's diagnostics were recorded")
	}
}

// docsFor reaches the document store behind a Server handle. Only tests do
// this; production code has no reason to.
func (h *harness) docsFor(srv Server) *documents {
	return srv.(*server).docs
}

func notificationDocumentTexts(t *testing.T, srv *scriptedServer, method string) []string {
	t.Helper()
	params := srv.notificationParams(method)
	out := make([]string, 0, len(params))
	for _, raw := range params {
		var payload struct {
			TextDocument struct {
				Text string `json:"text"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %s params: %v", method, err)
		}
		if len(payload.ContentChanges) > 0 {
			out = append(out, payload.ContentChanges[0].Text)
			continue
		}
		out = append(out, payload.TextDocument.Text)
	}
	return out
}
