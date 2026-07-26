package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/lsp/wire"
	"github.com/fingerskier/langer/protocol"
)

// maxDocumentBytes bounds what the daemon will read into memory and push to a
// language server. A 32 MiB minified bundle is not source code an agent needs
// semantics for, and pushing it stalls the server for everyone else.
const maxDocumentBytes = 8 << 20

// ---- path handling ---------------------------------------------------------

// relPath validates a client-supplied path and returns it workspace-relative
// and slash-separated.
//
// Anything that escapes the root is WORKSPACE_UNKNOWN — SPEC §3.6's "path not
// in an open workspace" — which is also the answer that keeps a confused client
// from reading /etc/passwd through the daemon.
func (w *Workspace) relPath(path string) (string, error) {
	if path == "" {
		return "", protocol.NewError(protocol.ErrWorkspaceUnknown, "a path is required")
	}

	clean := filepath.FromSlash(path)
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(w.root, clean)
	}
	clean = filepath.Clean(clean)

	rel, err := filepath.Rel(w.root, clean)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "%s is not inside workspace %s", path, w.root)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "%s is outside workspace %s", path, w.root)
	}
	return filepath.ToSlash(rel), nil
}

func (w *Workspace) absPath(rel string) string {
	return filepath.Join(w.root, filepath.FromSlash(rel))
}

// validateWorkspaceFile rejects filesystem escape through any symlinked path
// component. Lstat on only the leaf follows an intermediate symlink and can
// otherwise read a file outside the workspace.
func (w *Workspace) validateWorkspaceFile(rel string) error {
	parts := strings.Split(filepath.Clean(filepath.FromSlash(rel)), string(filepath.Separator))
	current := w.root
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
				"%s is not a project file in workspace %s", rel, w.root)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
				"%s is not a regular project file in workspace %s", rel, w.root)
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
					"%s is not a regular project file in workspace %s", rel, w.root)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
				"%s is not a regular project file in workspace %s", rel, w.root)
		}
	}
	return nil
}

// cacheEligibleFile reports whether rel belongs to the authoritative
// project-file snapshot. Safe in-root dependency/ignored files may still be
// queried live, as SPEC §3.4 permits, but must never enter SQLite or
// workspace-wide cached answers.
func (w *Workspace) cacheEligibleFile(rel string) (bool, error) {
	if err := w.validateWorkspaceFile(rel); err != nil {
		return false, err
	}
	if w.store == nil {
		return false, nil
	}
	w.indexMu.Lock()
	_, known := w.known[rel]
	w.indexMu.Unlock()
	return known, nil
}

// hasRootMarker reports whether any of an entry's declared root markers exists
// at the workspace root. SPEC §3.3 lists root markers as a declarative registry
// field; this is what they are for.
func (w *Workspace) hasRootMarker(entry config.LanguageServer) bool {
	for _, marker := range entry.RootMarkers {
		if marker == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(w.root, filepath.FromSlash(marker))); err == nil {
			return true
		}
	}
	return false
}

// ---- language server selection --------------------------------------------

// serverFor acquires the language server that claims path's extension.
func (w *Workspace) serverFor(ctx context.Context, rel string) (lsp.Server, error) {
	entry, ok := w.cfg.LanguageServerForFile(rel)
	if !ok {
		return nil, protocol.NewErrorf(protocol.ErrUnsupported,
			"no language server is configured for %s", filepath.Ext(rel))
	}
	srv, err := w.sup.Acquire(ctx, entry.Name)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	return srv, nil
}

// languageIDFor maps a file extension onto the LSP language identifier a
// language server expects in didOpen. It falls back to the registry entry's
// name, which is what a user-configured server is most likely to answer to.
func (w *Workspace) languageIDFor(rel string) string {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".py", ".pyi":
		return "python"
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	}
	if entry, ok := w.cfg.LanguageServerForFile(rel); ok {
		return entry.Name
	}
	return "plaintext"
}

// ---- document freshness ----------------------------------------------------

type documentLock struct {
	held chan struct{}
}

