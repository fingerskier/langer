package testutil

import (
	"bytes"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// NoGoroutineLeaks snapshots interesting goroutine stack signatures now and,
// in t.Cleanup, polls for up to two seconds for every extra signature to
// disappear, dumping the residual stacks on failure.
//
// Comparing signatures (not mere counts) is load-bearing: a real leak is
// masked whenever an unrelated goroutine exits concurrently if only
// runtime.NumGoroutine is checked (PLAN M6 / docs/ARCHITECTURE.md §5.10).
func NoGoroutineLeaks(t *testing.T) {
	t.Helper()
	baseline := stackSignatures()

	t.Cleanup(func() {
		if t.Failed() {
			// A failed test tears down early; leaks it reports are noise.
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		var leaked []string
		for {
			runtime.Gosched()
			leaked = subtractSignatures(stackSignatures(), baseline)
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("goroutine leak: %d unexpected stack signature(s)\n\n%s",
					len(leaked), strings.Join(leaked, "\n---\n"))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// stackSignatures returns a multiset of normalized stacks for goroutines that
// look like product code (or anything not on the ignore list).
func stackSignatures() map[string]int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}

	out := map[string]int{}
	for _, block := range bytes.Split(buf, []byte("\n\n")) {
		if len(block) == 0 {
			continue
		}
		sig := normalizeStack(string(block))
		if sig == "" || ignoreStack(sig) {
			continue
		}
		out[sig]++
	}
	return out
}

func subtractSignatures(current, baseline map[string]int) []string {
	var leaked []string
	for sig, count := range current {
		extra := count - baseline[sig]
		for i := 0; i < extra; i++ {
			leaked = append(leaked, sig)
		}
	}
	sort.Strings(leaked)
	return leaked
}

// normalizeStack drops the "goroutine N [state]:" header and pointer addresses
// so two instances of the same call stack compare equal.
func normalizeStack(block string) string {
	lines := strings.Split(block, "\n")
	if len(lines) == 0 {
		return ""
	}
	// Drop "goroutine 42 [running]:"
	if strings.HasPrefix(lines[0], "goroutine ") {
		lines = lines[1:]
	}
	var b strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// created by ... in goroutine N
		if i := strings.Index(line, " in goroutine "); i >= 0 {
			line = line[:i]
		}
		// strip 0xhex pointers
		line = stripPointers(line)
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

func stripPointers(line string) string {
	var out strings.Builder
	i := 0
	for i < len(line) {
		if i+2 <= len(line) && line[i] == '0' && (line[i+1] == 'x' || line[i+1] == 'X') {
			out.WriteString("0x?")
			i += 2
			for i < len(line) && isHex(line[i]) {
				i++
			}
			continue
		}
		out.WriteByte(line[i])
		i++
	}
	return out.String()
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// ignoreStack filters runtime/testing noise that comes and goes independently
// of the system under test.
func ignoreStack(sig string) bool {
	// The leak checker itself appears on the stack while sampling.
	if strings.Contains(sig, "internal/testutil.stackSignatures") ||
		strings.Contains(sig, "internal/testutil.NoGoroutineLeaks") ||
		strings.Contains(sig, "internal/testutil.subtractSignatures") {
		return true
	}
	// Pure runtime / testing parking without product frames is ambient.
	if !strings.Contains(sig, "github.com/fingerskier/langer") {
		return true
	}
	// testing.tRunner with only incidental product frames from this package's
	// own tests is ambient test infrastructure.
	if strings.Contains(sig, "testing.tRunner") &&
		!strings.Contains(sig, "github.com/fingerskier/langer/") {
		return true
	}
	return false
}

// FormatSignatures is exported for diagnostics in other packages' tests.
func FormatSignatures(sigs map[string]int) string {
	keys := make([]string, 0, len(sigs))
	for k := range sigs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "x%d\n%s\n---\n", sigs[k], k)
	}
	return b.String()
}
