package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/watch"
)

type mutableScopeScanner struct {
	watch.Scanner

	mu        sync.Mutex
	files     []string
	dirs      []string
	listCalls int
}

func (s *mutableScopeScanner) List(context.Context, string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return append([]string(nil), s.files...), nil
}

func (s *mutableScopeScanner) WatchDirectories(
	context.Context,
	string,
	[]string,
) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dirs...), nil
}

func (s *mutableScopeScanner) setFiles(files ...string) {
	s.mu.Lock()
	s.files = append([]string(nil), files...)
	s.mu.Unlock()
}

func (s *mutableScopeScanner) setDirectories(dirs ...string) {
	s.mu.Lock()
	s.dirs = append([]string(nil), dirs...)
	s.mu.Unlock()
}

func (s *mutableScopeScanner) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
}

func TestWatcherReportsChangedAndDeletedFilesAsOneDebouncedBatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.ts")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	w, err := watch.NewWatcher(root, sc, clock.New(), 30*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)

	newPath := filepath.Join(root, "new.ts")
	if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}

	batch := awaitBatch(t, w.Events())
	assertContains(t, batch.Changed, "new.ts")
	assertContains(t, batch.Deleted, "old.ts")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherAddsNewDirectoriesRecursivelyAndNormalizesAtomicSave(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	w, err := watch.NewWatcher(root, sc, clock.New(), 30*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runWatcher(t, ctx, w)

	nested := filepath.Join(root, "new", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	// Give the backend a bounded opportunity to deliver the directory CREATE
	// events and for Run to install the recursive watch.
	deadline := time.Now().Add(2 * time.Second)
	var target string
	for time.Now().Before(deadline) {
		tmp := filepath.Join(nested, "save.tmp")
		target = filepath.Join(nested, "saved.ts")
		if err := os.WriteFile(tmp, []byte("saved"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, target); err != nil {
			t.Fatal(err)
		}
		select {
		case batch := <-w.Events():
			if contains(batch.Changed, "new/nested/saved.ts") {
				cancel()
				if err := <-done; err != nil {
					t.Fatalf("Run: %v", err)
				}
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
		_ = os.Remove(target)
	}
	t.Fatal("watcher never reported an atomic save in a dynamically-created directory")
}

func TestWatcherPrunesDependencyDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	w, err := watch.NewWatcher(root, sc, clock.New(), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)

	if err := os.WriteFile(filepath.Join(root, "node_modules", "pkg", "index.ts"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case batch := <-w.Events():
		t.Fatalf("dependency change produced a batch: %#v", batch)
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherUsesScannerScopeForIgnoredAndNewlyUnignoredFiles(t *testing.T) {
	root := t.TempDir()
	ignored := filepath.Join(root, "generated.ts")
	ignoreFile := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(ignored, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignoreFile, []byte("generated.ts\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	base := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	sc := &mutableScopeScanner{
		Scanner: base,
		files:   []string{".gitignore"},
		dirs:    []string{"."},
	}
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)

	if err := os.WriteFile(ignored, []byte("still ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case batch := <-w.Events():
		t.Fatalf("ignored file write produced a batch: %#v", batch)
	case <-time.After(150 * time.Millisecond):
	}

	// Changing ignore rules must both wake the watcher and make the newly
	// admitted file visible even though that file receives no second write.
	sc.setFiles(".gitignore", "generated.ts")
	if err := os.WriteFile(ignoreFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	batch := awaitBatch(t, w.Events())
	assertContains(t, batch.Changed, "generated.ts")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherDropsWatchesWhenDirectoryBecomesIgnored(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "generated")
	if err := os.Mkdir(generated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generated, "old.ts"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignoreFile := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(ignoreFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	base := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	sc := &mutableScopeScanner{
		Scanner: base,
		files:   []string{".gitignore", "generated/old.ts"},
		dirs:    []string{".", "generated"},
	}
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)

	sc.setFiles(".gitignore")
	sc.setDirectories(".")
	if err := os.WriteFile(ignoreFile, []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	batch := awaitBatch(t, w.Events())
	assertContains(t, batch.Deleted, "generated/old.ts")
	calls := sc.calls()

	if err := os.WriteFile(filepath.Join(generated, "churn.ts"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := sc.calls(); got != calls {
		t.Fatalf("newly ignored directory caused %d later scope rescans, want 0", got-calls)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherDoesNotWatchOrRescanInsideIgnoredDirectoryTrees(t *testing.T) {
	root := t.TempDir()
	ignored := filepath.Join(root, "generated", "deep")
	if err := os.MkdirAll(ignored, 0o700); err != nil {
		t.Fatal(err)
	}

	base := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	sc := &mutableScopeScanner{
		Scanner: base,
		files:   []string{"main.ts"},
		dirs:    []string{"."},
	}
	if err := os.WriteFile(filepath.Join(root, "main.ts"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)
	calls := sc.calls()

	if err := os.WriteFile(filepath.Join(ignored, "churn.ts"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := sc.calls(); got != calls {
		t.Fatalf("ignored-tree write caused %d scope rescans, want 0", got-calls)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherRemovesRenamedDirectoryWatchesAfterScopeReconciliation(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	if err := os.MkdirAll(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "main.ts"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}

	base := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	sc := &mutableScopeScanner{
		Scanner: base,
		files:   []string{"active/main.ts"},
		dirs:    []string{".", "active"},
	}
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)

	sc.setFiles()
	sc.setDirectories(".")
	renamed := filepath.Join(root, "ignored")
	if err := os.Rename(active, renamed); err != nil {
		t.Fatal(err)
	}
	batch := awaitBatch(t, w.Events())
	assertContains(t, batch.Deleted, "active/main.ts")
	calls := sc.calls()

	if err := os.WriteFile(filepath.Join(renamed, "churn.ts"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := sc.calls(); got != calls {
		t.Fatalf("renamed, out-of-scope directory caused %d scope rescans, want 0", got-calls)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherRescansWhenRepositoryExcludeControlsChange(t *testing.T) {
	root := t.TempDir()
	info := filepath.Join(root, ".git", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(info, "exclude")
	if err := os.WriteFile(exclude, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	base := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	sc := &mutableScopeScanner{Scanner: base, dirs: []string{"."}}
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)
	calls := sc.calls()

	if err := os.WriteFile(exclude, []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, func() bool { return sc.calls() > calls }, "repository exclude change did not rescan scope")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherRescansForLinkedWorktreeExcludeChangesWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	metadataRoot := t.TempDir()
	worktreeGitDir := filepath.Join(metadataRoot, "worktrees", "checkout")
	commonGitDir := filepath.Join(metadataRoot, "common")
	if err := os.MkdirAll(worktreeGitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(commonGitDir, "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".git"),
		[]byte("gitdir: "+worktreeGitDir+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(worktreeGitDir, "commondir"),
		[]byte(commonGitDir+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(info, "exclude")
	if err := os.WriteFile(exclude, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	base := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	sc := &mutableScopeScanner{Scanner: base, dirs: []string{"."}}
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)
	calls := sc.calls()

	if err := os.WriteFile(exclude, []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, func() bool { return sc.calls() > calls }, "linked-worktree exclude change did not rescan scope")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherDoesNotFollowSymlinkedRepositoryControls(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	info := filepath.Join(external, "info")
	if err := os.Mkdir(info, 0o700); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(info, "exclude")
	if err := os.WriteFile(exclude, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, ".git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	base := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	sc := &mutableScopeScanner{Scanner: base, dirs: []string{"."}}
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)
	calls := sc.calls()

	if err := os.WriteFile(exclude, []byte("secret/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := sc.calls(); got != calls {
		t.Fatalf("symlinked repository control caused %d scope rescans, want 0", got-calls)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherNeverReportsASymlinkedFile(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.ts")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	w, err := watch.NewWatcher(root, sc, clock.New(), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)

	if err := os.Symlink(outside, filepath.Join(root, "link.ts")); err != nil {
		cancel()
		<-done
		t.Skipf("symlinks unavailable: %v", err)
	}
	select {
	case batch := <-w.Events():
		t.Fatalf("symlink creation produced a batch: %#v", batch)
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWatcherClosesEventsWhenCancelled(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	w, err := watch.NewWatcher(root, sc, clock.New(), time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatcher(t, ctx, w)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case _, ok := <-w.Events():
		if ok {
			t.Fatal("Events remained open after Run returned")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events did not close after cancellation")
	}
}

func runWatcher(t *testing.T, ctx context.Context, w watch.Watcher) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case <-w.Ready():
	case err := <-done:
		t.Fatalf("Run returned before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}
	return done
}

func awaitBatch(t *testing.T, events <-chan watch.Batch) watch.Batch {
	t.Helper()
	select {
	case batch, ok := <-events:
		if !ok {
			t.Fatal("Events closed before a batch arrived")
		}
		return batch
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watcher batch")
		return watch.Batch{}
	}
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	if !contains(values, want) {
		t.Fatalf("%#v does not contain %q", values, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func awaitCondition(t *testing.T, condition func() bool, failure string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(failure)
}
