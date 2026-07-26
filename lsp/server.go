// Package lsp owns the language servers of one workspace: lazy start,
// initialize and capability capture, document synchronisation, the SPEC §4.3
// diagnostics settle rule, crash detection and exponential-backoff restart.
//
// It is the only thing in the tree holding a procx.Process, and it is the last
// layer that knows any LSP vocabulary: nothing above the Server interface has
// heard of a MarkupContent or a LocationLink.
//
// SPEC §8 — "a language server crash must not terminate the daemon" — is a
// property of the types here, not of reviewer vigilance: a crash cancels only
// the server's own context, fails its pending calls with SERVER_CRASHED, and
// schedules a restart.
package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

// LSP ServerCapabilities field names, used with Server.Supports.
const (
	CapDefinition       = "definitionProvider"
	CapReferences       = "referencesProvider"
	CapHover            = "hoverProvider"
	CapDocumentSymbol   = "documentSymbolProvider"
	CapWorkspaceSymbol  = "workspaceSymbolProvider"
	CapRename           = "renameProvider"
	CapPullDiagnostics  = "diagnosticProvider"
	CapPushDiagnostics  = "publishDiagnostics"
	defaultSymbolLimit  = 200
	maxPreviewFileBytes = 4 << 20
)

// Server is a capability-checked handle to one running language server,
// speaking protocol types.
//
// Every method returns a *protocol.Error. A crashed server returns
// SERVER_CRASHED from every method rather than blocking.
type IndexSymbol struct {
	protocol.Symbol
	SelectionRange    protocol.Range
	HasSelectionRange bool
}

type Server interface {
	// Generation increments on every restart. A caller that cached state
	// resyncs when it changes.
	Generation() uint64

	// Supports gates a request on the initialize result so an unsupported
	// capability returns UNSUPPORTED without a round trip.
	Supports(capability string) bool

	// Open sends didOpen if the document is not already open and returns the
	// document's current diagnostics epoch. Idempotent, safe concurrently.
	Open(ctx context.Context, path, languageID, text string) (epoch uint64, err error)
	Close(ctx context.Context, path string) error

	// WithText runs fn while the server's view of path is text, holding the
	// per-(server, path) lock for the WHOLE call, and restores the previous
	// text before releasing (SPEC §4.2 overlay isolation, docs §6.6). The
	// callback context is scoped to fn and must not be retained or used by work
	// that outlives fn.
	WithText(ctx context.Context, path, text string, fn func(ctx context.Context, epoch uint64) error) error
	// WithDiskText serializes indexing against overlays and live queries. It
	// presents caller-supplied disk bytes for the callback, marks the callback
	// context as holding the path lock, then unconditionally restores the
	// document's prior open/closed state. Its callback context has the same
	// synchronous-only lifetime as WithText's.
	WithDiskText(ctx context.Context, path, languageID, text string, fn func(ctx context.Context, epoch uint64) error) error

	Definition(ctx context.Context, path string, pos protocol.Position) ([]protocol.Location, error)
	References(ctx context.Context, path string, pos protocol.Position, includeDecl bool) ([]protocol.Location, error)
	Hover(ctx context.Context, path string, pos protocol.Position) (*protocol.Hover, error)
	DocumentSymbols(ctx context.Context, path string) ([]protocol.Symbol, error)
	DocumentSymbolsForIndex(ctx context.Context, path string) ([]IndexSymbol, error)
	WorkspaceSymbols(ctx context.Context, query string, limit int) ([]protocol.Symbol, error)
	Rename(ctx context.Context, path string, pos protocol.Position, newName string) ([]protocol.FileEdit, error)

	// Diagnostics implements the SPEC §4.3 settle rule. On budget expiry it
	// returns the latest known diagnostics with possiblyStale=true — NOT an
	// error.
	Diagnostics(ctx context.Context, path string, sinceEpoch uint64) (diags []protocol.Diagnostic, possiblyStale bool, err error)
}

// session is one generation of a live connection: the transport plus the
// capabilities the server advertised when it started.
type session struct {
	conn           *conn
	caps           map[string]json.RawMessage
	generation     uint64
	analysis       *analysisState
	symbolProgress bool
}

