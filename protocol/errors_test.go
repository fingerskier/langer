package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// TestErrorCodesMatchSpec pins the error code table from SPEC §3.6 exactly:
// every listed code exists, and no code exists that the spec does not list.
func TestErrorCodesMatchSpec(t *testing.T) {
	// Verbatim from the SPEC §3.6 table.
	specCodes := []ErrorCode{
		"NOT_READY",
		"UNSUPPORTED",
		"NO_RESULT",
		"STALE_EDIT",
		"WORKSPACE_UNKNOWN",
		"SERVER_CRASHED",
		"INTERNAL",
	}

	if len(ErrorCodes) != len(specCodes) {
		t.Fatalf("ErrorCodes has %d entries, SPEC §3.6 lists %d: %v", len(ErrorCodes), len(specCodes), ErrorCodes)
	}
	for i, want := range specCodes {
		if ErrorCodes[i] != want {
			t.Errorf("ErrorCodes[%d] = %q, want %q", i, ErrorCodes[i], want)
		}
		if !want.Valid() {
			t.Errorf("ErrorCode(%q).Valid() = false, want true", want)
		}
	}
	if ErrorCode("MADE_UP").Valid() {
		t.Error(`ErrorCode("MADE_UP").Valid() = true, want false`)
	}
	if ErrorCode("").Valid() {
		t.Error(`ErrorCode("").Valid() = true, want false`)
	}
}

func TestErrorCodeConstants(t *testing.T) {
	pairs := []struct {
		got  ErrorCode
		want string
	}{
		{ErrNotReady, "NOT_READY"},
		{ErrUnsupported, "UNSUPPORTED"},
		{ErrNoResult, "NO_RESULT"},
		{ErrStaleEdit, "STALE_EDIT"},
		{ErrWorkspaceUnknown, "WORKSPACE_UNKNOWN"},
		{ErrServerCrashed, "SERVER_CRASHED"},
		{ErrInternal, "INTERNAL"},
	}
	for _, p := range pairs {
		if string(p.got) != p.want {
			t.Errorf("got %q, want %q", p.got, p.want)
		}
	}
}

func TestErrorImplementsError(t *testing.T) {
	var err error = NewError(ErrWorkspaceUnknown, "no workspace for /tmp/x")
	if got, want := err.Error(), "WORKSPACE_UNKNOWN: no workspace for /tmp/x"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	var target *Error
	if !errors.As(err, &target) {
		t.Fatal("errors.As did not unwrap to *Error")
	}
	if target.Code != ErrWorkspaceUnknown {
		t.Errorf("Code = %q, want %q", target.Code, ErrWorkspaceUnknown)
	}
}

func TestAsErrorWrapsPlainErrors(t *testing.T) {
	t.Run("nil stays nil", func(t *testing.T) {
		if got := AsError(nil); got != nil {
			t.Errorf("AsError(nil) = %v, want nil", got)
		}
	})

	t.Run("structured error passes through", func(t *testing.T) {
		orig := NewError(ErrNotReady, "starting").WithRetryAfterMS(1500)
		got := AsError(orig)
		if got != orig {
			t.Errorf("AsError did not pass the structured error through: %v", got)
		}
	})

	t.Run("wrapped structured error is recovered", func(t *testing.T) {
		orig := NewError(ErrStaleEdit, "file changed")
		got := AsError(fmt.Errorf("apply failed: %w", orig))
		if got.Code != ErrStaleEdit {
			t.Errorf("Code = %q, want %q", got.Code, ErrStaleEdit)
		}
	})

	t.Run("plain error becomes INTERNAL", func(t *testing.T) {
		got := AsError(errors.New("boom"))
		if got.Code != ErrInternal {
			t.Errorf("Code = %q, want %q", got.Code, ErrInternal)
		}
		if got.Message != "boom" {
			t.Errorf("Message = %q, want %q", got.Message, "boom")
		}
	})
}

func TestWithRetryAfterMS(t *testing.T) {
	e := NewError(ErrNotReady, "pyright is indexing").WithRetryAfterMS(1500)
	if e.RetryAfterMS != 1500 {
		t.Fatalf("RetryAfterMS = %d, want 1500", e.RetryAfterMS)
	}
	b, err := json.Marshal(e.Result())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"error":{"code":"NOT_READY","message":"pyright is indexing","retry_after_ms":1500}}`
	if string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
}

// TestIPCEnvelopeCarriesVersion covers SPEC §3.5: "The IPC protocol carries a
// version field for daemon/MCP-server compatibility checks."
func TestIPCEnvelopeCarriesVersion(t *testing.T) {
	req := NewRequest(7, "get_definition", json.RawMessage(`{"path":"a.ts"}`))
	if req.Version != Version {
		t.Errorf("Request.Version = %d, want %d", req.Version, Version)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"id":7,"method":"get_definition","params":{"path":"a.ts"}}`
	if string(b) != want {
		t.Errorf("request json = %s, want %s", b, want)
	}

	errResp := NewErrorResponse(7, NewError(ErrUnsupported, "no rename support"))
	b, err = json.Marshal(errResp)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"version":1,"id":7,"error":{"code":"UNSUPPORTED","message":"no rename support"}}`
	if string(b) != want {
		t.Errorf("error response json = %s, want %s", b, want)
	}

	okResp, err := NewResponse(7, []Location{{Path: "a.ts", IsDefinition: true}})
	if err != nil {
		t.Fatal(err)
	}
	if okResp.Error != nil {
		t.Errorf("Error = %v, want nil", okResp.Error)
	}
	if okResp.Version != Version {
		t.Errorf("Response.Version = %d, want %d", okResp.Version, Version)
	}
}

func TestSymbolKindFromLSP(t *testing.T) {
	cases := map[int]SymbolKind{
		1:  SymbolKindFile,
		5:  SymbolKindClass,
		6:  SymbolKindMethod,
		12: SymbolKindFunction,
		13: SymbolKindVariable,
		23: SymbolKindStruct,
		26: SymbolKindTypeParameter,
		0:  SymbolKindUnknown,
		99: SymbolKindUnknown,
	}
	for in, want := range cases {
		if got := SymbolKindFromLSP(in); got != want {
			t.Errorf("SymbolKindFromLSP(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSeverityFromLSP(t *testing.T) {
	cases := map[int]Severity{
		1: SeverityError,
		2: SeverityWarning,
		3: SeverityInformation,
		4: SeverityHint,
		0: SeverityError, // LSP: absent severity is client-defined; we assume error
		9: SeverityError,
	}
	for in, want := range cases {
		if got := SeverityFromLSP(in); got != want {
			t.Errorf("SeverityFromLSP(%d) = %q, want %q", in, got, want)
		}
	}
}
