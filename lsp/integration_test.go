//go:build integration

// Integration tests for the M1 LSP client, driving the REAL
// typescript-language-server and pyright-langserver against the checked-in
// fixtures (SPEC §11.1).
//
// Every expected value comes from testdata/README.md, which was captured from
// those servers rather than from this implementation. When a value here and a
// real server disagree, one of the two is wrong and it must be investigated —
// never "fixed" by writing down whatever the code happened to produce.
//
// Run with: go test -tags=integration ./lsp/
package lsp_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/testutil"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/protocol"
)

// A real server takes seconds to initialise; pyright needs ~4s to first
// settled diagnostics.
const integrationTimeout = 120 * time.Second

type liveServer struct {
	srv   lsp.Server
	sup   lsp.Supervisor
	root  string
	files []string
}

// start brings up one real language server over a fixture and opens every
// source file, which is what makes cross-file references resolve.
func start(t *testing.T, fixture, serverName string, sources []string) *liveServer {
	t.Helper()

	entry := testutil.RequireLanguageServer(t, serverName)
	root := testutil.FixtureRoot(t, fixture)
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture %s missing: %v", fixture, err)
	}

	// The tripwire must stay silent for the whole run (SPEC §9). M6 asserts
	// this end to end; wiring it here means five milestones of process-spawning
	// code cannot quietly grow past the boundary.
	sentinel := filepath.Join(t.TempDir(), "tripwire-sentinel")
	t.Setenv("LANGER_TRIPWIRE_SENTINEL", sentinel)
	t.Cleanup(func() {
		if _, err := os.Stat(sentinel); err == nil {
			data, _ := os.ReadFile(sentinel)
			t.Fatalf("SPEC §9 violation: a workspace-local binary was executed:\n%s", data)
		}
	})

	sup, err := lsp.NewSupervisor(lsp.Options{
		Root:    root,
		Servers: []config.LanguageServer{entry},
		Clock:   clock.New(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := sup.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	t.Cleanup(cancel)

	srv, err := sup.Acquire(ctx, serverName)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", serverName, err)
	}

	live := &liveServer{srv: srv, sup: sup, root: root, files: sources}
	for _, rel := range sources {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		if _, err := srv.Open(ctx, rel, languageIDFor(rel), string(data)); err != nil {
			t.Fatalf("Open(%s): %v", rel, err)
		}
	}
	return live
}

func languageIDFor(rel string) string {
	switch filepath.Ext(rel) {
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".py":
		return "python"
	default:
		return "plaintext"
	}
}

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	t.Cleanup(cancel)
	return ctx
}

func at(line, character int) protocol.Position {
	return protocol.Position{Line: line, Character: character}
}

func rng(sl, sc, el, ec int) protocol.Range {
	return protocol.Range{Start: at(sl, sc), End: at(el, ec)}
}

// place renders a location as "path [l,c]-[l,c]" for comparison and messages.
func place(l protocol.Location) string {
	return l.Path + " " + span(l.Range)
}