// analysisState belongs to exactly one connection candidate. Keeping progress
// off the stable server handle prevents late notifications from a failed or
// replaced connection from blessing a later generation's symbol index.
type analysisState struct {
	analyzing atomic.Int64
	indexed   atomic.Bool
}

// setAnalyzing records the candidate entering (true) or leaving (false) a
// workspace analysis pass. Called from that connection's reader goroutine.
func (a *analysisState) setAnalyzing(active bool) {
	if active {
		a.analyzing.Add(1)
		return
	}
	if a.analyzing.Add(-1) < 0 {
		a.analyzing.Store(0)
	}
	// A completed pass means the index is populated. Latched, because later
	// passes re-analyse an already-indexed workspace.
	a.indexed.Store(true)
}

// symbolsIndexed reports whether this generation's workspace-wide symbol
// queries can be trusted.
func (sess *session) symbolsIndexed() bool {
	return !sess.symbolProgress || sess.analysis.indexed.Load()
}

// server implements Server for one registry entry. The supervisor swaps its
// session on every restart; the handle a caller holds stays valid.
type server struct {
	name string
	root string

	docs  *documents
	diags *diagnostics
	// mutations orders document changes below restart replay/publication. See
	// documentMutationGate for the global-before-path lock order.
	mutations documentMutationGate

	settleQuiet  time.Duration
	settleBudget time.Duration

	mu      sync.RWMutex
	current *session
	// down carries the reason the server is unavailable when current is nil.
	down       error
	generation uint64
}

func (s *server) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// setSession installs a new generation.
func (s *server) setSession(sess *session) {
	s.mu.Lock()
	s.current = sess
	s.down = nil
	s.generation = sess.generation
	s.mu.Unlock()
}

// resetGenerationState runs before a replacement connection can emit
// diagnostics. Analysis state is candidate-local and is likewise constructed
// before newConn starts its reader.
func (s *server) resetGenerationState() {
	s.diags.reset()
}

// reportsSymbolProgress is true when workspaceSymbolProvider is the OBJECT form
// carrying workDoneProgress, rather than a bare `true`. A server that reports
// progress for this request is telling us the index is not ready on demand.
func reportsSymbolProgress(caps map[string]json.RawMessage) bool {
	raw, ok := caps[CapWorkspaceSymbol]
	if !ok {
		return false
	}
	var opts struct {
		WorkDoneProgress bool `json:"workDoneProgress"`
	}
	if err := json.Unmarshal(raw, &opts); err != nil {
		return false // the bare `true` form: no progress reporting
	}
	return opts.WorkDoneProgress
}

// clearSession marks the server unavailable.
func (s *server) clearSession(cause error) {
	s.mu.Lock()
	s.current = nil
	s.down = cause
	s.mu.Unlock()
}

// live returns the current session, or SERVER_CRASHED.
func (s *server) live() (*session, error) {
	s.mu.RLock()
	sess, down := s.current, s.down
	s.mu.RUnlock()
	if sess == nil {
		return nil, crashedError(down)
	}
	return sess, nil
}

// Supports reports whether the server advertised a capability.
func (s *server) Supports(capability string) bool {
	s.mu.RLock()
	sess := s.current
	s.mu.RUnlock()
	if sess == nil {
		return false
	}
	return sess.supports(capability)
}

func (sess *session) supports(capability string) bool {
	if capability == CapPushDiagnostics {
		// No server advertises push diagnostics: publishDiagnostics is a
		// CLIENT capability. What a server does advertise is diagnosticProvider,
		// meaning it expects the 3.17 PULL model instead. v0.1 implements push
		// only, so a pull-model server gets UNSUPPORTED rather than a silent
		// empty result (docs/ARCHITECTURE.md §11 item 5).
		return !capabilityAdvertised(sess.caps, CapPullDiagnostics)
	}
	return capabilityAdvertised(sess.caps, capability)
}

// capabilityAdvertised treats an absent key, null and false as "no". Anything
// else — true, or an options object — is "yes".
func capabilityAdvertised(caps map[string]json.RawMessage, name string) bool {
	raw, ok := caps[name]
	if !ok {
		return false
	}
	trimmed := string(raw)
	return trimmed != "false" && trimmed != "null" && trimmed != ""
}

