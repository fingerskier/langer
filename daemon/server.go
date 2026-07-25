package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/workspace"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/protocol"
)

// Defaults for the daemon's operational bounds.
const (
	// DefaultIdleTimeout is the SPEC §3.1 sunset period.
	DefaultIdleTimeout = 30 * time.Minute
	// DefaultMaxInFlight bounds concurrent requests. Over the limit a caller
	// gets NOT_READY with a retry hint rather than the daemon OOMing a language
	// server on everyone's behalf (docs/ARCHITECTURE.md §6.9).
	DefaultMaxInFlight = 64
	// DefaultRequestTimeout stops one wedged language server call from pinning
	// a slot and a goroutine forever. It is an operational bound, not spec'd
	// behaviour.
	DefaultRequestTimeout = 2 * time.Minute
	// DefaultDrainGrace is how long in-flight requests get to finish before
	// their contexts are cancelled (docs/ARCHITECTURE.md §6.5 step 2).
	DefaultDrainGrace = 5 * time.Second
	// shutdownGrace bounds the language server teardown.
	shutdownGrace = 10 * time.Second
	// backpressureRetryMS is the hint attached to a NOT_READY produced by the
	// in-flight limit.
	backpressureRetryMS = 100
)

// Options configures a daemon Server.
type Options struct {
	// Root is the absolute workspace root this daemon serves (SPEC §3.1: one
	// daemon per workspace).
	Root   string
	Config *config.Config
	Clock  clock.Clock
	Logger *slog.Logger

	IdleTimeout    time.Duration
	MaxInFlight    int
	RequestTimeout time.Duration
	DrainGrace     time.Duration

	// Resolver and Runner are the SPEC §9 process boundary, injectable so the
	// daemon's crash handling can be driven without a real language server.
	Resolver procx.Resolver
	Runner   procx.Runner
	// NewSupervisor builds the language server supervisor for a workspace.
	// Defaults to lsp.NewSupervisor.
	NewSupervisor func(lsp.Options) (lsp.Supervisor, error)
}

