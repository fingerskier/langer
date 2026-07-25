package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fingerskier/langer/internal/clock"
)

// stopReason is why the daemon stopped serving.
type stopReason string

const (
	// reasonIdle is the SPEC §3.1 sunset: no clients and no activity.
	reasonIdle stopReason = "idle"
	// reasonDrain is a client asking the daemon to stand down, which is what a
	// version mismatch triggers (SPEC §3.1).
	reasonDrain stopReason = "drain"
	// reasonContext is a signal or an explicit cancellation.
	reasonContext stopReason = "context"
)

// sunset is the daemon's single liveness serialisation point.
//
// Check-idle-then-close-the-listener always leaves a window where a client
// connects between the check and the close, gets a connection, and issues
// requests into a dying daemon. There is no correct lock-based fix for that
// shape. Here, "am I idle enough to stop?" and "may this client join?" are
// decided under ONE mutex, so a client arriving at the instant the timer fires
// either joins first (and the daemon does not stop) or is refused (and its
// client starts a fresh daemon). docs/ARCHITECTURE.md §6.7.
type sunset struct {
	ck   clock.Clock
	idle time.Duration
	log  *slog.Logger

	// poke is a buffered wake-up. Every state change sends into it without
	// blocking, so no caller can ever be parked waiting for the actor.
	poke     chan struct{}
	drainReq chan string

	mu           sync.Mutex
	clients      int
	inflight     int
	lastActivity time.Time
	draining     bool
	reason       stopReason
	// quiet is closed and replaced whenever inflight reaches zero. A closed
	// channel composes with a deadline; a sync.Cond does not.
	quiet chan struct{}
}

func newSunset(ck clock.Clock, idle time.Duration, log *slog.Logger) *sunset {
	return &sunset{
		ck:           ck,
		idle:         idle,
		log:          log,
		poke:         make(chan struct{}, 1),
		drainReq:     make(chan string, 1),
		lastActivity: ck.Now(),
		quiet:        make(chan struct{}),
	}
}

// wake nudges the actor without ever blocking the caller.
func (s *sunset) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// addClient registers a connected client. It returns false once the daemon is
// draining, in which case the caller must refuse the connection so the client
// starts a fresh daemon instead of issuing doomed requests.
func (s *sunset) addClient() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.clients++
	s.lastActivity = s.ck.Now()
	s.wake()
	return true
}

func (s *sunset) removeClient() {
	s.mu.Lock()
	if s.clients > 0 {
		s.clients--
	}
	s.lastActivity = s.ck.Now()
	s.mu.Unlock()
	s.wake()
}

// beginRequest marks a request in flight. The sunset timer cannot fire while
// one is outstanding.
func (s *sunset) beginRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.inflight++
	s.lastActivity = s.ck.Now()
	return true
}

func (s *sunset) endRequest() {
	s.mu.Lock()
	if s.inflight > 0 {
		s.inflight--
	}
	s.lastActivity = s.ck.Now()
	if s.inflight == 0 {
		close(s.quiet)
		s.quiet = make(chan struct{})
	}
	s.mu.Unlock()
	s.wake()
}

// noteActivity records file activity. SPEC §3.1 defines idle as no connected
// clients AND no file activity; M3's watcher calls this.
func (s *sunset) noteActivity() {
	s.mu.Lock()
	s.lastActivity = s.ck.Now()
	s.mu.Unlock()
	s.wake()
}

// requestDrain asks the daemon to stand down. It returns false if it is already
// draining, which is not a failure — the caller's goal is already met.
func (s *sunset) requestDrain(reason string) bool {
	s.mu.Lock()
	already := s.draining
	s.mu.Unlock()
	if already {
		return false
	}
	select {
	case s.drainReq <- reason:
		return true
	default:
		// A drain is already queued.
		return false
	}
}

func (s *sunset) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *sunset) counts() (clients, inflight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients, s.inflight
}

// run is the actor loop. It returns once the daemon should stop serving, with
// draining already set, so no new client can be admitted after it returns.
func (s *sunset) run(ctx context.Context) stopReason {
	timer := s.ck.NewTimer(s.idle)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			s.setDraining(reasonContext)
			return reasonContext

		case why := <-s.drainReq:
			s.log.Info("daemon draining", "reason", why)
			s.setDraining(reasonDrain)
			return reasonDrain

		case <-s.poke:
			timer.Reset(s.untilIdle())

		case <-timer.C():
			if s.tryIdleDrain() {
				return reasonIdle
			}
			timer.Reset(s.untilIdle())
		}
	}
}

// untilIdle is how long from now the daemon could next become idle. While a
// client is connected or a request is in flight the answer is "not before a
// full idle period from now", and the actor is woken again the moment that
// changes.
func (s *sunset) untilIdle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients > 0 || s.inflight > 0 {
		return s.idle
	}
	remaining := s.idle - s.ck.Now().Sub(s.lastActivity)
	if remaining <= 0 {
		// Do not return 0 or a negative: a timer armed for "immediately" spins.
		return time.Nanosecond
	}
	return remaining
}

// tryIdleDrain stops the daemon only if it is genuinely idle. The check and the
// draining flag are set under one lock, which is what closes the race with a
// client connecting at the same instant.
func (s *sunset) tryIdleDrain() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return true
	}
	if s.clients > 0 || s.inflight > 0 {
		return false
	}
	if s.ck.Now().Sub(s.lastActivity) < s.idle {
		return false
	}
	s.draining = true
	s.reason = reasonIdle
	s.log.Info("daemon sunsetting", "idle_timeout", s.idle)
	return true
}

func (s *sunset) setDraining(reason stopReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.draining = true
		s.reason = reason
	}
}

// waitQuiet blocks until no request is in flight, or grace elapses.
//
// The timer here is deliberately real time, not the injected clock. This is the
// shutdown backstop of docs/ARCHITECTURE.md §6.5 step 2, and a fake clock that
// no test remembers to advance must not be able to wedge shutdown forever — the
// lesson M1 paid for. Nothing spec'd is being measured.
func (s *sunset) waitQuiet(grace time.Duration) {
	s.mu.Lock()
	if s.inflight == 0 {
		s.mu.Unlock()
		return
	}
	quiet := s.quiet
	s.mu.Unlock()

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-quiet:
	case <-timer.C:
	}
}
