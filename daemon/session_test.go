package daemon

import (
	"encoding/json"
	"testing"

	"github.com/fingerskier/langer/protocol"
)

func TestSessionSetTracksAndReleases(t *testing.T) {
	set := newSessionSet()

	set.note("alice")
	set.note("alice")
	set.note("bob")
	set.note("") // an empty id is not a session

	all := set.all()
	if len(all) != 2 {
		t.Fatalf("all() = %v, want two sessions", all)
	}

	set.forget("alice")
	if all := set.all(); len(all) != 1 || all[0] != "bob" {
		t.Errorf("after forgetting alice, all() = %v", all)
	}
}

// TestSessionOfRequiresASessionID: overlay isolation (SPEC §4.2) and
// cleanup-on-disconnect both key on it, so a missing id must fail loudly
// instead of defaulting to a shared one.
func TestSessionOfRequiresASessionID(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want protocol.SessionID
		fail bool
	}{
		{name: "present", raw: `{"session_id":"alice"}`, want: "alice"},
		{name: "nested params still carry it", raw: `{"session_id":"alice","workspace_id":"ws-1","path":"a.ts"}`, want: "alice"},
		{name: "absent", raw: `{"workspace_id":"ws-1"}`, fail: true},
		{name: "empty", raw: `{"session_id":""}`, fail: true},
		{name: "no params at all", raw: ``, fail: true},
		{name: "malformed", raw: `{`, fail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.raw != "" {
				raw = json.RawMessage(tt.raw)
			}
			got, err := sessionOf(protocol.MethodIndexStatus, raw)
			if tt.fail {
				if err == nil {
					t.Fatalf("sessionOf(%q) = %q, want an error", tt.raw, got)
				}
				if code := protocol.AsError(err).Code; code != protocol.ErrInternal {
					t.Errorf("code = %s, want %s", code, protocol.ErrInternal)
				}
				return
			}
			if err != nil {
				t.Fatalf("sessionOf(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("sessionOf(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
