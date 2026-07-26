package protocol

// Params and result envelopes for the SPEC §3.5 method surface.
//
// MCP requires a tool's structured result to be a JSON object, so
// list-returning methods need a one-key wrapper that SPEC §4.4 does not name.
// [SPEC-ADDITION — docs/ARCHITECTURE.md §4.3, §11 item 2.] The element shapes
// inside the wrappers are exactly the SPEC §4.4 types in protocol/types.go.

// ---- params ----

// OpenWorkspaceParams opens (or joins) the workspace rooted at Root.
type OpenWorkspaceParams struct {
	Session SessionID `json:"session_id"`
	Root    string    `json:"root_path"`
}

// CloseWorkspaceParams releases this session's reference to a workspace.
type CloseWorkspaceParams struct {
	Session   SessionID   `json:"session_id"`
	Workspace WorkspaceID `json:"workspace_id"`
}

// DocumentParams addresses one file. Path is workspace-relative and
// slash-separated (SPEC §4.4).
type DocumentParams struct {
	Session   SessionID   `json:"session_id"`
	Workspace WorkspaceID `json:"workspace_id"`
	Path      string      `json:"path"`
}

// OpenDocumentParams notifies the daemon that a file is active.
type OpenDocumentParams struct {
	DocumentParams
	LanguageID string `json:"language_id,omitempty"`
}

// PositionParams addresses a point inside a file. Positions are 0-based with
// UTF-16 characters (SPEC §4.3).
type PositionParams struct {
	DocumentParams
	Position Position `json:"position"`
}

// WorkspaceSymbolsParams is a fuzzy search across the workspace.
type WorkspaceSymbolsParams struct {
	Session   SessionID   `json:"session_id"`
	Workspace WorkspaceID `json:"workspace_id"`
	Query     string      `json:"query"`
	Limit     int         `json:"limit,omitempty"` // 0 = server default
}

// DiagnosticsParams asks for one file's diagnostics, or the workspace's when
// Path is empty.
type DiagnosticsParams struct {
	Session   SessionID   `json:"session_id"`
	Workspace WorkspaceID `json:"workspace_id"`
	Path      string      `json:"path,omitempty"`
}

// RenameParams is a rename dry-run at a position.
type RenameParams struct {
	PositionParams
	NewName string `json:"new_name"`
}

// ApplyEditParams applies a previously computed plan, identified by the token
// its dry-run returned.
type ApplyEditParams struct {
	Session   SessionID   `json:"session_id"`
	Workspace WorkspaceID `json:"workspace_id"`
	EditToken string      `json:"edit_token"`
}

// SimulateEditParams replaces a file's text in memory and reports the resulting
// diagnostics. Nothing is written to disk (SPEC §4.2).
type SimulateEditParams struct {
	DocumentParams
	NewText string `json:"new_text"`
}

// IndexStatusParams asks for one workspace's indexing and supervision state.
type IndexStatusParams struct {
	Session   SessionID   `json:"session_id"`
	Workspace WorkspaceID `json:"workspace_id"`
}

// EndSessionParams drops everything a session owns.
type EndSessionParams struct {
	Session SessionID `json:"session_id"`
}

// HandshakeParams opens a connection and declares the client's protocol
// version. The version also rides on Request.Version; carrying it here too
// means the daemon's answer names both sides even when the request envelope was
// the thing that mismatched.
type HandshakeParams struct {
	Session       SessionID `json:"session_id"`
	ClientVersion int       `json:"client_version"`
}

// DrainParams asks a daemon to stop accepting work and shut down once its
// in-flight requests finish (SPEC §3.1).
type DrainParams struct {
	Session SessionID `json:"session_id"`
	// Reason is recorded in the daemon's log so an operator can tell a
	// version-mismatch drain from an idle sunset.
	Reason string `json:"reason,omitempty"`
}

// ---- results ----
//
// Success envelopes carry payload ONLY. A failure is never expressed by a field
// inside these structs: it is the ErrorResult envelope, i.e. exactly
// {"error":{…}} and nothing else (SPEC §4.4).

// EmptyResult marshals to {}, which is a valid MCP structured result; a bare
// null is not.
type EmptyResult struct{}

// OpenWorkspaceResult identifies the workspace a client may now query.
type OpenWorkspaceResult struct {
	Workspace WorkspaceID `json:"workspace_id"`
	Root      string      `json:"root_path"`
}

// LocationsResult wraps get_definition and get_references results.
type LocationsResult struct {
	Locations []Location `json:"locations"`
	Truncated bool       `json:"truncated,omitempty"`
}

// HoverResult wraps get_hover. A nil Hover never travels: NO_RESULT does
// (docs/ARCHITECTURE.md §10.7).
type HoverResult struct {
	Hover *Hover `json:"hover"`
}

// SymbolsResult wraps document_symbols and workspace_symbols.
type SymbolsResult struct {
	Symbols   []Symbol `json:"symbols"`
	Truncated bool     `json:"truncated,omitempty"`
}

// DiagnosticsResult wraps get_diagnostics. PossiblyStale is a property of the
// settle attempt, not of any individual diagnostic (SPEC §4.3,
// docs/ARCHITECTURE.md §10.8).
type DiagnosticsResult struct {
	Diagnostics   []Diagnostic `json:"diagnostics"`
	PossiblyStale bool         `json:"possibly_stale,omitempty"`
}

// EditPlanResult is the dry-run product of rename_symbol. EditToken embeds the
// content hash of every affected file; apply_edit verifies them against disk
// and rejects with STALE_EDIT on mismatch (SPEC §4.2).
type EditPlanResult struct {
	EditToken string     `json:"edit_token"`
	Files     []FileEdit `json:"files"`
}

// ApplyResult lists the files apply_edit wrote, workspace-relative.
type ApplyResult struct {
	Applied []string `json:"applied"`
}

// IndexStatusResult reports indexing progress and supervision state.
type IndexStatusResult struct {
	Root              string         `json:"root_path"`
	State             IndexState     `json:"state"`
	FilesIndexed      int            `json:"files_indexed"`
	FilesTotal        int            `json:"files_total"`
	LanguageServers   []ServerStatus `json:"language_servers,omitempty"`
	LastIndexedUnixMS int64          `json:"last_indexed_unix_ms,omitempty"`
	// Error is present if and only if State is IndexFailed. Background
	// indexing failures remain structured SPEC §3.6 errors rather than being
	// disguised as an empty or ready result.
	Error *Error `json:"error,omitempty"`
}

// HandshakeResult tells a client which daemon it reached.
type HandshakeResult struct {
	ProtocolVersion int    `json:"protocol_version"`
	Root            string `json:"root_path"`
	PID             int    `json:"pid"`
}

// DrainResult acknowledges a drain request. The daemon has stopped accepting
// new connections by the time this is written.
type DrainResult struct {
	Draining bool `json:"draining"`
}
