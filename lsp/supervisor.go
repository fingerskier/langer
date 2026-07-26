package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

// Defaults for the durations SPEC names. Every one is injectable so it can be
// asserted with a fake clock instead of slept through.
const (
	DefaultSettleQuiet       = 300 * time.Millisecond
	DefaultSettleBudget      = 2 * time.Second // SPEC §4.3 "≤2 s"
	DefaultBackoffInitial    = 250 * time.Millisecond
	DefaultBackoffMax        = 30 * time.Second
	DefaultHealthyResetAfter = 60 * time.Second
	shutdownGrace            = 3 * time.Second
)

// Supervisor owns the SPEC §3.3 state machine for the language servers of ONE
// workspace.
type Supervisor interface {
	// Acquire returns a ready Server for languageID, or a structured error:
	//
	//	UNSUPPORTED     no registry entry claims this language
	//	NOT_READY       starting or indexing (carries retry_after_ms)
	//	SERVER_CRASHED  a restart is in flight (carries retry_after_ms)
	Acquire(ctx context.Context, languageID string) (Server, error)

	// Status reports supervision state WITHOUT starting anything: index_status
	// must never have the side effect of spawning a language server.
	Status() []protocol.ServerStatus

	// Shutdown performs the SPEC §8 teardown for every server it owns:
	// shutdown request → exit notification → Wait with timeout → Kill.
	Shutdown(ctx context.Context) error
}

// Options configures a Supervisor.
type Options struct {
	Root              string // absolute workspace root
	Servers           []config.LanguageServer
	Resolver          procx.Resolver
	Runner            procx.Runner
	Clock             clock.Clock
	Logger            *slog.Logger
	SettleQuiet       time.Duration
	SettleBudget      time.Duration
	BackoffInitial    time.Duration
	BackoffMax        time.Duration
	HealthyResetAfter time.Duration

	// beforePublish is an in-package test seam for the narrow boundary after
	// replay and before replacement-session publication. External callers
	// cannot set it.
	beforePublish func(generation uint64)
}

func (o *Options) applyDefaults() {
	if o.Resolver == nil {
		o.Resolver = procx.NewResolver()
	}
	if o.Runner == nil {
		o.Runner = procx.NewRunner()
	}
	if o.Clock == nil {
		o.Clock = clock.New()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.SettleQuiet <= 0 {
		o.SettleQuiet = DefaultSettleQuiet
	}
	if o.SettleBudget <= 0 {
		o.SettleBudget = DefaultSettleBudget
	}
	if o.BackoffInitial <= 0 {
		o.BackoffInitial = DefaultBackoffInitial
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = DefaultBackoffMax
	}
	if o.HealthyResetAfter <= 0 {
		o.HealthyResetAfter = DefaultHealthyResetAfter
	}
}

// NewSupervisor builds a Supervisor. It starts nothing: SPEC §3.3 says language
// servers start on demand.
func NewSupervisor(opts Options) (Supervisor, error) {
	if opts.Root == "" {
		return nil, protocol.NewError(protocol.ErrInternal, "lsp: Options.Root is required")
	}
	opts.applyDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	return &supervisor{
		opts:     opts,
		log:      opts.Logger.With("component", "lsp", "root", opts.Root),
		ctx:      ctx,
		cancel:   cancel,
		managed:  map[string]*managed{},
		shutdown: make(chan struct{}),
	}, nil
}

type supervisor struct {
	opts   Options
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	managed  map[string]*managed
	closing  bool
	shutdown chan struct{}
	wg       sync.WaitGroup
}

// managed is one registry entry's supervision state plus the Server handle
// callers hold across restarts.
// startWorkers atomically reserves every worker before Shutdown can begin
// waiting. sync.WaitGroup forbids Add racing a Wait that observed a zero
// count; using the supervisor lifecycle mutex makes that rule structural.
func (s *supervisor) startWorkers(workers ...func()) bool {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return false
	}
	s.wg.Add(len(workers))
	s.mu.Unlock()

	for _, worker := range workers {
		worker := worker
		go func() {
			defer s.wg.Done()
			worker()
		}()
	}
	return true
}

type managed struct {
	sup   *supervisor
	entry config.LanguageServer
	srv   *server

	mu         sync.Mutex
	state      protocol.ServerState
	ready      chan struct{} // closed when a start attempt finishes
	startErr   error
	restarts   int
	backoff    time.Duration
	retryAt    time.Time
	readySince time.Time
	generation uint64
	proc       procx.Process
	conn       *conn
	candidate  *connectionCandidate
	stderrDone chan struct{}
}

