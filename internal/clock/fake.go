package clock

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Fake is a deterministic Clock. Time moves only when Advance is called.
//
// Advance delivers every timer whose deadline has passed, in deadline order,
// BEFORE returning — so a test that advances 30 minutes can immediately assert
// that sunset fired, with no sleeps and no flakes.
type Fake struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters int
	timers  []*fakeTimer
}

// NewFake returns a Fake reading start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now reports the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d, firing every timer that becomes due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now

	due := make([]*fakeTimer, 0, len(f.timers))
	for _, t := range f.timers {
		if !t.stopped && !t.deadline.After(now) {
			due = append(due, t)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].deadline.Before(due[j].deadline) })

	for _, t := range due {
		t.fire(now)
	}
	f.reapLocked()
	f.cond.Broadcast()
	f.mu.Unlock()
}

// BlockUntil blocks until n goroutines are parked on a timer, ticker or Sleep.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.waiters < n {
		f.cond.Wait()
	}
}

// NewTimer returns a Timer that fires d of fake time from now.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newTimerLocked(d, 0)
}

// NewTicker returns a Ticker that fires every d of fake time.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeTicker{t: f.newTimerLocked(d, d)}
}

// fakeTicker adapts a periodic fakeTimer to the Ticker interface, whose Stop
// returns nothing.
type fakeTicker struct{ t *fakeTimer }

func (k fakeTicker) C() <-chan time.Time { return k.t.C() }
func (k fakeTicker) Stop()               { k.t.Stop() }

// After returns a channel that receives d of fake time from now.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.NewTimer(d).C()
}

// Sleep blocks for d of fake time or until ctx is done.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := f.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fake) newTimerLocked(d, period time.Duration) *fakeTimer {
	t := &fakeTimer{
		f:        f,
		ch:       make(chan time.Time, 1),
		deadline: f.now.Add(d),
		period:   period,
	}
	f.timers = append(f.timers, t)
	f.waiters++
	f.cond.Broadcast()
	return t
}

// reapLocked drops fired one-shot timers from the waiter accounting.
func (f *Fake) reapLocked() {
	kept := f.timers[:0]
	for _, t := range f.timers {
		if t.stopped || (t.fired && t.period == 0) {
			continue
		}
		kept = append(kept, t)
	}
	for i := len(kept); i < len(f.timers); i++ {
		f.timers[i] = nil
	}
	f.timers = kept
	f.waiters = len(f.timers)
}

type fakeTimer struct {
	f        *Fake
	ch       chan time.Time
	deadline time.Time
	period   time.Duration
	stopped  bool
	fired    bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

// fire is called with f.mu held.
func (t *fakeTimer) fire(now time.Time) {
	t.fired = true
	select {
	case t.ch <- t.deadline:
	default:
		// A pending tick is still unread; drop this one exactly as
		// time.Ticker does.
	}
	if t.period > 0 {
		next := t.deadline.Add(t.period)
		for !next.After(now) {
			next = next.Add(t.period)
		}
		t.deadline = next
		t.fired = false
	}
}

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	t.f.reapLocked()
	t.f.cond.Broadcast()
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = false
	t.fired = false
	t.deadline = t.f.now.Add(d)
	if !t.inListLocked() {
		t.f.timers = append(t.f.timers, t)
	}
	t.f.waiters = len(t.f.timers)
	t.f.cond.Broadcast()
	return was
}

func (t *fakeTimer) inListLocked() bool {
	for _, other := range t.f.timers {
		if other == t {
			return true
		}
	}
	return false
}

var _ Clock = (*Fake)(nil)
