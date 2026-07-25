package clock_test

import (
	"context"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
)

func TestRealClockNowAdvances(t *testing.T) {
	t.Parallel()
	ck := clock.New()
	first := ck.Now()
	if first.IsZero() {
		t.Fatal("Now returned the zero time")
	}
	if got := ck.Now(); got.Before(first) {
		t.Fatalf("Now went backwards: %v then %v", first, got)
	}
}

func TestRealClockTimerFires(t *testing.T) {
	t.Parallel()
	ck := clock.New()
	tm := ck.NewTimer(time.Millisecond)
	defer tm.Stop()
	select {
	case <-tm.C():
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not fire within 2s")
	}
}

func TestRealClockAfterFires(t *testing.T) {
	t.Parallel()
	ck := clock.New()
	select {
	case <-ck.After(time.Millisecond):
	case <-time.After(2 * time.Second):
		t.Fatal("After did not fire within 2s")
	}
}

func TestRealClockTickerTicks(t *testing.T) {
	t.Parallel()
	ck := clock.New()
	tk := ck.NewTicker(time.Millisecond)
	defer tk.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-tk.C():
		case <-time.After(2 * time.Second):
			t.Fatalf("tick %d did not arrive within 2s", i)
		}
	}
}

func TestRealClockSleepHonoursContext(t *testing.T) {
	t.Parallel()
	ck := clock.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ck.Sleep(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("Sleep returned %v, want context.Canceled", err)
	}
}

func TestRealClockSleepReturnsNilWhenElapsed(t *testing.T) {
	t.Parallel()
	ck := clock.New()
	if err := ck.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
}