func (o *Options) applyDefaults() error {
	if o.Root == "" {
		return protocol.NewError(protocol.ErrInternal, "daemon: Options.Root is required")
	}
	if o.Config == nil {
		return protocol.NewError(protocol.ErrInternal, "daemon: Options.Config is required")
	}
	if o.Clock == nil {
		o.Clock = clock.New()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = DefaultMaxInFlight
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.DrainGrace < 0 {
		o.DrainGrace = DefaultDrainGrace
	}
	return nil
}

// Server is the long-running daemon process (SPEC §3.1, §8).
type Server struct {
	opts   Options
	log    *slog.Logger
	reg    *workspace.Registry
	core   *Core
	sun    *sunset
	socket string

	ready chan struct{}
	sem   chan struct{}
	wg    sync.WaitGroup

	// reqCtx is the parent of every request context. It is NOT derived from the
	// caller's context: shutdown gives in-flight work a grace window and only
	// then cancels this, which a context inherited from a SIGINT could not do.
	reqCtx    context.Context
	cancelReq context.CancelFunc

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// NewServer prepares a daemon. It binds nothing and starts nothing: Run does
// both, so a failure to acquire the single-instance lock is reported to Run's
// caller rather than to a constructor.
func NewServer(opts Options) (*Server, error) {
	if err := opts.applyDefaults(); err != nil {
		return nil, err
	}

	root, err := workspace.CanonicalRoot(opts.Root)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	opts.Root = root

	socket, err := SocketPath(opts.Config, root)
	if err != nil {
		return nil, err
	}

	log := opts.Logger.With("component", "daemon", "root", root)
	reg := workspace.NewRegistry(workspace.RegistryOptions{
		Config:        opts.Config,
		Clock:         opts.Clock,
		Logger:        opts.Logger,
		Resolver:      opts.Resolver,
		Runner:        opts.Runner,
		NewSupervisor: opts.NewSupervisor,
	})

	reqCtx, cancelReq := context.WithCancel(context.Background())
	return &Server{
		opts:      opts,
		log:       log,
		reg:       reg,
		core:      NewCore(reg, opts.Clock),
		sun:       newSunset(opts.Clock, opts.IdleTimeout, log),
		socket:    socket,
		ready:     make(chan struct{}),
		sem:       make(chan struct{}, opts.MaxInFlight),
		reqCtx:    reqCtx,
		cancelReq: cancelReq,
		conns:     map[net.Conn]struct{}{},
	}, nil
}

// Ready is closed once the daemon is listening. A client that spawned this
// process polls the socket instead; this is for in-process callers and tests.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// SocketPath is where this daemon listens.
func (s *Server) SocketPath() string { return s.socket }

// Root is the workspace this daemon serves.
func (s *Server) Root() string { return s.opts.Root }

// Run holds the liveness lock, binds the socket at 0600, serves until ctx is
// done or the sunset actor fires, then executes the docs §6.5 shutdown
// ordering. It does not return until every goroutine it started has exited.
func (s *Server) Run(ctx context.Context) error {
	if _, err := s.opts.Config.EnsureRuntimeDir(); err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "%v", err)
	}

	livenessPath, _, err := LockPaths(s.opts.Config, s.opts.Root)
	if err != nil {
		return err
	}
	lock, err := acquireLiveness(livenessPath)
	if err != nil {
		return err
	}
	defer func() { _ = lock.release() }()

	listener, err := listenUnix(s.socket)
	if err != nil {
		return err
	}
	// The socket file is removed on the way out; the lock file is not (see
	// livenessLock.release).
	defer func() { _ = os.Remove(s.socket) }()

	s.log.Info("daemon listening", "socket", s.socket, "pid", os.Getpid(), "idle_timeout", s.opts.IdleTimeout)
	close(s.ready)

	s.wg.Add(1)
	go s.accept(listener)

	// The sunset actor runs on this goroutine: it is the single serialisation
	// point for liveness, and running it here means Run cannot return while it
	// is still deciding.
	reason := s.sun.run(ctx)
	s.log.Info("daemon stopping", "reason", string(reason))

	// docs/ARCHITECTURE.md §6.5, in order.
	// 1. Stop accepting. New connections are already refused by the sunset
	//    actor's draining flag; closing the listener makes it explicit.
	_ = listener.Close()
	// 2. Give in-flight requests a grace window, then cancel them.
	s.sun.waitQuiet(s.opts.DrainGrace)
	s.cancelReq()
	// 3. Close client connections, which unblocks their readers.
	s.closeConns()
	// Join the acceptor and every connection goroutine before touching the
	// language servers they might still be calling into.
	s.wg.Wait()

	// 4-6. Workspaces, then their language servers: shutdown request, exit
	// notification, wait, kill the process group.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := s.reg.Shutdown(stopCtx); err != nil {
		s.log.Warn("workspace shutdown reported an error", "error", err)
	}

	// 8. Socket and lock go in the deferred calls above.
	if reason == reasonContext && ctx.Err() != nil {
		// A signal is a normal way to stop; it is not a failure to report.
		return nil
	}
	return nil
}

// accept owns the listener and hands every connection to its own goroutine.
func (s *Server) accept(listener net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !s.sun.isDraining() {
				s.log.Warn("accept failed", "error", err)
			}
			return
		}
		s.wg.Add(1)
		go s.serve(conn)
	}
}

func (s *Server) trackConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[conn] = struct{}{}
}

func (s *Server) untrackConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

func (s *Server) closeConns() {
	s.mu.Lock()
	all := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		all = append(all, conn)
	}
	s.mu.Unlock()
	for _, conn := range all {
		_ = conn.Close()
	}
}

