package testutil

import (
	"strings"
	"testing"
	"time"
)

func TestNoGoroutineLeaksPassesWhenQuiet(t *testing.T) {
	// Nested test so the Cleanup of NoGoroutineLeaks runs before we return.
	ok := t.Run("quiet", func(t *testing.T) {
		NoGoroutineLeaks(t)
	})
	if !ok {
		t.Fatal("NoGoroutineLeaks failed on a quiet test")
	}
}

func TestNormalizeStackStripsIDsAndPointers(t *testing.T) {
	a := normalizeStack("goroutine 12 [running]:\nmain.foo(0xc000012345)\n\t/tmp/main.go:1 +0x20")
	b := normalizeStack("goroutine 99 [running]:\nmain.foo(0xc0000abcde)\n\t/tmp/main.go:1 +0x20")
	if a != b {
		t.Fatalf("normalize mismatch:\n%q\n%q", a, b)
	}
	if strings.Contains(a, "goroutine ") || strings.Contains(a, "0xc0") {
		t.Fatalf("normalize left identity noise: %q", a)
	}
}

func TestSubtractSignaturesDetectsNewStack(t *testing.T) {
	base := map[string]int{"alpha": 1}
	cur := map[string]int{"alpha": 1, "leaked": 1}
	got := subtractSignatures(cur, base)
	if len(got) != 1 || got[0] != "leaked" {
		t.Fatalf("subtract = %v, want [leaked]", got)
	}
}

func TestStackSignaturesIgnoreTheSampler(t *testing.T) {
	// Taking a snapshot must not invent a "leak" of the sampler itself.
	before := stackSignatures()
	time.Sleep(5 * time.Millisecond)
	after := stackSignatures()
	if leaked := subtractSignatures(after, before); len(leaked) > 0 {
		t.Fatalf("sampler introduced signatures:\n%s", strings.Join(leaked, "\n---\n"))
	}
}