// lockDoc serialises the read/compare/reopen sequence for one path. Unlike a
// sync.Mutex, the channel leaf lets a request abandon its wait when its context
// expires without acquiring or leaking the lock.
func (w *Workspace) lockDoc(ctx context.Context, rel string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, protocol.NewErrorf(protocol.ErrNotReady,
			"waiting for exclusive access to %s: %v", rel, err).WithRetryAfterMS(200)
	}

	w.docMu.Lock()
	leaf, ok := w.docLock[rel]
	if !ok {
		leaf = &documentLock{held: make(chan struct{}, 1)}
		w.docLock[rel] = leaf
	}
	w.docMu.Unlock()

	select {
	case leaf.held <- struct{}{}:
		return func() { <-leaf.held }, nil
	case <-ctx.Done():
		return nil, protocol.NewErrorf(protocol.ErrNotReady,
			"waiting for exclusive access to %s: %v", rel, ctx.Err()).WithRetryAfterMS(200)
	}
}

// prepare is the common preamble of every positional query: validate the path,
// acquire the server, and make sure the server's copy of the file is the file
// that is on disk right now. It also returns the diagnostics epoch to settle
// against (SPEC §4.3), because the caller that needs it must not re-do the
// whole read-and-compare to get it.
func (w *Workspace) prepare(ctx context.Context, path string) (lsp.Server, string, uint64, error) {
	return w.prepareWithSettle(ctx, path, true)
}

func (w *Workspace) prepareWithSettle(ctx context.Context, path string, requireSettled bool) (lsp.Server, string, uint64, error) {
	rel, err := w.relPath(path)
	if err != nil {
		return nil, "", 0, err
	}
	if _, err := w.cachedFileFresh(ctx, rel); err != nil {
		return nil, "", 0, err
	}
	srv, err := w.serverFor(ctx, rel)
	if err != nil {
		return nil, "", 0, err
	}

	unlock, err := w.lockDoc(ctx, rel)
	if err != nil {
		return nil, "", 0, err
	}
	defer unlock()

	epoch, err := w.ensureDocumentLocked(ctx, srv, rel, "", requireSettled)
	if err != nil {
		return nil, "", 0, err
	}
	return srv, rel, epoch, nil
}

// ensureDocumentLocked makes the language server's view of rel match disk and
// returns the diagnostics epoch of the resulting state.
//
// This is M2's entire staleness story. SPEC §3.4 says a file whose content
// changed underneath is stale and must be answered live; with no index and no
// watcher until M3, re-reading before every query is what makes that true. When
// the bytes have changed the document is CLOSED and reopened rather than left
// alone: a language server that still holds yesterday's text answers confidently
// and wrongly, which is worse than any error.
//
// The caller must hold lockDoc(rel).
func (w *Workspace) ensureDocumentLocked(ctx context.Context, srv lsp.Server, rel, languageID string, requireSettled bool) (uint64, error) {
	text, err := w.readDocument(rel)
	if err != nil {
		return 0, err
	}
	if languageID == "" {
		languageID = w.languageIDFor(rel)
	}

	w.mu.Lock()
	st := w.docs[rel]
	w.mu.Unlock()

	if st != nil && st.text != text {
		if err := srv.Close(ctx, rel); err != nil {
			// A close that fails is not fatal — the reopen below still pushes
			// the current text — but it is worth seeing in the log.
			w.log.Debug("closing a stale document failed", "path", rel, "error", err)
		}
	}

	pushed := st == nil || st.text != text

	epoch, err := srv.Open(ctx, rel, languageID, text)
	if err != nil {
		return 0, protocol.AsError(err)
	}

	if pushed {
		if err := w.awaitAnalysis(ctx, srv, rel, epoch); err != nil && requireSettled {
			return 0, err
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if st == nil {
		st = &docState{sessions: map[protocol.SessionID]struct{}{}}
		w.docs[rel] = st
	}
	st.text = text
	return epoch, nil
}

// awaitAnalysis blocks until the language server has finished its first
// analysis of a freshly opened document.
//
// This is not about diagnostics. It is about ANSWER COMPLETENESS. Measured
// against typescript-language-server 5.3.0 on testdata/ts-project: asking for
// the references of `getUserById` in the same instant the document was opened
// returns ONE location — the declaration — and asking again 250 ms later
// returns all six. The project loads asynchronously, and a reference query that
// lands mid-load gets a truthful answer about a program that is still half
// built.
//
// An agent cannot tell that from "this symbol is unused", which is the single
// most dangerous thing this bridge could do. So the first query on a document
// waits for the server to settle on it, which is the only completion signal
// either real server gives us (tsserver reports no workDoneProgress for a
// project load). The wait is bounded by the SPEC §4.3 settle budget, and
// subsequent queries on the same document pay nothing.
//
// Its limits are honest: settling on file A proves the server built a program
// containing A and its imports, not that it has read every file in the
// workspace. The complete answer is the SPEC §3.4 index, which arrives in M3.
func (w *Workspace) awaitAnalysis(ctx context.Context, srv lsp.Server, rel string, epoch uint64) error {
	if !srv.Supports(lsp.CapPushDiagnostics) {
		return nil
	}
	if _, stale, err := srv.Diagnostics(ctx, rel, epoch); err != nil {
		return protocol.AsError(err)
	} else if stale {
		return protocol.NewError(protocol.ErrNotReady,
			"initial language-server analysis has not settled; retry").WithRetryAfterMS(250)
	}
	return nil
}

// readDocument reads a workspace file, mapping a missing or unreadable path
// onto WORKSPACE_UNKNOWN.
func (w *Workspace) readDocument(rel string) (string, error) {
	if err := w.validateWorkspaceFile(rel); err != nil {
		return "", err
	}
	abs := w.absPath(rel)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "%s is not a file in workspace %s", rel, w.root)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
			"%s is not a regular, non-symlink file in workspace %s", rel, w.root)
	}
	if info.Size() > maxDocumentBytes {
		return "", protocol.NewErrorf(protocol.ErrUnsupported,
			"%s is %d bytes; langer does not push files over %d bytes to a language server",
			rel, info.Size(), maxDocumentBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "reading %s: %v", rel, err)
	}
	return string(data), nil
}

