package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestEveryParamsCarriesASessionID: overlays cannot be isolated and a dead
// client's state cannot be dropped without it (docs/ARCHITECTURE.md §10.9).
func TestEveryParamsCarriesASessionID(t *testing.T) {
	params := []any{
		OpenWorkspaceParams{}, CloseWorkspaceParams{},
		OpenDocumentParams{}, DocumentParams{},
		PositionParams{}, WorkspaceSymbolsParams{}, DiagnosticsParams{},
		RenameParams{}, ApplyEditParams{}, SimulateEditParams{},
		IndexStatusParams{}, EndSessionParams{}, HandshakeParams{}, DrainParams{},
	}

	for _, p := range params {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshalling %T: %v", p, err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%T does not marshal to an object: %v", p, err)
		}
		if _, ok := decoded["session_id"]; !ok {
			t.Errorf("%T has no session_id field: %s", p, raw)
		}
	}
}

// TestResultEnvelopeNames pins the wrapper names an agent's prompt contract
// depends on (docs/ARCHITECTURE.md §11 item 2). Changing one is breaking.
func TestResultEnvelopeNames(t *testing.T) {
	tests := []struct {
		result any
		want   string
	}{
		{EmptyResult{}, `{}`},
		{OpenWorkspaceResult{Workspace: "w1", Root: "/repo"}, `{"workspace_id":"w1","root_path":"/repo"}`},
		{LocationsResult{Locations: []Location{}}, `{"locations":[]}`},
		{HoverResult{}, `{"hover":null}`},
		{SymbolsResult{Symbols: []Symbol{}}, `{"symbols":[]}`},
		{DiagnosticsResult{Diagnostics: []Diagnostic{}}, `{"diagnostics":[]}`},
		{EditPlanResult{EditToken: "tok", Files: []FileEdit{}}, `{"edit_token":"tok","files":[]}`},
		{ApplyResult{Applied: []string{}}, `{"applied":[]}`},
	}

	for _, tt := range tests {
		raw, err := json.Marshal(tt.result)
		if err != nil {
			t.Fatalf("marshalling %T: %v", tt.result, err)
		}
		if string(raw) != tt.want {
			t.Errorf("%T marshalled to %s, want %s", tt.result, raw, tt.want)
		}
	}
}

// TestEveryResultIsAJSONObject: MCP requires structured tool output to be an
// object, and a bare null is not one (docs/ARCHITECTURE.md §4.2).
func TestEveryResultIsAJSONObject(t *testing.T) {
	results := []any{
		EmptyResult{}, OpenWorkspaceResult{}, LocationsResult{}, HoverResult{},
		SymbolsResult{}, DiagnosticsResult{}, EditPlanResult{}, ApplyResult{},
		IndexStatusResult{}, HandshakeResult{}, DrainResult{},
	}
	for _, r := range results {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshalling %T: %v", r, err)
		}
		if !strings.HasPrefix(string(raw), "{") {
			t.Errorf("%T marshalled to %s, want a JSON object", r, raw)
		}
	}
}

// TestPossiblyStaleIsOnTheEnvelope, not on an individual diagnostic: SPEC §4.3
// describes staleness as a property of the settle attempt
// (docs/ARCHITECTURE.md §10.8).
func TestPossiblyStaleIsOnTheEnvelope(t *testing.T) {
	if _, ok := reflect.TypeOf(DiagnosticsResult{}).FieldByName("PossiblyStale"); !ok {
		t.Fatal("DiagnosticsResult has no PossiblyStale field")
	}
	if _, ok := reflect.TypeOf(Diagnostic{}).FieldByName("PossiblyStale"); ok {
		t.Error("Diagnostic carries PossiblyStale; it belongs on the result envelope")
	}

	raw, err := json.Marshal(DiagnosticsResult{Diagnostics: []Diagnostic{}, PossiblyStale: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"diagnostics":[],"possibly_stale":true}` {
		t.Errorf("marshalled to %s", raw)
	}
}

// TestPositionParamsEmbedsDocumentParams keeps one path/session/workspace
// spelling across the whole surface.
func TestPositionParamsRoundTrip(t *testing.T) {
	in := PositionParams{
		DocumentParams: DocumentParams{Session: "sess", Workspace: "ws", Path: "src/user.ts"},
		Position:       Position{Line: 5, Character: 16},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"session_id":"sess","workspace_id":"ws","path":"src/user.ts","position":{"line":5,"character":16}}`
	if string(raw) != want {
		t.Fatalf("marshalled to %s\nwant           %s", raw, want)
	}

	var out PositionParams
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip gave %+v, want %+v", out, in)
	}
}
