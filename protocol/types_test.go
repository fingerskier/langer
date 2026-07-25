package protocol

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// specJSON values below are copied verbatim from SPEC.md §4.4 "Result Shapes".
// They are the contract. If a change to the Go types changes this JSON, the
// spec has been violated — fix the types, not the test.

const (
	specLocationJSON = `{
  "path": "src/user/service.ts",
  "range": { "start": { "line": 41, "character": 9 },
             "end":   { "line": 41, "character": 20 } },
  "is_definition": true,
  "preview": "export function getUserById(id: string): User {"
}`

	specHoverJSON = `{
  "contents": "function getUserById(id: string): User",
  "documentation": "Fetches a user by primary key. Throws NotFoundError.",
  "range": { "start": { "line": 12, "character": 6 },
             "end":   { "line": 12, "character": 17 } }
}`

	specSymbolJSON = `{
  "name": "getUserById",
  "kind": "function",
  "container": "UserService",
  "path": "src/user/service.ts",
  "range": { "start": { "line": 41, "character": 0 },
             "end":   { "line": 48, "character": 1 } },
  "detail": "(id: string) => User"
}`

	specDiagnosticJSON = `{
  "path": "src/user/service.ts",
  "severity": "error",
  "code": "TS2339",
  "source": "typescript",
  "message": "Property 'idd' does not exist on type 'User'.",
  "range": { "start": { "line": 44, "character": 14 },
             "end":   { "line": 44, "character": 17 } }
}`

	specErrorJSON = `{ "error": { "code": "NOT_READY", "message": "pyright is indexing", "retry_after_ms": 1500 } }`
)

func TestSpecResultShapes(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name: "Location",
			value: Location{
				Path: "src/user/service.ts",
				Range: Range{
					Start: Position{Line: 41, Character: 9},
					End:   Position{Line: 41, Character: 20},
				},
				IsDefinition: true,
				Preview:      "export function getUserById(id: string): User {",
			},
			want: specLocationJSON,
		},
		{
			name: "Hover",
			value: Hover{
				Contents:      "function getUserById(id: string): User",
				Documentation: "Fetches a user by primary key. Throws NotFoundError.",
				Range: &Range{
					Start: Position{Line: 12, Character: 6},
					End:   Position{Line: 12, Character: 17},
				},
			},
			want: specHoverJSON,
		},
		{
			name: "Symbol",
			value: Symbol{
				Name:      "getUserById",
				Kind:      SymbolKindFunction,
				Container: "UserService",
				Path:      "src/user/service.ts",
				Range: Range{
					Start: Position{Line: 41, Character: 0},
					End:   Position{Line: 48, Character: 1},
				},
				Detail: "(id: string) => User",
			},
			want: specSymbolJSON,
		},
		{
			name: "Diagnostic",
			value: Diagnostic{
				Path:     "src/user/service.ts",
				Severity: SeverityError,
				Code:     "TS2339",
				Source:   "typescript",
				Message:  "Property 'idd' does not exist on type 'User'.",
				Range: Range{
					Start: Position{Line: 44, Character: 14},
					End:   Position{Line: 44, Character: 17},
				},
			},
			want: specDiagnosticJSON,
		},
		{
			name: "ErrorResult",
			value: (&Error{
				Code:         ErrNotReady,
				Message:      "pyright is indexing",
				RetryAfterMS: 1500,
			}).Result(),
			want: specErrorJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatalf("json.Marshal(%T) failed: %v", tt.value, err)
			}
			gotCanon := canonical(t, got)
			wantCanon := canonical(t, []byte(tt.want))
			if diff := cmp.Diff(wantCanon, gotCanon); diff != "" {
				t.Errorf("marshalled JSON does not match SPEC §4.4 (-spec +got):\n%s", diff)
			}
		})
	}
}

// TestSpecResultShapesRoundTrip proves the Go types can also consume the exact
// JSON from SPEC §4.4 without losing any field.
func TestSpecResultShapesRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		spec string
		into func() any
	}{
		{"Location", specLocationJSON, func() any { return new(Location) }},
		{"Hover", specHoverJSON, func() any { return new(Hover) }},
		{"Symbol", specSymbolJSON, func() any { return new(Symbol) }},
		{"Diagnostic", specDiagnosticJSON, func() any { return new(Diagnostic) }},
		{"ErrorResult", specErrorJSON, func() any { return new(ErrorResult) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.into()
			if err := json.Unmarshal([]byte(tt.spec), v); err != nil {
				t.Fatalf("json.Unmarshal into %T failed: %v", v, err)
			}
			got, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("json.Marshal(%T) failed: %v", v, err)
			}
			if diff := cmp.Diff(canonical(t, []byte(tt.spec)), canonical(t, got)); diff != "" {
				t.Errorf("round-trip lost or altered data (-spec +got):\n%s", diff)
			}
		})
	}
}

// TestOptionalFieldsOmitted documents which fields disappear when unset, so a
// minimal result stays token-cheap (SPEC §4.2: "concise, structured results
// optimized for low token usage").
func TestOptionalFieldsOmitted(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "Location without preview keeps is_definition",
			value: Location{Path: "a.ts"},
			want:  `{"path":"a.ts","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"is_definition":false}`,
		},
		{
			name:  "Hover without documentation or range",
			value: Hover{Contents: "int"},
			want:  `{"contents":"int"}`,
		},
		{
			name:  "Symbol without container or detail",
			value: Symbol{Name: "main", Kind: SymbolKindFunction, Path: "main.go"},
			want:  `{"name":"main","kind":"function","path":"main.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}}}`,
		},
		{
			name:  "Diagnostic without code or source",
			value: Diagnostic{Path: "a.py", Severity: SeverityWarning, Message: "unused"},
			want:  `{"path":"a.py","severity":"warning","message":"unused","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}}}`,
		},
		{
			name:  "Error without retry_after_ms",
			value: (&Error{Code: ErrNoResult, Message: "nothing found"}).Result(),
			want:  `{"error":{"code":"NO_RESULT","message":"nothing found"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatalf("json.Marshal(%T) failed: %v", tt.value, err)
			}
			if diff := cmp.Diff(canonical(t, []byte(tt.want)), canonical(t, got)); diff != "" {
				t.Errorf("unexpected JSON (-want +got):\n%s", diff)
			}
		})
	}
}

// canonical re-encodes JSON with sorted keys and stable indentation so the
// comparison is about keys and values, not whitespace or field order.
func canonical(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canonical: input is not valid JSON: %v\n%s", err, b)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("canonical: re-encode failed: %v", err)
	}
	return buf.String()
}