// ---- edits -----------------------------------------------------------------

// editPlan is a remembered rename dry-run. Files is what to write; hashes is
// what the affected files looked like when the plan was computed.
type editPlan struct {
	files  []protocol.FileEdit
	hashes map[string]string
}

// tokenPayload is the decoded edit_token. It is opaque to clients but is
// deliberately not a secret: it carries no authority, only the hashes
// apply_edit re-checks against disk (SPEC §4.2).
type tokenPayload struct {
	Workspace protocol.WorkspaceID `json:"ws"`
	Files     map[string]string    `json:"files"`
}

// mintEditToken encodes the affected files and their content hashes.
func mintEditToken(ws protocol.WorkspaceID, hashes map[string]string) string {
	raw, err := json.Marshal(tokenPayload{Workspace: ws, Files: hashes})
	if err != nil {
		// tokenPayload is plain data; marshalling it cannot fail.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func parseEditToken(token string) (tokenPayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return tokenPayload{}, protocol.NewError(protocol.ErrInternal,
			"edit_token is not a langer edit token")
	}
	var payload tokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return tokenPayload{}, protocol.NewError(protocol.ErrInternal,
			"edit_token is not a langer edit token")
	}
	if len(payload.Files) == 0 {
		return tokenPayload{}, protocol.NewError(protocol.ErrInternal, "edit_token names no files")
	}
	return payload, nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hashFile returns the SHA-256 of a workspace file, or ok=false when it cannot
// be read — which apply_edit treats exactly like a mismatch.
func (w *Workspace) hashFile(rel string) (string, bool) {
	if err := w.validateWorkspaceFile(rel); err != nil {
		return "", false
	}
	info, err := os.Lstat(w.absPath(rel))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(w.absPath(rel))
	if err != nil {
		return "", false
	}
	return hashBytes(data), true
}

// Rename computes a rename dry-run. Nothing is written: the caller gets the
// edits plus an edit_token that apply_edit will verify against disk
// (SPEC §4.2 — dry-run is the default for any mutating operation).
func (w *Workspace) Rename(ctx context.Context, _ protocol.SessionID, path string, pos protocol.Position, newName string) (protocol.EditPlanResult, error) {
	srv, rel, _, err := w.prepare(ctx, path)
	if err != nil {
		return protocol.EditPlanResult{}, err
	}
	if !srv.Supports(lsp.CapRename) {
		return protocol.EditPlanResult{}, protocol.NewErrorf(protocol.ErrUnsupported,
			"this language server does not provide rename_symbol")
	}

	files, err := srv.Rename(ctx, rel, pos, newName)
	if err != nil {
		return protocol.EditPlanResult{}, protocol.AsError(err)
	}
	if len(files) == 0 {
		return protocol.EditPlanResult{}, protocol.NewErrorf(protocol.ErrNoResult,
			"nothing to rename at %s:%d:%d", rel, pos.Line, pos.Character)
	}

	hashes := make(map[string]string, len(files))
	for _, f := range files {
		hash, ok := w.hashFile(f.Path)
		if !ok {
			return protocol.EditPlanResult{}, protocol.NewErrorf(protocol.ErrStaleEdit,
				"%s is named by the rename but cannot be read", f.Path)
		}
		hashes[f.Path] = hash
	}

	token := mintEditToken(w.id, hashes)
	w.rememberPlan(token, editPlan{files: files, hashes: hashes})

	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return protocol.EditPlanResult{EditToken: token, Files: files}, nil
}

