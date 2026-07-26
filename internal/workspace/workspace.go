package workspace

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/index"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/watch"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/protocol"
)

// maxRememberedPlans bounds the rename dry-run plans a workspace keeps. A plan
// is small, but an agent looping on rename_symbol must not be able to grow the
// daemon without limit.
const maxRememberedPlans = 64

// Workspace is one repository root and everything the daemon knows about it.
//
// Every exported method returns either a value or a *protocol.Error: nothing
// unstructured escapes (SPEC §3.6).
type Workspace struct {
	id    protocol.WorkspaceID
	root  string
	cfg   *config.Config
	sup   lsp.Supervisor
	log   *slog.Logger
	clock clock.Clock

	store          index.Store
	scanner        watch.Scanner
	watcher        watch.Watcher
	repoNamespace  string
	onFileActivity func()

	indexCtx    context.Context
	cancelIndex context.CancelFunc
	indexWG     sync.WaitGroup
	indexWake   chan struct{}
	healWake    chan struct{}
	stagingDone chan struct{}

	jobMu    sync.Mutex
	jobQueue []indexJob
	jobHead  int

	cacheMu      sync.RWMutex
	indexMu      sync.Mutex
	generations  map[string]uint64
	pending      map[string]uint64
	failed       map[string]*protocol.Error
	known        map[string]struct{}
	scanComplete bool
	indexState   protocol.IndexState
	indexError   *protocol.Error
	indexFatal   bool

	shutdownOnce sync.Once
	shutdownErr  error

	// docMu owns the map of context-aware per-path leaves. A leaf is held across
	// language server calls on purpose: two operations for the same file cannot
	// interleave a close with an open. It remains the outermost lock, and
	// callers waiting on it can still honor request cancellation.
	docMu   sync.Mutex
	docLock map[string]*documentLock

	mu    sync.Mutex
	docs  map[string]*docState
	plans map[string]editPlan
	order []string // plan tokens, oldest first

	// overlays holds per-session speculative text (SPEC §4.2 / M5).
	overlays      *overlays
	cancelOverlay context.CancelFunc
	// overlayWG joins the clock-driven TTL sweeper on shutdown.
	overlayWG sync.WaitGroup
}

// docState is what the language server currently believes about a file.
type docState struct {
	text string
	// sessions holds the sessions that called open_document explicitly. A file
	// opened only to answer a query has an empty set and stays open: the server
	// keeps no meaningful state beyond the text, and closing it would just make
	// the next query pay for a reopen.
	sessions map[protocol.SessionID]struct{}
	explicit bool
}

func newWorkspace(ctx context.Context, id protocol.WorkspaceID, root string, opts RegistryOptions) (*Workspace, error) {
	log := opts.Logger.With("component", "workspace", "root", root)

	var (
		scanner       watch.Scanner
		repoNamespace string
	)
	if opts.Store != nil {
		scanner = opts.NewScanner(opts.Resolver, opts.Runner)
		if scanner == nil {
			return nil, protocol.NewError(protocol.ErrInternal, "workspace scanner factory returned nil")
		}
		var err error
		repoNamespace, err = scanner.RepositoryNamespace(ctx, root)
		if err != nil {
			return nil, protocol.AsError(err)
		}
		ensuredID, err := opts.Store.EnsureWorkspace(ctx, root, repoNamespace)
		if err != nil {
			return nil, protocol.AsError(err)
		}
		if ensuredID != id {
			return nil, protocol.NewErrorf(protocol.ErrInternal,
				"index workspace id %q does not match root-derived id %q", ensuredID, id)
		}
	}

	sup, err := opts.NewSupervisor(lsp.Options{
		Root:     root,
		Servers:  opts.Config.LanguageServers,
		Resolver: opts.Resolver,
		Runner:   opts.Runner,
		Clock:    opts.Clock,
		Logger:   log,
	})
	if err != nil {
		return nil, protocol.AsError(err)
	}

	w := &Workspace{
		id:             id,
		root:           root,
		cfg:            opts.Config,
		sup:            sup,
		log:            log,
		clock:          opts.Clock,
		store:          opts.Store,
		scanner:        scanner,
		repoNamespace:  repoNamespace,
		onFileActivity: opts.OnFileActivity,
		docLock:        map[string]*documentLock{},
		docs:           map[string]*docState{},
		plans:          map[string]editPlan{},
		generations:    map[string]uint64{},
		pending:        map[string]uint64{},
		failed:         map[string]*protocol.Error{},
		known:          map[string]struct{}{},
		indexState: protocol.IndexIdle,
		overlays:   newOverlays(opts.Clock, opts.OverlayTTL),
	}
	overlayCtx, cancelOverlay := context.WithCancel(context.Background())
	w.cancelOverlay = cancelOverlay
	w.startOverlaySweep(overlayCtx)
	if opts.Store != nil {
		if err := w.startIndex(ctx, opts); err != nil {
			cancelOverlay()
			w.overlayWG.Wait()
			_ = sup.Shutdown(context.Background())
			return nil, err
		}
	}
	return w, nil
}

