package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/lsp/wire"
)

// This file is the whole fake-server apparatus for lsp's unit tests. There is
// deliberately no separate lsptest package and no fakelsp binary
// (docs/ARCHITECTURE.md §10.10): an in-package scripted server over pipes
// covers the protocol layer, and a fake procx.Runner covers supervision.

type handlerFunc func(params json.RawMessage) (any, *wire.RPCError)

// scriptedServer is an LSP server that answers exactly what a test tells it to.
type scriptedServer struct {
	t      *testing.T
	framer *wire.Framer

	mu       sync.Mutex
	handlers map[string]handlerFunc
	seen     []string
	nextID   int64
	pending  map[string]chan wire.Message

	closeOnce sync.Once
	done      chan struct{}
	stop      func()
}

func newScriptedServer(t *testing.T, rw io.ReadWriter) *scriptedServer {
	s := &scriptedServer{
		t:        t,
		framer:   wire.NewFramer(rw),
		handlers: map[string]handlerFunc{},
		pending:  map[string]chan wire.Message{},
		done:     make(chan struct{}),
	}
	s.handle("initialize", func(json.RawMessage) (any, *wire.RPCError) {
		return map[string]any{"capabilities": defaultCapabilities()}, nil
	})
	return s
}

// defaultCapabilities is a realistic ServerCapabilities block: everything the
// nine v0.1 tools need, and nothing more.
func defaultCapabilities() map[string]any {
	return map[string]any{
		"textDocumentSync":        2,
		"definitionProvider":      true,
		"referencesProvider":      true,
		"hoverProvider":           true,
		"documentSymbolProvider":  true,
		"workspaceSymbolProvider": true,
		"renameProvider":          true,
	}
}

func (s *scriptedServer) handle(method string, fn handlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

// reply is a convenience for handlers that answer with a fixed JSON literal.
func reply(literal string) handlerFunc {
	return func(json.RawMessage) (any, *wire.RPCError) {
		return json.RawMessage(literal), nil
	}
}

// hangs returns a handler that never answers until the test ends. It parks on
// a channel rather than `select {}` so the goroutine is reclaimed and the leak
// detector stays honest.
func hangs(t *testing.T) handlerFunc {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(json.RawMessage) (any, *wire.RPCError) {
		<-release
		return nil, &wire.RPCError{Code: -32800, Message: "test over"}
	}
}

func (s *scriptedServer) run() {
	defer func() {
		close(s.done)
		// A real language server's process exits after `exit`; the fake must
		// too, or Shutdown waits out its whole grace window.
		if s.stop != nil {
			s.stop()
		}
	}()
	for {
		m, err := s.framer.Read()
		if err != nil {
			return
		}

		switch {
		case m.IsResponse():
			s.mu.Lock()
			ch, ok := s.pending[string(m.ID)]
			delete(s.pending, string(m.ID))
			s.mu.Unlock()
			if ok {
				ch <- m
			}

		case m.IsRequest():
			s.record(m.Method)
			// Real language servers answer concurrently and keep reading while
			// a request is in flight. Handling inline would let one slow
			// handler stall the whole stream, which is a property of this fake
			// rather than of any server we must work with.
			go func(m wire.Message) {
				result, rpcErr := s.dispatch(m.Method, m.Params)
				out := wire.Message{ID: m.ID}
				if rpcErr != nil {
					out.Error = rpcErr
				} else if result != nil {
					encoded, err := json.Marshal(result)
					if err != nil {
						out.Error = &wire.RPCError{Code: -32603, Message: err.Error()}
					} else {
						out.Result = encoded
					}
				}
				_ = s.framer.Write(out)
			}(m)

		case m.IsNotification():
			s.record(m.Method)
			if m.Method == "exit" {
				return
			}
		}
	}
}

func (s *scriptedServer) dispatch(method string, params json.RawMessage) (any, *wire.RPCError) {
	s.mu.Lock()
	fn, ok := s.handlers[method]
	s.mu.Unlock()
	if !ok {
		return nil, &wire.RPCError{Code: -32601, Message: "Unhandled method " + method}
	}
	return fn(params)
}

func (s *scriptedServer) record(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, method)
}

func (s *scriptedServer) sawMethod(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.seen {
		if m == method {
			return true
		}
	}
	return false
}

func (s *scriptedServer) countMethod(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.seen {
		if m == method {
			n++
		}
	}
	return n
}

// push sends a server-initiated notification.
func (s *scriptedServer) push(method string, params any) {
	encoded, err := json.Marshal(params)
	if err != nil {
		s.t.Fatalf("marshalling %s params: %v", method, err)
	}
	_ = s.framer.Write(wire.Message{Method: method, Params: encoded})
}

// publishDiagnostics is the push every settle test is built on.
func (s *scriptedServer) publishDiagnostics(uri string, diagnostics []any) {
	s.push("textDocument/publishDiagnostics", map[string]any{
		"uri":         uri,
		"diagnostics": diagnostics,
	})
}

