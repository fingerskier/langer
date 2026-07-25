package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"

	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

// LSP JSON-RPC error codes we translate rather than pass through.
const (
	rpcMethodNotFound  = -32601
	rpcRequestCanceled = -32800
	rpcContentModified = -32801
)

// connHandlers are the callbacks a conn invokes for server-initiated traffic.
// Both are optional; a nil handler means "ignore".
type connHandlers struct {
	// diagnostics receives every textDocument/publishDiagnostics push. It runs
	// on the reader goroutine, so it must not block.
	diagnostics func(uri string, diags []wire.RawDiagnostic)
	// closed is invoked exactly once, when the reader stops. It is how the
	// supervisor learns the server died.
	closed func(err error)
	// analyzing reports whether the server has started (true) or finished
	// (false) a workspace analysis pass. It runs on the reader goroutine, so it
	// must not block.
	analyzing func(bool)
}

// conn is one bidirectional JSON-RPC peering with a language server.
//
// The client MUST be a peer, not a caller: servers send client/registerCapability
// and window/workDoneProgress/create as server-to-client REQUESTS and block
// until answered. A write-request-then-read-response client deadlocks, and the
// symptom is a timeout rather than a protocol error.
type conn struct {
	framer *wire.Framer
	log    *slog.Logger
	h      connHandlers

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan wire.Message
	closed  bool
	cause   error

	done   chan struct{}
	reader chan struct{} // closed when the reader goroutine has exited
}

// newConn starts the reader goroutine and returns immediately.
func newConn(rw io.ReadWriter, h connHandlers) *conn {
	c := &conn{
		framer:  wire.NewFramer(rw),
		log:     slog.Default(),
		h:       h,
		pending: map[string]chan wire.Message{},
		done:    make(chan struct{}),
		reader:  make(chan struct{}),
	}
	go c.read()
	return c
}

// withLogger replaces the connection's logger. Kept separate from newConn so
// tests do not have to supply one.
func (c *conn) withLogger(log *slog.Logger) *conn {
	if log != nil {
		c.log = log
	}
	return c
}

// call issues a request and waits for its response.
func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	encoded, err := encodeParams(params)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "encoding %s params: %v", method, err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, crashedError(c.cause)
	}
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	key := string(id)
	ch := make(chan wire.Message, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	// Whatever happens, this request's slot goes away: a cancelled call that
	// left its entry behind would grow the map without bound.
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	if err := c.framer.Write(wire.Message{ID: id, Method: method, Params: encoded}); err != nil {
		return nil, crashedError(err)
	}

	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, rpcToProtocol(method, m.Error)
		}
		return m.Result, nil
	case <-c.done:
		return nil, crashedError(c.closeCause())
	case <-ctx.Done():
		return nil, ctxError(ctx, method)
	}
}

// notify sends a notification. Notifications have no id and are never answered.
func (c *conn) notify(method string, params any) error {
	encoded, err := encodeParams(params)
	if err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "encoding %s params: %v", method, err)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return crashedError(c.closeCause())
	}
	if err := c.framer.Write(wire.Message{Method: method, Params: encoded}); err != nil {
		return crashedError(err)
	}
	return nil
}

// read owns the receive side for the connection's whole life.
func (c *conn) read() {
	defer close(c.reader)

	for {
		m, err := c.framer.Read()
		if err != nil {
			c.close(err)
			return
		}

		switch {
		case m.IsResponse():
			c.deliver(m)
		case m.IsRequest():
			c.answer(m)
		case m.IsNotification():
			c.dispatchNotification(m)
		default:
			c.log.Debug("lsp: dropping unrecognised frame", "method", m.Method)
		}
	}
}

func (c *conn) deliver(m wire.Message) {
	c.mu.Lock()
	ch, ok := c.pending[string(m.ID)]
	if ok {
		delete(c.pending, string(m.ID))
	}
	c.mu.Unlock()
	if !ok {
		// A response to a request we already gave up on. Dropping it is
		// correct; it must never be handed to a different waiter.
		c.log.Debug("lsp: response for an unknown id", "id", string(m.ID))
		return
	}
	ch <- m
}

// answer replies to a server-to-client request.
//
// Every answer is canned and tiny, so replying inline on the reader goroutine
// cannot deadlock on pipe backpressure — and it guarantees the server is
// unblocked before we read its next frame.
func (c *conn) answer(m wire.Message) {
	result, err := c.serverRequestResult(m)
	if err != nil {
		c.log.Debug("lsp: failed to build a reply", "method", m.Method, "error", err)
		result = nil
	}
	// A null result is the correct answer to every request in this set; an
	// error reply would make some servers retry forever.
	if err := c.framer.Write(wire.Message{ID: m.ID, Result: result}); err != nil {
		c.log.Debug("lsp: failed to answer a server request", "method", m.Method, "error", err)
	}
}