// ID is the workspace's identifier on the wire.
func (w *Workspace) ID() protocol.WorkspaceID { return w.id }

// Root is the absolute, symlink-resolved workspace root.
func (w *Workspace) Root() string { return w.root }

// ---- documents -------------------------------------------------------------

// OpenDocument notifies the daemon that a session has a file active.
func (w *Workspace) OpenDocument(ctx context.Context, sid protocol.SessionID, path, languageID string) error {
	rel, err := w.relPath(path)
	if err != nil {
		return err
	}
	if err := w.validateWorkspaceFile(rel); err != nil {
		return err
	}
	srv, err := w.serverFor(ctx, rel)
	if err != nil {
		return err
	}

	unlock, err := w.lockDoc(ctx, rel)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := w.ensureDocumentLocked(ctx, srv, rel, languageID, true); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.docs[rel]
	if st == nil {
		// ensureDocumentLocked always records state; this cannot happen, but a
		// nil map write would be a panic rather than an error.
		return protocol.NewErrorf(protocol.ErrInternal, "document state for %s vanished", rel)
	}
	if st.sessions == nil {
		st.sessions = map[protocol.SessionID]struct{}{}
	}
	st.sessions[sid] = struct{}{}
	st.explicit = true
	return nil
}

// CloseDocument drops one session's claim on a file. The language server is
// told only when the last claim goes away.
func (w *Workspace) CloseDocument(ctx context.Context, sid protocol.SessionID, path string) error {
	rel, err := w.relPath(path)
	if err != nil {
		return err
	}
	return w.releaseDocument(ctx, rel, sid)
}

// EndSession releases everything sid owns in this workspace. Idempotent.
func (w *Workspace) EndSession(ctx context.Context, sid protocol.SessionID) {
	// SPEC §4.2: overlays are dropped when the session disconnects.
	if w.overlays != nil {
		w.overlays.dropSession(sid)
	}

	w.mu.Lock()
	paths := make([]string, 0, len(w.docs))
	for rel, st := range w.docs {
		if _, ok := st.sessions[sid]; ok {
			paths = append(paths, rel)
		}
	}
	w.mu.Unlock()

	for _, rel := range paths {
		if err := w.releaseDocument(ctx, rel, sid); err != nil {
			w.log.Debug("releasing session document failed", "path", rel, "error", err)
		}
	}
}

// releaseDocument removes one session's claim and closes the document on the
// language server when nothing holds it any more.
func (w *Workspace) releaseDocument(ctx context.Context, rel string, sid protocol.SessionID) error {
	unlock, err := w.lockDoc(ctx, rel)
	if err != nil {
		return err
	}
	defer unlock()

	w.mu.Lock()
	st, ok := w.docs[rel]
	if !ok {
		w.mu.Unlock()
		return nil
	}
	delete(st.sessions, sid)
	// A document opened implicitly to answer a query is not owned by anyone and
	// is left open; only an explicitly opened one is closed when its last
	// claimant leaves.
	stillHeld := len(st.sessions) > 0 || !st.explicit
	if !stillHeld {
		delete(w.docs, rel)
	}
	w.mu.Unlock()

	if stillHeld {
		return nil
	}

	srv, err := w.serverFor(ctx, rel)
	if err != nil {
		return nil // no server, nothing to tell
	}
	if err := srv.Close(ctx, rel); err != nil {
		w.log.Debug("closing document failed", "path", rel, "error", err)
	}
	return nil
}

// ---- queries ---------------------------------------------------------------

// Definition answers get_definition live from the language server.
func (w *Workspace) Definition(ctx context.Context, _ protocol.SessionID, path string, pos protocol.Position) ([]protocol.Location, error) {
	srv, rel, _, err := w.prepare(ctx, path)
	if err != nil {
		return nil, err
	}
	locations, err := srv.Definition(ctx, rel, pos)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	return nonNilLocations(locations), nil
}

