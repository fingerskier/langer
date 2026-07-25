package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

// This file is the daemon package's fake language server.
//
// It is a real LSP peer — Content-Length framing, JSON-RPC ids, server-pushed
// diagnostics — behind a fake procx.Runner. That means the daemon tests drive
// the PRODUCTION stack (daemon → workspace → lsp → lsp/wire) and can still kill
// the "process" on demand, which is how SPEC §8's "a language server crash must
// not terminate the daemon" gets tested without a language server installed.
//
// It follows the pattern lsp/fake_test.go established (docs/ARCHITECTURE.md
// §10.10: no separate fake package, no fakelsp binary).

type fakeLSP struct {
	t *testing.T

	mu       sync.Mutex
	starts   int
	procs    []*fakeProcess
	sessions []*fakeSession
	setup    func(n int, s *fakeSession)
}

func newFakeLSP(t *testing.T, setup func(n int, s *fakeSession)) *fakeLSP {
	return &fakeLSP{t: t, setup: setup}
}

// Start implements procx.Runner.
func (f *fakeLSP) Start(_ context.Context, _ procx.Spec) (procx.Process, error) {
	f.mu.Lock()
	f.starts++
	n := f.starts
	f.mu.Unlock()

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()

	proc := &fakeProcess{
		stdin:        clientWriter,
		stdout:       clientReader,
		stderr:       stderrReader,
		stderrWriter: stderrWriter,
		serverWriter: serverWriter,
		serverReader: serverReader,
		exited:       make(chan struct{}),
		pid:          20000 + n,
	}

	sess := &fakeSession{
		t:      f.t,
		framer: wire.NewFramer(readWriter{Reader: serverReader, Writer: serverWriter}),
		stop:   proc.crash,
		diags:  map[string][]any{},
	}
	if f.setup != nil {
		f.setup(n, sess)
	}
	go sess.run()

	f.mu.Lock()
	f.procs = append(f.procs, proc)
	f.sessions = append(f.sessions, sess)
	f.mu.Unlock()
	return proc, nil
}

// Output implements procx.Runner. The daemon never runs short-lived commands.
func (f *fakeLSP) Output(context.Context, procx.Spec) ([]byte, error) {
	return nil, errors.New("fakeLSP.Output is not used")
}

// Resolve implements procx.Resolver: the fake command is already "absolute".
func (f *fakeLSP) Resolve(command, _ string, _ bool) (string, error) {
	if filepath.IsAbs(command) {
		return command, nil
	}
	return "/fake/" + command, nil
}

func (f *fakeLSP) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeLSP) process(n int) *fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 1 || n > len(f.procs) {
		f.t.Fatalf("no fake language server process %d (have %d)", n, len(f.procs))
	}
	return f.procs[n-1]
}

// waitForStart blocks until at least n language servers have been started.
func (f *fakeLSP) waitForStart(n int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.startCount() >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

type readWriter struct {
	io.Reader
	io.Writer
}

// fakeSession is one generation of the fake language server.
type fakeSession struct {
	t      *testing.T
	framer *wire.Framer
	stop   func()

	mu        sync.Mutex
	diags     map[string][]any
	locations string
	hover     string
	symbols   string
	rename    string
	noRename  bool
	seen      []string

	// hold, when set, makes documentSymbol block until it is closed, and
	// announces its arrival on entered. That is how a test saturates the
	// daemon's in-flight limit deterministically instead of racing it.
	hold    chan struct{}
	entered chan struct{}
}

func (s *fakeSession) run() {
	for {
		m, err := s.framer.Read()
		if err != nil {
			return
		}
		switch {
		case m.IsRequest():
			s.record(m.Method)
			go s.answer(m)
		case m.IsNotification():
			s.record(m.Method)
			switch m.Method {
			case "exit":
				if s.stop != nil {
					s.stop()
				}
				return
			case "textDocument/didOpen", "textDocument/didChange":
				s.publishFor(m.Params)
			}
		}
	}
}

func (s *fakeSession) answer(m wire.Message) {
	out := wire.Message{ID: m.ID}
	switch m.Method {
	case "initialize":
		out.Result = json.RawMessage(`{"capabilities":{` +
			`"textDocumentSync":2,` +
			`"definitionProvider":true,` +
			`"referencesProvider":true,` +
			`"hoverProvider":true,` +
			`"documentSymbolProvider":true,` +
			`"workspaceSymbolProvider":true,` +
			s.renameCapability() +
			`}}`)
	case "textDocument/definition", "textDocument/references":
		out.Result = json.RawMessage(s.get(func(x *fakeSession) string { return x.locations }))
	case "textDocument/hover":
		out.Result = json.RawMessage(s.get(func(x *fakeSession) string { return x.hover }))
	case "textDocument/documentSymbol", "workspace/symbol":
		if hold, entered := s.holdChannels(); hold != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-hold
		}
		out.Result = json.RawMessage(s.get(func(x *fakeSession) string { return x.symbols }))
	case "textDocument/rename":
		out.Result = json.RawMessage(s.get(func(x *fakeSession) string { return x.rename }))
	case "shutdown":
		out.Result = json.RawMessage(`null`)
	default:
		out.Error = &wire.RPCError{Code: -32601, Message: "unhandled " + m.Method}
	}
	_ = s.framer.Write(out)
}

