package protocol

import "encoding/json"

// Version is the daemon IPC protocol version. It rides on every request and
// response so the MCP server and daemon can detect a binary/daemon mismatch and
// drain-and-restart the old daemon (SPEC §3.1, §3.5).
const Version = 1

// Method names for the daemon API (SPEC §3.5). The daemon is the source of
// truth; the MCP server is a pure access layer over these.
const (
	MethodOpenWorkspace  = "open_workspace"
	MethodCloseWorkspace = "close_workspace"

	MethodOpenDocument  = "open_document"
	MethodCloseDocument = "close_document"

	MethodGetDefinition   = "get_definition"
	MethodGetReferences   = "get_references"
	MethodGetHover        = "get_hover"
	MethodDocumentSymbols = "document_symbols"
	MethodWorkspaceSymbol = "workspace_symbols"
	MethodGetDiagnostics  = "get_diagnostics"

	MethodRenameSymbol = "rename_symbol"
	MethodApplyEdit    = "apply_edit"
	MethodSimulateEdit = "simulate_edit"
	MethodIndexStatus  = "index_status"
)

// Request is one newline-delimited JSON message from the MCP server to the
// daemon (SPEC §3.5).
type Request struct {
	Version int             `json:"version"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the daemon's reply. Exactly one of Result or Error is set; a
// failure always carries a structured code from SPEC §3.6.
type Response struct {
	Version int             `json:"version"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// NewRequest builds a request stamped with the current protocol version.
func NewRequest(id int64, method string, params json.RawMessage) *Request {
	return &Request{Version: Version, ID: id, Method: method, Params: params}
}

// NewResponse marshals result into a successful response. A marshalling failure
// is the caller's bug, so it surfaces as a plain error for them to convert.
func NewResponse(id int64, result any) (*Response, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &Response{Version: Version, ID: id, Result: raw}, nil
}

// NewErrorResponse builds a failing response. A nil error is coerced to
// INTERNAL so a response can never claim failure without a code.
func NewErrorResponse(id int64, err *Error) *Response {
	if err == nil {
		err = NewError(ErrInternal, "unspecified error")
	}
	return &Response{Version: Version, ID: id, Error: err}
}
