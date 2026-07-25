package protocol

// IndexState is the indexing progress of one workspace, surfaced by
// index_status (SPEC §3.5).
type IndexState string

// Index states.
const (
	IndexIdle     IndexState = "idle"
	IndexScanning IndexState = "scanning"
	IndexIndexing IndexState = "indexing"
	IndexReady    IndexState = "ready"
)

// ServerState is the SPEC §3.3 language server supervision state machine:
//
//	stopped ──start──▶ starting ──initialize ok──▶ ready
//	                      │                          │
//	                      └──────── failure ◀────────┘
//	                                  ▼
//	                              crashed ──▶ backoff ──▶ starting
type ServerState string

// Supervision states.
const (
	ServerStopped  ServerState = "stopped"
	ServerStarting ServerState = "starting"
	ServerReady    ServerState = "ready"
	ServerCrashed  ServerState = "crashed"
	ServerBackoff  ServerState = "backoff"
)

// ServerStatus reports one language server's supervision state. It is derived
// without side effects: asking for status must never start a server.
type ServerStatus struct {
	Name     string      `json:"name"`
	State    ServerState `json:"state"`
	Restarts int         `json:"restarts,omitempty"`
	// RetryAfterMS is the remaining backoff when State is ServerBackoff.
	RetryAfterMS int `json:"retry_after_ms,omitempty"`
}
