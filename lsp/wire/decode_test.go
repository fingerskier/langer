package wire_test

import (
	"encoding/json"
	"testing"

	"github.com/fingerskier/langer/lsp/wire"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// pyright answers textDocument/definition with a bare Location.
func TestDecodeDefinitionSingleLocation(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeDefinition(raw(`{"uri":"file:///r/user.py","range":{"start":{"line":9,"character":4},"end":{"line":9,"character":18}}}`))
	if err != nil {
		t.Fatalf("DecodeDefinition: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d locations, want 1", len(got))
	}
	if got[0].URI != "file:///r/user.py" || got[0].Range.Start.Line != 9 || got[0].Range.End.Character != 18 {
		t.Fatalf("location = %+v", got[0])
	}
}

func TestDecodeDefinitionLocationArray(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeDefinition(raw(`[
		{"uri":"file:///r/a.py","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":3}}},
		{"uri":"file:///r/b.py","range":{"start":{"line":4,"character":5},"end":{"line":4,"character":6}}}
	]`))
	if err != nil {
		t.Fatalf("DecodeDefinition: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d locations, want 2", len(got))
	}
}

// typescript-language-server answers with LocationLink[]. The interesting range
// is targetSelectionRange (the identifier), not targetRange (the whole decl).
func TestDecodeDefinitionLocationLinkPrefersSelectionRange(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeDefinition(raw(`[{
		"originSelectionRange":{"start":{"line":6,"character":21},"end":{"line":6,"character":32}},
		"targetUri":"file:///r/src/user.ts",
		"targetRange":{"start":{"line":5,"character":0},"end":{"line":7,"character":1}},
		"targetSelectionRange":{"start":{"line":5,"character":16},"end":{"line":5,"character":27}}
	}]`))
	if err != nil {
		t.Fatalf("DecodeDefinition: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d locations, want 1", len(got))
	}
	if got[0].URI != "file:///r/src/user.ts" {
		t.Fatalf("uri = %q", got[0].URI)
	}
	if got[0].Range.Start.Line != 5 || got[0].Range.Start.Character != 16 ||
		got[0].Range.End.Character != 27 {
		t.Fatalf("range = %+v, want the selection range [5,16]-[5,27]", got[0].Range)
	}
}

func TestDecodeDefinitionLocationLinkFallsBackToTargetRange(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeDefinition(raw(`[{
		"targetUri":"file:///r/a.ts",
		"targetRange":{"start":{"line":5,"character":0},"end":{"line":7,"character":1}}
	}]`))
	if err != nil {
		t.Fatalf("DecodeDefinition: %v", err)
	}
	if got[0].Range.Start.Line != 5 || got[0].Range.End.Line != 7 {
		t.Fatalf("range = %+v", got[0].Range)
	}
}

// pyright returns bare null where typescript-language-server returns [].
// Both mean "nothing found" and neither is an error.
func TestDecodeDefinitionEmptyForms(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`null`, `[]`, ``} {
		got, err := wire.DecodeDefinition(raw(in))
		if err != nil {
			t.Fatalf("DecodeDefinition(%q): %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("DecodeDefinition(%q) = %v, want empty", in, got)
		}
	}
}

func TestDecodeHoverMarkupContent(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeHover(raw(`{"contents":{"kind":"markdown","value":"# hi"},
		"range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}`))
	if err != nil {
		t.Fatalf("DecodeHover: %v", err)
	}
	if got == nil || got.Value != "# hi" {
		t.Fatalf("hover = %+v", got)
	}
	if got.Range == nil || got.Range.Start.Character != 2 {
		t.Fatalf("range = %+v", got.Range)
	}
}

func TestDecodeHoverPlainString(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeHover(raw(`{"contents":"just text"}`))
	if err != nil {
		t.Fatalf("DecodeHover: %v", err)
	}
	if got.Value != "just text" {
		t.Fatalf("value = %q", got.Value)
	}
	if got.Range != nil {
		t.Fatalf("range = %+v, want nil", got.Range)
	}
}

func TestDecodeHoverMarkedStringObject(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeHover(raw(`{"contents":{"language":"typescript","value":"function f(): void"}}`))
	if err != nil {
		t.Fatalf("DecodeHover: %v", err)
	}
	// A MarkedString is a code block in the language it names.
	if got.Value != "```typescript\nfunction f(): void\n```" {
		t.Fatalf("value = %q", got.Value)
	}
}

func TestDecodeHoverMarkedStringArray(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeHover(raw(`{"contents":[
		{"language":"python","value":"def f() -> None"},
		"Docs go here."
	]}`))
	if err != nil {
		t.Fatalf("DecodeHover: %v", err)
	}
	want := "```python\ndef f() -> None\n```\n\nDocs go here."
	if got.Value != want {
		t.Fatalf("value = %q, want %q", got.Value, want)
	}
}

func TestDecodeHoverNull(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`null`, ``, `{"contents":""}`, `{"contents":[]}`} {
		got, err := wire.DecodeHover(raw(in))
		if err != nil {
			t.Fatalf("DecodeHover(%q): %v", in, err)
		}
		if got != nil {
			t.Fatalf("DecodeHover(%q) = %+v, want nil", in, got)
		}
	}
}

