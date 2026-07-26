package wire_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

const root = "/repo"

func TestPathToURIAndBack(t *testing.T) {
	t.Parallel()
	uri := wire.PathToURI(root, "src/user.ts")
	if uri != "file:///repo/src/user.ts" {
		t.Fatalf("PathToURI = %q", uri)
	}
	rel, ok := wire.URIToPath(root, uri)
	if !ok || rel != "src/user.ts" {
		t.Fatalf("URIToPath = (%q, %v)", rel, ok)
	}
}

// Paths on the wire are slash-separated, workspace-relative and never absolute.
func TestURIToPathIsSlashSeparated(t *testing.T) {
	t.Parallel()
	rel, ok := wire.URIToPath(root, "file:///repo/a/b/c.py")
	if !ok {
		t.Fatal("URIToPath refused an in-root URI")
	}
	if strings.Contains(rel, string(filepath.Separator)) && filepath.Separator != '/' {
		t.Fatalf("path %q is not slash-separated", rel)
	}
	if rel != "a/b/c.py" {
		t.Fatalf("rel = %q", rel)
	}
}

// SPEC §3.4: dependency files are never indexed, but a live query may
// legitimately resolve into one. The caller drops those, so URIToPath must say
// so rather than emitting a "../../.." path.
func TestURIToPathRejectsOutsideRoot(t *testing.T) {
	t.Parallel()
	for _, uri := range []string{
		"file:///elsewhere/x.ts",
		"file:///repo-evil/x.ts",
		"file:///usr/lib/node_modules/typescript/lib/lib.es5.d.ts",
	} {
		if rel, ok := wire.URIToPath(root, uri); ok {
			t.Errorf("URIToPath(%q) = %q, want ok=false", uri, rel)
		}
	}
}

func TestURIToPathRejectsNonFileScheme(t *testing.T) {
	t.Parallel()
	for _, uri := range []string{"untitled:Untitled-1", "http://example.com/x.ts", ""} {
		if _, ok := wire.URIToPath(root, uri); ok {
			t.Errorf("URIToPath accepted %q", uri)
		}
	}
}

func TestURIRoundTripWithSpacesAndUnicode(t *testing.T) {
	t.Parallel()
	rel := "src/my dir/🚀.ts"
	uri := wire.PathToURI(root, rel)
	if strings.Contains(uri, " ") {
		t.Fatalf("URI %q contains a raw space", uri)
	}
	back, ok := wire.URIToPath(root, uri)
	if !ok || back != rel {
		t.Fatalf("round trip = (%q, %v), want %q", back, ok, rel)
	}
}

func TestURIToPathRootItself(t *testing.T) {
	t.Parallel()
	if _, ok := wire.URIToPath(root, "file:///repo"); ok {
		t.Fatal("the root directory itself is not a file in the workspace")
	}
}

func TestToLocations(t *testing.T) {
	t.Parallel()
	content := "export interface User {\n  id: string;\n}\n\nexport function getUserById(id: string): User {\n"
	lines := wire.NewLineIndex(content)

	got, err := wire.ToLocations(root, []wire.RawLocation{{
		URI:   "file:///repo/src/user.ts",
		Range: wire.RawRange{Start: wire.RawPosition{Line: 4, Character: 16}, End: wire.RawPosition{Line: 4, Character: 27}},
	}}, lines)
	if err != nil {
		t.Fatalf("ToLocations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d locations, want 1", len(got))
	}
	want := protocol.Location{
		Path: "src/user.ts",
		Range: protocol.Range{
			Start: protocol.Position{Line: 4, Character: 16},
			End:   protocol.Position{Line: 4, Character: 27},
		},
		Preview: "export function getUserById(id: string): User {",
	}
	if got[0] != want {
		t.Fatalf("location = %+v, want %+v", got[0], want)
	}
}

