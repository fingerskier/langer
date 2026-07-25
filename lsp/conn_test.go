package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

// newTestConn wires a conn to a scripted server over net.Pipe.
func newTestConn(t *testing.T) (*conn, *scriptedServer) {
	t.Helper()
	clientSide, serverSide := net.Pipe()

	srv := newScriptedServer(t, serverSide)
	go srv.run()

	c := newConn(clientSide, connHandlers{})
	t.Cleanup(func() {
		c.close(nil)
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	return c, srv
}

func TestConnRoundTripsARequest(t *testing.T) {
	c, srv := newTestConn(t)
	srv.handle("textDocument/hover", reply(`{"contents":"hi"}`))

	got, err := c.call(testContext(t), "textDocument/hover", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(got) != `{"contents":"hi"}` {
		t.Fatalf("result = %s", got)
	}
}

// Responses arrive OUT OF ORDER. Correlating by anything other than the id
// hands one request's answer to another.
func TestConnCorrelatesOutOfOrderResponses(t *testing.T) {
	c, srv := newTestConn(t)

	release := make(chan struct{})
	srv.handle("slow", func(json.RawMessage) (any, *wire.RPCError) {
		<-release
		return json.RawMessage(`"slow-result"`), nil
	})
	srv.handle("fast", reply(`"fast-result"`))

	slowDone := make(chan string, 1)
	go func() {
		out, err := c.call(testContext(t), "slow", nil)
		if err != nil {
			slowDone <- "error: " + err.Error()
			return
		}
		slowDone <- string(out)
	}()

	// Give the slow request time to be in flight, then overtake it.
	time.Sleep(50 * time.Millisecond)
	fast, err := c.call(testContext(t), "fast", nil)
	if err != nil {
		t.Fatalf("fast call: %v", err)
	}
	if string(fast) != `"fast-result"` {
		t.Fatalf("fast result = %s", fast)
	}

	close(release)
	select {
	case got := <-slowDone:
		if got != `"slow-result"` {
			t.Fatalf("slow result = %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow call never completed")
	}
}

// Servers send client/registerCapability and window/workDoneProgress/create as
// SERVER-TO-CLIENT requests and block until answered. A client that only ever
// writes-then-reads deadlocks here, and it looks like a timeout.
func TestConnAnswersServerToClientRequests(t *testing.T) {
	c, srv := newTestConn(t)
	_ = c

	for _, method := range []string{
		"client/registerCapability",
		"window/workDoneProgress/create",
		"window/showMessageRequest",
		"some/method/we/have/never/heard/of",
	} {
		m, err := srv.request(method, map[string]any{}, 3*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if m.Error != nil {
			t.Fatalf("%s answered with an error: %v", method, m.Error)
		}
	}
}

// pyright asks for configuration and expects ONE settings entry per requested
// item. A bare null makes it retry or misbehave.
func TestConnAnswersWorkspaceConfigurationPerItem(t *testing.T) {
	_, srv := newTestConn(t)

	m, err := srv.request("workspace/configuration", map[string]any{
		"items": []any{
			map[string]any{"section": "python"},
			map[string]any{"section": "python.analysis"},
		},
	}, 3*time.Second)
	if err != nil {
		t.Fatalf("workspace/configuration: %v", err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(m.Result, &items); err != nil {
		t.Fatalf("result %s is not an array: %v", m.Result, err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d settings entries, want one per requested item (2)", len(items))
	}
}

// A request that blocks the reader goroutine would stall every response behind
// it. Answering server requests must not serialise behind our own in-flight
// calls either.
func TestConnServerRequestDuringAnInFlightCall(t *testing.T) {
	c, srv := newTestConn(t)

	release := make(chan struct{})
	srv.handle("slow", func(json.RawMessage) (any, *wire.RPCError) {
		// While answering, ask the client something. If the client cannot
		// answer until its own call returns, this deadlocks.
		go func() {
			_, _ = srv.request("client/registerCapability", map[string]any{}, 3*time.Second)
			close(release)
		}()
		<-release
		return json.RawMessage(`"done"`), nil
	})

	got, err := c.call(testContext(t), "slow", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(got) != `"done"` {
		t.Fatalf("result = %s", got)
	}
}

// pyright sends pyright/beginProgress with `params` as an ARRAY. An unknown
// notification must be ignored, never fatal.
func TestConnIgnoresUnknownNotifications(t *testing.T) {
	c, srv := newTestConn(t)
	srv.handle("ping", reply(`"pong"`))

	srv.push("pyright/beginProgress", []any{"work", 1})
	srv.push("window/logMessage", map[string]any{"type": 3, "message": "noise"})
	srv.push("$/progress", map[string]any{"token": "t", "value": map[string]any{"kind": "begin"}})
	srv.push("telemetry/event", nil)

	got, err := c.call(testContext(t), "ping", nil)
	if err != nil {
		t.Fatalf("connection died on an unknown notification: %v", err)
	}
	if string(got) != `"pong"` {
		t.Fatalf("result = %s", got)
	}
}

func TestConnSurfacesRPCErrors(t *testing.T) {
	c, srv := newTestConn(t)
	srv.handle("boom", func(json.RawMessage) (any, *wire.RPCError) {
		return nil, &wire.RPCError{Code: -32603, Message: "internal server trouble"}
	})

	_, err := c.call(testContext(t), "boom", nil)
	if err == nil {
		t.Fatal("call returned nil for an error response")
	}
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not structured", err)
	}
	if pe.Code != protocol.ErrInternal {
		t.Fatalf("code = %s, want INTERNAL", pe.Code)
	}
}

// LSP's MethodNotFound is the server telling us it does not implement a
// capability. That is UNSUPPORTED, not INTERNAL.
func TestConnMapsMethodNotFoundToUnsupported(t *testing.T) {
	c, _ := newTestConn(t)

	_, err := c.call(testContext(t), "textDocument/somethingUnimplemented", nil)
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not structured", err)
	}
	if pe.Code != protocol.ErrUnsupported {
		t.Fatalf("code = %s, want UNSUPPORTED", pe.Code)
	}
}

// On EOF the reader must fail EVERY pending call with SERVER_CRASHED. Leaving
// them to time out leaks a goroutine and a channel per in-flight request, on
// every crash.
func TestConnFailsPendingCallsOnEOF(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newScriptedServer(t, serverSide)
	srv.handle("hang", hangs(t))
	go srv.run()

	c := newConn(clientSide, connHandlers{})

	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := c.call(context.Background(), "hang", nil)
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)

	// The server process dies.
	_ = serverSide.Close()

	for i := 0; i < 4; i++ {
		select {
		case err := <-errs:
			var pe *protocol.Error
			if !errors.As(err, &pe) {
				t.Fatalf("pending call %d failed with %v, want a structured error", i, err)
			}
			if pe.Code != protocol.ErrServerCrashed {
				t.Fatalf("pending call %d code = %s, want SERVER_CRASHED", i, pe.Code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("pending call %d was never failed after EOF", i)
		}
	}
	_ = clientSide.Close()
}

func TestConnCallAfterCloseIsServerCrashed(t *testing.T) {
	c, _ := newTestConn(t)
	c.close(nil)

	_, err := c.call(testContext(t), "anything", nil)
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not structured", err)
	}
	if pe.Code != protocol.ErrServerCrashed {
		t.Fatalf("code = %s, want SERVER_CRASHED", pe.Code)
	}
}

func TestConnCallHonoursContextCancellation(t *testing.T) {
	c, srv := newTestConn(t)
	srv.handle("hang", hangs(t))

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, "hang", nil)
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			var pe *protocol.Error
			if !errors.As(err, &pe) {
				t.Fatalf("call returned %v, want a cancellation", err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call ignored its cancelled context")
	}
}

// A cancelled call must not leave its slot in the pending map: a long-lived
// server plus a chatty agent would otherwise grow it without bound.
func TestConnCancelledCallReleasesItsPendingSlot(t *testing.T) {
	c, srv := newTestConn(t)
	srv.handle("hang", hangs(t))

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, _ = c.call(ctx, "hang", nil)
		cancel()
	}

	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d pending entries left after cancellation, want 0", n)
	}
}

func TestConnNotifyDoesNotWaitForAnAnswer(t *testing.T) {
	c, srv := newTestConn(t)

	if err := c.notify("textDocument/didOpen", map[string]any{"x": 1}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !srv.sawMethod("textDocument/didOpen") {
		if time.Now().After(deadline) {
			t.Fatal("server never saw the notification")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConnDiagnosticsNotificationsReachTheHandler(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newScriptedServer(t, serverSide)
	go srv.run()

	got := make(chan string, 1)
	c := newConn(clientSide, connHandlers{
		diagnostics: func(uri string, _ []wire.RawDiagnostic) {
			select {
			case got <- uri:
			default:
			}
		},
	})
	t.Cleanup(func() {
		c.close(nil)
		_ = clientSide.Close()
		_ = serverSide.Close()
	})

	srv.publishDiagnostics("file:///r/a.ts", nil)
	select {
	case uri := <-got:
		if uri != "file:///r/a.ts" {
			t.Fatalf("uri = %q", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publishDiagnostics never reached the handler")
	}
}