// connectionCandidate owns notifications that must never bleed across process
// generations. Analysis progress is inherently candidate-local. Diagnostics
// remain disabled until replay has completed and publication is imminent, and
// deactivate waits out an in-flight publish before the next candidate resets
// the shared diagnostics cache.
type connectionCandidate struct {
	mu       sync.Mutex
	active   bool
	armed    map[string]bool
	pending  map[string]candidateDiagnostics
	analysis analysisState
}

type candidateDiagnostics struct {
	values    []protocol.Diagnostic
	version   int
	versioned bool
}

func (c *connectionCandidate) onDiagnostics(
	m *managed,
	uri string,
	version *int,
	raws []wire.RawDiagnostic,
) {
	rel, ok := wire.URIToPath(m.sup.opts.Root, uri)
	if !ok {
		return // a dependency file: never indexed, never reported (SPEC Â§3.4)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active && !c.armed[rel] {
		return
	}
	diags, err := wire.ToDiagnostics(m.sup.opts.Root, raws)
	if err != nil {
		return
	}
	_, currentVersion, _, open := m.srv.docs.state(rel)
	if !open || version != nil && *version != currentVersion {
		return
	}
	entry := candidateDiagnostics{values: diags}
	if version != nil {
		entry.version = *version
		entry.versioned = true
	}
	if c.active {
		entry.publish(m.srv.diags, rel)
		return
	}
	c.pending[rel] = entry
}

// arm preserves diagnostics that can be causally attributed to the replayed
// didOpen. Initialize-time disk diagnostics are discarded because no path is
// armed yet; a push arriving while the one-way didOpen write is in flight is
// buffered because it may be the server's only response to that replay.
func (c *connectionCandidate) arm(path string) {
	c.mu.Lock()
	if c.armed == nil {
		c.armed = make(map[string]bool)
		c.pending = make(map[string]candidateDiagnostics)
	}
	c.armed[path] = true
	delete(c.pending, path)
	c.mu.Unlock()
}

func (c *connectionCandidate) activate(diags *diagnostics) {
	c.mu.Lock()
	for path, entry := range c.pending {
		entry.publish(diags, path)
	}
	c.active = true
	c.armed = nil
	c.pending = nil
	c.mu.Unlock()
}

func (d candidateDiagnostics) publish(cache *diagnostics, path string) {
	if d.versioned {
		cache.publishVersioned(path, d.version, d.values)
		return
	}
	cache.publish(path, d.values)
}

func (c *connectionCandidate) deactivate() {
	c.mu.Lock()
	c.active = false
	c.armed = nil
	c.pending = nil
	c.mu.Unlock()
}

func (s *supervisor) Acquire(ctx context.Context, languageID string) (Server, error) {
	entry, ok := s.entryFor(languageID)
	if !ok {
		return nil, protocol.NewErrorf(protocol.ErrUnsupported,
			"no language server is configured for %q", languageID)
	}

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, protocol.NewError(protocol.ErrServerCrashed, "language server supervisor is shutting down")
	}
	key := strings.ToLower(entry.Name)
	m, ok := s.managed[key]
	if !ok {
		m = &managed{
			sup:     s,
			entry:   entry,
			state:   protocol.ServerStopped,
			backoff: s.opts.BackoffInitial,
			srv: &server{
				name:         entry.Name,
				root:         s.opts.Root,
				docs:         newDocuments(),
				diags:        newDiagnostics(s.opts.Clock),
				settleQuiet:  s.opts.SettleQuiet,
				settleBudget: s.opts.SettleBudget,
			},
		}
		s.managed[key] = m
	}
	s.mu.Unlock()

	if err := m.ensureReady(ctx); err != nil {
		return nil, err
	}
	return m.srv, nil
}

// entryFor maps a language id onto a registry entry: by name first, then by the
// file extensions the entry claims.
func (s *supervisor) entryFor(languageID string) (config.LanguageServer, bool) {
	for _, entry := range s.opts.Servers {
		if strings.EqualFold(entry.Name, languageID) {
			return entry, true
		}
	}
	extensions, known := languageExtensions[strings.ToLower(languageID)]
	if !known {
		return config.LanguageServer{}, false
	}
	for _, entry := range s.opts.Servers {
		for _, claimed := range entry.FileExtensions {
			for _, want := range extensions {
				if strings.EqualFold(claimed, want) {
					return entry, true
				}
			}
		}
	}
	return config.LanguageServer{}, false
}