// require gates a request on a capability before any round trip.
func (s *server) require(capability, tool string) (*session, error) {
	sess, err := s.live()
	if err != nil {
		return nil, err
	}
	if !sess.supports(capability) {
		return nil, protocol.NewErrorf(protocol.ErrUnsupported,
			"language server %s does not provide %s (%s)", s.name, tool, capability)
	}
	return sess, nil
}

// ---- document synchronisation ----

// Open sends didOpen and returns the diagnostics epoch to settle against.
func (s *server) Open(ctx context.Context, path, languageID, text string) (uint64, error) {
	ctx, unlockMutation, err := s.lockDocumentMutation(ctx)
	if err != nil {
		return 0, err
	}
	defer unlockMutation()

	doc, err := s.lockDoc(ctx, path)
	if err != nil {
		return 0, err
	}
	defer s.unlockDoc(ctx, doc)

	sess, err := s.live()
	if err != nil {
		return 0, err
	}

	if _, version, _, open := s.docs.state(path); open {
		// Already open: no new push is coming, so settle against what the
		// server has already told us about this file.
		return s.diags.currentForVersion(path, version), nil
	}

	// The mark must be taken BEFORE the notification, or a push racing us could
	// satisfy the wait with pre-edit results.
	epoch := s.diags.mark()
	version := s.docs.markOpen(path, languageID, text)
	s.diags.invalidate(path)

	if err := sess.conn.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        wire.PathToURI(s.root, path),
			"languageId": languageID,
			"version":    version,
			"text":       text,
		},
	}); err != nil {
		s.docs.markClosed(path)
		return 0, err
	}
	return epoch, nil
}

// Close sends didClose. Closing a document that is not open succeeds.
func (s *server) Close(ctx context.Context, path string) error {
	ctx, unlockMutation, err := s.lockDocumentMutation(ctx)
	if err != nil {
		return err
	}
	defer unlockMutation()

	doc, err := s.lockDoc(ctx, path)
	if err != nil {
		return err
	}
	defer s.unlockDoc(ctx, doc)

	if _, _, _, open := s.docs.state(path); !open {
		return nil
	}
	s.docs.markClosed(path)
	s.diags.invalidate(path)

	sess, err := s.live()
	if err != nil {
		// The server is gone; it has no document to forget.
		return nil
	}
	return sess.conn.notify("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": wire.PathToURI(s.root, path)},
	})
}

// WithText is how SPEC §4.2's per-session overlays are made isolated without a
// second language server process (docs §6.6).
func (s *server) WithText(
	ctx context.Context,
	path, text string,
	fn func(context.Context, uint64) error,
) (retErr error) {
	ctx, unlockMutation, err := s.lockDocumentMutation(ctx)
	if err != nil {
		return err
	}
	defer unlockMutation()

	doc, err := s.lockDoc(ctx, path)
	if err != nil {
		return err
	}
	defer s.unlockDoc(ctx, doc)

	// Resolve the session only after both mutation locks. A restart may have
	// completed while this call waited for another operation on the same path.
	sess, err := s.live()
	if err != nil {
		return err
	}

	base, _, _, open := s.docs.state(path)
	if !open {
		return protocol.NewErrorf(protocol.ErrNotReady,
			"%s must be open before a speculative edit", path)
	}

	// pushText records the new local version before writing didChange. Arm the
	// restore first so a transport failure during that write cannot leave the
	// authoritative snapshot holding speculative bytes.
	defer func() {
		restoreErr := s.restoreAfterText(sess, path, base)
		if retErr == nil && restoreErr != nil {
			retErr = restoreErr
		}
	}()

	epoch, err := s.pushText(sess, path, text)
	if err != nil {
		return err
	}
	return callWithDocLock(ctx, path, epoch, fn)
}

func (s *server) restoreAfterText(original *session, path, base string) error {
	// Restore local state first and unconditionally. If the original generation
	// died, the writer waiting on the mutation gate will replay this base text
	// before it publishes the replacement session.
	version := s.docs.markChanged(path, base)
	s.diags.invalidate(path)
	live, err := s.live()
	if err != nil || live != original {
		return nil
	}
	return live.conn.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": wire.PathToURI(s.root, path), "version": version},
		"contentChanges": []any{map[string]any{"text": base}},
	})
}

