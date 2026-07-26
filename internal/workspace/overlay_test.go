package workspace

import (
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

func TestOverlayPutAndLive(t *testing.T) {
	ck := clock.NewFake(time.Unix(1_700_000_000, 0))
	o := newOverlays(ck, time.Minute)

	o.put("s1", "a.ts", "overlay-text", "hash1")
	text, hash, ok, err := o.live("s1", "a.ts")
	if err != nil || !ok {
		t.Fatalf("live = (%v, %v), want ok", ok, err)
	}
	if text != "overlay-text" || hash != "hash1" {
		t.Fatalf("live = %q/%q", text, hash)
	}
	if o.count() != 1 {
		t.Fatalf("count = %d, want 1", o.count())
	}
}

func TestOverlaySessionsAreIsolated(t *testing.T) {
	ck := clock.NewFake(time.Unix(1_700_000_000, 0))
	o := newOverlays(ck, time.Minute)

	o.put("s1", "a.ts", "from-s1", "h1")
	o.put("s2", "a.ts", "from-s2", "h2")

	t1, _, ok1, err1 := o.live("s1", "a.ts")
	t2, _, ok2, err2 := o.live("s2", "a.ts")
	if err1 != nil || err2 != nil || !ok1 || !ok2 {
		t.Fatalf("live errors: %v %v ok=%v/%v", err1, err2, ok1, ok2)
	}
	if t1 != "from-s1" || t2 != "from-s2" {
		t.Fatalf("isolation broken: s1=%q s2=%q", t1, t2)
	}
}

func TestOverlayInvalidateMarksStaleAndNextUseIsStaleEdit(t *testing.T) {
	ck := clock.NewFake(time.Unix(1_700_000_000, 0))
	o := newOverlays(ck, time.Minute)
	o.put("s1", "a.ts", "overlay", "h1")

	o.invalidatePath("a.ts")

	_, _, ok, err := o.live("s1", "a.ts")
	if ok {
		t.Fatal("stale overlay was still live")
	}
	wantCode(t, err, protocol.ErrStaleEdit)

	// After the one-shot STALE_EDIT, the entry is gone so a fresh put can land.
	_, _, ok, err = o.live("s1", "a.ts")
	if err != nil || ok {
		t.Fatalf("after stale consume: ok=%v err=%v", ok, err)
	}
	o.put("s1", "a.ts", "fresh", "h2")
	text, _, ok, err := o.live("s1", "a.ts")
	if err != nil || !ok || text != "fresh" {
		t.Fatalf("replacement overlay = %q ok=%v err=%v", text, ok, err)
	}
}

func TestOverlayTTLExpiry(t *testing.T) {
	ck := clock.NewFake(time.Unix(1_700_000_000, 0))
	o := newOverlays(ck, time.Minute)
	o.put("s1", "a.ts", "overlay", "h1")

	ck.Advance(time.Minute)
	_, _, ok, err := o.live("s1", "a.ts")
	if err != nil || ok {
		t.Fatalf("expired overlay still live: ok=%v err=%v", ok, err)
	}
	if o.count() != 0 {
		t.Fatalf("count after expiry = %d", o.count())
	}
}

func TestOverlayTTLRefreshedOnUse(t *testing.T) {
	ck := clock.NewFake(time.Unix(1_700_000_000, 0))
	o := newOverlays(ck, time.Minute)
	o.put("s1", "a.ts", "overlay", "h1")

	ck.Advance(45 * time.Second)
	if _, _, ok, err := o.live("s1", "a.ts"); err != nil || !ok {
		t.Fatalf("refresh use failed: ok=%v err=%v", ok, err)
	}
	ck.Advance(45 * time.Second)
	if _, _, ok, err := o.live("s1", "a.ts"); err != nil || !ok {
		t.Fatalf("TTL was not refreshed on use: ok=%v err=%v", ok, err)
	}
}

func TestOverlayDropSession(t *testing.T) {
	ck := clock.NewFake(time.Unix(1_700_000_000, 0))
	o := newOverlays(ck, time.Minute)
	o.put("s1", "a.ts", "one", "h1")
	o.put("s1", "b.ts", "two", "h2")
	o.put("s2", "a.ts", "other", "h3")

	o.dropSession("s1")
	if _, _, ok, _ := o.live("s1", "a.ts"); ok {
		t.Fatal("s1 overlay survived dropSession")
	}
	if text, _, ok, err := o.live("s2", "a.ts"); err != nil || !ok || text != "other" {
		t.Fatalf("s2 overlay damaged: %q ok=%v err=%v", text, ok, err)
	}
}

func TestOverlaySweepDropsExpiredNotStale(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	ck := clock.NewFake(start)
	o := newOverlays(ck, time.Minute)

	o.put("s1", "fresh.ts", "ok", "h1")
	o.put("s1", "old.ts", "gone", "h2")
	o.put("s1", "stale.ts", "keep-for-signal", "h3")
	o.invalidatePath("stale.ts")

	// Age old.ts past the TTL without refreshing it; keep fresh.ts current by
	// rewriting its lastUsed after the advance.
	ck.Advance(time.Minute)
	o.mu.Lock()
	if e := o.bySession["s1"]["fresh.ts"]; e != nil {
		e.lastUsed = ck.Now()
	}
	o.mu.Unlock()

	o.sweep()

	if _, _, ok, _ := o.live("s1", "old.ts"); ok {
		t.Fatal("sweep left an expired overlay")
	}
	_, _, ok, err := o.live("s1", "stale.ts")
	if ok || protocol.AsError(err).Code != protocol.ErrStaleEdit {
		t.Fatalf("sweep must not free stale entries before STALE_EDIT: ok=%v err=%v", ok, err)
	}
	if text, _, ok, err := o.live("s1", "fresh.ts"); err != nil || !ok || text != "ok" {
		t.Fatalf("fresh overlay damaged: %q ok=%v err=%v", text, ok, err)
	}
}