// References uses a fresh, unambiguous and complete cached reference set when
// one exists, otherwise it asks the language server live.
func (w *Workspace) References(ctx context.Context, _ protocol.SessionID, path string, pos protocol.Position) ([]protocol.Location, error) {
	rel, err := w.relPath(path)
	if err != nil {
		return nil, err
	}
	var cachedLocations []protocol.Location
	cacheUsable := false
	cacheEligible, err := w.cacheEligibleFile(rel)
	if err != nil {
		return nil, err
	}
	if cacheEligible {
		// References are completeness-sensitive. While the workspace index is
		// incomplete, a live language server can return a plausible subset that
		// an agent cannot distinguish from the full call graph. Require the
		// workspace barrier and teach the MCP caller to retry NOT_READY instead.
		if err := w.requireReady(); err != nil {
			return nil, err
		}
		fresh, err := w.readFreshCache(ctx, rel, func() error {
			key, unique, err := w.store.SymbolKeyAt(ctx, w.id, rel, pos)
			if err != nil {
				return err
			}
			if !unique {
				return nil
			}
			locations, err := w.store.ReferencesBySymbolKey(ctx, w.id, key)
			if err != nil {
				// An unavailable reference set is not evidence that this file's
				// symbols/diagnostics snapshot is stale. This is expected for
				// SymbolInformation and malformed DocumentSymbol results that had
				// no trustworthy selectionRange, and after a referenced usage file
				// changes. Fall back live without invalidating/requeueing the
				// otherwise-fresh definition file.
				if protocol.AsError(err).Code == protocol.ErrNotReady {
					return nil
				}
				return err
			}
			cachedLocations = locations
			cacheUsable = true
			return nil
		})
		if err != nil {
			return nil, err
		}
		if fresh && cacheUsable {
			return nonNilLocations(cachedLocations), nil
		}
	}

	srv, rel, _, err := w.prepare(ctx, path)
	if err != nil {
		return nil, err
	}
	locations, err := srv.References(ctx, rel, pos, true)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	return nonNilLocations(locations), nil
}

// Hover answers get_hover. Nothing to say is NO_RESULT, not an empty Hover:
// "no hover here" is not expressible in the SPEC §4.4 shape
// (docs/ARCHITECTURE.md §10.7).
func (w *Workspace) Hover(ctx context.Context, _ protocol.SessionID, path string, pos protocol.Position) (*protocol.Hover, error) {
	srv, rel, _, err := w.prepare(ctx, path)
	if err != nil {
		return nil, err
	}
	hover, err := srv.Hover(ctx, rel, pos)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	if hover == nil || (hover.Contents == "" && hover.Documentation == "") {
		return nil, protocol.NewErrorf(protocol.ErrNoResult, "no hover information at %s:%d:%d",
			rel, pos.Line, pos.Character)
	}
	return hover, nil
}

// DocumentSymbols answers document_symbols.
func (w *Workspace) DocumentSymbols(ctx context.Context, _ protocol.SessionID, path string) ([]protocol.Symbol, error) {
	rel, err := w.relPath(path)
	if err != nil {
		return nil, err
	}
	var cachedSymbols []protocol.Symbol
	cacheEligible, err := w.cacheEligibleFile(rel)
	if err != nil {
		return nil, err
	}
	if cacheEligible {
		fresh, err := w.readFreshCache(ctx, rel, func() error {
			var err error
			cachedSymbols, err = w.store.DocumentSymbols(ctx, w.id, rel)
			return err
		})
		if err != nil {
			return nil, err
		}
		if fresh {
			return nonNilSymbols(cachedSymbols), nil
		}
	}

	srv, rel, _, err := w.prepare(ctx, path)
	if err != nil {
		return nil, err
	}
	symbols, err := srv.DocumentSymbols(ctx, rel)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	return nonNilSymbols(symbols), nil
}

// WorkspaceSymbols answers workspace_symbols across every language server that
// claims this workspace.
//
// The merge rule is the honest one: real results from any server are returned
// even if another server failed, but an EMPTY merged result never masks a
// failure — the failure is returned instead, so the agent can tell "no such
// symbol" from "ask me again in a moment".
func (w *Workspace) WorkspaceSymbols(ctx context.Context, _ protocol.SessionID, query string, limit int) ([]protocol.Symbol, error) {
	if w.store != nil {
		w.cacheMu.RLock()
		if err := w.requireReady(); err != nil {
			w.cacheMu.RUnlock()
			return nil, err
		}
		symbols, err := w.store.SearchSymbols(ctx, w.id, query, limit)
		w.cacheMu.RUnlock()
		if err != nil {
			structured := protocol.AsError(err)
			if structured.Code == protocol.ErrNotReady {
				w.triggerIndexHeal()
			}
			return nil, structured
		}
		return nonNilSymbols(symbols), nil
	}

	entries := w.applicableServers()
	if len(entries) == 0 {
		return nil, protocol.NewError(protocol.ErrUnsupported,
			"no language server is configured for this workspace")
	}

	var (
		merged   []protocol.Symbol
		firstErr error
	)
	for _, entry := range entries {
		srv, err := w.sup.Acquire(ctx, entry.Name)
		if err != nil {
			if firstErr == nil {
				firstErr = protocol.AsError(err)
			}
			continue
		}
		symbols, err := srv.WorkspaceSymbols(ctx, query, limit)
		if err != nil {
			if firstErr == nil {
				firstErr = protocol.AsError(err)
			}
			continue
		}
		merged = append(merged, symbols...)
	}

	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Path != merged[j].Path {
			return merged[i].Path < merged[j].Path
		}
		return merged[i].Range.Start.Line < merged[j].Range.Start.Line
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return nonNilSymbols(merged), nil
}

// Diagnostics answers get_diagnostics. An empty path means the whole workspace.
//
// When the caller has a live speculative overlay for the path, diagnostics are
// computed against that text (SPEC §4.2 session isolation) and nothing is
// written to disk. A stale overlay returns STALE_EDIT.
func (w *Workspace) Diagnostics(ctx context.Context, sid protocol.SessionID, path string) ([]protocol.Diagnostic, bool, error) {
	if path == "" {
		if w.store != nil {
			w.cacheMu.RLock()
			if err := w.requireReady(); err != nil {
				w.cacheMu.RUnlock()
				return nil, false, err
			}
			diags, err := w.store.Diagnostics(ctx, w.id, "")
			w.cacheMu.RUnlock()
			if err != nil {
				structured := protocol.AsError(err)
				if structured.Code == protocol.ErrNotReady {
					w.triggerIndexHeal()
				}
				return nil, false, structured
			}
			return nonNilDiagnostics(diags), false, nil
		}
		return w.workspaceDiagnostics(ctx)
	}

	rel, err := w.relPath(path)
	if err != nil {
		return nil, false, err
	}
	if diags, stale, ok, err := w.diagnosticsThroughOverlay(ctx, sid, rel); ok || err != nil {
		return diags, stale, err
	}

	var cachedDiagnostics []protocol.Diagnostic
	cacheEligible, err := w.cacheEligibleFile(rel)
	if err != nil {
		return nil, false, err
	}
	if cacheEligible {
		fresh, err := w.readFreshCache(ctx, rel, func() error {
			var err error
			cachedDiagnostics, err = w.store.Diagnostics(ctx, w.id, rel)
			return err
		})
		if err != nil {
			return nil, false, err
		}
		if fresh {
			return nonNilDiagnostics(cachedDiagnostics), false, nil
		}
	}

	srv, rel, epoch, err := w.prepareWithSettle(ctx, path, false)
	if err != nil {
		return nil, false, err
	}
	diags, stale, err := srv.Diagnostics(ctx, rel, epoch)
	if err != nil {
		return nil, false, protocol.AsError(err)
	}
	return nonNilDiagnostics(diags), stale, nil
}

// workspaceDiagnostics reports what the daemon can actually speak for.
//
// Without the SQLite index (M3) that is exactly the set of files this daemon
// has opened. The result is therefore ALWAYS flagged possibly_stale: an agent
// that cannot distinguish "this workspace is clean" from "I have not looked at
// most of it" is the silent wrongness this bridge exists to prevent.
func (w *Workspace) workspaceDiagnostics(ctx context.Context) ([]protocol.Diagnostic, bool, error) {
	w.mu.Lock()
	paths := make([]string, 0, len(w.docs))
	for rel := range w.docs {
		paths = append(paths, rel)
	}
	w.mu.Unlock()
	sort.Strings(paths)

	var (
		all      []protocol.Diagnostic
		firstErr error
	)
	const anySession = protocol.SessionID("")
	for _, rel := range paths {
		diags, _, err := w.Diagnostics(ctx, anySession, rel)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		all = append(all, diags...)
	}
	if len(all) == 0 && firstErr != nil {
		return nil, false, firstErr
	}
	return nonNilDiagnostics(all), true, nil
}

// Status reports index and supervision state without starting anything.
func (w *Workspace) Status(ctx context.Context) (protocol.IndexStatusResult, error) {
	if w.store != nil {
		w.cacheMu.RLock()
		status, err := w.store.Status(ctx, w.id)
		if err != nil {
			w.cacheMu.RUnlock()
			return protocol.IndexStatusResult{}, protocol.AsError(err)
		}
		w.indexMu.Lock()
		localState := w.indexState
		localError := w.indexError
		localFatal := w.indexFatal
		localTotal := len(w.known)
		localIndexed := status.FilesIndexed
		if w.scanComplete {
			localIndexed = localTotal - len(w.pending)
			if localIndexed < 0 {
				localIndexed = 0
			}
		}
		w.indexMu.Unlock()
		w.cacheMu.RUnlock()

		if localTotal > status.FilesTotal {
			status.FilesTotal = localTotal
		}
		if localIndexed < status.FilesIndexed {
			status.FilesIndexed = localIndexed
		}
		degraded := status.FilesIndexed < status.FilesTotal
		switch {
		case localState == protocol.IndexFailed:
			status.State = protocol.IndexFailed
			status.Error = localError
			if !localFatal {
				w.triggerIndexHeal()
			}
		case localState != protocol.IndexReady:
			status.State = localState
			status.Error = nil
		case degraded:
			status.State = protocol.IndexIndexing
			status.Error = nil
			w.triggerIndexHeal()
		case status.FilesTotal == 0:
			status.State = protocol.IndexReady
			status.Error = nil
		default:
			status.State = protocol.IndexReady
			status.Error = nil
		}
		status.Root = w.root
		status.LanguageServers = w.sup.Status()
		return status, nil
	}
	return protocol.IndexStatusResult{
		Root: w.root,
		// M2 has no index. Reporting "idle" rather than "ready" is the honest
		// answer: nothing has been indexed and no indexing is under way.
		State:           protocol.IndexIdle,
		LanguageServers: w.sup.Status(),
	}, nil
}

// ---- shutdown --------------------------------------------------------------

func (w *Workspace) shutdown(ctx context.Context) error {
	w.shutdownOnce.Do(func() {
		if w.cancelOverlay != nil {
			w.cancelOverlay()
		}
		w.overlayWG.Wait()
		if w.cancelIndex != nil {
			w.cancelIndex()
			w.indexWG.Wait()
		}
		w.shutdownErr = w.sup.Shutdown(ctx)
	})
	return w.shutdownErr
}

// startOverlaySweep runs ONE clock-driven goroutine that reaps TTL-expired
// overlays. Never a timer per overlay (docs/ARCHITECTURE.md §5.7).
func (w *Workspace) startOverlaySweep(ctx context.Context) {
	w.overlayWG.Add(1)
	go func() {
		defer w.overlayWG.Done()
		ticker := w.clock.NewTicker(overlaySweepPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
				if w.overlays != nil {
					w.overlays.sweep()
				}
			}
		}
	}()
}

// ---- helpers ---------------------------------------------------------------

// applicableServers returns the registry entries that claim this workspace.
// Root markers are what the entry declares for exactly this purpose; when no
// entry names a marker that exists here, every entry is a candidate.
func (w *Workspace) applicableServers() []config.LanguageServer {
	var matched []config.LanguageServer
	for _, entry := range w.cfg.LanguageServers {
		if w.hasRootMarker(entry) {
			matched = append(matched, entry)
		}
	}
	if len(matched) > 0 {
		return matched
	}
	return w.cfg.LanguageServers
}

func nonNilLocations(in []protocol.Location) []protocol.Location {
	if in == nil {
		return []protocol.Location{}
	}
	return in
}

func nonNilSymbols(in []protocol.Symbol) []protocol.Symbol {
	if in == nil {
		return []protocol.Symbol{}
	}
	return in
}

func nonNilDiagnostics(in []protocol.Diagnostic) []protocol.Diagnostic {
	if in == nil {
		return []protocol.Diagnostic{}
	}
	return in
}