// Both DocumentSymbol[] (hierarchical) and SymbolInformation[] (flat) are legal
// server responses to textDocument/documentSymbol.
func TestDecodeSymbolsDocumentSymbolHierarchy(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeSymbols(raw(`[{
		"name":"getUserById","kind":12,"detail":"(id: string) => User",
		"range":{"start":{"line":5,"character":0},"end":{"line":7,"character":1}},
		"selectionRange":{"start":{"line":5,"character":16},"end":{"line":5,"character":27}},
		"children":[{
			"name":"id","kind":7,
			"range":{"start":{"line":6,"character":11},"end":{"line":6,"character":13}},
			"selectionRange":{"start":{"line":6,"character":11},"end":{"line":6,"character":13}}
		}]
	}]`))
	if err != nil {
		t.Fatalf("DecodeSymbols: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d top-level symbols, want 1", len(got))
	}
	s := got[0]
	if s.Name != "getUserById" || s.Kind != 12 || s.Detail != "(id: string) => User" {
		t.Fatalf("symbol = %+v", s)
	}
	if s.Range.Start.Line != 5 || s.Range.End.Line != 7 {
		t.Fatalf("range = %+v", s.Range)
	}
	if s.SelectionRange.Start.Character != 16 {
		t.Fatalf("selectionRange = %+v", s.SelectionRange)
	}
	if len(s.Children) != 1 || s.Children[0].Name != "id" {
		t.Fatalf("children = %+v", s.Children)
	}
	if s.URI != "" {
		t.Fatalf("DocumentSymbol carries no uri, got %q", s.URI)
	}
}

func TestDecodeSymbolsSymbolInformationFlat(t *testing.T) {
	t.Parallel()
	got, err := wire.DecodeSymbols(raw(`[{
		"name":"get_user_by_id","kind":12,"containerName":"user",
		"location":{"uri":"file:///r/user.py","range":{"start":{"line":9,"character":4},"end":{"line":9,"character":18}}}
	}]`))
	if err != nil {
		t.Fatalf("DecodeSymbols: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d symbols, want 1", len(got))
	}
	s := got[0]
	if s.Name != "get_user_by_id" || s.Container != "user" {
		t.Fatalf("symbol = %+v", s)
	}
	if s.URI != "file:///r/user.py" {
		t.Fatalf("uri = %q", s.URI)
	}
	if s.Range.Start.Line != 9 || s.Range.End.Character != 18 {
		t.Fatalf("range = %+v", s.Range)
	}
	// With no selectionRange, the identifier range is the whole range.
	if s.SelectionRange != s.Range {
		t.Fatalf("selectionRange = %+v, want it to mirror range", s.SelectionRange)
	}
}

func TestDecodeSymbolsEmptyForms(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`null`, `[]`, ``} {
		got, err := wire.DecodeSymbols(raw(in))
		if err != nil {
			t.Fatalf("DecodeSymbols(%q): %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("DecodeSymbols(%q) = %v", in, got)
		}
	}
}

