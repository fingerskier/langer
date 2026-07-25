// Package daemonctl is the client half of the daemon IPC: dialling a
// workspace daemon, auto-starting one when none is listening (SPEC §3.1), and
// speaking protocol.Service over the socket.
//
// It runs IN THE MCP PROCESS and deliberately cannot reach daemon state: it
// imports neither daemon/ nor internal/workspace/, so the process boundary
// stays honest and the MCP tool surface remains testable with no daemon at all.
package daemonctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/fingerskier/langer/protocol"
)

// Client is a protocol.Service backed by a socket connection to a daemon.
//
// It is safe for concurrent use: one reader goroutine demultiplexes responses
// by id, because the daemon answers requests concurrently and therefore out of
// order.
type Client struct {
	conn  net.Conn
	codec *protocol.Codec
	log   *slog.Logger
	root  string

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *protocol.Response
	closed  bool
	cause   error

	done chan struct{}
}

var _ protocol.Service = (*Client)(nil)

func newClient(conn net.Conn, root string, log *slog.Logger) *Client {
	c := &Client{
		conn:    conn,
		codec:   protocol.NewCodec(conn),
		log:     log,
		root:    root,
		pending: map[int64]chan *protocol.Response{},
		done:    make(chan struct{}),
	}
	go c.read()
	return c
}

// Root is the workspace this client is connected to.
func (c *Client) Root() string { return c.root }

// Close hangs up. In-flight calls fail with a structured error rather than
// waiting out their deadlines.
func (c *Client) Close() error {
	c.fail(protocol.NewError(protocol.ErrNotReady, "daemon connection closed by this client").
		WithRetryAfterMS(50))
	return nil
}

// Done is closed when the connection is gone, whether we closed it or the
// daemon did.
func (c *Client) Done() <-chan struct{} { return c.done }

// read demultiplexes responses until the connection dies.
func (c *Client) read() {
	for {
		resp, err := c.codec.ReadResponse()
		if err != nil {
			c.fail(connectionLost(err))
			return
		}

		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if !ok {
			// A response to a call that already gave up. Dropping it is right;
			// logging it is how a protocol bug becomes visible.
			c.log.Debug("response for an unknown request id", "id", resp.ID)
			continue
		}
		ch <- resp
	}
}

// fail tears the client down and completes every waiting call with cause. A
// caller left to time out is a caller that cannot tell a dead daemon from a
// slow one.
func (c *Client) fail(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.cause = cause
	pending := c.pending
	c.pending = map[int64]chan *protocol.Response{}
	c.mu.Unlock()

	_ = c.codec.Close()
	close(c.done)

	for id, ch := range pending {
		ch <- protocol.NewErrorResponse(id, protocol.AsError(cause))
	}
}

// connectionLost renders a dead socket as a SPEC §3.6 code.
//
// NOT_READY, not INTERNAL: a daemon that sunset or was restarted is a normal
// event, and the correct agent action is exactly what NOT_READY means — retry,
// at which point the MCP layer auto-starts a fresh daemon.
func connectionLost(cause error) error {
	if cause == nil || errors.Is(cause, io.EOF) || errors.Is(cause, net.ErrClosed) {
		return protocol.NewError(protocol.ErrNotReady, "daemon connection closed").WithRetryAfterMS(100)
	}
	return protocol.NewErrorf(protocol.ErrNotReady, "daemon connection lost: %v", cause).WithRetryAfterMS(100)
}

// send performs one round trip.
func (c *Client) send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "encoding %s params: %v", method, err)
	}

	c.mu.Lock()
	if c.closed {
		cause := c.cause
		c.mu.Unlock()
		return nil, protocol.AsError(cause)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *protocol.Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.codec.WriteRequest(protocol.NewRequest(id, method, raw)); err != nil {
		c.forget(id)
		return nil, protocol.AsError(connectionLost(err))
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, ctxError(ctx, method)
	case <-c.done:
		c.mu.Lock()
		cause := c.cause
		c.mu.Unlock()
		return nil, protocol.AsError(cause)
	}
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

// ctxError renders an expired or cancelled context as a SPEC §3.6 code, the
// same way lsp/conn.go does: §3.6 admits no unstructured error, because the
// agent on the far end has to be able to branch on a code.
func ctxError(ctx context.Context, method string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return protocol.NewErrorf(protocol.ErrNotReady, "%s timed out", method).WithRetryAfterMS(200)
	}
	return protocol.NewErrorf(protocol.ErrNotReady, "%s was cancelled", method)
}

// call is one typed round trip. Every Service method has the same shape, so
// this is the only marshalling code in the client.
func call[P any, R any](ctx context.Context, c *Client, method string, params P) (R, error) {
	var result R
	raw, err := c.send(ctx, method, params)
	if err != nil {
		return result, err
	}
	if len(raw) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, protocol.NewErrorf(protocol.ErrInternal, "decoding %s result: %v", method, err)
	}
	return result, nil
}

// ---- protocol.Service ------------------------------------------------------

// OpenWorkspace opens (or joins) a workspace on the daemon.
func (c *Client) OpenWorkspace(ctx context.Context, p protocol.OpenWorkspaceParams) (protocol.OpenWorkspaceResult, error) {
	return call[protocol.OpenWorkspaceParams, protocol.OpenWorkspaceResult](ctx, c, protocol.MethodOpenWorkspace, p)
}