func TestToLocationsDropsOutOfRootResults(t *testing.T) {
	t.Parallel()
	got, err := wire.ToLocations(root, []wire.RawLocation{
		{URI: "file:///repo/a.ts"},
		{URI: "file:///usr/lib/lib.d.ts"},
	}, wire.NewLineIndex(""))
	if err != nil {
		t.Fatalf("ToLocations: %v", err)
	}
	if len(got) != 1 || got[0].Path != "a.ts" {
		t.Fatalf("locations = %+v", got)
	}
}

func TestFlattenSymbolsDerivesContainerFromTheParentChain(t *testing.T) {
	t.Parallel()
	raws := []wire.RawSymbol{{
		Name: "UserService", Kind: 5,
		Range:          wire.RawRange{Start: wire.RawPosition{Line: 0}, End: wire.RawPosition{Line: 20}},
		SelectionRange: wire.RawRange{Start: wire.RawPosition{Line: 0, Character: 6}},
		Children: []wire.RawSymbol{{
			Name: "getUserById", Kind: 6, Detail: "(id: string) => User",
			Range:          wire.RawRange{Start: wire.RawPosition{Line: 41}, End: wire.RawPosition{Line: 48, Character: 1}},
			SelectionRange: wire.RawRange{Start: wire.RawPosition{Line: 41, Character: 2}},
		}},
	}}

	got := wire.FlattenSymbols(root, "src/user/service.ts", raws)
	if len(got) != 2 {
		t.Fatalf("got %d symbols, want 2 (flat list, docs §10.4)", len(got))
	}
	if got[0].Name != "UserService" || got[0].Container != "" {
		t.Fatalf("parent = %+v", got[0])
	}
	child := got[1]
	if child.Name != "getUserById" || child.Container != "UserService" {
		t.Fatalf("child = %+v", child)
	}
	if child.Kind != protocol.SymbolKindMethod {
		t.Fatalf("kind = %q, want method", child.Kind)
	}
	if child.Path != "src/user/service.ts" {
		t.Fatalf("path = %q", child.Path)
	}
	if child.Detail != "(id: string) => User" {
		t.Fatalf("detail = %q", child.Detail)
	}
	if child.Range.Start.Line != 41 || child.Range.End.Line != 48 {
		t.Fatalf("range = %+v, want the full declaration range", child.Range)
	}
}

func TestFlattenSymbolsForIndexRetainsSelectionRange(t *testing.T) {
	t.Parallel()
	raws := []wire.RawSymbol{{
		Name: "UserService", Kind: 5,
		Range: wire.RawRange{
			Start: wire.RawPosition{Line: 3, Character: 0},
			End:   wire.RawPosition{Line: 20, Character: 1},
		},
		SelectionRange: wire.RawRange{
			Start: wire.RawPosition{Line: 3, Character: 6},
			End:   wire.RawPosition{Line: 3, Character: 17},
		},
		Children: []wire.RawSymbol{{
			Name: "getUserById", Kind: 6,
			Range: wire.RawRange{
				Start: wire.RawPosition{Line: 8, Character: 1},
				End:   wire.RawPosition{Line: 12, Character: 2},
			},
			SelectionRange: wire.RawRange{
				Start: wire.RawPosition{Line: 8, Character: 9},
				End:   wire.RawPosition{Line: 8, Character: 20},
			},
		}},
	}}

	got := wire.FlattenSymbolsForIndex(root, "src/user/service.ts", raws)
	if len(got) != 2 {
		t.Fatalf("got %d symbols, want 2", len(got))
	}
	if got[0].Symbol.Name != "UserService" {
		t.Fatalf("parent = %+v", got[0])
	}
	if got[0].SelectionRange != (protocol.Range{
		Start: protocol.Position{Line: 3, Character: 6},
		End:   protocol.Position{Line: 3, Character: 17},
	}) {
		t.Fatalf("parent selection range = %+v", got[0].SelectionRange)
	}
	if got[1].Symbol.Container != "UserService" {
		t.Fatalf("child = %+v", got[1])
	}
	if got[1].SelectionRange != (protocol.Range{
		Start: protocol.Position{Line: 8, Character: 9},
		End:   protocol.Position{Line: 8, Character: 20},
	}) {
		t.Fatalf("child selection range = %+v", got[1].SelectionRange)
	}

	public := wire.FlattenSymbols(root, "src/user/service.ts", raws)
	if len(public) != len(got) || public[1] != got[1].Symbol {
		t.Fatalf("public symbols diverged from detailed normalization:\npublic=%+v\ndetailed=%+v", public, got)
	}
}