// languageExtensions lets Acquire("typescript") find an entry that names itself
// something else but claims ".ts".
var languageExtensions = map[string][]string{
	"typescript":      {".ts"},
	"typescriptreact": {".tsx"},
	"javascript":      {".js"},
	"javascriptreact": {".jsx"},
	"python":          {".py", ".pyi"},
	"go":              {".go"},
	"rust":            {".rs"},
}

func (s *supervisor) Status() []protocol.ServerStatus {
	s.mu.Lock()
	all := make([]*managed, 0, len(s.managed))
	for _, m := range s.managed {
		all = append(all, m)
	}
	s.mu.Unlock()

	out := make([]protocol.ServerStatus, 0, len(all))
	for _, m := range all {
		out = append(out, m.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *managed) status() protocol.ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := protocol.ServerStatus{
		Name:     m.entry.Name,
		State:    m.state,
		Restarts: m.restarts,
	}
	if m.state == protocol.ServerBackoff {
		if remaining := m.retryAt.Sub(m.sup.opts.Clock.Now()); remaining > 0 {
			status.RetryAfterMS = int(remaining / time.Millisecond)
		}
	}
	return status
}

// ensureReady blocks until the server is usable, or returns a structured error.
// It never starts more than one process per language concurrently.
func (m *managed) ensureReady(ctx context.Context) error {
	for {
		m.mu.Lock()
		switch m.state {
		case protocol.ServerReady:
			m.mu.Unlock()
			return nil

		case protocol.ServerBackoff:
			retry := int(m.retryAt.Sub(m.sup.opts.Clock.Now()) / time.Millisecond)
			if retry < 0 {
				retry = 0
			}
			restarts := m.restarts
			m.mu.Unlock()
			return protocol.NewErrorf(protocol.ErrServerCrashed,
				"%s crashed; restart %d is in flight", m.entry.Name, restarts).WithRetryAfterMS(retry)

		case protocol.ServerStarting:
			ready := m.ready
			m.mu.Unlock()
			select {
			case <-ready:
				continue // re-read the state; it may be ready, crashed or backing off
			case <-ctx.Done():
				return protocol.NewErrorf(protocol.ErrNotReady,
					"%s is still starting", m.entry.Name).WithRetryAfterMS(500)
			case <-m.sup.shutdown:
				return protocol.NewError(protocol.ErrServerCrashed, "language server supervisor is shutting down")
			}

		default: // stopped or crashed with no restart scheduled
			m.state = protocol.ServerStarting
			m.ready = make(chan struct{})
			ready := m.ready
			m.mu.Unlock()

			m.startAttempt()

			select {
			case <-ready:
			case <-ctx.Done():
				return protocol.NewErrorf(protocol.ErrNotReady,
					"%s is still starting", m.entry.Name).WithRetryAfterMS(500)
			}

			m.mu.Lock()
			err := m.startErr
			state := m.state
			m.mu.Unlock()
			if err != nil {
				return err
			}
			if state != protocol.ServerReady {
				continue
			}
			return nil
		}
	}
}

// startAttempt runs one start synchronously and records the outcome. The caller
// has already put the state machine in ServerStarting and owns m.ready.
func (m *managed) startAttempt() {
	m.mu.Lock()
	ready := m.ready
	m.mu.Unlock()

	err := m.start()

	m.mu.Lock()
	if m.ready == ready {
		m.startErr = err
		// A connection can close asynchronously while start or resync is in
		// flight. Preserve the backoff/stopped state installed by that event
		// instead of publishing a dead generation as ready.
		if m.state == protocol.ServerStarting {
			if err == nil {
				m.state = protocol.ServerReady
				m.readySince = m.sup.opts.Clock.Now()
			} else {
				m.state = protocol.ServerCrashed
			}
		}
	}
	m.mu.Unlock()
	close(ready)

	if err != nil {
		m.sup.log.Warn("language server failed to start", "server", m.entry.Name, "error", err)
	}
}

// start resolves, spawns and initializes one language server.
func (m *managed) start() error {
	sup := m.sup

	// SPEC §9's single enforcement point. A workspace-local binary is refused
	// here, five milestones before M6's end-to-end tripwire.
	binary, err := sup.opts.Resolver.Resolve(m.entry.Command, sup.opts.Root, m.entry.AllowWorkspaceLocal)
	if err != nil {
		return err
	}

	proc, err := sup.opts.Runner.Start(sup.ctx, procx.Spec{
		Path: binary,
		Args: m.entry.Args,
		Dir:  sup.opts.Root,
		Env:  os.Environ(),
	})
	if err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "starting %s: %v", m.entry.Name, err)
	}

	// A replacement is a fresh analysis generation. Reset before newConn starts
	// its reader: initialize/initialized may immediately trigger progress or
	// diagnostics, and resetting at publication would erase those events.
	m.srv.resetGenerationState()
	candidate := &connectionCandidate{}
	registered := make(chan *conn, 1)
	c := newConn(procStdio{proc}, connHandlers{
		diagnostics: func(uri string, version *int, raws []wire.RawDiagnostic) {
			candidate.onDiagnostics(m, uri, version, raws)
		},
		closed: func(cause error) {
			m.onConnectionClosed(<-registered, candidate, cause)
		},
		analyzing: candidate.analysis.setAnalyzing,
	}).withLogger(sup.log.With("server", m.entry.Name))

	// Reserve both process-lifetime workers together before exposing the process
	// through m.proc. Besides making Add-vs-Wait safe, this ensures shutdown can
	// never miss an untracked process started concurrently with it. An unread
	// stderr pipe fills at 64 KiB and blocks pyright, so draining is mandatory.
	stderrDone := make(chan struct{})
	if !sup.startWorkers(
		func() {
			defer close(stderrDone)
			m.drainStderr(proc.Stderr())
		},
		func() {
			_ = proc.Wait()
			c.close(errors.New("language server process exited"))
		},
	) {
		c.close(errors.New("language server supervisor is shutting down"))
		_ = proc.Kill()
		_ = proc.Wait()
		c.wait()
		return protocol.NewError(protocol.ErrServerCrashed, "language server supervisor is shutting down")
	}

	m.mu.Lock()
	if m.state != protocol.ServerStarting {
		m.mu.Unlock()
		registered <- c
		c.close(errors.New("language server start was cancelled"))
		_ = proc.Kill()
		return protocol.NewError(protocol.ErrServerCrashed, "language server start was cancelled")
	}
	m.proc = proc
	m.conn = c
	m.candidate = candidate
	m.stderrDone = stderrDone
	m.mu.Unlock()
	registered <- c

	caps, err := m.initialize(c)
	if err != nil {
		c.close(err)
		_ = proc.Kill()
		return err
	}

	m.mu.Lock()
	if m.state != protocol.ServerStarting || m.conn != c || m.candidate != candidate {
		m.mu.Unlock()
		c.close(errors.New("language server connection closed during initialization"))
		_ = proc.Kill()
		return protocol.NewError(protocol.ErrServerCrashed, "language server connection closed during initialization")
	}
	m.generation++
	generation := m.generation
	m.mu.Unlock()

	if err := m.srv.mutations.writeLock(sup.ctx); err != nil {
		c.close(err)
		_ = proc.Kill()
		return err
	}

	// After a restart the server has no memory of our open documents; ours is
	// the only copy left. Do not expose the replacement session until this
	// barrier completes: callers holding an old Server handle must not query a
	// half-resynchronized process.
	if generation > 1 {
		if err := m.resync(c, candidate); err != nil {
			m.srv.mutations.writeUnlock()
			c.close(err)
			_ = proc.Kill()
			return err
		}
	}
	if sup.opts.beforePublish != nil {
		sup.opts.beforePublish(generation)
	}
	sess := &session{
		conn:           c,
		caps:           caps,
		generation:     generation,
		analysis:       &candidate.analysis,
		symbolProgress: reportsSymbolProgress(caps),
	}
	m.mu.Lock()
	select {
	case <-c.done:
		err = crashedError(c.closeCause())
	default:
		if m.state != protocol.ServerStarting || m.conn != c || m.candidate != candidate {
			err = protocol.NewError(protocol.ErrServerCrashed, "language server connection closed before publication")
		} else {
			candidate.activate(m.srv.diags)
			m.srv.setSession(sess)
		}
	}
	m.mu.Unlock()
	if err != nil {
		m.srv.mutations.writeUnlock()
		c.close(err)
		_ = proc.Kill()
		return err
	}
	m.srv.mutations.writeUnlock()

	sup.log.Info("language server ready",
		"server", m.entry.Name,
		"generation", generation,
		"pid", proc.PID(),
		"capabilities", strings.Join(sortedCapabilityNames(caps), ","))
	return nil
}

