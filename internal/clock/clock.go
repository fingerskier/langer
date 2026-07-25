// Package clock is the only source of time in langer.
//
// time.Now, time.After, time.Sleep and time.NewTimer are forbidden outside this
// package. Every deadline the spec names is behaviour that must be *asserted*,
// not slept through: the 30-minute sunset (SPEC §3.1), the 5-minute overlay TTL
// (SPEC §4.2), the ≤2 s diagnostics settle window (SPEC §4.3) and the
// exponential restart backoff (SPEC §3.3).
//
// Production code takes a Clock; tests hand it a Fake and advance it.
package clock

import (
	"context"
	"time"
)

// Clock supplies the current time and timers.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
	// Sleep blocks for d or until ctx is done, returning ctx.Err() if the
	// latter wins.
	Sleep(ctx context.Context, d time.Duration) error
	// After is shorthand for NewTimer(d).C(); the timer is reclaimed when it
	// fires. Do not use it in a loop.
	After(d time.Duration) <-chan time.Time
}

// Timer mirrors *time.Timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker mirrors *time.Ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// New returns a Clock backed by the real monotonic clock.
func New() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer         { return &realTimer{t: time.NewTimer(d)} }
func (realClock) NewTicker(d time.Duration) Ticker       { return &realTicker{t: time.NewTicker(d)} }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time        { return r.t.C }
func (r *realTimer) Stop() bool                 { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }
