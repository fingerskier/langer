package workspace

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/fingerskier/langer/config"
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
	id   protocol.WorkspaceID
	root string
	cfg  *config.Config
	sup  lsp.Supervisor
	log  *slog.Logger

	// docMu serialises the read-file/compare/reopen sequence per path. It is
	// held across language server calls on purpose — the whole point is that
	// two queries for the same file cannot interleave a close with an open —
	// but it is a leaf: nothing else is locked underneath it.
	docMu   sync.Mutex
	docLock map[string]*sync.Mutex

	mu    sync.Mutex
	docs  map[string]*docState
	plans map[string]editPlan
	order []string // plan tokens, oldest first
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

func newWorkspace(id protocol.WorkspaceID, root string, opts RegistryOptions) (*Workspace, error) {
	log := opts.Logger.With("component", "workspace", "root", root)

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

	return &Workspace{
		id:      id,
		root:    root,
		cfg:     opts.Config,
		sup:     sup,
		log:     log,
		docLock: map[string]*sync.Mutex{},
		docs:    map[string]*docState{},
		plans:   map[string]editPlan{},
	}, nil
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
	srv, err := w.serverFor(ctx, rel)
	if err != nil {
		return err
	}

	unlock := w.lockDoc(rel)
	defer unlock()

	if _, err := w.ensureDocumentLocked(ctx, srv, rel, languageID); err != nil {
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
	w.releaseDocument(ctx, rel, sid)
	return nil
}

// EndSession releases everything sid owns in this workspace. Idempotent.
func (w *Workspace) EndSession(ctx context.Context, sid protocol.SessionID) {
	w.mu.Lock()
	paths := make([]string, 0, len(w.docs))
	for rel, st := range w.docs {
		if _, ok := st.sessions[sid]; ok {
			paths = append(paths, rel)
		}
	}
	w.mu.Unlock()

	for _, rel := range paths {
		w.releaseDocument(ctx, rel, sid)
	}
}

// releaseDocument removes one session's claim and closes the document on the
// language server when nothing holds it any more.
func (w *Workspace) releaseDocument(ctx context.Context, rel string, sid protocol.SessionID) {
	unlock := w.lockDoc(rel)
	defer unlock()

	w.mu.Lock()
	st, ok := w.docs[rel]
	if !ok {
		w.mu.Unlock()
		return
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
		return
	}

	srv, err := w.serverFor(ctx, rel)
	if err != nil {
		return // no server, nothing to tell
	}
	if err := srv.Close(ctx, rel); err != nil {
		w.log.Debug("closing document failed", "path", rel, "error", err)
	}
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

// References answers get_references live, declaration included: an agent asking
// "where is this used" almost always wants the definition in the list too, and
// SPEC §4.4's Location carries is_definition so it can tell them apart.
func (w *Workspace) References(ctx context.Context, _ protocol.SessionID, path string, pos protocol.Position) ([]protocol.Location, error) {
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
func (w *Workspace) Diagnostics(ctx context.Context, _ protocol.SessionID, path string) ([]protocol.Diagnostic, bool, error) {
	if path == "" {
		return w.workspaceDiagnostics(ctx)
	}

	srv, rel, epoch, err := w.prepare(ctx, path)
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
func (w *Workspace) Status(_ context.Context) (protocol.IndexStatusResult, error) {
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
	return w.sup.Shutdown(ctx)
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
