package lsp

import (
	"context"
	"sync"

	"github.com/fingerskier/langer/protocol"
)

// documents is the docstate leaf: it owns every open document's text and the
// monotonic version counter the language server correlates changes by.
//
// Two locks, on purpose:
//
//   - documents.mu guards the map and each document's fields. It is held only
//     for field reads and writes, never across an LSP round trip.
//   - document.lock is the per-(server, path) MUTUAL EXCLUSION that
//     docs/ARCHITECTURE.md §6.6 requires. A language server holds exactly one
//     document state per URI, so speculative-edit isolation is achieved by
//     serialising everything that touches a path. It is a channel rather than a
//     sync.Mutex so a waiter can honour its context instead of parking forever.
type documents struct {
	mu   sync.Mutex
	docs map[string]*document
}

type document struct {
	path string
	lock chan struct{}

	// Guarded by documents.mu.
	languageID string
	text       string
	version    int
	open       bool
}

func newDocuments() *documents {
	return &documents{docs: map[string]*document{}}
}

// get returns the document record for path, creating it if needed.
func (d *documents) get(path string) *document {
	d.mu.Lock()
	defer d.mu.Unlock()
	doc, ok := d.docs[path]
	if !ok {
		doc = &document{path: path, lock: make(chan struct{}, 1)}
		d.docs[path] = doc
	}
	return doc
}

// acquire takes the per-(server, path) lock, or gives up when ctx does.
func (d *documents) acquire(ctx context.Context, path string) (*document, error) {
	doc := d.get(path)
	select {
	case doc.lock <- struct{}{}:
		return doc, nil
	case <-ctx.Done():
		return nil, protocol.NewErrorf(protocol.ErrNotReady,
			"timed out waiting for exclusive access to %s", path).WithRetryAfterMS(200)
	}
}

func (d *documents) release(doc *document) {
	select {
	case <-doc.lock:
	default:
	}
}

// state returns a snapshot of a document's fields.
func (d *documents) state(path string) (text string, version int, languageID string, open bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	doc, ok := d.docs[path]
	if !ok {
		return "", 0, "", false
	}
	return doc.text, doc.version, doc.languageID, doc.open
}

// markOpen records a didOpen and returns the version to send.
func (d *documents) markOpen(path, languageID, text string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	doc := d.docs[path]
	if doc == nil {
		doc = &document{path: path, lock: make(chan struct{}, 1)}
		d.docs[path] = doc
	}
	doc.version++
	doc.languageID = languageID
	doc.text = text
	doc.open = true
	return doc.version
}

// markChanged records a didChange and returns the version to send. Versions are
// strictly increasing for the life of the process: a server that sees a version
// go backwards may silently keep its old view of the file.
func (d *documents) markChanged(path, text string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	doc := d.docs[path]
	if doc == nil {
		doc = &document{path: path, lock: make(chan struct{}, 1)}
		d.docs[path] = doc
	}
	doc.version++
	doc.text = text
	return doc.version
}

// markClosed records a didClose.
func (d *documents) markClosed(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if doc := d.docs[path]; doc != nil {
		doc.open = false
		doc.text = ""
	}
}

// openSnapshot lists the documents that must be re-sent after a restart. The
// language server's memory of them died with it; ours did not.
func (d *documents) openSnapshot() []documentSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]documentSnapshot, 0, len(d.docs))
	for _, doc := range d.docs {
		if doc.open {
			out = append(out, documentSnapshot{Path: doc.path, LanguageID: doc.languageID, Text: doc.text})
		}
	}
	return out
}

type documentSnapshot struct {
	Path       string
	LanguageID string
	Text       string
}

// docLockKey marks a context as already holding a path's document lock.
//
// SPEC §4.2's overlay isolation makes get_diagnostics take the same lock as
// simulate_edit (docs §6.6), but simulate_edit's own callback runs INSIDE that
// lock and asks for diagnostics. Without this marker the two requirements
// deadlock against each other.
type docLockKey struct{}

func withDocLock(ctx context.Context, path string) context.Context {
	held, _ := ctx.Value(docLockKey{}).(map[string]bool)
	next := make(map[string]bool, len(held)+1)
	for k, v := range held {
		next[k] = v
	}
	next[path] = true
	return context.WithValue(ctx, docLockKey{}, next)
}

func holdsDocLock(ctx context.Context, path string) bool {
	held, _ := ctx.Value(docLockKey{}).(map[string]bool)
	return held[path]
}