// initialize performs the LSP handshake and captures ServerCapabilities.
func (m *managed) initialize(c *conn) (map[string]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(m.sup.ctx, 30*time.Second)
	defer cancel()

	params := map[string]any{
		"processId": os.Getpid(),
		"clientInfo": map[string]any{
			"name":    "langer",
			"version": "0.1",
		},
		"rootUri":  wire.PathToURI(m.sup.opts.Root, "."),
		"rootPath": m.sup.opts.Root,
		"workspaceFolders": []any{map[string]any{
			"uri":  wire.PathToURI(m.sup.opts.Root, "."),
			"name": m.entry.Name,
		}},
		"capabilities": clientCapabilities(),
	}
	if len(m.entry.InitializationOptions) > 0 {
		params["initializationOptions"] = m.entry.InitializationOptions
	}

	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "initializing %s: %v", m.entry.Name, err)
	}

	var result struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding %s initialize result: %v", m.entry.Name, err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	if result.Capabilities == nil {
		result.Capabilities = map[string]json.RawMessage{}
	}
	return result.Capabilities, nil
}

// clientCapabilities is what testdata/README.md §4 recorded every fixture value
// against. Changing it can change what the servers answer.
func clientCapabilities() map[string]any {
	return map[string]any{
		"general": map[string]any{
			// SPEC §4.3: UTF-16 is the LSP default and v0.1 negotiates no
			// alternative. Saying so explicitly stops a server picking UTF-8.
			"positionEncodings": []string{"utf-16"},
		},
		"workspace": map[string]any{
			"workspaceFolders": true,
			"configuration":    true,
			"symbol": map[string]any{
				"symbolKind": map[string]any{"valueSet": symbolKindValueSet()},
			},
		},
		"textDocument": map[string]any{
			"synchronization": map[string]any{
				"dynamicRegistration": false,
				"didSave":             false,
				"willSave":            false,
			},
			"hover": map[string]any{
				"contentFormat": []string{"markdown", "plaintext"},
			},
			"definition": map[string]any{"linkSupport": true},
			"references": map[string]any{},
			"documentSymbol": map[string]any{
				"hierarchicalDocumentSymbolSupport": true,
				"symbolKind":                        map[string]any{"valueSet": symbolKindValueSet()},
			},
			"rename": map[string]any{"prepareSupport": false},
			"publishDiagnostics": map[string]any{
				"relatedInformation": true,
				"versionSupport":     true,
			},
		},
		"window": map[string]any{"workDoneProgress": true},
	}
}

