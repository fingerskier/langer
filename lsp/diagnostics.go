package lsp

import (
	"context"
	"sync"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

// diagnostics is the SPEC §4.3 settle machinery.
//
// Fan-in comes from the connection's reader goroutine; fan-out goes to N
// waiters through a broadcast channel that is CLOSED AND REPLACED on every
// push. Deliberately not a sync.Cond: a Cond cannot compose with a context
// deadline, and it strands its waiters at shutdown, which hangs the test binary
// rather than failing it.
type diagnostics struct {
	ck clock.Clock

	mu      sync.Mutex
	epoch   uint64
	entries map[string]*diagEntry
	wake    chan struct{}
}

type diagEntry struct {
	epoch uint64
	at    time.Time
	diags []protocol.Diagnostic
}

func newDiagnostics(ck clock.Clock) *diagnostics {
	return &diagnostics{
		ck:      ck,
		entries: map[string]*diagEntry{},
		wake:    make(chan struct{}),
	}
}

// mark returns the lowest epoch a future push can carry.
//
// Callers take a mark BEFORE the didOpen or didChange that will cause the push,
// so "wait for an epoch ≥ this mark" cannot be satisfied by a stale push that
// landed a moment earlier.
func (d *diagnostics) mark() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.epoch + 1
}

// current returns the epoch already recorded for path, or a fresh mark when the
// server has never published for it.
func (d *diagnostics) current(path string) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.entries[path]; ok {
		return e.epoch
	}
	return d.epoch + 1
}

// publish records a server push and wakes every waiter.
func (d *diagnostics) publish(path string, diags []protocol.Diagnostic) {
	d.mu.Lock()
	d.epoch++
	d.entries[path] = &diagEntry{epoch: d.epoch, at: d.ck.Now(), diags: diags}
	wake := d.wake
	d.wake = make(chan struct{})
	d.mu.Unlock()

	close(wake)
}

// reset drops every cached diagnostic and wakes waiters. Called when a server
// restarts: its previous analysis died with it, and reporting those results as
// current would be a lie.
func (d *diagnostics) reset() {
	d.mu.Lock()
	d.entries = map[string]*diagEntry{}
	wake := d.wake
	d.wake = make(chan struct{})
	d.mu.Unlock()

	close(wake)
}

// snapshot returns what is currently known for path.
func (d *diagnostics) snapshot(path string) ([]protocol.Diagnostic, uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[path]
	if !ok {
		return nil, 0, false
	}
	return e.diags, e.epoch, true
}

// settle implements SPEC §4.3: wait for a push whose epoch is ≥ since AND a
// quiet period with no further push, bounded by budget.
//
// The quiet period is mandatory. Both tsserver and pyright publish an EMPTY
// diagnostics array before the real results, so "first push wins" reports a
// clean file that does not compile.
//
// On budget expiry it returns the latest known diagnostics with
// possiblyStale=true — that is a successful answer, not an error.
//
// dead is the current connection's done channel. A crash is not a push, not the
// budget, and not the caller's context, so without this the waiter is stranded
// until its own deadline expires (SPEC §8: a crash must not take a caller down
// with it). A nil dead channel never fires, which is what unit tests that drive
// settle directly want.
func (d *diagnostics) settle(ctx context.Context, path string, since uint64, quiet, budget time.Duration, dead <-chan struct{}) ([]protocol.Diagnostic, bool, error) {
	budgetTimer := d.ck.NewTimer(budget)
	defer budgetTimer.Stop()

	for {
		// The entry and the current time are read together, under one lock.
		// Sampling them separately lets a push land in between and makes the
		// quiet period appear to have elapsed when it has not — which returns
		// the EMPTY first push as a settled answer.
		d.mu.Lock()
		entry := d.entries[path]
		wake := d.wake
		now := d.ck.Now()
		d.mu.Unlock()

		if entry != nil && entry.epoch >= since {
			elapsed := now.Sub(entry.at)
			if elapsed >= quiet {
				return entry.diags, false, nil
			}

			quietTimer := d.ck.NewTimer(quiet - elapsed)
			select {
			case <-wake:
				// A newer push landed: the quiet period restarts.
			case <-quietTimer.C():
				// Nothing arrived; the next iteration sees elapsed ≥ quiet and
				// returns the settled result.
			case <-budgetTimer.C():
				quietTimer.Stop()
				return entry.diags, true, nil
			case <-dead:
				quietTimer.Stop()
				return nil, false, crashedError(nil)
			case <-ctx.Done():
				quietTimer.Stop()
				return nil, false, ctxError(ctx, "waiting for diagnostics to settle")
			}
			quietTimer.Stop()
			continue
		}

		var latest []protocol.Diagnostic
		if entry != nil {
			latest = entry.diags
		}
		select {
		case <-wake:
		case <-budgetTimer.C():
			return latest, true, nil
		case <-dead:
			return nil, false, crashedError(nil)
		case <-ctx.Done():
			return nil, false, ctxError(ctx, "waiting for diagnostics to settle")
		}
	}
}
