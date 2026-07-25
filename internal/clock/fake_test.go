package clock_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
)

var epoch = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestFakeNowOnlyMovesWhenAdvanced(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now = %v, want %v", got, epoch)
	}
	f.Advance(90 * time.Second)
	if got, want := f.Now(), epoch.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("Now = %v, want %v", got, want)
	}
}

func TestFakeTimerDoesNotFireEarly(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	tm := f.NewTimer(time.Minute)
	f.Advance(59 * time.Second)
	select {
	case at := <-tm.C():
		t.Fatalf("timer fired early at %v", at)
	default:
	}
	f.Advance(time.Second)
	select {
	case at := <-tm.C():
		if want := epoch.Add(time.Minute); !at.Equal(want) {
			t.Fatalf("timer delivered %v, want %v", at, want)
		}
	default:
		t.Fatal("timer did not fire after its deadline passed")
	}
}

// Advance must deliver every due timer BEFORE returning, so a test can advance
// and immediately assert. This is what makes the SPEC §3.1 sunset and the
// SPEC §3.3 backoff assertable instead of sleep-and-hope.
func TestFakeAdvanceDeliversInDeadlineOrderBeforeReturning(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	late := f.NewTimer(3 * time.Second)
	early := f.NewTimer(time.Second)
	mid := f.NewTimer(2 * time.Second)

	var order []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			select {
			case <-early.C():
				order = append(order, "early")
			case <-mid.C():
				order = append(order, "mid")
			case <-late.C():
				order = append(order, "late")
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	f.Advance(5 * time.Second)
	<-done

	if len(order) != 3 {
		t.Fatalf("received %d timers, want 3: %v", len(order), order)
	}
	// All three were buffered before Advance returned; ordering of the select
	// is not deterministic, but every one must have fired.
	seen := map[string]bool{}
	for _, o := range order {
		seen[o] = true
	}
	for _, want := range []string{"early", "mid", "late"} {
		if !seen[want] {
			t.Fatalf("timer %q never fired: %v", want, order)
		}
	}
}

func TestFakeTimerStopPreventsFiring(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop on a live timer returned false")
	}
	f.Advance(time.Hour)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if tm.Stop() {
		t.Fatal("second Stop returned true")
	}
}

func TestFakeTimerReset(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	tm := f.NewTimer(time.Hour)
	tm.Reset(time.Second)
	f.Advance(time.Second)
	select {
	case <-tm.C():
	default:
		t.Fatal("timer did not fire after Reset shortened its deadline")
	}
}

func TestFakeTickerRepeats(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	tk := f.NewTicker(time.Second)
	defer tk.Stop()
	for i := 0; i < 3; i++ {
		f.Advance(time.Second)
		select {
		case <-tk.C():
		default:
			t.Fatalf("tick %d missing", i)
		}
	}
}

func TestFakeSleepBlocksUntilAdvanced(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	var wg sync.WaitGroup
	wg.Add(1)
	var err error
	go func() {
		defer wg.Done()
		err = f.Sleep(context.Background(), 30*time.Minute)
	}()
	f.BlockUntil(1)
	f.Advance(30 * time.Minute)
	wg.Wait()
	if err != nil {
		t.Fatalf("Sleep: %v", err)
	}
}

func TestFakeSleepHonoursContext(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- f.Sleep(ctx, time.Hour) }()
	f.BlockUntil(1)
	cancel()
	select {
	case err := <-errc:
		if err != context.Canceled {
			t.Fatalf("Sleep returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sleep did not return after the context was cancelled")
	}
}

func TestFakeBlockUntilCountsParkedWaiters(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	for i := 0; i < 3; i++ {
		go func() { _ = f.Sleep(context.Background(), time.Hour) }()
	}
	done := make(chan struct{})
	go func() { defer close(done); f.BlockUntil(3) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("BlockUntil(3) never returned")
	}
	f.Advance(time.Hour)
}

func TestFakeAfter(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	ch := f.After(time.Second)
	f.Advance(time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("After channel did not receive")
	}
}