// rememberPlan stores a dry-run plan, evicting the oldest when the bound is
// reached.
func (w *Workspace) rememberPlan(token string, plan editPlan) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.plans[token]; !exists {
		w.order = append(w.order, token)
	}
	w.plans[token] = plan
	for len(w.order) > maxRememberedPlans {
		delete(w.plans, w.order[0])
		w.order = w.order[1:]
	}
}

func (w *Workspace) planFor(token string) (editPlan, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	plan, ok := w.plans[token]
	return plan, ok
}

// ApplyEdit writes a previously computed plan, refusing if anything changed
// since the dry-run (SPEC §4.2 STALE_EDIT).
func (w *Workspace) ApplyEdit(ctx context.Context, _ protocol.SessionID, token string) ([]string, error) {
	payload, err := parseEditToken(token)
	if err != nil {
		return nil, err
	}
	if payload.Workspace != w.id {
		return nil, protocol.NewErrorf(protocol.ErrStaleEdit,
			"edit_token was issued for workspace %s, not %s", payload.Workspace, w.id)
	}

	plan, ok := w.planFor(token)
	if !ok {
		// A well-formed token this daemon did not mint (or has forgotten) is
		// not something we can verify. SPEC §4.2 says apply verifies hashes and
		// rejects when it cannot prove the files are unchanged; refusing is the
		// only safe answer, and re-running the dry-run is cheap.
		return nil, protocol.NewError(protocol.ErrStaleEdit,
			"this edit plan is no longer available; re-run the dry-run")
	}

	for rel, want := range payload.Files {
		got, ok := w.hashFile(rel)
		if !ok {
			return nil, protocol.NewErrorf(protocol.ErrStaleEdit, "%s no longer exists", rel)
		}
		if got != want {
			return nil, protocol.NewErrorf(protocol.ErrStaleEdit, "%s changed since the dry-run", rel)
		}
	}

	applied := make([]string, 0, len(plan.files))
	for _, file := range plan.files {
		if err := w.applyFileEdits(ctx, file); err != nil {
			return nil, err
		}
		applied = append(applied, file.Path)
	}
	sort.Strings(applied)
	return applied, nil
}

// applyFileEdits rewrites one file. Edits arrive in the language server's order
// and are applied back-to-front so an earlier edit cannot shift a later one's
// offsets.
func (w *Workspace) applyFileEdits(ctx context.Context, file protocol.FileEdit) error {
	if err := w.validateWorkspaceFile(file.Path); err != nil {
		return protocol.NewErrorf(protocol.ErrStaleEdit,
			"%s is no longer a safe workspace file", file.Path)
	}
	abs := w.absPath(file.Path)
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return protocol.NewErrorf(protocol.ErrStaleEdit, "%s no longer exists", file.Path)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return protocol.NewErrorf(protocol.ErrStaleEdit, "reading %s: %v", file.Path, err)
	}

	content := string(data)
	starts := lineStarts(content)

	type resolved struct {
		start, end int
		text       string
	}
	spans := make([]resolved, 0, len(file.Edits))
	for _, edit := range file.Edits {
		start, err := byteOffset(content, starts, edit.Range.Start)
		if err != nil {
			return err
		}
		end, err := byteOffset(content, starts, edit.Range.End)
		if err != nil {
			return err
		}
		if end < start {
			return protocol.NewErrorf(protocol.ErrInternal, "%s: edit end precedes its start", file.Path)
		}
		spans = append(spans, resolved{start: start, end: end, text: edit.NewText})
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start > spans[j].start })

	prevStart := len(content) + 1
	for _, span := range spans {
		if span.end > prevStart {
			return protocol.NewErrorf(protocol.ErrInternal, "%s: overlapping edits in one plan", file.Path)
		}
		content = content[:span.start] + span.text + content[span.end:]
		prevStart = span.start
	}

	if err := os.WriteFile(abs, []byte(content), info.Mode().Perm()); err != nil {
		return protocol.NewErrorf(protocol.ErrInternal, "writing %s: %v", file.Path, err)
	}

	// The language server is now holding the pre-edit text. Forget our record
	// so the next query notices and reopens (ensureDocumentLocked).
	cleanupCtx := context.WithoutCancel(ctx)
	if w.indexCtx != nil {
		cleanupCtx = w.indexCtx
	}
	unlock, err := w.lockDoc(cleanupCtx, file.Path)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if st, ok := w.docs[file.Path]; ok {
		st.text = ""
	}
	w.mu.Unlock()
	unlock()
	if err := w.invalidateAndQueue(ctx, file.Path, false); err != nil {
		return err
	}
	return nil
}

