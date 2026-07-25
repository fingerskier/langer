package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/fingerskier/langer/protocol"
)

func TestFileEditJSONShape(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(protocol.FileEdit{
		Path: "src/user.ts",
		Edits: []protocol.TextEdit{{
			Range: protocol.Range{
				Start: protocol.Position{Line: 5, Character: 16},
				End:   protocol.Position{Line: 5, Character: 27},
			},
			NewText: "findUserById",
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"path":"src/user.ts","edits":[{"range":{"start":{"line":5,"character":16},"end":{"line":5,"character":27}},"new_text":"findUserById"}]}`
	if string(got) != want {
		t.Fatalf("FileEdit JSON =\n  %s\nwant\n  %s", got, want)
	}
}

func TestFileEditRoundTrip(t *testing.T) {
	t.Parallel()
	in := protocol.FileEdit{
		Path:  "a/b.py",
		Edits: []protocol.TextEdit{{NewText: "x"}, {NewText: "y"}},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out protocol.FileEdit
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Path != in.Path || len(out.Edits) != 2 || out.Edits[1].NewText != "y" {
		t.Fatalf("round trip = %+v", out)
	}
}

// An empty replacement is a deletion — a meaningful edit, so new_text must not
// be omitted when it is empty.
func TestTextEditKeepsEmptyNewText(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(protocol.TextEdit{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["new_text"]; !ok {
		t.Fatalf("new_text was omitted: %s", got)
	}
}