func (s *fakeSession) renameCapability() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noRename {
		return `"renameProvider":false`
	}
	return `"renameProvider":true`
}

func (s *fakeSession) get(pick func(*fakeSession) string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := pick(s); v != "" {
		return v
	}
	return "null"
}

// publishFor pushes diagnostics for the URI a didOpen/didChange named. Both
// real servers push an empty array first and the real payload after, and the
// settle rule exists because of it — so the fake does the same.
func (s *fakeSession) publishFor(params json.RawMessage) {
	var decoded struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(params, &decoded); err != nil {
		return
	}
	uri := decoded.TextDocument.URI

	s.mu.Lock()
	diags := s.diags[uri]
	s.mu.Unlock()

	s.push(uri, []any{})
	if diags != nil {
		s.push(uri, diags)
	}
}

func (s *fakeSession) push(uri string, diags []any) {
	params, err := json.Marshal(map[string]any{"uri": uri, "diagnostics": diags})
	if err != nil {
		return
	}
	_ = s.framer.Write(wire.Message{Method: "textDocument/publishDiagnostics", Params: params})
}

func (s *fakeSession) holdChannels() (chan struct{}, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hold, s.entered
}

func (s *fakeSession) record(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, method)
}

// fakeProcess is a procx.Process whose exit a test triggers directly.
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

// crash tears down every pipe, exactly as a real process exit tears down its
// descriptors.
func (p *fakeProcess) crash() {
	p.once.Do(func() {
		_ = p.serverWriter.Close()
		_ = p.serverReader.Close()
		_ = p.stderrWriter.Close()
		_ = p.stdin.Close()
		close(p.exited)
	})
}

// exitedProcess is a child that has already gone. It stands in for the detached
// daemon spawn: the client stops caring about the process the instant it is
// started, and everything it reads from it is at EOF.
type exitedProcess struct{}

func (exitedProcess) Stdin() io.WriteCloser { return nopWriteCloser{} }
func (exitedProcess) Stdout() io.Reader     { return strings.NewReader("") }
func (exitedProcess) Stderr() io.Reader     { return strings.NewReader("") }
func (exitedProcess) Wait() error           { return nil }
func (exitedProcess) Kill() error           { return nil }
func (exitedProcess) PID() int              { return 0 }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// ---- shared test fixtures --------------------------------------------------

// shortTempDir returns a directory short enough to hold a Unix socket path.
// t.TempDir embeds the test name under macOS's deep per-user TMPDIR and blows
// past sockaddr_un's ~104 byte limit, which the daemon refuses outright.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "langer")
	if err != nil {
		t.Skipf("cannot create a short temporary directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fixtureRoot builds a tiny TypeScript-ish workspace and returns its canonical
// path.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "src/user.ts", "export interface User {\n  id: string;\n}\n")
	writeFile(t, root, "src/service.ts", "import { User } from \"./user\";\n")
	writeFile(t, root, "README.md", "# fixture\n")

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testConfig points the runtime directory at a short temp dir and registers one
// fake language server for .ts files.
func testConfig(t *testing.T, servers ...config.LanguageServer) *config.Config {
	t.Helper()
	if servers == nil {
		servers = []config.LanguageServer{{
			Name:           "typescript",
			Command:        "fake-language-server",
			Args:           []string{"--stdio"},
			FileExtensions: []string{".ts", ".tsx"},
		}}
	}
	return &config.Config{
		SocketPath:      filepath.Join(shortTempDir(t), "daemon.sock"),
		LogLevel:        "error",
		LanguageServers: servers,
	}
}

func wantCode(t *testing.T, err error, code protocol.ErrorCode) *protocol.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", code)
	}
	got := protocol.AsError(err)
	if got.Code != code {
		t.Fatalf("error code = %s (%s), want %s", got.Code, got.Message, code)
	}
	return got
}

// testLogger keeps daemon logs out of the test output unless something fails to
// build; the daemon's own behaviour is asserted, not its log lines.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func base64URL(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// readFileBytes is a thin wrapper so the integration tests can read a fixture
// without importing os for one call.
func readFileBytes(path string) ([]byte, error) { return os.ReadFile(path) }