// SimulateEdit reports the diagnostics a file WOULD produce with different
// text, writing nothing to disk (SPEC §4.2).
//
// Isolation between callers is achieved by serialisation, not by parallel
// state: lsp.Server.WithText holds the per-(server, path) lock for the whole
// call and restores the base text before releasing it, so a concurrent
// get_diagnostics for the same file can never observe another session's
// speculative text (docs/ARCHITECTURE.md §6.6).
func (w *Workspace) SimulateEdit(ctx context.Context, _ protocol.SessionID, path, newText string) ([]protocol.Diagnostic, bool, error) {
	srv, rel, _, err := w.prepare(ctx, path)
	if err != nil {
		return nil, false, err
	}

	var (
		diags []protocol.Diagnostic
		stale bool
	)
	err = srv.WithText(ctx, rel, newText, func(ctx context.Context, epoch uint64) error {
		var innerErr error
		diags, stale, innerErr = srv.Diagnostics(ctx, rel, epoch)
		return innerErr
	})
	// WithText restores the base text on the way out, but the diagnostics that
	// restore produces are still in flight: the server's cache still holds the
	// SPECULATIVE results, at an epoch a later query would accept as settled.
	// A get_diagnostics arriving now would therefore report an error in a file
	// that compiles cleanly on disk.
	//
	// Forgetting our record of the document is what closes that window: the next
	// query sees a mismatch, reopens, and settles against a fresh epoch. It is
	// deliberately done even when fn failed, because the speculative push
	// happened either way.
	w.forgetDocument(ctx, rel)

	if err != nil {
		return nil, false, protocol.AsError(err)
	}
	return nonNilDiagnostics(diags), stale, nil
}

// forgetDocument drops our record of what the server holds for rel, so the next
// query resynchronises it from disk.
func (w *Workspace) forgetDocument(ctx context.Context, rel string) {
	cleanupCtx := context.WithoutCancel(ctx)
	if w.indexCtx != nil {
		cleanupCtx = w.indexCtx
	}
	unlock, err := w.lockDoc(cleanupCtx, rel)
	if err != nil {
		w.log.Debug("forgetting document after speculative edit failed", "path", rel, "error", err)
		return
	}
	defer unlock()

	w.mu.Lock()
	defer w.mu.Unlock()
	if st, ok := w.docs[rel]; ok {
		st.text = ""
	}
}

// ---- text offsets ----------------------------------------------------------

// lineStarts records the byte offset at which each line begins.
func lineStarts(content string) []int {
	starts := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// byteOffset converts a 0-based, UTF-16 protocol.Position into a byte offset.
// The UTF-16 half of the conversion is lsp/wire's, which is the single
// converter in the tree (SPEC §4.3).
func byteOffset(content string, starts []int, pos protocol.Position) (int, error) {
	if pos.Line < 0 || pos.Character < 0 {
		return 0, protocol.NewErrorf(protocol.ErrInternal, "negative position %d:%d", pos.Line, pos.Character)
	}
	if pos.Line >= len(starts) {
		// A position one past the last line means "end of file", which is a
		// legitimate end for a whole-file edit.
		return len(content), nil
	}
	start := starts[pos.Line]
	end := len(content)
	if pos.Line+1 < len(starts) {
		end = starts[pos.Line+1] - 1 // exclude the newline itself
	}
	line := content[start:end]
	return start + wire.ByteOffset(line, pos.Character), nil
}