// serve owns one connection's READ side. Writes go through the codec's internal
// mutex, so handlers running concurrently cannot interleave frames.
func (s *Server) serve(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	s.trackConn(conn)
	defer s.untrackConn(conn)

	codec := protocol.NewCodec(conn)
	sessions := newSessionSet()

	admitted := s.sun.addClient()
	if admitted {
		defer s.sun.removeClient()
	}

	// Every handler for this connection must finish before serve returns, or
	// the daemon could tear down a language server out from under one.
	var handlers sync.WaitGroup
	defer func() {
		handlers.Wait()
		// The client is gone: drop everything its sessions owned (SPEC §4.2).
		for _, id := range sessions.all() {
			_, _ = s.core.EndSession(context.WithoutCancel(s.reqCtx), protocol.EndSessionParams{Session: id})
		}
	}()

	for {
		req, err := codec.ReadRequest()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !s.sun.isDraining() {
				s.log.Debug("connection read ended", "error", err)
			}
			return
		}

		if !admitted {
			// Draining: refuse politely and structurally, so the client starts a
			// fresh daemon instead of surfacing a transport error to the agent.
			s.reply(codec, protocol.NewErrorResponse(req.ID,
				protocol.NewError(protocol.ErrNotReady, "daemon is draining; start a new one").
					WithRetryAfterMS(50)))
			continue
		}

		handlers.Add(1)
		go func(req *protocol.Request) {
			defer handlers.Done()
			s.handle(codec, sessions, req)
		}(req)
	}
}

func (s *Server) reply(codec *protocol.Codec, resp *protocol.Response) {
	if err := codec.WriteResponse(resp); err != nil {
		s.log.Debug("writing a response failed", "error", err)
	}
}

// handle answers one request.
func (s *Server) handle(codec *protocol.Codec, sessions *sessionSet, req *protocol.Request) {
	// handshake and drain are answered regardless of protocol version. That is
	// the whole point of them: a client that has just discovered a version
	// mismatch must still be able to ask the old daemon to stand down
	// (SPEC §3.1).
	switch req.Method {
	case protocol.MethodHandshake:
		s.reply(codec, s.handshake(req))
		return
	case protocol.MethodDrain:
		s.reply(codec, s.drain(req))
		return
	}

	if err := protocol.CheckVersion(req.Version); err != nil {
		s.reply(codec, protocol.NewErrorResponse(req.ID, err))
		return
	}

	handler, ok := s.dispatch(req.Method)
	if !ok {
		s.reply(codec, protocol.NewErrorResponse(req.ID,
			protocol.NewErrorf(protocol.ErrInternal, "unknown method %q", req.Method)))
		return
	}

	session, err := sessionOf(req.Method, req.Params)
	if err != nil {
		s.reply(codec, protocol.NewErrorResponse(req.ID, protocol.AsError(err)))
		return
	}
	sessions.note(session)

	// Backpressure. An unbounded goroutine-per-request model lets one confused
	// agent take the workspace down for every other client (docs §6.9).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.reply(codec, protocol.NewErrorResponse(req.ID,
			protocol.NewErrorf(protocol.ErrNotReady, "daemon is at its %d request limit", s.opts.MaxInFlight).
				WithRetryAfterMS(backpressureRetryMS)))
		return
	}

	if !s.sun.beginRequest() {
		s.reply(codec, protocol.NewErrorResponse(req.ID,
			protocol.NewError(protocol.ErrNotReady, "daemon is draining; start a new one").
				WithRetryAfterMS(50)))
		return
	}
	defer s.sun.endRequest()

	ctx, cancel := context.WithTimeout(s.reqCtx, s.opts.RequestTimeout)
	defer cancel()

	result, err := handler(ctx, req.Params)
	if err != nil {
		s.reply(codec, protocol.NewErrorResponse(req.ID, protocol.AsError(err)))
		return
	}
	resp, err := protocol.NewResponse(req.ID, result)
	if err != nil {
		s.reply(codec, protocol.NewErrorResponse(req.ID,
			protocol.NewErrorf(protocol.ErrInternal, "encoding %s result: %v", req.Method, err)))
		return
	}
	s.reply(codec, resp)
}

