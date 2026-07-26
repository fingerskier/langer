package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
	"github.com/fsnotify/fsnotify"
)

const defaultDebounce = 100 * time.Millisecond

// Batch is one de-duplicated group of project-file changes.
type Batch struct {
	Changed []string
	Deleted []string
}

// Watcher reports recursive, debounced project-file changes.
type Watcher interface {
	Ready() <-chan struct{}
	Events() <-chan Batch
	Run(ctx context.Context) error
}

type watchDirectoryScanner interface {
	WatchDirectories(ctx context.Context, root string, projectFiles []string) ([]string, error)
}

// NewWatcher prepares a recursive watcher. Run installs all initial watches
// and closes Ready before it begins delivering events.
func NewWatcher(root string, sc Scanner, ck clock.Clock, debounce time.Duration) (Watcher, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "watch root %s is not a directory", canonical)
	}
	if sc == nil {
		return nil, protocol.NewError(protocol.ErrInternal, "watch scanner is nil")
	}
	if ck == nil {
		ck = clock.New()
	}
	if debounce <= 0 {
		debounce = defaultDebounce
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "creating filesystem watcher: %v", err)
	}
	return &watcher{
		root:     canonical,
		scanner:  sc,
		clock:    ck,
		debounce: debounce,
		fw:       fw,
		ready:    make(chan struct{}),
		events:   make(chan Batch, 32),
	}, nil
}

type watcher struct {
	root     string
	scanner  Scanner
	clock    clock.Clock
	debounce time.Duration
	fw       *fsnotify.Watcher

	ready  chan struct{}
	events chan Batch

	mu      sync.Mutex
	running bool
}

func (w *watcher) Ready() <-chan struct{} { return w.ready }
func (w *watcher) Events() <-chan Batch   { return w.events }

func (w *watcher) Run(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return protocol.NewError(protocol.ErrInternal, "filesystem watcher Run called more than once")
	}
	w.running = true
	w.mu.Unlock()

	defer close(w.events)
	defer w.fw.Close()

	watched := make(map[string]struct{})
	if err := w.addWatch(w.root, watched); err != nil {
		return err
	}

	// The root watch is installed before the authoritative scope scan. New
	// top-level entries created during enumeration are therefore queued; all
	// deeper current state is captured by Scanner.List before Ready closes.
	initial, desired, controls, err := w.scanScope(ctx)
	if err != nil {
		return err
	}
	if err := w.reconcileWatches(desired, watched); err != nil {
		return err
	}
	// Repeat once after installing the derived directory and repository-control
	// watches. This closes the only setup gap: a nested ignore-control change
	// during the first enumeration is reflected before Ready even if its parent
	// was not watched at the start.
	initial, desired, controls, err = w.scanScope(ctx)
	if err != nil {
		return err
	}
	if err := w.reconcileWatches(desired, watched); err != nil {
		return err
	}
	known := make(map[string]struct{}, len(initial))
	for _, path := range initial {
		known[path] = struct{}{}
	}
	close(w.ready)

	changed := make(map[string]struct{})
	deleted := make(map[string]struct{})
	var (
		timer  clock.Timer
		timerC <-chan time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	resetTimer := func() {
		if timer == nil {
			timer = w.clock.NewTimer(w.debounce)
			timerC = timer.C()
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C():
			default:
			}
		}
		timer.Reset(w.debounce)
		timerC = timer.C()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case fsEvent, ok := <-w.fw.Events:
			if !ok {
				return nil
			}
			dirty, err := w.consume(fsEvent, known, changed, deleted, watched, controls)
			if err != nil {
				return err
			}
			if dirty {
				resetTimer()
			}
		case watchErr, ok := <-w.fw.Errors:
			if !ok {
				continue
			}
			return protocol.NewErrorf(protocol.ErrInternal, "filesystem watcher: %v", watchErr)
		case <-timerC:
			paths, nextDesired, nextControls, err := w.scanScope(ctx)
			if err != nil {
				return err
			}
			if err := w.reconcileWatches(nextDesired, watched); err != nil {
				return err
			}
			controls = nextControls
			w.refreshScope(paths, known, changed, deleted)
			batch := Batch{
				Changed: sortedKeys(changed),
				Deleted: sortedKeys(deleted),
			}
			clear(changed)
			clear(deleted)
			timer = nil
			timerC = nil
			if len(batch.Changed)+len(batch.Deleted) == 0 {
				continue
			}
			select {
			case w.events <- batch:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (w *watcher) consume(
	event fsnotify.Event,
	known, changed, deleted, watched, controls map[string]struct{},
) (bool, error) {
	eventPath := filepath.Clean(event.Name)
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if _, isWatchedDirectory := watched[eventPath]; isWatchedDirectory {
			if err := w.removeWatchTree(eventPath, watched); err != nil {
				return false, err
			}
		}
	}
	if eventPath == filepath.Join(w.root, ".git") {
		return true, nil
	}
	if isControlEvent(eventPath, controls) {
		return true, nil
	}

	rel, err := filepath.Rel(w.root, eventPath)
	if err != nil {
		return false, protocol.NewErrorf(
			protocol.ErrInternal,
			"relativizing watch event %s: %v",
			event.Name,
			err,
		)
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if !w.scanner.InScope(w.root, rel) {
		return false, nil
	}

	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.noteDeleted(rel, known, changed, deleted)
		return true, nil
	}

	if event.Op&fsnotify.Create != 0 {
		info, statErr := os.Lstat(eventPath)
		switch {
		case statErr != nil:
			if os.IsNotExist(statErr) {
				return true, nil
			}
			return false, protocol.NewErrorf(
				protocol.ErrInternal,
				"inspecting created path %s: %v",
				eventPath,
				statErr,
			)
		case info.Mode()&os.ModeSymlink != 0:
			return false, nil
		case info.IsDir():
			return true, nil
		case info.Mode().IsRegular():
			noteChanged(rel, changed, deleted)
			return true, nil
		}
	}

	if event.Op&fsnotify.Write != 0 {
		if info, statErr := os.Lstat(eventPath); statErr == nil &&
			info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			noteChanged(rel, changed, deleted)
			return true, nil
		}
	}
	return false, nil
}