// WithDiskText gives the M3 indexer a document view made exclusively from disk
// bytes while holding the same per-path lock as speculative overlays. A file
// that was implicitly opened for indexing is always closed again; a file that
// was already open gets its original text and language state back.
func (s *server) WithDiskText(
	ctx context.Context,
	path, languageID, text string,
	fn func(context.Context, uint64) error,
) (retErr error) {
	ctx, unlockMutation, err := s.lockDocumentMutation(ctx)
	if err != nil {
		return err
	}
	defer unlockMutation()

	doc, err := s.lockDoc(ctx, path)
	if err != nil {
		return err
	}
	defer s.unlockDoc(ctx, doc)

	sess, err := s.live()
	if err != nil {
		return err
	}

	baseText, baseVersion, baseLanguageID, wasOpen := s.docs.state(path)
	if wasOpen && baseText == text && strings.EqualFold(baseLanguageID, languageID) {
		// The language server is already analyzing exactly this document view.
		// Sending a no-op didChange would reserve a future diagnostics epoch
		// that many servers never publish, making the indexer time out against
		// diagnostics that were current before this call.
		return callWithDocLock(ctx, path, s.diags.currentForVersion(path, baseVersion), fn)
	}
	defer func() {
		restoreErr := s.restoreAfterDiskText(sess.generation, path, baseText, baseLanguageID, wasOpen)
		if retErr == nil && restoreErr != nil {
			retErr = restoreErr
		}
	}()

	var epoch uint64
	if wasOpen {
		epoch, err = s.pushText(sess, path, text)
	} else {
		epoch = s.diags.mark()
		version := s.docs.markOpen(path, languageID, text)
		s.diags.invalidate(path)
		err = sess.conn.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        wire.PathToURI(s.root, path),
				"languageId": languageID,
				"version":    version,
				"text":       text,
			},
		})
	}
	if err != nil {
		return err
	}

	return callWithDocLock(ctx, path, epoch, fn)
}

func callWithDocLock(
	ctx context.Context,
	path string,
	epoch uint64,
	fn func(context.Context, uint64) error,
) error {
	ctx, release := withDocLock(ctx, path)
	defer release()
	return fn(ctx, epoch)
}

// restoreAfterDiskText updates our document snapshot before touching the
// connection. If the server died during the callback, a replacement generation
// therefore resyncs the original state rather than the temporary disk view.
func (s *server) restoreAfterDiskText(
	generation uint64,
	path, baseText, baseLanguageID string,
	wasOpen bool,
) error {
	if wasOpen {
		version := s.docs.markChanged(path, baseText)
		s.diags.invalidate(path)
		sess, err := s.live()
		if err != nil {
			// The local snapshot is authoritative across restarts. If this
			// generation died, the replacement will replay the restored text
			// before it becomes visible to callers.
			return nil
		}
		if sess.generation != generation {
			return nil
		}
		return sess.conn.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": wire.PathToURI(s.root, path), "version": version},
			"contentChanges": []any{map[string]any{"text": baseText}},
		})
	}

	s.docs.restoreClosed(path, baseLanguageID)
	s.diags.invalidate(path)
	sess, err := s.live()
	if err != nil {
		// As above, a replacement generation will observe the restored closed
		// state during its serialized resync.
		return nil
	}
	if sess.generation != generation {
		return nil
	}
	return sess.conn.notify("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": wire.PathToURI(s.root, path)},
	})
}

// pushText sends a full-document didChange and returns the epoch to settle
// against.
func (s *server) pushText(sess *session, path, text string) (uint64, error) {
	epoch := s.diags.mark()
	version := s.docs.markChanged(path, text)
	s.diags.invalidate(path)
	err := sess.conn.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": wire.PathToURI(s.root, path), "version": version},
		"contentChanges": []any{map[string]any{"text": text}},
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

func (s *server) lockDoc(ctx context.Context, path string) (*document, error) {
	if holdsDocLock(ctx, path) {
		return nil, nil
	}
	return s.docs.acquire(ctx, path)
}

func (s *server) unlockDoc(_ context.Context, doc *document) {
	if doc != nil {
		s.docs.release(doc)
	}
}

func (s *server) lockDocumentMutation(
	ctx context.Context,
) (context.Context, func(), error) {
	if holdsDocumentMutationGate(ctx, s) {
		return ctx, func() {}, nil
	}
	if err := s.mutations.readLock(ctx); err != nil {
		return ctx, nil, err
	}
	lease := &documentMutationLease{}
	lease.active.Store(true)
	unlock := func() {
		if lease.active.Swap(false) {
			s.mutations.readUnlock()
		}
	}
	return withDocumentMutationGate(ctx, s, lease), unlock, nil
}

// ---- queries ----

func (s *server) Definition(ctx context.Context, path string, pos protocol.Position) ([]protocol.Location, error) {
	sess, err := s.require(CapDefinition, "get_definition")
	if err != nil {
		return nil, err
	}

	raw, err := sess.conn.call(ctx, "textDocument/definition", positionParams(s.root, path, pos))
	if err != nil {
		return nil, err
	}
	raws, err := wire.DecodeDefinition(raw)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding definition result: %v", err)
	}

	locations := s.toLocations(raws)
	// Everything a definition query returns IS a definition.
	for i := range locations {
		locations[i].IsDefinition = true
	}
	return locations, nil
}