func symbolKindValueSet() []int {
	set := make([]int, 0, 26)
	for i := 1; i <= 26; i++ {
		set = append(set, i)
	}
	return set
}

// onConnectionClosed is crash detection. It cancels nothing above this server,
// which is exactly why SPEC §8 holds.
func (m *managed) onConnectionClosed(
	closed *conn,
	candidate *connectionCandidate,
	cause error,
) {
	m.mu.Lock()
	candidate.deactivate()
	if m.conn != closed || m.candidate != candidate {
		m.mu.Unlock()
		return
	}
	proc := m.proc
	m.proc = nil
	m.conn = nil
	m.candidate = nil
	m.srv.clearSession(cause)

	if m.state == protocol.ServerStopped {
		// Deliberate shutdown, not a crash.
		m.mu.Unlock()
		if proc != nil {
			_ = proc.Kill()
		}
		return
	}
	if m.sup.isClosing() {
		m.state = protocol.ServerStopped
		m.mu.Unlock()
		if proc != nil {
			_ = proc.Kill()
		}
		return
	}

	// A server that ran healthily for a while and then died gets a fresh
	// budget; a server crash-looping on startup keeps backing off.
	if !m.readySince.IsZero() && m.sup.opts.Clock.Now().Sub(m.readySince) >= m.sup.opts.HealthyResetAfter {
		m.backoff = m.sup.opts.BackoffInitial
	}
	delay := m.backoff
	m.backoff *= 2
	if m.backoff > m.sup.opts.BackoffMax {
		m.backoff = m.sup.opts.BackoffMax
	}

	m.restarts++
	m.state = protocol.ServerBackoff
	m.retryAt = m.sup.opts.Clock.Now().Add(delay)
	m.readySince = time.Time{}
	restarts := m.restarts
	attemptReady := m.ready
	m.mu.Unlock()
	if proc != nil {
		_ = proc.Kill()
	}

	m.sup.log.Warn("language server crashed; scheduling restart",
		"server", m.entry.Name, "restart", restarts, "backoff", delay, "cause", cause)

	m.sup.startWorkers(func() {
		if err := m.sup.opts.Clock.Sleep(m.sup.ctx, delay); err != nil {
			return // the supervisor is shutting down
		}
		if m.sup.isClosing() {
			return
		}
		select {
		case <-attemptReady:
		case <-m.sup.shutdown:
			return
		}

		m.mu.Lock()
		if m.state != protocol.ServerBackoff {
			m.mu.Unlock()
			return // somebody else already restarted it
		}
		m.state = protocol.ServerStarting
		m.ready = make(chan struct{})
		m.mu.Unlock()

		m.startAttempt()
	})
}

