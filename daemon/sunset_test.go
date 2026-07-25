package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

const testIdle = 30 * time.Minute

// startIdleDaemon runs a daemon on a fake clock so the SPEC §3.1 sunset can be
// asserted instead of slept through.
func startIdleDaemon(t *testing.T) (*harness, *clock.Fake) {
	t.Helper()
	fake := clock.NewFake(time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))
	h := startDaemon(t, func(o *Options) {
		o.Clock = fake
		o.IdleTimeout = testIdle
	}, nil)
	return h, fake
}

// advanceUntilExit steps the fake clock forward until the daemon stops, or
// gives up.
//
// Stepping rather than jumping once matters: the actor re-arms its timer after
// every wake-up, and a single huge jump can land while the timer is unarmed —
// after which the newly armed deadline sits beyond every time the test intends
// to cross. Real time never stops, so this hazard belongs to the fake.
func advanceUntilExit(t *testing.T, h *harness, fake *clock.Fake, step time.Duration, rounds int) (error, bool) {
	t.Helper()
	for i := 0; i < rounds; i++ {
		if err, ok := h.waitExit(20 * time.Millisecond); ok {
			return err, true
		}
		fake.Advance(step)
	}
	return h.waitExit(2 * time.Second)
}

// TestDaemonSunsetsAfterTheIdleTimeout is PLAN M2's "daemon exits after the
// idle timeout", driven by the fake clock rather than a real sleep.
func TestDaemonSunsetsAfterTheIdleTimeout(t *testing.T) {
	h, fake := startIdleDaemon(t)

	// Nothing has connected: the daemon should stand itself down.
	err, exited := advanceUntilExit(t, h, fake, testIdle, 20)
	if !exited {
		t.Fatal("the daemon did not sunset after the idle timeout")
	}
	if err != nil {
		t.Errorf("Run returned %v after sunsetting; a clean sunset is not a failure", err)
	}
}

// TestSunsetDoesNotFireWhileAClientIsConnected: SPEC §3.1 defines idle as no
// connected clients AND no file activity.
func TestSunsetDoesNotFireWhileAClientIsConnected(t *testing.T) {
	h, fake := startIdleDaemon(t)

	client, ws := h.session("alice")

	for i := 0; i < 10; i++ {
		fake.Advance(testIdle)
		time.Sleep(2 * time.Millisecond)
	}
	if _, exited := h.waitExit(50 * time.Millisecond); exited {
		t.Fatal("the daemon sunset while a client was connected")
	}

	// It is still serving, not merely alive.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := client.IndexStatus(ctx, protocol.IndexStatusParams{Session: "alice", Workspace: ws}); err != nil {
		t.Fatalf("the daemon stopped answering: %v", err)
	}
	cancel()

	// Once the client leaves, the clock runs again.
	_ = client.Close()
	if _, exited := advanceUntilExit(t, h, fake, testIdle, 40); !exited {
		t.Fatal("the daemon did not sunset after its last client left")
	}
}

// ---- unit tests on the actor itself ---------------------------------------

func newTestSunset(t *testing.T, fake *clock.Fake) *sunset {
	t.Helper()
	return newSunset(fake, testIdle, testLogger(t))
}

// TestSunsetWaitsForInFlightRequests: the timer must not fire mid-request, or a
// client gets a truncated answer from a daemon that is already tearing down its
// language servers.
func TestSunsetWaitsForInFlightRequests(t *testing.T) {
	fake := clock.NewFake(time.Now())
	s := newTestSunset(t, fake)

	if !s.beginRequest() {
		t.Fatal("beginRequest was refused on a fresh daemon")
	}
	fake.Advance(2 * testIdle)
	if s.tryIdleDrain() {
		t.Fatal("the daemon decided to sunset with a request in flight")
	}

	s.endRequest()
	fake.Advance(2 * testIdle)
	if !s.tryIdleDrain() {
		t.Fatal("the daemon did not sunset once the last request finished")
	}
}

// TestDrainingRefusesNewClientsAndRequests closes the SPEC §3.1 window: a
// client that arrives as the daemon stands down must be told to start a fresh
// one, not admitted into a dying process.
func TestDrainingRefusesNewClientsAndRequests(t *testing.T) {
	fake := clock.NewFake(time.Now())
	s := newTestSunset(t, fake)

	if !s.addClient() {
		t.Fatal("addClient was refused on a fresh daemon")
	}
	s.removeClient()

	fake.Advance(2 * testIdle)
	if !s.tryIdleDrain() {
		t.Fatal("an idle daemon refused to sunset")
	}
	if s.addClient() {
		t.Error("a draining daemon admitted a new client")
	}
	if s.beginRequest() {
		t.Error("a draining daemon accepted a new request")
	}
}

// TestFileActivityPostponesSunset: SPEC §3.1 counts file activity, not just
// clients. M3's watcher is what will call this; the accounting exists now so it
// cannot be forgotten then.
func TestFileActivityPostponesSunset(t *testing.T) {
	fake := clock.NewFake(time.Now())
	s := newTestSunset(t, fake)

	fake.Advance(testIdle - time.Minute)
	s.noteActivity()

	fake.Advance(2 * time.Minute)
	if s.tryIdleDrain() {
		t.Fatal("file activity did not postpone the sunset")
	}

	fake.Advance(testIdle)
	if !s.tryIdleDrain() {
		t.Fatal("the daemon never sunset after activity stopped")
	}
}

// TestUntilIdleNeverReturnsZero: a timer armed for "immediately" spins the
// actor at 100% CPU.
func TestUntilIdleNeverReturnsZero(t *testing.T) {
	fake := clock.NewFake(time.Now())
	s := newTestSunset(t, fake)

	fake.Advance(10 * testIdle)
	if d := s.untilIdle(); d <= 0 {
		t.Errorf("untilIdle() = %v, want a positive duration", d)
	}
}

// TestWaitQuietReturnsImmediatelyWhenNothingIsInFlight keeps shutdown from
// paying a grace window it does not need.
func TestWaitQuietReturnsImmediatelyWhenNothingIsInFlight(t *testing.T) {
	fake := clock.NewFake(time.Now())
	s := newTestSunset(t, fake)

	start := time.Now()
	s.waitQuiet(5 * time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitQuiet took %v with nothing in flight", elapsed)
	}
}

// TestWaitQuietWakesWhenTheLastRequestFinishes.
func TestWaitQuietWakesWhenTheLastRequestFinishes(t *testing.T) {
	fake := clock.NewFake(time.Now())
	s := newTestSunset(t, fake)

	if !s.beginRequest() {
		t.Fatal("beginRequest was refused")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.endRequest()
	}()

	start := time.Now()
	s.waitQuiet(10 * time.Second)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waitQuiet waited %v instead of waking when the request finished", elapsed)
	}
	if clients, inflight := s.counts(); inflight != 0 || clients != 0 {
		t.Errorf("counts = (%d clients, %d in flight), want (0, 0)", clients, inflight)
	}
}
