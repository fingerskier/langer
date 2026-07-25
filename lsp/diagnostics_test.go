package lsp

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

var diagEpochStart = time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)

func diagsFor(messages ...string) []protocol.Diagnostic {
	out := make([]protocol.Diagnostic, 0, len(messages))
	for _, m := range messages {
		out = append(out, protocol.Diagnostic{Path: "a.ts", Severity: protocol.SeverityError, Message: m})
	}
	return out
}

// A mark must not be satisfiable by a push that landed BEFORE it: that is the
// whole point of the epoch.
func TestDiagnosticsMarkIsNotSatisfiedByAnEarlierPush(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)

	d.publish("a.ts", diagsFor("stale"))
	since := d.mark()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		diags []protocol.Diagnostic
		stale bool
	}
	done := make(chan result, 1)
	go func() {
		diags, stale, _ := d.settle(ctx, "a.ts", since, 300*time.Millisecond, 2*time.Second, nil)
		done <- result{diags, stale}
	}()

	f.BlockUntil(1)
	// Nothing new has arrived; the settle must still be waiting.
	if len(done) > 0 {
		t.Fatal("settle was satisfied by a push that predates the mark")
	}

	d.publish("a.ts", diagsFor("fresh"))
	advanceUntil(t, f, time.Second, 50*time.Millisecond, func() bool { return len(done) > 0 })
	r := <-done
	got, stale := r.diags, r.stale

	if stale {
		t.Fatal("possiblyStale set for a settled result")
	}
	if len(got) != 1 || got[0].Message != "fresh" {
		t.Fatalf("diagnostics = %+v, want the post-mark push", got)
	}
}

// THE settle rule (SPEC §4.3). Both tsserver and pyright publish an EMPTY array
// before the real results; first-push-wins reports a clean file that does not
// compile.
func TestDiagnosticsQuietPeriodOutlastsTheEmptyFirstPush(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)
	since := d.mark()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		diags []protocol.Diagnostic
		stale bool
	}
	done := make(chan result, 1)
	go func() {
		diags, stale, _ := d.settle(ctx, "a.ts", since, 300*time.Millisecond, 2*time.Second, nil)
		done <- result{diags, stale}
	}()

	f.BlockUntil(1)
	d.publish("a.ts", []protocol.Diagnostic{}) // the empty first push

	// Not yet quiet: the real results are still coming.
	f.Advance(200 * time.Millisecond)
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
	if len(done) > 0 {
		r := <-done
		t.Fatalf("settle returned the empty first push after 200ms: %+v", r)
	}

	d.publish("a.ts", diagsFor("Property 'missingProp' does not exist on type 'Widget'."))
	advanceUntil(t, f, time.Second, 50*time.Millisecond, func() bool { return len(done) > 0 })

	r := <-done
	if r.stale {
		t.Fatal("possiblyStale set for a settled result")
	}
	if len(r.diags) != 1 {
		t.Fatalf("diagnostics = %+v, want the real result", r.diags)
	}
}

// SPEC §4.3: "if the settle window elapses, it returns the latest known
// diagnostics flagged possibly_stale: true" — a successful answer, not an error.
func TestDiagnosticsBudgetExpiryFlagsPossiblyStale(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)

	d.publish("a.ts", diagsFor("older result"))
	since := d.mark() // demand something newer, which never comes

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		diags []protocol.Diagnostic
		stale bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		diags, stale, err := d.settle(ctx, "a.ts", since, 300*time.Millisecond, 2*time.Second, nil)
		done <- result{diags, stale, err}
	}()

	f.BlockUntil(1)
	f.Advance(2 * time.Second)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("settle returned an error on budget expiry: %v", r.err)
		}
		if !r.stale {
			t.Fatal("possiblyStale not set after the settle window elapsed")
		}
		if len(r.diags) != 1 || r.diags[0].Message != "older result" {
			t.Fatalf("diagnostics = %+v, want the latest known result", r.diags)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settle never returned")
	}
}

// A file the server never publishes for at all: bounded, empty, flagged.
func TestDiagnosticsBudgetExpiryWithNothingKnown(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)
	since := d.mark()

	done := make(chan bool, 1)
	go func() {
		_, stale, _ := d.settle(context.Background(), "never.ts", since, 300*time.Millisecond, 2*time.Second, nil)
		done <- stale
	}()

	f.BlockUntil(1)
	f.Advance(2 * time.Second)

	select {
	case stale := <-done:
		if !stale {
			t.Fatal("possiblyStale not set for a file with no diagnostics at all")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settle never returned")
	}
}

// A chatty server that keeps republishing must not let settle run past its
// budget forever.
func TestDiagnosticsBudgetBoundsAContinuousPushStream(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)
	since := d.mark()

	done := make(chan bool, 1)
	go func() {
		_, stale, _ := d.settle(context.Background(), "a.ts", since, 300*time.Millisecond, 2*time.Second, nil)
		done <- stale
	}()

	f.BlockUntil(1)
	// 20 × 200ms = 4s of pushes, none of them ever 300ms apart. Cumulative time
	// passes the 2s budget half way through.
	for i := 0; i < 20; i++ {
		d.publish("a.ts", diagsFor("churn"))
		f.Advance(200 * time.Millisecond)
		select {
		case stale := <-done:
			if !stale {
				t.Fatal("a never-quiet stream settled cleanly")
			}
			return
		default:
		}
		runtime.Gosched()
	}

	select {
	case stale := <-done:
		if !stale {
			t.Fatal("a never-quiet stream settled cleanly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settle outlived its 2s budget under a continuous push stream")
	}
}

func TestDiagnosticsSettleReleasesEveryWaiter(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)
	since := d.mark()

	const waiters = 5
	done := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			_, _, _ = d.settle(context.Background(), "a.ts", since, 300*time.Millisecond, 2*time.Second, nil)
			done <- struct{}{}
		}()
	}

	f.BlockUntil(waiters)
	d.publish("a.ts", diagsFor("x"))
	advanceUntil(t, f, time.Second, 50*time.Millisecond, func() bool { return len(done) == waiters })

	for i := 0; i < waiters; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d waiters were released", i, waiters)
		}
	}
}

func TestDiagnosticsSettleHonoursContext(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, _, err := d.settle(ctx, "a.ts", d.mark(), 300*time.Millisecond, 2*time.Second, nil)
		errs <- err
	}()

	f.BlockUntil(1)
	cancel()

	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("settle ignored its cancelled context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settle never returned after cancellation")
	}
}

// A restart invalidates the previous generation's analysis. Reporting it as
// current would be a lie the agent cannot detect.
func TestDiagnosticsResetDropsThePreviousGeneration(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)
	d.publish("a.ts", diagsFor("from the dead server"))

	d.reset()

	if _, _, ok := d.snapshot("a.ts"); ok {
		t.Fatal("diagnostics survived a reset")
	}
}

// Diagnostics for one path must not satisfy a wait on another.
func TestDiagnosticsAreKeyedByPath(t *testing.T) {
	f := clock.NewFake(diagEpochStart)
	d := newDiagnostics(f)
	since := d.mark()

	done := make(chan bool, 1)
	go func() {
		_, stale, _ := d.settle(context.Background(), "wanted.ts", since, 300*time.Millisecond, 2*time.Second, nil)
		done <- stale
	}()

	f.BlockUntil(1)
	d.publish("other.ts", diagsFor("not the file we asked about"))
	f.Advance(300 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("a push for a different path satisfied the wait")
	default:
	}

	f.Advance(2 * time.Second)
	select {
	case stale := <-done:
		if !stale {
			t.Fatal("expected a stale result after the budget elapsed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settle never returned")
	}
}