// docs §10.5: the server's code is rendered verbatim. TypeScript sends the
// integer 2339 → "2339"; pyright sends "reportAttributeAccessIssue". No TS
// prefix is synthesised.
func TestDecodeCodeIsVerbatim(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`2339`:                         "2339",
		`"reportAttributeAccessIssue"`: "reportAttributeAccessIssue",
		`null`:                         "",
		``:                             "",
		`"E501"`:                       "E501",
		`0`:                            "0",
	} {
		if got := wire.DecodeCode(raw(in)); got != want {
			t.Errorf("DecodeCode(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodePublishDiagnostics(t *testing.T) {
	t.Parallel()
	got, uri, err := wire.DecodePublishDiagnostics(raw(`{
		"uri":"file:///r/src/broken.ts",
		"version":1,
		"diagnostics":[{
			"range":{"start":{"line":7,"character":34},"end":{"line":7,"character":45}},
			"severity":1,"code":2339,"source":"typescript",
			"message":"Property 'missingProp' does not exist on type 'Widget'."
		}]
	}`))
	if err != nil {
		t.Fatalf("DecodePublishDiagnostics: %v", err)
	}
	if uri != "file:///r/src/broken.ts" {
		t.Fatalf("uri = %q", uri)
	}
	if len(got) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(got))
	}
	d := got[0]
	if d.Severity != 1 || d.Code != "2339" || d.Source != "typescript" {
		t.Fatalf("diagnostic = %+v", d)
	}
	if d.Range.Start.Character != 34 || d.Range.End.Character != 45 {
		t.Fatalf("range = %+v", d.Range)
	}
	if d.URI != uri {
		t.Fatalf("diagnostic uri = %q, want %q", d.URI, uri)
	}
}

func TestDecodePublishDiagnosticsEmpty(t *testing.T) {
	t.Parallel()
	got, uri, err := wire.DecodePublishDiagnostics(raw(`{"uri":"file:///r/a.ts","diagnostics":[]}`))
	if err != nil {
		t.Fatalf("DecodePublishDiagnostics: %v", err)
	}
	if uri != "file:///r/a.ts" || len(got) != 0 {
		t.Fatalf("got %d diagnostics for %q, want 0", len(got), uri)
	}
}

func TestDecodeWorkspaceEdit(t *testing.T) {
	t.Parallel()
	edits, err := wire.DecodeWorkspaceEdit(raw(`{"changes":{
		"file:///r/a.ts":[{"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":3}},"newText":"abc"}]
	}}`))
	if err != nil {
		t.Fatalf("DecodeWorkspaceEdit: %v", err)
	}
	if len(edits) != 1 || edits[0].URI != "file:///r/a.ts" || len(edits[0].Edits) != 1 {
		t.Fatalf("edits = %+v", edits)
	}
	if edits[0].Edits[0].NewText != "abc" {
		t.Fatalf("newText = %q", edits[0].Edits[0].NewText)
	}
}

func TestDecodeWorkspaceEditDocumentChanges(t *testing.T) {
	t.Parallel()
	edits, err := wire.DecodeWorkspaceEdit(raw(`{"documentChanges":[{
		"textDocument":{"uri":"file:///r/b.py","version":2},
		"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"z"}]
	}]}`))
	if err != nil {
		t.Fatalf("DecodeWorkspaceEdit: %v", err)
	}
	if len(edits) != 1 || edits[0].URI != "file:///r/b.py" {
		t.Fatalf("edits = %+v", edits)
	}
}

func TestDecodeWorkspaceEditEmpty(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`null`, ``, `{}`} {
		edits, err := wire.DecodeWorkspaceEdit(raw(in))
		if err != nil {
			t.Fatalf("DecodeWorkspaceEdit(%q): %v", in, err)
		}
		if len(edits) != 0 {
			t.Fatalf("DecodeWorkspaceEdit(%q) = %+v", in, edits)
		}
	}
}

func TestDecodeMalformedPayloadIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := wire.DecodeDefinition(raw(`"a string"`)); err == nil {
		t.Error("DecodeDefinition accepted a string")
	}
	if _, err := wire.DecodeSymbols(raw(`{"not":"an array"}`)); err == nil {
		t.Error("DecodeSymbols accepted an object")
	}
	if _, err := wire.DecodeHover(raw(`[1,2,3]`)); err == nil {
		t.Error("DecodeHover accepted an array")
	}
}