func (w *watcher) scanScope(
	ctx context.Context,
) ([]string, map[string]struct{}, map[string]struct{}, error) {
	paths, err := w.scanner.List(ctx, w.root)
	if err != nil {
		return nil, nil, nil, protocol.AsError(err)
	}

	directories := sortedKeys(requiredDirectoryAncestors(paths))
	if directoryScanner, ok := w.scanner.(watchDirectoryScanner); ok {
		directories, err = directoryScanner.WatchDirectories(ctx, w.root, paths)
		if err != nil {
			return nil, nil, nil, protocol.AsError(err)
		}
	}

	desired := map[string]struct{}{w.root: {}}
	for _, rel := range directories {
		if abs, ok := w.projectDirectory(rel); ok {
			desired[abs] = struct{}{}
		}
	}
	controls := w.controlDirectories()
	for path := range controls {
		desired[path] = struct{}{}
	}
	return paths, desired, controls, nil
}

func (w *watcher) projectDirectory(rel string) (string, bool) {
	rel = filepath.Clean(filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")))
	if rel == "." {
		return w.root, true
	}
	if filepath.IsAbs(rel) || !w.scanner.InScope(w.root, rel) {
		return "", false
	}

	current := w.root
	for _, part := range strings.FieldsFunc(filepath.ToSlash(rel), func(r rune) bool { return r == '/' }) {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return filepath.Clean(current), true
}

func (w *watcher) controlDirectories() map[string]struct{} {
	controls := make(map[string]struct{})
	gitEntry := filepath.Join(w.root, ".git")
	info, err := os.Lstat(gitEntry)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return controls
	}

	if info.IsDir() {
		if gitDir, ok := validatedControlDirectory(gitEntry); ok {
			addGitControlDirectory(controls, gitDir)
		}
		return controls
	}
	if !info.Mode().IsRegular() {
		return controls
	}

	data, ok := readSmallControlFile(gitEntry)
	if !ok {
		return controls
	}
	gitDirValue, ok := gitDirFileValue(data)
	if !ok {
		return controls
	}
	gitDir, ok := resolveControlDirectory(w.root, gitDirValue)
	if !ok {
		return controls
	}
	addGitControlDirectory(controls, gitDir)

	commonDir := gitDir
	if data, ok := readSmallControlFile(filepath.Join(gitDir, "commondir")); ok {
		if value, valid := oneLineValue(data); valid {
			if resolved, valid := resolveControlDirectory(gitDir, value); valid {
				commonDir = resolved
			}
		}
	}
	addGitControlDirectory(controls, commonDir)
	return controls
}

func addGitControlDirectory(controls map[string]struct{}, gitDir string) {
	controls[gitDir] = struct{}{}
	if infoDir, ok := validatedControlDirectory(filepath.Join(gitDir, "info")); ok {
		controls[infoDir] = struct{}{}
	}
}

func readSmallControlFile(path string) ([]byte, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() > 4096 {
		return nil, false
	}
	data, err := os.ReadFile(path)
	return data, err == nil
}

func gitDirFileValue(data []byte) (string, bool) {
	value, ok := oneLineValue(data)
	if !ok {
		return "", false
	}
	const prefix = "gitdir:"
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", false
	}
	value = strings.TrimSpace(value[len(prefix):])
	return value, value != ""
}

