package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/index"
	"github.com/fingerskier/langer/internal/clock"
)

type lifecycleStore struct {
	index.Store

	mu     sync.Mutex
	events []string
	gcRan  chan struct{}
}

func newLifecycleStore() *lifecycleStore {
	return &lifecycleStore{gcRan: make(chan struct{}, 1)}
}

func (s *lifecycleStore) record(event string) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *lifecycleStore) GC(context.Context) (bool, index.GCStats, error) {
	s.record("gc")
	select {
	case s.gcRan <- struct{}{}:
	default:
	}
	return true, index.GCStats{}, nil
}

func (s *lifecycleStore) Checkpoint(context.Context) error {
	s.record("checkpoint")
	return nil
}

func (s *lifecycleStore) Close() error {
	s.record("close")
	return nil
}

func (s *lifecycleStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestRunOwnsTheIndexGCLifecycle(t *testing.T) {
	root := fixtureRoot(t)
	cfg := testConfig(t)
	cfg.DatabasePath = filepath.Join(shortTempDir(t), "index.db")
	ck := clock.NewFake(time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))
	store := newLifecycleStore()
	opened := make(chan struct{})

	srv, err := NewServer(Options{
		Root:        root,
		Config:      cfg,
		Clock:       ck,
		Logger:      testLogger(t),
		IdleTimeout: 24 * time.Hour,
		OpenStore: func(_ context.Context, path string, gotClock clock.Clock) (index.Store, error) {
			if path != cfg.DatabasePath {
				t.Errorf("OpenStore path = %q, want %q", path, cfg.DatabasePath)
			}
			if gotClock != ck {
				t.Error("OpenStore did not receive the daemon clock")
			}
			close(opened)
			return store, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	select {
	case <-opened:
		t.Fatal("NewServer opened the index before Run owned daemon lifecycle")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan error, 1)
	go func() { exit <- srv.Run(ctx) }()

	select {
	case <-srv.Ready():
	case err := <-exit:
		t.Fatalf("Run exited before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not become ready")
	}
	select {
	case <-opened:
	default:
		t.Fatal("Run became ready without opening the index")
	}

	// The GC ticker and the sunset timer are both installed before Ready can
	// be observed. Advancing one GC interval must attempt exactly one pass.
	ck.BlockUntil(2)
	ck.Advance(index.DefaultGCAttemptInterval)
	select {
	case <-store.gcRan:
	case <-time.After(5 * time.Second):
		t.Fatal("hourly GC was not attempted")
	}

	cancel()
	select {
	case err := <-exit:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}

	got := store.snapshot()
	if len(got) < 3 {
		t.Fatalf("store lifecycle events = %v, want GC then checkpoint and close", got)
	}
	if got[len(got)-2] != "checkpoint" || got[len(got)-1] != "close" {
		t.Fatalf("store shutdown tail = %v, want [checkpoint close]", got)
	}
}