func span(r protocol.Range) string {
	return "[" + itoa(r.Start.Line) + "," + itoa(r.Start.Character) + "]-[" +
		itoa(r.End.Line) + "," + itoa(r.End.Character) + "]"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func places(locations []protocol.Location) []string {
	out := make([]string, 0, len(locations))
	for _, l := range locations {
		out = append(out, place(l))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// settledDiagnostics retries until the server's analysis has actually settled.
//
// Each individual call stays inside the SPEC §4.3 ≤2 s budget; the retry loop
// is what a real agent does when it gets possibly_stale, and it is what lets
// pyright's several-second first pass finish.
func settledDiagnostics(t *testing.T, srv lsp.Server, path string, wantCount int) []protocol.Diagnostic {
	t.Helper()
	ctx := ctxFor(t)

	deadline := time.Now().Add(60 * time.Second)
	var last []protocol.Diagnostic
	for time.Now().Before(deadline) {
		diags, stale, err := srv.Diagnostics(ctx, path, 0)
		if err != nil {
			t.Fatalf("Diagnostics(%s): %v", path, err)
		}
		last = diags
		if !stale && len(diags) == wantCount {
			return diags
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("diagnostics for %s never settled at %d entries; last = %+v", path, wantCount, last)
	return last
}

// workspaceSymbols runs the query the way an agent is supposed to: NOT_READY
// means "the server is still analysing, ask again", not "no such symbol".
// pyright answers workspace/symbol with an empty array until its first analysis
// pass ends, so without this retry the query races the indexer.
func workspaceSymbols(t *testing.T, srv lsp.Server, query string) []protocol.Symbol {
	t.Helper()
	ctx := ctxFor(t)

	deadline := time.Now().Add(60 * time.Second)
	for {
		got, err := srv.WorkspaceSymbols(ctx, query, 0)
		if err == nil {
			return got
		}
		var perr *protocol.Error
		if !errors.As(err, &perr) || perr.Code != protocol.ErrNotReady {
			t.Fatalf("WorkspaceSymbols(%q): %v", query, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("WorkspaceSymbols(%q) never became ready: %v", query, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ============================================================================
// TypeScript — testdata/README.md §1
// ============================================================================

var tsSources = []string{
	"src/user.ts", "src/service.ts", "src/lookalike.ts", "src/unicode.ts", "src/broken.ts",
}

func startTS(t *testing.T) *liveServer { return start(t, "ts-project", "typescript", tsSources) }

func TestTSCapabilitiesAreCaptured(t *testing.T) {
	live := startTS(t)
	for _, capability := range []string{
		lsp.CapDefinition, lsp.CapReferences, lsp.CapHover,
		lsp.CapDocumentSymbol, lsp.CapWorkspaceSymbol, lsp.CapRename,
	} {
		if !live.srv.Supports(capability) {
			t.Errorf("typescript-language-server did not advertise %s", capability)
		}
	}
	if !live.srv.Supports(lsp.CapPushDiagnostics) {
		t.Error("typescript-language-server was classified as pull-model; v0.1 expects push")
	}
	if got := live.srv.Generation(); got != 1 {
		t.Errorf("Generation = %d, want 1", got)
	}
}

// testdata/README.md §1.3: the definition of getUserById is src/user.ts
// [5,16]-[5,27].
func TestTSDefinitionAcrossFiles(t *testing.T) {
	live := startTS(t)

	got, err := live.srv.Definition(ctxFor(t), "src/service.ts", at(6, 21))
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d definitions, want 1: %v", len(got), places(got))
	}
	if want := "src/user.ts " + span(rng(5, 16, 5, 27)); place(got[0]) != want {
		t.Fatalf("definition = %s, want %s", place(got[0]), want)
	}
	if !got[0].IsDefinition {
		t.Error("a definition result was not marked is_definition")
	}
	if want := "export function getUserById(id: string): User {"; got[0].Preview != want {
		t.Errorf("preview = %q, want %q", got[0].Preview, want)
	}
}

// testdata/README.md §1.3, verbatim: 6 results with includeDeclaration true,
// 5 with false, 3 real cross-file call sites in 2 files.
func TestTSReferenceGraph(t *testing.T) {
	live := startTS(t)
	ctx := ctxFor(t)

	got, err := live.srv.References(ctx, "src/user.ts", at(5, 16), true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	want := []string{
		"src/user.ts " + span(rng(5, 16, 5, 27)),
		"src/service.ts " + span(rng(0, 9, 0, 20)),
		"src/service.ts " + span(rng(6, 21, 6, 32)),
		"src/service.ts " + span(rng(11, 16, 11, 27)),
		"src/unicode.ts " + span(rng(0, 9, 0, 20)),
		"src/unicode.ts " + span(rng(5, 69, 5, 80)),
	}
	if !equalStrings(places(got), want) {
		t.Fatalf("references =\n  %v\nwant\n  %v", places(got), want)
	}

	// The declaration, and only the declaration, is marked.
	for i, l := range got {
		if (i == 0) != l.IsDefinition {
			t.Errorf("reference %d (%s) is_definition = %v", i, place(l), l.IsDefinition)
		}
	}

	withoutDecl, err := live.srv.References(ctx, "src/user.ts", at(5, 16), false)
	if err != nil {
		t.Fatalf("References(includeDeclaration=false): %v", err)
	}
	if len(withoutDecl) != 5 {
		t.Fatalf("got %d references without the declaration, want 5: %v", len(withoutDecl), places(withoutDecl))
	}
}

// testdata/README.md §1.4: a grep-based implementation returns all 10 textual
// occurrences. These are the ones that must NOT come back.
func TestTSTextualVersusSemantic(t *testing.T) {
	live := startTS(t)
	ctx := ctxFor(t)

	// Inside a // comment.
	if got, err := live.srv.Definition(ctx, "src/service.ts", at(2, 3)); err != nil || len(got) != 0 {
		t.Errorf("definition inside a comment = %v (err %v), want none", places(got), err)
	}
	// Inside a string literal.
	if got, err := live.srv.Definition(ctx, "src/service.ts", at(3, 14)); err != nil || len(got) != 0 {
		t.Errorf("definition inside a string = %v (err %v), want none", places(got), err)
	}
	if got, err := live.srv.Hover(ctx, "src/service.ts", at(3, 14)); err != nil || got != nil {
		t.Errorf("hover inside a string = %+v (err %v), want nil", got, err)
	}

	// A DIFFERENT symbol that merely shares the name: its reference set must
	// contain only itself, and must never leak into user.ts's.
	lookalike, err := live.srv.References(ctx, "src/lookalike.ts", at(3, 16), true)
	if err != nil {
		t.Fatalf("References(lookalike): %v", err)
	}
	want := []string{"src/lookalike.ts " + span(rng(3, 16, 3, 27))}
	if !equalStrings(places(lookalike), want) {
		t.Fatalf("lookalike references = %v, want %v", places(lookalike), want)
	}
}

// THE UTF-16 test (PLAN M1, testdata/README.md §1.5).
//
// src/unicode.ts line 5 puts eight non-BMP rockets before the symbols. The
// codepoint reading (61) is the dangerous one: it resolves a DIFFERENT symbol
// silently, so asserting non-emptiness would pass for the wrong reason.
func TestTSUTF16ColumnsResolveTheRightSymbol(t *testing.T) {
	live := startTS(t)
	ctx := ctxFor(t)

	correct, err := live.srv.Definition(ctx, "src/unicode.ts", at(5, 69))
	if err != nil {
		t.Fatalf("Definition at the UTF-16 column: %v", err)
	}
	if len(correct) != 1 {
		t.Fatalf("got %d definitions at column 69, want 1: %v", len(correct), places(correct))
	}
	if want := "src/user.ts " + span(rng(5, 16, 5, 27)); place(correct[0]) != want {
		t.Fatalf("definition at UTF-16 column 69 = %s, want %s", place(correct[0]), want)
	}

	// Codepoint offset: a plausible WRONG answer, not an error.
	codepoint, err := live.srv.Definition(ctx, "src/unicode.ts", at(5, 61))
	if err != nil {
		t.Fatalf("Definition at the codepoint column: %v", err)
	}
	if len(codepoint) != 1 {
		t.Fatalf("got %d definitions at column 61, want 1: %v", len(codepoint), places(codepoint))
	}
	if want := "src/unicode.ts " + span(rng(5, 56, 5, 66)); place(codepoint[0]) != want {
		t.Fatalf("column 61 resolved %s; testdata/README.md §1.5 records %s (rocketName)",
			place(codepoint[0]), want)
	}
	if place(codepoint[0]) == place(correct[0]) {
		t.Fatal("columns 61 and 69 resolve the same symbol; this fixture cannot detect an encoding bug")
	}

	// Byte offset: off the end of the identifier entirely.
	if byteCol, err := live.srv.Definition(ctx, "src/unicode.ts", at(5, 85)); err != nil || len(byteCol) != 0 {
		t.Errorf("definition at the byte column 85 = %v (err %v), want none", places(byteCol), err)
	}

	// Hover at the same UTF-16 column reports the same range.
	hover, err := live.srv.Hover(ctx, "src/unicode.ts", at(5, 69))
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if hover == nil || hover.Range == nil {
		t.Fatalf("hover = %+v, want a ranged hover", hover)
	}
	if got, want := span(*hover.Range), span(rng(5, 69, 5, 80)); got != want {
		t.Fatalf("hover range = %s, want %s", got, want)
	}
}

// testdata/README.md §1.6.
func TestTSHover(t *testing.T) {
	live := startTS(t)

	got, err := live.srv.Hover(ctxFor(t), "src/user.ts", at(5, 16))
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got == nil {
		t.Fatal("Hover returned nil at the definition")
	}
	if want := "function getUserById(id: string): User"; got.Contents != want {
		t.Fatalf("contents = %q, want %q", got.Contents, want)
	}
	if got.Range == nil {
		t.Fatal("hover carries no range")
	}
	if have, want := span(*got.Range), span(rng(5, 16, 5, 27)); have != want {
		t.Fatalf("hover range = %s, want %s", have, want)
	}
	// §1.6: no TS fixture symbol carries a doc comment.
	if got.Documentation != "" {
		t.Errorf("documentation = %q, want empty for the TS fixture", got.Documentation)
	}
}

// testdata/README.md §1.2.
func TestTSDocumentSymbols(t *testing.T) {
	live := startTS(t)

	got, err := live.srv.DocumentSymbols(ctxFor(t), "src/user.ts")
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}

	find := func(name string) (protocol.Symbol, bool) {
		for _, s := range got {
			if s.Name == name && s.Container == "" {
				return s, true
			}
		}
		return protocol.Symbol{}, false
	}

	fn, ok := find("getUserById")
	if !ok {
		t.Fatalf("getUserById missing from %+v", got)
	}
	if fn.Kind != protocol.SymbolKindFunction {
		t.Errorf("getUserById kind = %q, want function", fn.Kind)
	}
	if have, want := span(fn.Range), span(rng(5, 0, 7, 1)); have != want {
		t.Errorf("getUserById range = %s, want %s", have, want)
	}
	if fn.Path != "src/user.ts" {
		t.Errorf("path = %q", fn.Path)
	}

	iface, ok := find("User")
	if !ok {
		t.Fatalf("User missing from %+v", got)
	}
	if iface.Kind != protocol.SymbolKindInterface {
		t.Errorf("User kind = %q, want interface", iface.Kind)
	}
	if have, want := span(iface.Range), span(rng(0, 0, 3, 1)); have != want {
		t.Errorf("User range = %s, want %s", have, want)
	}

	// §1.2: getUserById has two children, the object-literal properties in its
	// return statement. Hierarchy travels in `container` (docs §10.4).
	children := 0
	for _, s := range got {
		if s.Container == "getUserById" {
			children++
		}
	}
	if children != 2 {
		t.Errorf("getUserById has %d children in the flat list, want 2", children)
	}
}

// testdata/README.md §1.7.
func TestTSWorkspaceSymbols(t *testing.T) {
	live := startTS(t)
	ctx := ctxFor(t)

	got, err := live.srv.WorkspaceSymbols(ctx, "describeUser", 0)
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	found := false
	for _, s := range got {
		if s.Name != "describeUser" {
			continue
		}
		found = true
		if s.Path != "src/service.ts" {
			t.Errorf("describeUser path = %q, want src/service.ts", s.Path)
		}
		// §1.7: typescript-language-server returns the FULL declaration range.
		if have, want := span(s.Range), span(rng(5, 0, 8, 1)); have != want {
			t.Errorf("describeUser range = %s, want %s", have, want)
		}
		if s.Kind != protocol.SymbolKindFunction {
			t.Errorf("describeUser kind = %q, want function", s.Kind)
		}
	}
	if !found {
		t.Fatalf("describeUser not found among %+v", got)
	}

	// Fuzzy search: assert containment, not an exact result-set size.
	both, err := live.srv.WorkspaceSymbols(ctx, "getUserById", 0)
	if err != nil {
		t.Fatalf("WorkspaceSymbols(getUserById): %v", err)
	}
	paths := map[string]bool{}
	for _, s := range both {
		if s.Name == "getUserById" {
			paths[s.Path] = true
		}
	}
	for _, want := range []string{"src/user.ts", "src/lookalike.ts"} {
		if !paths[want] {
			t.Errorf("workspace/symbol did not return getUserById in %s: %+v", want, both)
		}
	}
}

// testdata/README.md §1.8. src/broken.ts holds the ONLY deliberate error in the
// whole fixture; the other four files publish zero diagnostics.
func TestTSDiagnostics(t *testing.T) {
	live := startTS(t)

	diags := settledDiagnostics(t, live.srv, "src/broken.ts", 1)
	got := diags[0]

	if got.Path != "src/broken.ts" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Severity != protocol.SeverityError {
		t.Errorf("severity = %q, want error", got.Severity)
	}
	// docs §10.5 / §11 item 8: the code is carried VERBATIM. The server sends
	// the integer 2339; no "TS" prefix is synthesised anywhere in the bridge.
	if got.Code != "2339" {
		t.Errorf("code = %q, want %q", got.Code, "2339")
	}
	if got.Source != "typescript" {
		t.Errorf("source = %q, want %q", got.Source, "typescript")
	}
	if have, want := span(got.Range), span(rng(7, 34, 7, 45)); have != want {
		t.Errorf("range = %s, want %s", have, want)
	}
	if want := "Property 'missingProp' does not exist on type 'Widget'."; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
}

func TestTSCleanFilesHaveNoDiagnostics(t *testing.T) {
	live := startTS(t)
	// Wait for the erroring file first, so the analysis pass is demonstrably
	// complete before we assert an absence.
	settledDiagnostics(t, live.srv, "src/broken.ts", 1)

	ctx := ctxFor(t)
	for _, rel := range []string{"src/user.ts", "src/service.ts", "src/lookalike.ts", "src/unicode.ts"} {
		diags, _, err := live.srv.Diagnostics(ctx, rel, 0)
		if err != nil {
			t.Fatalf("Diagnostics(%s): %v", rel, err)
		}
		if len(diags) != 0 {
			t.Errorf("%s published %d diagnostics, want 0: %+v", rel, len(diags), diags)
		}
	}
}

// ============================================================================
// Python — testdata/README.md §2
// ============================================================================

var pySources = []string{
	"user.py", "service.py", "lookalike.py", "unicode_positions.py", "broken.py",
}

func startPy(t *testing.T) *liveServer { return start(t, "py-project", "python", pySources) }

func TestPyCapabilitiesAreCaptured(t *testing.T) {
	live := startPy(t)
	for _, capability := range []string{
		lsp.CapDefinition, lsp.CapReferences, lsp.CapHover,
		lsp.CapDocumentSymbol, lsp.CapWorkspaceSymbol, lsp.CapRename,
	} {
		if !live.srv.Supports(capability) {
			t.Errorf("pyright did not advertise %s", capability)
		}
	}
	if !live.srv.Supports(lsp.CapPushDiagnostics) {
		t.Error("pyright was classified as pull-model; v0.1 expects push")
	}
}

// testdata/README.md §2.3.
func TestPyDefinitionAcrossFiles(t *testing.T) {
	live := startPy(t)

	got, err := live.srv.Definition(ctxFor(t), "service.py", at(9, 17))
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d definitions, want 1: %v", len(got), places(got))
	}
	if want := "user.py " + span(rng(9, 4, 9, 18)); place(got[0]) != want {
		t.Fatalf("definition = %s, want %s", place(got[0]), want)
	}
	if !got[0].IsDefinition {
		t.Error("a definition result was not marked is_definition")
	}
}

func TestPyReferenceGraph(t *testing.T) {
	live := startPy(t)
	ctx := ctxFor(t)

	got, err := live.srv.References(ctx, "user.py", at(9, 4), true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	want := []string{
		"user.py " + span(rng(9, 4, 9, 18)),
		"service.py " + span(rng(2, 23, 2, 37)),
		"service.py " + span(rng(9, 17, 9, 31)),
		"service.py " + span(rng(14, 12, 14, 26)),
		"unicode_positions.py " + span(rng(7, 17, 7, 31)),
		"unicode_positions.py " + span(rng(9, 44, 9, 58)),
	}
	if !equalStrings(places(got), want) {
		t.Fatalf("references =\n  %v\nwant\n  %v", places(got), want)
	}

	withoutDecl, err := live.srv.References(ctx, "user.py", at(9, 4), false)
	if err != nil {
		t.Fatalf("References(includeDeclaration=false): %v", err)
	}
	if len(withoutDecl) != 5 {
		t.Fatalf("got %d references without the declaration, want 5: %v", len(withoutDecl), places(withoutDecl))
	}
}

// testdata/README.md §2.4. Note pyright returns bare null where
// typescript-language-server returns [] — both must normalise to "nothing".
func TestPyTextualVersusSemantic(t *testing.T) {
	live := startPy(t)
	ctx := ctxFor(t)

	if got, err := live.srv.Definition(ctx, "service.py", at(4, 2)); err != nil || len(got) != 0 {
		t.Errorf("definition inside a comment = %v (err %v), want none", places(got), err)
	}
	if got, err := live.srv.Definition(ctx, "service.py", at(5, 8)); err != nil || len(got) != 0 {
		t.Errorf("definition inside a string = %v (err %v), want none", places(got), err)
	}
	if got, err := live.srv.Hover(ctx, "service.py", at(5, 8)); err != nil || got != nil {
		t.Errorf("hover inside a string = %+v (err %v), want nil", got, err)
	}

	lookalike, err := live.srv.References(ctx, "lookalike.py", at(7, 4), true)
	if err != nil {
		t.Fatalf("References(lookalike): %v", err)
	}
	want := []string{"lookalike.py " + span(rng(7, 4, 7, 18))}
	if !equalStrings(places(lookalike), want) {
		t.Fatalf("lookalike references = %v, want %v", places(lookalike), want)
	}
}

// THE UTF-16 test, Python twin (testdata/README.md §2.5).
func TestPyUTF16ColumnsResolveTheRightSymbol(t *testing.T) {
	live := startPy(t)
	ctx := ctxFor(t)

	correct, err := live.srv.Definition(ctx, "unicode_positions.py", at(9, 44))
	if err != nil {
		t.Fatalf("Definition at the UTF-16 column: %v", err)
	}
	if len(correct) != 1 {
		t.Fatalf("got %d definitions at column 44, want 1: %v", len(correct), places(correct))
	}
	if want := "user.py " + span(rng(9, 4, 9, 18)); place(correct[0]) != want {
		t.Fatalf("definition at UTF-16 column 44 = %s, want %s", place(correct[0]), want)
	}

	codepoint, err := live.srv.Definition(ctx, "unicode_positions.py", at(9, 36))
	if err != nil {
		t.Fatalf("Definition at the codepoint column: %v", err)
	}
	if len(codepoint) != 1 {
		t.Fatalf("got %d definitions at column 36, want 1: %v", len(codepoint), places(codepoint))
	}
	if want := "unicode_positions.py " + span(rng(9, 30, 9, 41)); place(codepoint[0]) != want {
		t.Fatalf("column 36 resolved %s; testdata/README.md §2.5 records %s (rocket_name)",
			place(codepoint[0]), want)
	}
	if place(codepoint[0]) == place(correct[0]) {
		t.Fatal("columns 36 and 44 resolve the same symbol; this fixture cannot detect an encoding bug")
	}

	if byteCol, err := live.srv.Definition(ctx, "unicode_positions.py", at(9, 60)); err != nil || len(byteCol) != 0 {
		t.Errorf("definition at the byte column 60 = %v (err %v), want none", places(byteCol), err)
	}
}

// testdata/README.md §2.6 — the fixture that supplies SPEC §4.4's Hover
// `documentation` field.
func TestPyHoverSplitsDocumentation(t *testing.T) {
	live := startPy(t)

	got, err := live.srv.Hover(ctxFor(t), "user.py", at(9, 4))
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got == nil {
		t.Fatal("Hover returned nil at the definition")
	}
	if want := "(function) def get_user_by_id(user_id: str) -> User"; got.Contents != want {
		t.Fatalf("contents = %q, want %q", got.Contents, want)
	}
	if want := "Return a User for the given id."; got.Documentation != want {
		t.Fatalf("documentation = %q, want %q", got.Documentation, want)
	}
	if got.Range == nil {
		t.Fatal("hover carries no range")
	}
	if have, want := span(*got.Range), span(rng(9, 4, 9, 18)); have != want {
		t.Fatalf("hover range = %s, want %s", have, want)
	}
}

// testdata/README.md §2.2.
func TestPyDocumentSymbols(t *testing.T) {
	live := startPy(t)

	got, err := live.srv.DocumentSymbols(ctxFor(t), "user.py")
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}

	var class, fn *protocol.Symbol
	for i := range got {
		switch {
		case got[i].Name == "User" && got[i].Container == "":
			class = &got[i]
		case got[i].Name == "get_user_by_id" && got[i].Container == "":
			fn = &got[i]
		}
	}
	if class == nil || fn == nil {
		t.Fatalf("User and/or get_user_by_id missing from %+v", got)
	}
	if class.Kind != protocol.SymbolKindClass {
		t.Errorf("User kind = %q, want class", class.Kind)
	}
	if have, want := span(class.Range), span(rng(3, 0, 6, 24)); have != want {
		t.Errorf("User range = %s, want %s", have, want)
	}
	if fn.Kind != protocol.SymbolKindFunction {
		t.Errorf("get_user_by_id kind = %q, want function", fn.Kind)
	}
	if have, want := span(fn.Range), span(rng(9, 0, 11, 43)); have != want {
		t.Errorf("get_user_by_id range = %s, want %s", have, want)
	}

	// §2.2: __init__ is a method of User; hierarchy travels in `container`.
	found := false
	for _, s := range got {
		if s.Name == "__init__" && s.Container == "User" {
			found = true
			if s.Kind != protocol.SymbolKindMethod {
				t.Errorf("__init__ kind = %q, want method", s.Kind)
			}
		}
	}
	if !found {
		t.Errorf("__init__ with container User missing from %+v", got)
	}
}

// testdata/README.md §2.7. Pyright's workspace-symbol range is the IDENTIFIER,
// the opposite of typescript-language-server's full declaration range.
func TestPyWorkspaceSymbols(t *testing.T) {
	live := startPy(t)

	got := workspaceSymbols(t, live.srv, "describe_user")
	found := false
	for _, s := range got {
		if s.Name != "describe_user" {
			continue
		}
		found = true
		if s.Path != "service.py" {
			t.Errorf("describe_user path = %q, want service.py", s.Path)
		}
		if have, want := span(s.Range), span(rng(8, 4, 8, 17)); have != want {
			t.Errorf("describe_user range = %s, want %s (pyright returns the identifier range)", have, want)
		}
	}
	if !found {
		t.Fatalf("describe_user not found among %+v", got)
	}
}

// testdata/README.md §2.8.
func TestPyDiagnostics(t *testing.T) {
	live := startPy(t)

	diags := settledDiagnostics(t, live.srv, "broken.py", 1)
	got := diags[0]

	if got.Path != "broken.py" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Severity != protocol.SeverityError {
		t.Errorf("severity = %q, want error", got.Severity)
	}
	// The other half of the docs §10.5 pair: pyright sends a STRING code, and
	// a capital-P source. Both are carried verbatim.
	if got.Code != "reportAttributeAccessIssue" {
		t.Errorf("code = %q, want %q", got.Code, "reportAttributeAccessIssue")
	}
	if got.Source != "Pyright" {
		t.Errorf("source = %q, want %q (capital P, verbatim)", got.Source, "Pyright")
	}
	if have, want := span(got.Range), span(rng(9, 22, 9, 34)); have != want {
		t.Errorf("range = %s, want %s", have, want)
	}
	// §2.8: match the first line; the detail line is pyright-version-sensitive.
	const wantPrefix = `Cannot access attribute "missing_prop" for class "Widget"`
	if !strings.HasPrefix(got.Message, wantPrefix) {
		t.Errorf("message = %q, want a prefix of %q", got.Message, wantPrefix)
	}
}

func TestPyCleanFilesHaveNoDiagnostics(t *testing.T) {
	live := startPy(t)
	settledDiagnostics(t, live.srv, "broken.py", 1)

	ctx := ctxFor(t)
	for _, rel := range []string{"user.py", "service.py", "lookalike.py", "unicode_positions.py"} {
		diags, _, err := live.srv.Diagnostics(ctx, rel, 0)
		if err != nil {
			t.Fatalf("Diagnostics(%s): %v", rel, err)
		}
		if len(diags) != 0 {
			t.Errorf("%s published %d diagnostics, want 0: %+v", rel, len(diags), diags)
		}
	}
}

// ============================================================================
// Lifecycle against a real process
// ============================================================================

// SPEC §8: language server processes are children of the daemon and are cleaned
// up on shutdown. A node wrapper script's children survive a plain kill, so this
// is a process-group property, not a formality.
func TestRealServerShutdownLeavesNothingRunning(t *testing.T) {
	testutil.NoGoroutineLeaks(t)

	entry := testutil.RequireLanguageServer(t, "typescript")
	root := testutil.FixtureRoot(t, "ts-project")

	sup, err := lsp.NewSupervisor(lsp.Options{
		Root:    root,
		Servers: []config.LanguageServer{entry},
		Clock:   clock.New(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	srv, err := sup.Acquire(ctx, "typescript")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "src", "user.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Open(ctx, "src/user.ts", "typescript", string(data)); err != nil {
		t.Fatalf("Open: %v", err)
	}

	status := sup.Status()
	if len(status) != 1 || status[0].State != protocol.ServerReady {
		t.Fatalf("Status = %+v, want one ready server", status)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()
	if err := sup.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if _, err := sup.Acquire(ctx, "typescript"); err == nil {
		t.Fatal("Acquire succeeded after Shutdown")
	}
}
