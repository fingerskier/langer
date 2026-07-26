package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/fingerskier/langer/protocol"
)

// The SPEC §3.3 supervision states, spelled once so nothing downstream invents
// a sixth.
func TestServerStateSpellings(t *testing.T) {
	t.Parallel()
	for state, want := range map[protocol.ServerState]string{
		protocol.ServerStopped:  "stopped",
		protocol.ServerStarting: "starting",
		protocol.ServerReady:    "ready",
		protocol.ServerCrashed:  "crashed",
		protocol.ServerBackoff:  "backoff",
	} {
		if string(state) != want {
			t.Errorf("state = %q, want %q", state, want)
		}
	}
}

func TestIndexStateSpellings(t *testing.T) {
	t.Parallel()
	for state, want := range map[protocol.IndexState]string{
		protocol.IndexIdle:     "idle",
		protocol.IndexScanning: "scanning",
		protocol.IndexIndexing: "indexing",
		protocol.IndexReady:    "ready",
		protocol.IndexFailed:   "failed",
	} {
		if string(state) != want {
			t.Errorf("state = %q, want %q", state, want)
		}
	}
}

func TestIndexFailureStatusCarriesStructuredError(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(protocol.IndexStatusResult{
		Root:  "/repo",
		State: protocol.IndexFailed,
		Error: protocol.NewError(protocol.ErrInternal, "index worker failed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"root_path":"/repo","state":"failed","files_indexed":0,"files_total":0,"error":{"code":"INTERNAL","message":"index worker failed"}}`
	if string(got) != want {
		t.Fatalf("IndexStatusResult JSON =\n  %s\nwant\n  %s", got, want)
	}
}

func TestServerStatusJSONShape(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(protocol.ServerStatus{
		Name:         "typescript",
		State:        protocol.ServerBackoff,
		Restarts:     3,
		RetryAfterMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"typescript","state":"backoff","restarts":3,"retry_after_ms":2000}`
	if string(got) != want {
		t.Fatalf("ServerStatus JSON =\n  %s\nwant\n  %s", got, want)
	}
}

// A healthy server carries neither a restart count nor a retry hint.
func TestServerStatusOmitsZeroCounters(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(protocol.ServerStatus{Name: "python", State: protocol.ServerReady})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"python","state":"ready"}`
	if string(got) != want {
		t.Fatalf("ServerStatus JSON = %s, want %s", got, want)
	}
}