// resync re-opens every document the previous generation knew about.
func (m *managed) resync(c *conn, candidate *connectionCandidate) error {
	for _, snapshot := range m.srv.docs.openSnapshot() {
		doc, err := m.srv.docs.acquire(m.sup.ctx, snapshot.Path)
		if err != nil {
			return protocol.NewErrorf(protocol.ErrNotReady,
				"locking %s for %s resync: %v", snapshot.Path, m.entry.Name, err)
		}

		text, _, languageID, open := m.srv.docs.state(snapshot.Path)
		if !open {
			m.srv.docs.release(doc)
			continue
		}

		version := m.srv.docs.markChanged(snapshot.Path, text)
		m.srv.diags.invalidate(snapshot.Path)
		candidate.arm(snapshot.Path)
		err = c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        wire.PathToURI(m.sup.opts.Root, snapshot.Path),
				"languageId": languageID,
				"version":    version,
				"text":       text,
			},
		})
		m.srv.docs.release(doc)
		if err != nil {
			return protocol.NewErrorf(protocol.ErrServerCrashed,
				"resyncing %s in %s: %v", snapshot.Path, m.entry.Name, err)
		}
	}
	return nil
}

func (m *managed) drainStderr(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			m.sup.log.Debug("language server stderr",
				"server", m.entry.Name, "output", strings.TrimRight(string(buf[:n]), "\n"))
		}
		if err != nil {
			return
		}
	}
}

func (s *supervisor) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// Shutdown tears every server down in the SPEC §8 order and does not return
// until every goroutine it started has exited.
func (s *supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	all := make([]*managed, 0, len(s.managed))
	for _, m := range s.managed {
		all = append(all, m)
	}
	s.mu.Unlock()

	close(s.shutdown)

	var firstErr error
	for _, m := range all {
		if err := m.stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Cancelling last means a stop() still in flight can finish gracefully
	// before the process group is killed out from under it.
	s.cancel()
	s.wg.Wait()
	return firstErr
}

// stop performs shutdown → exit → Wait → Kill for one server.
func (m *managed) stop(ctx context.Context) error {
	m.mu.Lock()
	proc, c, candidate := m.proc, m.conn, m.candidate
	if candidate != nil {
		candidate.deactivate()
	}
	m.proc, m.conn = nil, nil
	m.candidate = nil
	m.state = protocol.ServerStopped
	m.mu.Unlock()

	m.srv.clearSession(errors.New("language server stopped"))
	if proc == nil {
		return nil
	}

	if c != nil {
		graceCtx, cancel := context.WithTimeout(ctx, shutdownGrace)
		if _, err := c.call(graceCtx, "shutdown", nil); err != nil {
			m.sup.log.Debug("shutdown request failed", "server", m.entry.Name, "error", err)
		}
		cancel()
		_ = c.notify("exit", nil)
	}

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = proc.Wait()
	}()

	select {
	case <-exited:
	case <-m.sup.opts.Clock.After(shutdownGrace):
		// A node wrapper script that ignores `exit` still has children; the
		// process-group kill is what stops them leaking.
		_ = proc.Kill()
		<-exited
	case <-ctx.Done():
		_ = proc.Kill()
		<-exited
	}

	if c != nil {
		c.close(errors.New("language server stopped"))
		c.wait()
	}
	m.mu.Lock()
	stderrDone := m.stderrDone
	m.mu.Unlock()
	if stderrDone != nil {
		<-stderrDone
	}
	return nil
}

// procStdio adapts a procx.Process to the io.ReadWriter a Framer wants.
type procStdio struct{ p procx.Process }

func (p procStdio) Read(b []byte) (int, error)  { return p.p.Stdout().Read(b) }
func (p procStdio) Write(b []byte) (int, error) { return p.p.Stdin().Write(b) }