// handshake answers the first request on a connection. It is deliberately
// version-agnostic: a client learns the daemon's version from the result and
// decides whether to drain it.
func (s *Server) handshake(req *protocol.Request) *protocol.Response {
	if s.sun.isDraining() {
		return protocol.NewErrorResponse(req.ID,
			protocol.NewError(protocol.ErrNotReady, "daemon is draining; start a new one").WithRetryAfterMS(50))
	}
	resp, err := protocol.NewResponse(req.ID, protocol.HandshakeResult{
		ProtocolVersion: protocol.Version,
		Root:            s.opts.Root,
		PID:             os.Getpid(),
	})
	if err != nil {
		return protocol.NewErrorResponse(req.ID, protocol.AsError(err))
	}
	return resp
}

// drain asks the daemon to stand down once its in-flight work finishes. It is
// NOT a kill: SPEC §3.1 says the old daemon drains.
func (s *Server) drain(req *protocol.Request) *protocol.Response {
	var params protocol.DrainParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	reason := params.Reason
	if reason == "" {
		reason = "client requested"
	}
	s.sun.requestDrain(reason)

	resp, err := protocol.NewResponse(req.ID, protocol.DrainResult{Draining: true})
	if err != nil {
		return protocol.NewErrorResponse(req.ID, protocol.AsError(err))
	}
	return resp
}

// handlerFunc is one method's decode-and-delegate step.
type handlerFunc func(ctx context.Context, raw json.RawMessage) (any, error)

// bind adapts a (ctx, Params) (Result, error) method into a handlerFunc. Every
// Service method has that shape, which is why the daemon can dispatch from a
// table instead of fifteen bespoke cases.
func bind[P any, R any](method string, fn func(context.Context, P) (R, error)) handlerFunc {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params P
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding %s params: %v", method, err)
			}
		}
		return fn(ctx, params)
	}
}

func (s *Server) dispatch(method string) (handlerFunc, bool) {
	switch method {
	case protocol.MethodOpenWorkspace:
		return bind(method, s.openWorkspace), true
	case protocol.MethodCloseWorkspace:
		return bind(method, s.core.CloseWorkspace), true
	case protocol.MethodOpenDocument:
		return bind(method, s.core.OpenDocument), true
	case protocol.MethodCloseDocument:
		return bind(method, s.core.CloseDocument), true
	case protocol.MethodGetDefinition:
		return bind(method, s.core.GetDefinition), true
	case protocol.MethodGetReferences:
		return bind(method, s.core.GetReferences), true
	case protocol.MethodGetHover:
		return bind(method, s.core.GetHover), true
	case protocol.MethodDocumentSymbols:
		return bind(method, s.core.DocumentSymbols), true
	case protocol.MethodWorkspaceSymbol:
		return bind(method, s.core.WorkspaceSymbols), true
	case protocol.MethodGetDiagnostics:
		return bind(method, s.core.GetDiagnostics), true
	case protocol.MethodRenameSymbol:
		return bind(method, s.core.RenameSymbol), true
	case protocol.MethodApplyEdit:
		return bind(method, s.core.ApplyEdit), true
	case protocol.MethodSimulateEdit:
		return bind(method, s.core.SimulateEdit), true
	case protocol.MethodIndexStatus:
		return bind(method, s.core.IndexStatus), true
	case protocol.MethodEndSession:
		return bind(method, s.core.EndSession), true
	default:
		return nil, false
	}
}

// openWorkspace refuses roots this daemon does not serve.
//
// One daemon per repo (SPEC §3.2) is what the per-root socket, the per-root
// lock and the per-root sunset accounting all assume. Silently hosting a second
// root here would give it language servers under another project's socket, and
// nothing downstream would ever notice.
func (s *Server) openWorkspace(ctx context.Context, p protocol.OpenWorkspaceParams) (protocol.OpenWorkspaceResult, error) {
	root, err := workspace.CanonicalRoot(p.Root)
	if err != nil {
		return protocol.OpenWorkspaceResult{}, protocol.AsError(err)
	}
	if root != s.opts.Root {
		return protocol.OpenWorkspaceResult{}, protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
			"this daemon serves %s, not %s; connect to that workspace's own daemon", s.opts.Root, root)
	}
	p.Root = root
	return s.core.OpenWorkspace(ctx, p)
}