// workspace/symbol results are SymbolInformation and carry their own URI; the
// relPath argument must not override it.
func TestFlattenSymbolsUsesTheSymbolsOwnURI(t *testing.T) {
	t.Parallel()
	got := wire.FlattenSymbols(root, "ignored.ts", []wire.RawSymbol{{
		Name: "describeUser", Kind: 12, URI: "file:///repo/src/service.ts",
		Range: wire.RawRange{Start: wire.RawPosition{Line: 5}, End: wire.RawPosition{Line: 8, Character: 1}},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d symbols", len(got))
	}
	if got[0].Path != "src/service.ts" {
		t.Fatalf("path = %q, want the symbol's own URI", got[0].Path)
	}
}

func TestFlattenSymbolsDropsOutOfRootSymbols(t *testing.T) {
	t.Parallel()
	got := wire.FlattenSymbols(root, "a.ts", []wire.RawSymbol{
		{Name: "inside", Kind: 12, URI: "file:///repo/a.ts"},
		{Name: "outside", Kind: 12, URI: "file:///usr/lib/lib.d.ts"},
	})
	if len(got) != 1 || got[0].Name != "inside" {
		t.Fatalf("symbols = %+v", got)
	}
}

func TestFlattenSymbolsKeepsExplicitContainerName(t *testing.T) {
	t.Parallel()
	got := wire.FlattenSymbols(root, "user.py", []wire.RawSymbol{{
		Name: "user_id", Kind: 13, Container: "get_user_by_id", URI: "file:///repo/user.py",
	}})
	if got[0].Container != "get_user_by_id" {
		t.Fatalf("container = %q", got[0].Container)
	}
}

func TestToDiagnostics(t *testing.T) {
	t.Parallel()
	got, err := wire.ToDiagnostics(root, []wire.RawDiagnostic{{
		URI:      "file:///repo/src/broken.ts",
		Range:    wire.RawRange{Start: wire.RawPosition{Line: 7, Character: 34}, End: wire.RawPosition{Line: 7, Character: 45}},
		Severity: 1,
		Code:     "2339",
		Source:   "typescript",
		Message:  "Property 'missingProp' does not exist on type 'Widget'.",
	}})
	if err != nil {
		t.Fatalf("ToDiagnostics: %v", err)
	}
	want := protocol.Diagnostic{
		Path:     "src/broken.ts",
		Severity: protocol.SeverityError,
		Code:     "2339",
		Source:   "typescript",
		Message:  "Property 'missingProp' does not exist on type 'Widget'.",
		Range: protocol.Range{
			Start: protocol.Position{Line: 7, Character: 34},
			End:   protocol.Position{Line: 7, Character: 45},
		},
	}
	if got[0] != want {
		t.Fatalf("diagnostic = %+v, want %+v", got[0], want)
	}
}

func TestToDiagnosticsSeverityMapping(t *testing.T) {
	t.Parallel()
	for severity, want := range map[int]protocol.Severity{
		1: protocol.SeverityError,
		2: protocol.SeverityWarning,
		3: protocol.SeverityInformation,
		4: protocol.SeverityHint,
		0: protocol.SeverityError, // absent severity must not drop below notice
	} {
		got, err := wire.ToDiagnostics(root, []wire.RawDiagnostic{{URI: "file:///repo/a.ts", Severity: severity}})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Severity != want {
			t.Errorf("severity %d → %q, want %q", severity, got[0].Severity, want)
		}
	}
}

// GOLDEN — captured payloads from testdata/README.md §1.6.
func TestSplitHoverTypeScriptDefinition(t *testing.T) {
	t.Parallel()
	h := wire.SplitHover("\n```typescript\nfunction getUserById(id: string): User\n```\n", nil)
	if h == nil {
		t.Fatal("SplitHover returned nil")
	}
	if want := "function getUserById(id: string): User"; h.Contents != want {
		t.Fatalf("contents = %q, want %q", h.Contents, want)
	}
	if h.Documentation != "" {
		t.Fatalf("documentation = %q, want empty (no doc comment in the TS fixture)", h.Documentation)
	}
}

func TestSplitHoverTypeScriptAliasKeepsBothLines(t *testing.T) {
	t.Parallel()
	h := wire.SplitHover("\n```typescript\n(alias) getUserById(id: string): User\nimport getUserById\n```\n", nil)
	want := "(alias) getUserById(id: string): User\nimport getUserById"
	if h.Contents != want {
		t.Fatalf("contents = %q, want %q", h.Contents, want)
	}
}

// GOLDEN — testdata/README.md §2.6. Pyright puts the docstring after a --- rule;
// that is where SPEC §4.4's Hover.documentation comes from.
func TestSplitHoverPythonSplitsDocumentation(t *testing.T) {
	t.Parallel()
	h := wire.SplitHover("```python\n(function) def get_user_by_id(user_id: str) -> User\n```\n---\nReturn a User for the given id.", nil)
	if want := "(function) def get_user_by_id(user_id: str) -> User"; h.Contents != want {
		t.Fatalf("contents = %q, want %q", h.Contents, want)
	}
	if want := "Return a User for the given id."; h.Documentation != want {
		t.Fatalf("documentation = %q, want %q", h.Documentation, want)
	}
}

func TestSplitHoverPythonClassHasNoDocumentation(t *testing.T) {
	t.Parallel()
	h := wire.SplitHover("```python\n(class) User\n```", nil)
	if h.Contents != "(class) User" || h.Documentation != "" {
		t.Fatalf("hover = %+v", h)
	}
}

func TestSplitHoverWithoutAFencedBlock(t *testing.T) {
	t.Parallel()
	h := wire.SplitHover("plain prose, no fences", nil)
	if h.Contents != "plain prose, no fences" || h.Documentation != "" {
		t.Fatalf("hover = %+v", h)
	}
}

func TestSplitHoverEmptyIsNil(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "\n", "   \n\n"} {
		if h := wire.SplitHover(in, nil); h != nil {
			t.Fatalf("SplitHover(%q) = %+v, want nil", in, h)
		}
	}
}

func TestSplitHoverCarriesRange(t *testing.T) {
	t.Parallel()
	rng := &protocol.Range{
		Start: protocol.Position{Line: 5, Character: 16},
		End:   protocol.Position{Line: 5, Character: 27},
	}
	h := wire.SplitHover("```ts\nx\n```", rng)
	if h.Range == nil || *h.Range != *rng {
		t.Fatalf("range = %+v, want %+v", h.Range, rng)
	}
}

func TestURIToPathHandlesSymlinkedRoots(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	// macOS temp dirs live under /var → /private/var. A language server may
	// report either spelling; both must resolve to the same relative path.
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == dir {
		t.Skip("temp dir is not behind a symlink on this machine")
	}
	rel, ok := wire.URIToPath(dir, "file://"+resolved+"/src/a.ts")
	if !ok || rel != "src/a.ts" {
		t.Fatalf("URIToPath(root=%q, uri under %q) = (%q, %v)", dir, resolved, rel, ok)
	}
}
