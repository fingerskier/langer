package protocol

import (
	"errors"
	"fmt"
)

// ErrorCode is a machine-readable error code from SPEC §3.6. Errors crossing an
// IPC or MCP boundary are always codified — never free text.
type ErrorCode string

// The complete SPEC §3.6 error code table. Adding a code here without adding it
// to the spec is a spec violation; TestErrorCodesMatchSpec enforces both
// directions.
const (
	// ErrNotReady means the language server is starting or indexing; retry.
	ErrNotReady ErrorCode = "NOT_READY"
	// ErrUnsupported means this language server does not provide the capability.
	ErrUnsupported ErrorCode = "UNSUPPORTED"
	// ErrNoResult means the query succeeded but found nothing (not an error).
	ErrNoResult ErrorCode = "NO_RESULT"
	// ErrStaleEdit means an apply was rejected: a file changed since the dry-run.
	ErrStaleEdit ErrorCode = "STALE_EDIT"
	// ErrWorkspaceUnknown means the path is not inside an open workspace.
	ErrWorkspaceUnknown ErrorCode = "WORKSPACE_UNKNOWN"
	// ErrServerCrashed means the language server died; a restart is in progress.
	ErrServerCrashed ErrorCode = "SERVER_CRASHED"
	// ErrInternal means a bug in the bridge.
	ErrInternal ErrorCode = "INTERNAL"
)

// ErrorCodes lists every valid code, in SPEC §3.6 table order.
var ErrorCodes = []ErrorCode{
	ErrNotReady,
	ErrUnsupported,
	ErrNoResult,
	ErrStaleEdit,
	ErrWorkspaceUnknown,
	ErrServerCrashed,
	ErrInternal,
}

// Valid reports whether c is one of the SPEC §3.6 codes.
func (c ErrorCode) Valid() bool {
	for _, known := range ErrorCodes {
		if c == known {
			return true
		}
	}
	return false
}

// Error is the structured error carried by every failing tool and daemon call
// (SPEC §3.6). It implements the error interface so it can travel through
// ordinary Go plumbing and still be recovered intact with errors.As.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	// RetryAfterMS is a hint for retryable codes such as NOT_READY. Omitted
	// when zero so results stay token-cheap.
	RetryAfterMS int `json:"retry_after_ms,omitempty"`
}

// ErrorResult is the top-level JSON envelope a tool returns on failure:
//
//	{"error": {"code": "NOT_READY", "message": "...", "retry_after_ms": 1500}}
type ErrorResult struct {
	Error *Error `json:"error"`
}

// NewError builds a structured error.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// NewErrorf builds a structured error with a formatted message.
func NewErrorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithRetryAfterMS attaches a retry hint and returns e for chaining.
func (e *Error) WithRetryAfterMS(ms int) *Error {
	e.RetryAfterMS = ms
	return e
}

// Error implements the error interface. The code always leads, so even a log
// line accidentally rendering the error keeps the machine-readable part.
func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Message
}

// Result wraps e in the JSON envelope tools return on failure.
func (e *Error) Result() ErrorResult {
	return ErrorResult{Error: e}
}

// AsError coerces any error into a structured one so nothing free-text can
// escape across a boundary. A nil error stays nil; an already-structured error
// (even wrapped) passes through unchanged; anything else becomes INTERNAL,
// which SPEC §3.6 defines as "bug in the bridge".
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var structured *Error
	if errors.As(err, &structured) {
		return structured
	}
	return NewError(ErrInternal, err.Error())
}