func (c *conn) serverRequestResult(m wire.Message) (json.RawMessage, error) {
	switch m.Method {
	case "workspace/configuration":
		// pyright expects exactly one settings entry per requested item.
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return json.RawMessage(`[]`), nil
		}
		settings := make([]json.RawMessage, len(params.Items))
		for i := range settings {
			settings[i] = json.RawMessage(`null`)
		}
		return json.Marshal(settings)

	case "workspace/applyEdit":
		// v0.1 never lets a server write to the workspace; SPEC §4.2 makes
		// every mutation a dry run the caller applies explicitly.
		return json.RawMessage(`{"applied":false}`), nil

	case "workspace/workspaceFolders":
		return json.RawMessage(`null`), nil

	default:
		// client/registerCapability, window/workDoneProgress/create,
		// window/showMessageRequest and anything a future server invents.
		return nil, nil
	}
}

func (c *conn) dispatchNotification(m wire.Message) {
	// A server that is still analysing the workspace answers workspace/symbol
	// with an EMPTY array rather than an error, which an agent cannot tell from
	// "no such symbol" (docs §10.6). These notifications are how it says so.
	// pyright sends them with `params` as an ARRAY, so params are never parsed.
	switch m.Method {
	case "pyright/beginProgress":
		if c.h.analyzing != nil {
			c.h.analyzing(true)
		}
		return
	case "pyright/endProgress":
		if c.h.analyzing != nil {
			c.h.analyzing(false)
		}
		return
	}

	if m.Method != "textDocument/publishDiagnostics" {
		// Unknown notifications are ignored, never fatal.
		c.log.Debug("lsp: ignoring notification", "method", m.Method)
		return
	}
	if c.h.diagnostics == nil {
		return
	}
	diags, uri, err := wire.DecodePublishDiagnostics(m.Params)
	if err != nil {
		c.log.Debug("lsp: undecodable publishDiagnostics", "error", err)
		return
	}
	c.h.diagnostics(uri, diags)
}

// close tears the connection down and fails every pending call with
// SERVER_CRASHED. It is idempotent.
func (c *conn) close(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.cause = cause
	pending := c.pending
	c.pending = map[string]chan wire.Message{}
	c.mu.Unlock()

	close(c.done)

	// Waiters select on c.done, so nothing is stranded; clearing the map here
	// simply drops our references.
	_ = pending

	if c.h.closed != nil {
		c.h.closed(cause)
	}
}

func (c *conn) closeCause() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// wait blocks until the reader goroutine has exited. Shutdown uses it so
// "every goroutine it started has exited" is a fact, not a hope.
func (c *conn) wait() { <-c.reader }

func encodeParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}

// ctxError renders a caller's expired or cancelled context as a SPEC §3.6
// code. A bare context error is unstructured, and §3.6 admits no unstructured
// error: the agent on the other end has to be able to branch on a code.
//
// NOT_READY is the honest answer — the work did not finish inside the caller's
// budget and retrying is reasonable — and it matches what documents.acquire
// already returns when its own wait expires.
func ctxError(ctx context.Context, what string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return protocol.NewErrorf(protocol.ErrNotReady, "%s timed out", what).WithRetryAfterMS(200)
	}
	return protocol.NewErrorf(protocol.ErrNotReady, "%s was cancelled", what)
}

// crashedError renders any transport failure as SPEC §3.6 SERVER_CRASHED.
func crashedError(cause error) error {
	switch {
	case cause == nil, errors.Is(cause, io.EOF):
		return protocol.NewError(protocol.ErrServerCrashed, "language server connection closed")
	default:
		return protocol.NewErrorf(protocol.ErrServerCrashed, "language server connection lost: %v", cause)
	}
}

// rpcToProtocol maps a JSON-RPC error onto the SPEC §3.6 model. MethodNotFound
// is the server saying it does not implement a capability, which is
// UNSUPPORTED; everything else is a bug somewhere, which is INTERNAL.
func rpcToProtocol(method string, e *wire.RPCError) error {
	switch e.Code {
	case rpcMethodNotFound:
		return protocol.NewErrorf(protocol.ErrUnsupported, "language server does not implement %s: %s", method, e.Message)
	case rpcRequestCanceled, rpcContentModified:
		return protocol.NewErrorf(protocol.ErrNotReady, "%s was superseded by a newer document version: %s", method, e.Message).
			WithRetryAfterMS(100)
	default:
		return protocol.NewErrorf(protocol.ErrInternal, "%s failed: %s (jsonrpc %d)", method, e.Message, e.Code)
	}
}
