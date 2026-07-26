package lsp

import (
	"context"
	"sync"
	"sync/atomic"

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

// documentMutationGate is a context-aware writer-preferring RW gate.
//
// Ordinary document mutations take a read slot and can still run concurrently
// for different paths. Restart resync takes the write slot from its candidate
// snapshot through session publication. The shared lock order is therefore:
//
//	documentMutationGate -> per-path document.lock -> documents.mu
//
// Keeping the global gate above the path leaves is what prevents Close from
// overtaking a replayed path without introducing a resync/path-lock deadlock.
type documentMutationGate struct {
	mu             sync.Mutex
	readers        int
	waitingReaders int
	writer         bool
	waitingWriters int
	changed        chan struct{}
}

func (g *documentMutationGate) readLock(ctx context.Context) error {
	waiting := false
	for {
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			if waiting {
				g.waitingReaders--
			}
			g.mu.Unlock()
			return ctxError(ctx, "waiting for language-server document resynchronization")
		}
		if !g.writer && g.waitingWriters == 0 {
			if waiting {
				g.waitingReaders--
			}
			g.readers++
			g.mu.Unlock()
			return nil
		}
		if !waiting {
			g.waitingReaders++
			waiting = true
		}
		changed := g.changedLocked()
		g.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			g.mu.Lock()
			if waiting {
				g.waitingReaders--
			}
			g.mu.Unlock()
			return ctxError(ctx, "waiting for language-server document resynchronization")
		}
	}
}

func (g *documentMutationGate) readUnlock() {
	g.mu.Lock()
	if g.readers > 0 {
		g.readers--
	}
	if g.readers == 0 {
		g.signalLocked()
	}
	g.mu.Unlock()
}

func (g *documentMutationGate) writeLock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return ctxError(ctx, "waiting to resynchronize language-server documents")
	}

	g.mu.Lock()
	g.waitingWriters++
	for {
		if err := ctx.Err(); err != nil {
			g.waitingWriters--
			g.signalLocked()
			g.mu.Unlock()
			return ctxError(ctx, "waiting to resynchronize language-server documents")
		}
		if !g.writer && g.readers == 0 {
			g.waitingWriters--
			g.writer = true
			g.mu.Unlock()
			return nil
		}
		changed := g.changedLocked()
		g.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			g.mu.Lock()
			g.waitingWriters--
			g.signalLocked()
			g.mu.Unlock()
			return ctxError(ctx, "waiting to resynchronize language-server documents")
		}
		g.mu.Lock()
	}
}

func (g *documentMutationGate) writeUnlock() {
	g.mu.Lock()
	g.writer = false
	g.signalLocked()
	g.mu.Unlock()
}

func (g *documentMutationGate) changedLocked() <-chan struct{} {
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.changed
}

func (g *documentMutationGate) signalLocked() {
	if g.changed == nil {
		return
	}
	close(g.changed)
	g.changed = make(chan struct{})
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

// restoreClosed returns a document that WithDiskText opened implicitly to its
// exact prior closed state. The version is deliberately not rewound: versions
// stay monotonic for the life of the process.
func (d *documents) restoreClosed(path, languageID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if doc := d.docs[path]; doc != nil {
		doc.open = false
		doc.text = ""
		doc.languageID = languageID
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

type docLockLease struct {
	active atomic.Bool
}

func withDocLock(ctx context.Context, path string) (context.Context, func()) {
	held, _ := ctx.Value(docLockKey{}).(map[string]*docLockLease)
	next := make(map[string]*docLockLease, len(held)+1)
	for k, v := range held {
		next[k] = v
	}
	lease := &docLockLease{}
	lease.active.Store(true)
	next[path] = lease
	release := func() {
		lease.active.Store(false)
	}
	return context.WithValue(ctx, docLockKey{}, next), release
}

func holdsDocLock(ctx context.Context, path string) bool {
	held, _ := ctx.Value(docLockKey{}).(map[string]*docLockLease)
	lease := held[path]
	return lease != nil && lease.active.Load()
}

// documentMutationGateKey makes a read slot re-entrant for a callback that
// invokes another document operation through the context it was given. Without
// this marker, a writer arriving between the outer and inner operations would
// wait for the outer reader while writer preference made the inner reader wait
// for that writer.
type documentMutationGateKey struct{}

type documentMutationLease struct {
	active atomic.Bool
}

func withDocumentMutationGate(
	ctx context.Context,
	srv *server,
	lease *documentMutationLease,
) context.Context {
	held, _ := ctx.Value(documentMutationGateKey{}).(map[*server]*documentMutationLease)
	next := make(map[*server]*documentMutationLease, len(held)+1)
	for candidate, value := range held {
		next[candidate] = value
	}
	next[srv] = lease
	return context.WithValue(ctx, documentMutationGateKey{}, next)
}

func holdsDocumentMutationGate(ctx context.Context, srv *server) bool {
	held, _ := ctx.Value(documentMutationGateKey{}).(map[*server]*documentMutationLease)
	lease := held[srv]
	return lease != nil && lease.active.Load()
}
