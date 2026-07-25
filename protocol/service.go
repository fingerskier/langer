package protocol

import "context"

// WorkspaceID identifies an open workspace within a daemon (SPEC §3.5).
type WorkspaceID string

// SessionID identifies one MCP caller. It is derived from the MCP SDK's
// ServerSession.ID() in the frontend and scopes speculative overlays
// (SPEC §4.2).
type SessionID string

// Service is the SPEC §3.5 daemon API rendered in Go. It has exactly two
// implementations: daemon.Core (in-process, server side) and daemonctl.Client
// (over the socket, client side). mcp/ programs against this interface and
// therefore never imports daemon/.
//
// Every returned error MUST be convertible by AsError to a *Error carrying a
// SPEC §3.6 code. Methods return `error`, not `*Error`, so a nil pointer can
// never masquerade as a non-nil error.
//
// Every method has the same shape — (ctx, Params) (Result, error) — so the
// daemon can dispatch from a table keyed on method name and the client can
// marshal generically, instead of fifteen bespoke round trips.
type Service interface {
	OpenWorkspace(ctx context.Context, p OpenWorkspaceParams) (OpenWorkspaceResult, error)
	CloseWorkspace(ctx context.Context, p CloseWorkspaceParams) (EmptyResult, error)

	OpenDocument(ctx context.Context, p OpenDocumentParams) (EmptyResult, error)
	CloseDocument(ctx context.Context, p DocumentParams) (EmptyResult, error)

	GetDefinition(ctx context.Context, p PositionParams) (LocationsResult, error)
	GetReferences(ctx context.Context, p PositionParams) (LocationsResult, error)
	GetHover(ctx context.Context, p PositionParams) (HoverResult, error)
	DocumentSymbols(ctx context.Context, p DocumentParams) (SymbolsResult, error)
	WorkspaceSymbols(ctx context.Context, p WorkspaceSymbolsParams) (SymbolsResult, error)
	GetDiagnostics(ctx context.Context, p DiagnosticsParams) (DiagnosticsResult, error)

	RenameSymbol(ctx context.Context, p RenameParams) (EditPlanResult, error)
	ApplyEdit(ctx context.Context, p ApplyEditParams) (ApplyResult, error)
	SimulateEdit(ctx context.Context, p SimulateEditParams) (DiagnosticsResult, error)

	IndexStatus(ctx context.Context, p IndexStatusParams) (IndexStatusResult, error)

	// EndSession releases everything owned by a session: overlays, refcounted
	// open documents, workspace references. Called on explicit close AND on
	// transport disconnect, so it must be idempotent. Ending an unknown session
	// succeeds — it is not WORKSPACE_UNKNOWN.
	EndSession(ctx context.Context, p EndSessionParams) (EmptyResult, error)
}

// Three IPC methods exist that Service does not carry, because they are daemon
// *lifecycle* operations rather than semantic queries: the MCP tool layer never
// calls them, only internal/daemonctl does.
//
// [SPEC-ADDITION — docs/ARCHITECTURE.md §4.2 declares MethodEndSession here and
// §11 item 7 records the reasoning. MethodHandshake and MethodDrain are the
// wire spelling of the two operations docs §5.9 requires of Connect —
// "handshake and compare protocol version" and "ask the old daemon to drain" —
// which that document mandates without naming.]
const (
	// MethodEndSession drops everything a session owns (SPEC §4.2).
	MethodEndSession = "end_session"
	// MethodHandshake is the first request on any connection. It carries no
	// workspace, so a client can learn the daemon's protocol version and root
	// before committing to it (SPEC §3.1 version-mismatch detection).
	MethodHandshake = "handshake"
	// MethodDrain asks a version-mismatched daemon to stop accepting work and
	// shut down once its in-flight requests finish (SPEC §3.1). It is NOT a
	// kill: the old daemon finishes what it started.
	MethodDrain = "drain"
)
