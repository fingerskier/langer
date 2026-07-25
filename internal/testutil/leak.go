package testutil

import (
	"runtime"
	"testing"
	"time"
)

// NoGoroutineLeaks snapshots the goroutine count now and, in t.Cleanup, polls
// for up to two seconds for it to return to that baseline, dumping every stack
// on failure.
//
// This is the only thing that makes "Run does not return until every goroutine
// it started has exited" a real contract rather than a comment: a language
// server client that leaks a reader goroutine per restart looks perfectly
// healthy in every functional test.
func NoGoroutineLeaks(t *testing.T) {
	t.Helper()
	baseline := runtime.NumGoroutine()

	t.Cleanup(func() {
		if t.Failed() {
			// A failed test tears down early; leaks it reports are noise.
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			runtime.Gosched()
			current := runtime.NumGoroutine()
			if current <= baseline {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				t.Errorf("goroutine leak: %d goroutines running, baseline was %d\n\n%s",
					current, baseline, buf[:n])
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}