// request sends a SERVER-TO-CLIENT request and waits for the answer. A client
// that does not answer these deadlocks the session, and the failure looks like
// a timeout rather than a protocol bug — so it gets its own test.
func (s *scriptedServer) request(method string, params any, timeout time.Duration) (wire.Message, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return wire.Message{}, err
	}

	s.mu.Lock()
	s.nextID++
	id := json.RawMessage(`"srv-` + itoa(s.nextID) + `"`)
	ch := make(chan wire.Message, 1)
	s.pending[string(id)] = ch
	s.mu.Unlock()

	if err := s.framer.Write(wire.Message{ID: id, Method: method, Params: encoded}); err != nil {
		return wire.Message{}, err
	}
	select {
	case m := <-ch:
		return m, nil
	case <-time.After(timeout):
		return wire.Message{}, errors.New("client never answered " + method)
	case <-s.done:
		return wire.Message{}, errors.New("server stopped")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// ---- fake procx.Runner ----

// fakeRunner hands out fakeProcesses and runs a scripted server behind each
// one. Supervision — crash detection, backoff, restart, resync — is exercised
// against this, with no real process and no wall-clock sleeping.
type fakeRunner struct {
	t *testing.T

	mu      sync.Mutex
	starts  int
	procs   []*fakeProcess
	servers []*scriptedServer
	// setup runs for every start, so a test can script generation N
	// differently from generation N-1.
	setup func(n int, s *scriptedServer)
	// failStart, when set, makes the Nth (1-based) Start call fail.
	failStart map[int]error
}

func newFakeRunner(t *testing.T, setup func(n int, s *scriptedServer)) *fakeRunner {
	return &fakeRunner{t: t, setup: setup, failStart: map[int]error{}}
}

func (r *fakeRunner) Start(_ context.Context, spec procx.Spec) (procx.Process, error) {
	r.mu.Lock()
	r.starts++
	n := r.starts
	if err, ok := r.failStart[n]; ok {
		r.mu.Unlock()
		return nil, err
	}
	r.mu.Unlock()

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()

	p := &fakeProcess{
		stdin:        clientWriter,
		stdout:       clientReader,
		stderr:       stderrReader,
		stderrWriter: stderrWriter,
		serverWriter: serverWriter,
		serverReader: serverReader,
		exited:       make(chan struct{}),
		pid:          10000 + n,
	}

	srv := newScriptedServer(r.t, readWriter{Reader: serverReader, Writer: serverWriter})
	srv.stop = p.crash
	if r.setup != nil {
		r.setup(n, srv)
	}
	go srv.run()

	r.mu.Lock()
	r.procs = append(r.procs, p)
	r.servers = append(r.servers, srv)
	r.mu.Unlock()

	return p, nil
}

func (r *fakeRunner) Output(context.Context, procx.Spec) ([]byte, error) {
	return nil, errors.New("fakeRunner.Output is not used by lsp")
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

func (r *fakeRunner) server(n int) *scriptedServer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 1 || n > len(r.servers) {
		r.t.Fatalf("no scripted server for start %d (have %d)", n, len(r.servers))
	}
	return r.servers[n-1]
}

func (r *fakeRunner) process(n int) *fakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 1 || n > len(r.procs) {
		r.t.Fatalf("no process for start %d (have %d)", n, len(r.procs))
	}
	return r.procs[n-1]
}

type readWriter struct {
	io.Reader
	io.Writer
}

// fakeProcess is a procx.Process whose "exit" a test triggers directly.
type fakeProcess struct {
	stdin        io.WriteCloser
	stdout       io.Reader
	stderr       io.Reader
	stderrWriter io.Closer
	serverWriter io.Closer
	serverReader io.Closer

	once   sync.Once
	exited chan struct{}
	pid    int
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.Reader     { return p.stdout }
func (p *fakeProcess) Stderr() io.Reader     { return p.stderr }
func (p *fakeProcess) PID() int              { return p.pid }

func (p *fakeProcess) Wait() error {
	<-p.exited
	return errors.New("fake language server exited")
}

func (p *fakeProcess) Kill() error {
	p.crash()
	return nil
}

// crash simulates the language server dying: every pipe is torn down, exactly
// as a real process exit tears down its descriptors.
func (p *fakeProcess) crash() {
	p.once.Do(func() {
		_ = p.serverWriter.Close()
		_ = p.serverReader.Close()
		_ = p.stderrWriter.Close()
		_ = p.stdin.Close()
		close(p.exited)
	})
}

// ---- assorted helpers ----

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// advanceUntil steps the fake clock forward in small increments until cond
// holds, or fails after limit of fake time.
//
// Stepping rather than jumping matters. A goroutine that has observed a
// diagnostics push but has not yet been scheduled far enough to ARM its quiet
// timer would, after one big jump, arm that timer relative to the new "now" —
// past every deadline the test intends to cross — and wait forever. Real time
// never stops, so this hazard belongs to the fake and is handled here rather
// than by weakening the settle logic.
func advanceUntil(t *testing.T, f *clock.Fake, limit, step time.Duration, cond func() bool) {
	t.Helper()
	realDeadline := time.Now().Add(20 * time.Second)
	for total := time.Duration(0); total < limit; total += step {
		if cond() {
			return
		}
		f.Advance(step)
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		if time.Now().After(realDeadline) {
			break
		}
	}
	if !cond() {
		t.Fatalf("condition still false after %v of fake time", limit)
	}
}
