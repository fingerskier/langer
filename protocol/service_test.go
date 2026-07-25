package protocol

import (
	"context"
	"reflect"
	"testing"
)

// TestServiceCoversTheSpecMethodSurface pins protocol.Service to SPEC §3.5's
// conceptual method list. The daemon and the socket client are the two
// implementations; if one of them drifts, this is what notices.
func TestServiceCoversTheSpecMethodSurface(t *testing.T) {
	want := []string{
		// SPEC §3.5, in the order the spec lists them.
		"OpenWorkspace", "CloseWorkspace",
		"OpenDocument", "CloseDocument",
		"GetDefinition", "GetReferences", "GetHover",
		"DocumentSymbols", "WorkspaceSymbols", "GetDiagnostics",
		"RenameSymbol", "ApplyEdit", "SimulateEdit",
		"IndexStatus",
		// EndSession is the one addition (docs/ARCHITECTURE.md §4.2, §11 item 7).
		"EndSession",
	}

	iface := reflect.TypeOf((*Service)(nil)).Elem()
	got := make(map[string]bool, iface.NumMethod())
	for i := 0; i < iface.NumMethod(); i++ {
		got[iface.Method(i).Name] = true
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("protocol.Service is missing %s", name)
		}
		delete(got, name)
	}
	for name := range got {
		t.Errorf("protocol.Service has undeclared method %s (SPEC §3.5 does not name it)", name)
	}
}

// TestServiceMethodsHaveTheUniformShape guards the property the daemon's
// dispatch table and the client's generic marshalling both depend on: every
// method is (ctx, Params) (Result, error).
func TestServiceMethodsHaveTheUniformShape(t *testing.T) {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()

	iface := reflect.TypeOf((*Service)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		fn := m.Type
		if fn.NumIn() != 2 {
			t.Errorf("%s takes %d arguments, want (ctx, params)", m.Name, fn.NumIn())
			continue
		}
		if fn.In(0) != ctxType {
			t.Errorf("%s's first argument is %s, want context.Context", m.Name, fn.In(0))
		}
		if fn.NumOut() != 2 {
			t.Errorf("%s returns %d values, want (result, error)", m.Name, fn.NumOut())
			continue
		}
		if fn.Out(1) != errType {
			// Returning *Error rather than error lets a nil pointer masquerade
			// as a non-nil error (docs/ARCHITECTURE.md §4.2).
			t.Errorf("%s's second result is %s, want error", m.Name, fn.Out(1))
		}
		if fn.Out(0).Kind() == reflect.Ptr || fn.Out(0).Kind() == reflect.Slice {
			t.Errorf("%s returns %s; results are objects so MCP can carry them structurally", m.Name, fn.Out(0))
		}
	}
}

// TestMethodNamesAreWireStable spells out every method name that travels on the
// socket. A rename here is a protocol break.
func TestMethodNamesAreWireStable(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{MethodEndSession, "end_session"},
		{MethodHandshake, "handshake"},
		{MethodDrain, "drain"},
	} {
		if tc.got != tc.want {
			t.Errorf("method name = %q, want %q", tc.got, tc.want)
		}
	}
}
