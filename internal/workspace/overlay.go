package workspace

import (
	"sync"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

// defaultOverlayTTL is SPEC §4.2's short-lived speculative overlay window.
const defaultOverlayTTL = 5 * time.Minute

// overlaySweepPeriod is how often the single clock-driven sweeper reaps expired
// overlays. It is deliberately coarser than the TTL: expiry is also enforced on
// every read, so the ticker only bounds how long dead entries occupy memory.
const overlaySweepPeriod = 30 * time.Second

// overlayEntry is one session's speculative text for one path.
//
// Overlays never touch disk and never enter the index (SPEC §4.2). Isolation
// between sessions is achieved by serialising language-server use through
// lsp.Server.WithText (docs/ARCHITECTURE.md §6.6); this store only remembers
// the text and the disk baseline it was computed against.
type overlayEntry struct {
	text     string
	diskHash string
	lastUsed time.Time
	// stale is set by InvalidatePath when the real file changes. The entry is
	// kept (not freed) so the next use returns STALE_EDIT rather than racing a
	// concurrent simulate_edit's restore (docs/ARCHITECTURE.md §5.7).
	stale bool
}

// overlays holds per-session speculative text for one workspace.
type overlays struct {
	mu    sync.Mutex
	ttl   time.Duration
	clock clock.Clock
	// bySession[session][path] → entry
	bySession map[protocol.SessionID]map[string]*overlayEntry
}

func newOverlays(ck clock.Clock, ttl time.Duration) *overlays {
	if ck == nil {
		ck = clock.New()
	}
	if ttl <= 0 {
		ttl = defaultOverlayTTL
	}
	return &overlays{
		ttl:       ttl,
		clock:     ck,
		bySession: map[protocol.SessionID]map[string]*overlayEntry{},
	}
}

// put records speculative text for session+path against the given disk hash.
// A fresh put replaces any prior entry, including a stale one: the agent is
// establishing a new speculative view against the current baseline.
func (o *overlays) put(session protocol.SessionID, path, text, diskHash string) {
	if session == "" || path == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	byPath := o.bySession[session]
	if byPath == nil {
		byPath = map[string]*overlayEntry{}
		o.bySession[session] = byPath
	}
	byPath[path] = &overlayEntry{
		text:     text,
		diskHash: diskHash,
		lastUsed: o.clock.Now(),
	}
}

// live returns the overlay text when it is still usable. A stale entry yields
// STALE_EDIT and is dropped so a subsequent put can establish a new view.
// Expired entries are dropped silently (they no longer exist). A missing
// overlay returns ok=false with a nil error.
func (o *overlays) live(session protocol.SessionID, path string) (text, diskHash string, ok bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	byPath := o.bySession[session]
	if byPath == nil {
		return "", "", false, nil
	}
	entry := byPath[path]
	if entry == nil {
		return "", "", false, nil
	}
	if entry.stale {
		delete(byPath, path)
		o.pruneSessionLocked(session)
		return "", "", false, protocol.NewErrorf(protocol.ErrStaleEdit,
			"speculative overlay for %s was invalidated by a disk change", path)
	}
	if o.expiredLocked(entry) {
		delete(byPath, path)
		o.pruneSessionLocked(session)
		return "", "", false, nil
	}
	entry.lastUsed = o.clock.Now()
	return entry.text, entry.diskHash, true, nil
}

// invalidatePath marks every overlay of path stale without freeing it, so a
// watcher event cannot race a concurrent restore into a silent miss.
func (o *overlays) invalidatePath(path string) {
	if path == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, byPath := range o.bySession {
		if entry, ok := byPath[path]; ok {
			entry.stale = true
		}
	}
}

// dropSession frees every overlay owned by session (SPEC §4.2: dropped when
// the session disconnects).
func (o *overlays) dropSession(session protocol.SessionID) {
	if session == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.bySession, session)
}

// sweep removes TTL-expired entries. Stale entries are left until live() or
// put() handles them — they still need to produce STALE_EDIT on next use.
func (o *overlays) sweep() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for session, byPath := range o.bySession {
		for path, entry := range byPath {
			if !entry.stale && o.expiredLocked(entry) {
				delete(byPath, path)
			}
		}
		o.pruneSessionLocked(session)
	}
}

func (o *overlays) expiredLocked(entry *overlayEntry) bool {
	return !entry.lastUsed.Add(o.ttl).After(o.clock.Now())
}

func (o *overlays) pruneSessionLocked(session protocol.SessionID) {
	if byPath := o.bySession[session]; byPath != nil && len(byPath) == 0 {
		delete(o.bySession, session)
	}
}

// count returns how many live (non-stale, non-expired) overlays exist. Tests
// use it; production code does not.
func (o *overlays) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, byPath := range o.bySession {
		for _, entry := range byPath {
			if !entry.stale && !o.expiredLocked(entry) {
				n++
			}
		}
	}
	return n
}