func (s *server) References(ctx context.Context, path string, pos protocol.Position, includeDecl bool) ([]protocol.Location, error) {
	sess, err := s.require(CapReferences, "get_references")
	if err != nil {
		return nil, err
	}

	params := positionParams(s.root, path, pos)
	params["context"] = map[string]any{"includeDeclaration": includeDecl}

	raw, err := sess.conn.call(ctx, "textDocument/references", params)
	if err != nil {
		return nil, err
	}
	raws, err := wire.DecodeLocations(raw)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding references result: %v", err)
	}
	locations := s.toLocations(raws)

	// LSP does not say which reference is the declaration, so ask. The answer
	// is already computed and cached by both servers, and without it
	// is_definition would be a permanently false field (SPEC §4.4).
	if defs, err := s.Definition(ctx, path, pos); err == nil {
		for i := range locations {
			for _, d := range defs {
				if locations[i].Path == d.Path && locations[i].Range == d.Range {
					locations[i].IsDefinition = true
					break
				}
			}
		}
	}
	return locations, nil
}

func (s *server) Hover(ctx context.Context, path string, pos protocol.Position) (*protocol.Hover, error) {
	sess, err := s.require(CapHover, "get_hover")
	if err != nil {
		return nil, err
	}

	raw, err := sess.conn.call(ctx, "textDocument/hover", positionParams(s.root, path, pos))
	if err != nil {
		return nil, err
	}
	rawHover, err := wire.DecodeHover(raw)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding hover result: %v", err)
	}
	if rawHover == nil {
		return nil, nil
	}

	var rng *protocol.Range
	if rawHover.Range != nil {
		converted := wire.ToRange(*rawHover.Range)
		rng = &converted
	}
	return wire.SplitHover(rawHover.Value, rng), nil
}

func (s *server) documentSymbolsForIndex(ctx context.Context, path string) ([]wire.NormalizedSymbol, error) {
	sess, err := s.require(CapDocumentSymbol, "document_symbols")
	if err != nil {
		return nil, err
	}

	raw, err := sess.conn.call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": wire.PathToURI(s.root, path)},
	})
	if err != nil {
		return nil, err
	}
	raws, err := wire.DecodeSymbols(raw)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding documentSymbol result: %v", err)
	}
	return wire.FlattenSymbolsForIndex(s.root, path, raws), nil
}

func (s *server) DocumentSymbols(ctx context.Context, path string) ([]protocol.Symbol, error) {
	normalized, err := s.documentSymbolsForIndex(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.Symbol, 0, len(normalized))
	for _, symbol := range normalized {
		out = append(out, symbol.Symbol)
	}
	return out, nil
}

func (s *server) DocumentSymbolsForIndex(ctx context.Context, path string) ([]IndexSymbol, error) {
	normalized, err := s.documentSymbolsForIndex(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]IndexSymbol, 0, len(normalized))
	for _, symbol := range normalized {
		out = append(out, IndexSymbol{
			Symbol:            symbol.Symbol,
			SelectionRange:    symbol.SelectionRange,
			HasSelectionRange: symbol.HasSelectionRange,
		})
	}
	return out, nil
}