func oneLineValue(data []byte) (string, bool) {
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	return value, true
}

func resolveControlDirectory(base, value string) (string, bool) {
	if filepath.IsAbs(value) {
		return validatedControlDirectory(value)
	}
	return validatedControlDirectory(filepath.Join(base, value))
}

func validatedControlDirectory(path string) (string, bool) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || !sameCleanPath(absolute, resolved) {
		return "", false
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return filepath.Clean(resolved), true
}

func sameCleanPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func (w *watcher) reconcileWatches(desired, watched map[string]struct{}) error {
	toAdd := make([]string, 0)
	for path := range desired {
		if _, exists := watched[path]; !exists {
			toAdd = append(toAdd, path)
		}
	}
	sort.Slice(toAdd, func(i, j int) bool {
		leftDepth := pathDepth(toAdd[i])
		rightDepth := pathDepth(toAdd[j])
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return toAdd[i] < toAdd[j]
	})
	for _, path := range toAdd {
		if err := w.addWatch(path, watched); err != nil {
			return err
		}
	}

	toRemove := make([]string, 0)
	for path := range watched {
		if _, keep := desired[path]; !keep {
			toRemove = append(toRemove, path)
		}
	}
	sort.Slice(toRemove, func(i, j int) bool {
		leftDepth := pathDepth(toRemove[i])
		rightDepth := pathDepth(toRemove[j])
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return toRemove[i] > toRemove[j]
	})
	for _, path := range toRemove {
		if err := w.removeWatch(path, watched); err != nil {
			return err
		}
	}
	return nil
}

func (w *watcher) addWatch(path string, watched map[string]struct{}) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return protocol.NewErrorf(protocol.ErrInternal, "inspecting watch directory %s: %v", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if err := w.fw.Add(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return protocol.NewErrorf(protocol.ErrInternal, "watching directory %s: %v", path, err)
	}
	watched[filepath.Clean(path)] = struct{}{}
	return nil
}

func (w *watcher) removeWatchTree(root string, watched map[string]struct{}) error {
	root = filepath.Clean(root)
	prefix := root + string(filepath.Separator)
	paths := make([]string, 0)
	for path := range watched {
		if path == root || strings.HasPrefix(path, prefix) {
			paths = append(paths, path)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return pathDepth(paths[i]) > pathDepth(paths[j]) })
	for _, path := range paths {
		if err := w.removeWatch(path, watched); err != nil {
			return err
		}
	}
	return nil
}

func (w *watcher) removeWatch(path string, watched map[string]struct{}) error {
	err := w.fw.Remove(path)
	if err == nil || errors.Is(err, fsnotify.ErrNonExistentWatch) || os.IsNotExist(err) {
		delete(watched, path)
		return nil
	}
	return protocol.NewErrorf(protocol.ErrInternal, "removing directory watch %s: %v", path, err)
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

func isControlEvent(path string, controls map[string]struct{}) bool {
	if _, exact := controls[path]; exact {
		return true
	}
	_, parent := controls[filepath.Dir(path)]
	return parent
}

func (w *watcher) noteDeleted(rel string, known, changed, deleted map[string]struct{}) {
	prefix := strings.TrimSuffix(rel, "/") + "/"
	for path := range known {
		if path == rel || strings.HasPrefix(path, prefix) {
			delete(known, path)
			delete(changed, path)
			deleted[path] = struct{}{}
		}
	}
}

func noteChanged(rel string, changed, deleted map[string]struct{}) {
	delete(deleted, rel)
	changed[rel] = struct{}{}
}

// refreshScope reconciles raw fsnotify candidates with Scanner.List, the
// repository's authoritative project-file oracle. Besides filtering ignored
// creates/writes, the set difference turns a .gitignore edit into deletions
// for newly ignored files and changes for newly admitted files.
func (w *watcher) refreshScope(
	paths []string,
	known, changed, deleted map[string]struct{},
) {
	next := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		next[path] = struct{}{}
	}

	for path := range known {
		if _, stillIncluded := next[path]; stillIncluded {
			continue
		}
		delete(changed, path)
		deleted[path] = struct{}{}
	}
	for path := range next {
		if _, wasIncluded := known[path]; !wasIncluded {
			noteChanged(path, changed, deleted)
		}
	}
	for path := range changed {
		if _, included := next[path]; !included {
			delete(changed, path)
		}
	}
	for path := range deleted {
		if _, existsAgain := next[path]; existsAgain {
			delete(deleted, path)
			changed[path] = struct{}{}
		}
	}

	clear(known)
	for path := range next {
		known[path] = struct{}{}
	}
}