// CloseWorkspace releases a workspace.
func (c *Client) CloseWorkspace(ctx context.Context, p protocol.CloseWorkspaceParams) (protocol.EmptyResult, error) {
	return call[protocol.CloseWorkspaceParams, protocol.EmptyResult](ctx, c, protocol.MethodCloseWorkspace, p)
}

// OpenDocument marks a file active.
func (c *Client) OpenDocument(ctx context.Context, p protocol.OpenDocumentParams) (protocol.EmptyResult, error) {
	return call[protocol.OpenDocumentParams, protocol.EmptyResult](ctx, c, protocol.MethodOpenDocument, p)
}

// CloseDocument drops a session's claim on a file.
func (c *Client) CloseDocument(ctx context.Context, p protocol.DocumentParams) (protocol.EmptyResult, error) {
	return call[protocol.DocumentParams, protocol.EmptyResult](ctx, c, protocol.MethodCloseDocument, p)
}

// GetDefinition asks for a definition.
func (c *Client) GetDefinition(ctx context.Context, p protocol.PositionParams) (protocol.LocationsResult, error) {
	return call[protocol.PositionParams, protocol.LocationsResult](ctx, c, protocol.MethodGetDefinition, p)
}

// GetReferences asks for references.
func (c *Client) GetReferences(ctx context.Context, p protocol.PositionParams) (protocol.LocationsResult, error) {
	return call[protocol.PositionParams, protocol.LocationsResult](ctx, c, protocol.MethodGetReferences, p)
}

// GetHover asks for hover information.
func (c *Client) GetHover(ctx context.Context, p protocol.PositionParams) (protocol.HoverResult, error) {
	return call[protocol.PositionParams, protocol.HoverResult](ctx, c, protocol.MethodGetHover, p)
}

// DocumentSymbols asks for a file's outline.
func (c *Client) DocumentSymbols(ctx context.Context, p protocol.DocumentParams) (protocol.SymbolsResult, error) {
	return call[protocol.DocumentParams, protocol.SymbolsResult](ctx, c, protocol.MethodDocumentSymbols, p)
}

// WorkspaceSymbols searches the workspace.
func (c *Client) WorkspaceSymbols(ctx context.Context, p protocol.WorkspaceSymbolsParams) (protocol.SymbolsResult, error) {
	return call[protocol.WorkspaceSymbolsParams, protocol.SymbolsResult](ctx, c, protocol.MethodWorkspaceSymbol, p)
}

// GetDiagnostics asks for diagnostics.
func (c *Client) GetDiagnostics(ctx context.Context, p protocol.DiagnosticsParams) (protocol.DiagnosticsResult, error) {
	return call[protocol.DiagnosticsParams, protocol.DiagnosticsResult](ctx, c, protocol.MethodGetDiagnostics, p)
}

// RenameSymbol computes a rename dry-run.
func (c *Client) RenameSymbol(ctx context.Context, p protocol.RenameParams) (protocol.EditPlanResult, error) {
	return call[protocol.RenameParams, protocol.EditPlanResult](ctx, c, protocol.MethodRenameSymbol, p)
}

// ApplyEdit applies a previously computed plan.
func (c *Client) ApplyEdit(ctx context.Context, p protocol.ApplyEditParams) (protocol.ApplyResult, error) {
	return call[protocol.ApplyEditParams, protocol.ApplyResult](ctx, c, protocol.MethodApplyEdit, p)
}

// SimulateEdit reports speculative diagnostics.
func (c *Client) SimulateEdit(ctx context.Context, p protocol.SimulateEditParams) (protocol.DiagnosticsResult, error) {
	return call[protocol.SimulateEditParams, protocol.DiagnosticsResult](ctx, c, protocol.MethodSimulateEdit, p)
}

// IndexStatus reports index and supervision state.
func (c *Client) IndexStatus(ctx context.Context, p protocol.IndexStatusParams) (protocol.IndexStatusResult, error) {
	return call[protocol.IndexStatusParams, protocol.IndexStatusResult](ctx, c, protocol.MethodIndexStatus, p)
}

// EndSession releases everything a session owns.
func (c *Client) EndSession(ctx context.Context, p protocol.EndSessionParams) (protocol.EmptyResult, error) {
	return call[protocol.EndSessionParams, protocol.EmptyResult](ctx, c, protocol.MethodEndSession, p)
}

// ---- lifecycle methods, not part of Service --------------------------------

func (c *Client) handshake(ctx context.Context, session protocol.SessionID) (protocol.HandshakeResult, error) {
	return call[protocol.HandshakeParams, protocol.HandshakeResult](ctx, c, protocol.MethodHandshake,
		protocol.HandshakeParams{Session: session, ClientVersion: protocol.Version})
}

func (c *Client) requestDrain(ctx context.Context, session protocol.SessionID, reason string) error {
	_, err := call[protocol.DrainParams, protocol.DrainResult](ctx, c, protocol.MethodDrain,
		protocol.DrainParams{Session: session, Reason: reason})
	return err
}