func (s *server) WorkspaceSymbols(ctx context.Context, query string, limit int) ([]protocol.Symbol, error) {
	sess, err := s.require(CapWorkspaceSymbol, "workspace_symbols")
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultSymbolLimit
	}

	raw, err := sess.conn.call(ctx, "workspace/symbol", map[string]any{"query": query})
	if err != nil {
		return nil, err
	}
	raws, err := wire.DecodeSymbols(raw)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding workspace/symbol result: %v", err)
	}

	symbols := wire.FlattenSymbols(s.root, "", raws)

	// A server mid-analysis answers with an empty array, not an error. Passing
	// that through as "no matches" is the silent wrongness this whole project
	// exists to prevent: the agent cannot tell "your symbol does not exist"
	// from "ask me again in a moment". SPEC §3.6 has a code for exactly this.
	//
	// Only the EMPTY case is converted. Real results are always returned, even
	// mid-analysis, because a partial answer is still a true answer.
	if len(symbols) == 0 && !sess.symbolsIndexed() {
		return nil, protocol.NewErrorf(protocol.ErrNotReady,
			"%s is still building its workspace symbol index", s.name).WithRetryAfterMS(300)
	}

	if len(symbols) > limit {
		symbols = symbols[:limit]
	}
	return symbols, nil
}

func (s *server) Rename(ctx context.Context, path string, pos protocol.Position, newName string) ([]protocol.FileEdit, error) {
	sess, err := s.require(CapRename, "rename_symbol")
	if err != nil {
		return nil, err
	}

	params := positionParams(s.root, path, pos)
	params["newName"] = newName

	raw, err := sess.conn.call(ctx, "textDocument/rename", params)
	if err != nil {
		return nil, err
	}
	raws, err := wire.DecodeWorkspaceEdit(raw)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "decoding rename result: %v", err)
	}
	return wire.ToFileEdits(s.root, raws)
}

// Diagnostics waits for the SPEC §4.3 settle window.
func (s *server) Diagnostics(ctx context.Context, path string, sinceEpoch uint64) ([]protocol.Diagnostic, bool, error) {
	sess, err := s.require(CapPushDiagnostics, "get_diagnostics")
	if err != nil {
		return nil, false, err
	}

	// A caller already inside WithText holds this path's lock; taking it again
	// would deadlock simulate_edit against its own diagnostics query.
	if !holdsDocLock(ctx, path) {
		doc, err := s.docs.acquire(ctx, path)
		if err != nil {
			return nil, false, err
		}
		defer s.docs.release(doc)
	}

	diags, stale, err := s.diags.settle(ctx, path, sinceEpoch, s.settleQuiet, s.settleBudget, sess.conn.done)
	if err != nil {
		return nil, false, err
	}
	if diags == nil {
		diags = []protocol.Diagnostic{}
	}
	return diags, stale, nil
}

// ---- helpers ----

func positionParams(root, path string, pos protocol.Position) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": wire.PathToURI(root, path)},
		"position":     map[string]any{"line": pos.Line, "character": pos.Character},
	}
}

// toLocations normalises raw locations, grouping by file so each group gets its
// own LineIndex for previews.
func (s *server) toLocations(raws []wire.RawLocation) []protocol.Location {
	byURI := map[string][]wire.RawLocation{}
	order := make([]string, 0, len(raws))
	for _, r := range raws {
		if _, seen := byURI[r.URI]; !seen {
			order = append(order, r.URI)
		}
		byURI[r.URI] = append(byURI[r.URI], r)
	}

	out := make([]protocol.Location, 0, len(raws))
	for _, uri := range order {
		rel, ok := wire.URIToPath(s.root, uri)
		if !ok {
			continue // a dependency file: never ours to report (SPEC §3.4)
		}
		locations, err := wire.ToLocations(s.root, byURI[uri], s.lineIndex(rel))
		if err != nil {
			continue
		}
		out = append(out, locations...)
	}
	return out
}

// lineIndex supplies preview text, preferring our in-memory copy of an open
// document over what is on disk.
func (s *server) lineIndex(rel string) wire.LineIndex {
	if text, _, _, open := s.docs.state(rel); open {
		return wire.NewLineIndex(text)
	}

	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil || info.Size() > maxPreviewFileBytes {
		return wire.LineIndex{}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return wire.LineIndex{}
	}
	return wire.NewLineIndex(string(data))
}

// sortedCapabilityNames is used only in diagnostics and error messages.
func sortedCapabilityNames(caps map[string]json.RawMessage) []string {
	names := make([]string, 0, len(caps))
	for name := range caps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
