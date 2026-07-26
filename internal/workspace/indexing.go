package workspace

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"

	"github.com/fingerskier/langer/index"
	"github.com/fingerskier/langer/internal/watch"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/protocol"
)

type indexJob struct {
	path       string
	generation uint64
}

type indexedSemantics struct {
	symbols     []index.SymbolRecord
	diagnostics []protocol.Diagnostic
	references  map[string][]index.Reference
}

func (w *Workspace) startIndex(openCtx context.Context, opts RegistryOptions) error {
	watcher, err := opts.NewWatcher(w.root, w.scanner, opts.Clock, 0)
	if err != nil {
		return protocol.AsError(err)
	}
	if watcher == nil {
		return protocol.NewError(protocol.ErrInternal, "workspace watcher factory returned nil")
	}

	w.indexCtx, w.cancelIndex = context.WithCancel(context.Background())
	w.indexWake = make(chan struct{}, 1)
	w.healWake = make(chan struct{}, 1)
	w.stagingDone = make(chan struct{})
	w.watcher = watcher
	w.indexState = protocol.IndexIndexing

	watchErr := make(chan error, 1)
	w.indexWG.Add(1)
	go func() {
		defer w.indexWG.Done()
		watchErr <- watcher.Run(w.indexCtx)
	}()

	select {
	case <-watcher.Ready():
	case err := <-watchErr:
		w.cancelIndex()
		w.indexWG.Wait()
		if err == nil {
			err = protocol.NewError(protocol.ErrInternal, "filesystem watcher stopped before becoming ready")
		}
		return protocol.AsError(err)
	case <-openCtx.Done():
		w.cancelIndex()
		w.indexWG.Wait()
		return protocol.NewErrorf(protocol.ErrNotReady,
			"opening workspace was cancelled before filesystem watches were ready: %v", openCtx.Err())
	}

	w.indexWG.Add(3)
	go func() {
		defer w.indexWG.Done()
		w.runIndexWorker()
	}()
	go func() {
		defer w.indexWG.Done()
		w.consumeWatcher(watchErr)
	}()
	go func() {
		defer w.indexWG.Done()
		w.runIndexHealer()
	}()

	paths, err := w.scanner.List(openCtx, w.root)
	if err != nil {
		w.failIndex(err, true)
		return nil
	}
	paths = w.indexablePaths(paths)

	w.cacheMu.Lock()
	w.indexMu.Lock()
	for _, path := range paths {
		w.known[path] = struct{}{}
	}
	w.indexMu.Unlock()
	w.cacheMu.Unlock()

	w.cacheMu.Lock()
	pruned, err := w.store.ReconcileWorkspace(openCtx, w.id, paths)
	w.cacheMu.Unlock()
	if err != nil {
		w.failIndex(err, true)
		return nil
	}

	w.indexWG.Add(1)
	go func() {
		defer w.indexWG.Done()
		w.stageInitialScan(paths, pruned > 0)
	}()
	return nil
}

func (w *Workspace) stageInitialScan(paths []string, rebuild bool) {
	for _, path := range paths {
		if w.indexCtx.Err() != nil {
			return
		}
		stale, err := w.initialPathNeedsRebuild(w.indexCtx, path)
		if err != nil {
			w.failIndexPath(path, err)
			rebuild = true
			continue
		}
		rebuild = rebuild || stale
	}

	jobs := make([]indexJob, 0, len(paths))
	if rebuild {
		for _, path := range paths {
			if w.indexCtx.Err() != nil {
				return
			}
			job, err := w.stageInitialInvalidation(w.indexCtx, path)
			if err != nil {
				w.failIndexPath(path, err)
				continue
			}
			if job != nil {
				jobs = append(jobs, *job)
			}
		}
	}
	for _, job := range jobs {
		w.queueIndexJob(job)
	}

	w.cacheMu.Lock()
	w.indexMu.Lock()
	w.scanComplete = true
	w.updateIndexStateLocked()
	w.indexMu.Unlock()
	close(w.stagingDone)
	w.cacheMu.Unlock()
}

func (w *Workspace) initialPathNeedsRebuild(ctx context.Context, rel string) (bool, error) {
	unlock, err := w.lockDoc(ctx, rel)
	if err != nil {
		return false, err
	}
	defer unlock()
	w.cacheMu.RLock()
	defer w.cacheMu.RUnlock()
	state, err := w.fileCacheStateLocked(ctx, rel)
	return state != fileCacheFresh, err
}

func (w *Workspace) stageInitialInvalidation(ctx context.Context, rel string) (*indexJob, error) {
	unlock, err := w.lockDoc(ctx, rel)
	if err != nil {
		return nil, err
	}
	defer unlock()
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	return w.invalidatePathLocked(ctx, rel, false)
}

func (w *Workspace) indexablePaths(paths []string) []string {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		rel, err := w.relPath(path)
		if err != nil || !w.scanner.InScope(w.root, rel) {
			continue
		}
		if err := w.validateWorkspaceFile(rel); err != nil {
			continue
		}
		if _, ok := w.cfg.LanguageServerForFile(rel); !ok {
			continue
		}
		set[rel] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (w *Workspace) resumeOrQueue(ctx context.Context, rel string) error {
	_, err := w.readFreshCache(ctx, rel, nil)
	return err
}

func (w *Workspace) consumeWatcher(watchErr <-chan error) {
	events := w.watcher.Events()
	for events != nil {
		select {
		case <-w.indexCtx.Done():
			return
		case batch, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			w.handleWatchBatch(batch)
		case err := <-watchErr:
			if w.indexCtx.Err() != nil {
				return
			}
			if err == nil {
				err = protocol.NewError(protocol.ErrInternal, "filesystem watcher stopped unexpectedly")
			}
			w.failIndex(err, true)
			return
		}
	}

	select {
	case <-w.indexCtx.Done():
	case err := <-watchErr:
		if w.indexCtx.Err() == nil {
			if err == nil {
				err = protocol.NewError(protocol.ErrInternal, "filesystem watcher stopped unexpectedly")
			}
			w.failIndex(err, true)
		}
	}
}

func (w *Workspace) handleWatchBatch(batch watch.Batch) {
	if len(batch.Changed)+len(batch.Deleted) == 0 {
		return
	}
	if w.onFileActivity != nil {
		w.onFileActivity()
	}

	deleted := append([]string(nil), batch.Deleted...)
	changed := append([]string(nil), batch.Changed...)
	sort.Strings(deleted)
	sort.Strings(changed)

	for _, path := range deleted {
		rel, err := w.relPath(path)
		if err != nil {
			continue
		}
		if err := w.deleteIndexedPath(w.indexCtx, rel); err != nil {
			w.failIndexPath(rel, err)
		}
	}
	for _, path := range changed {
		rel, err := w.relPath(path)
		if err != nil || !w.scanner.InScope(w.root, rel) {
			continue
		}
		if err := w.validateWorkspaceFile(rel); err != nil {
			if err := w.deleteIndexedPath(w.indexCtx, rel); err != nil {
				w.failIndexPath(rel, err)
			}
			continue
		}
		if _, ok := w.cfg.LanguageServerForFile(rel); !ok {
			if err := w.deleteIndexedPath(w.indexCtx, rel); err != nil {
				w.failIndexPath(rel, err)
			}
			continue
		}
		if err := w.invalidateAndQueue(w.indexCtx, rel, true); err != nil {
			w.failIndexPath(rel, err)
		}
	}
}

func (w *Workspace) cachedFileFresh(ctx context.Context, rel string) (bool, error) {
	eligible, err := w.cacheEligibleFile(rel)
	if err != nil || !eligible {
		return false, err
	}
	return w.readFreshCache(ctx, rel, nil)
}

type fileCacheState uint8

const (
	fileCacheStale fileCacheState = iota
	fileCacheFresh
	fileCachePending
	fileCacheMissing
)

// readFreshCache holds both the per-path leaf and the workspace cache read
// barrier from the disk hash check through read. A watcher therefore cannot
// turn a truthful cached result into a silently empty one between those steps.
func (w *Workspace) readFreshCache(
	ctx context.Context,
	rel string,
	read func() error,
) (bool, error) {
	if w.store == nil {
		return false, nil
	}

	unlockPath, err := w.lockDoc(ctx, rel)
	if err != nil {
		return false, err
	}
	w.cacheMu.RLock()
	state, err := w.fileCacheStateLocked(ctx, rel)
	if err != nil {
		w.cacheMu.RUnlock()
		unlockPath()
		return false, err
	}
	if state == fileCacheFresh {
		if read != nil {
			err = read()
		}
		if err != nil && protocol.AsError(err).Code == protocol.ErrNotReady {
			w.cacheMu.RUnlock()
			w.cacheMu.Lock()
			job, invalidateErr := w.invalidatePathLocked(ctx, rel, false)
			w.cacheMu.Unlock()
			unlockPath()
			if invalidateErr != nil {
				return false, invalidateErr
			}
			if job != nil {
				w.queueIndexJob(*job)
			}
			return false, nil
		}
		w.cacheMu.RUnlock()
		unlockPath()
		if err != nil {
			return false, protocol.AsError(err)
		}
		return true, nil
	}
	if state == fileCachePending {
		w.cacheMu.RUnlock()
		unlockPath()
		return false, nil
	}
	w.cacheMu.RUnlock()

	// Recheck after upgrading: a workspace-wide writer may have completed
	// while the read lock was released. The path lock prevents a same-path
	// watcher or index job from overtaking us.
	w.cacheMu.Lock()
	state, err = w.fileCacheStateLocked(ctx, rel)
	if err != nil {
		w.cacheMu.Unlock()
		unlockPath()
		return false, err
	}
	if state == fileCacheFresh {
		if read != nil {
			err = read()
		}
		if err != nil && protocol.AsError(err).Code == protocol.ErrNotReady {
			job, invalidateErr := w.invalidatePathLocked(ctx, rel, false)
			w.cacheMu.Unlock()
			unlockPath()
			if invalidateErr != nil {
				return false, invalidateErr
			}
			if job != nil {
				w.queueIndexJob(*job)
			}
			return false, nil
		}
		w.cacheMu.Unlock()
		unlockPath()
		if err != nil {
			return false, protocol.AsError(err)
		}
		return true, nil
	}
	if state == fileCachePending {
		w.cacheMu.Unlock()
		unlockPath()
		return false, nil
	}

	var job *indexJob
	switch state {
	case fileCacheMissing:
		err = w.deletePathLocked(ctx, rel)
	default:
		job, err = w.invalidatePathLocked(ctx, rel, false)
	}
	w.cacheMu.Unlock()
	unlockPath()
	if err != nil {
		return false, err
	}
	if job != nil {
		w.queueIndexJob(*job)
	}
	return false, nil
}

// fileCacheStateLocked requires the path leaf and at least cacheMu.RLock.
func (w *Workspace) fileCacheStateLocked(ctx context.Context, rel string) (fileCacheState, error) {
	abs := w.absPath(rel)
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return fileCacheMissing, nil
	}
	if err != nil {
		return fileCacheStale, protocol.NewErrorf(protocol.ErrInternal,
			"stating %s for a cache read: %v", rel, err)
	}
	hash, err := w.scanner.Hash(abs)
	if err != nil {
		return fileCacheStale, protocol.AsError(err)
	}
	w.indexMu.Lock()
	_, pending := w.pending[rel]
	w.indexMu.Unlock()
	if pending {
		return fileCachePending, nil
	}
	cached, found, err := w.store.FileState(ctx, w.id, rel)
	if err != nil {
		return fileCacheStale, protocol.AsError(err)
	}
	if found && cached == hash {
		return fileCacheFresh, nil
	}
	return fileCacheStale, nil
}

func (w *Workspace) mutationContext(ctx context.Context) context.Context {
	if w.indexCtx != nil {
		return w.indexCtx
	}
	return ctx
}

// invalidatePathLocked requires the path leaf and cacheMu.Lock.
func (w *Workspace) invalidatePathLocked(
	ctx context.Context,
	rel string,
	admit bool,
) (*indexJob, error) {
	w.indexMu.Lock()
	_, known := w.known[rel]
	w.indexMu.Unlock()
	if !known && !admit {
		return nil, nil
	}
	info, err := os.Lstat(w.absPath(rel))
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return nil, w.deletePathLocked(ctx, rel)
	}
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal,
			"stating %s before invalidation: %v", rel, err)
	}
	if err := w.store.InvalidateFile(w.mutationContext(ctx), w.id, rel); err != nil {
		return nil, protocol.AsError(err)
	}

	w.indexMu.Lock()
	w.generations[rel]++
	job := &indexJob{path: rel, generation: w.generations[rel]}
	w.known[rel] = struct{}{}
	w.pending[rel] = job.generation
	delete(w.failed, rel)
	if !w.indexFatal {
		w.indexState = protocol.IndexIndexing
	}
	w.indexMu.Unlock()
	return job, nil
}

// deletePathLocked requires the path leaf and cacheMu.Lock.
func (w *Workspace) deletePathLocked(ctx context.Context, rel string) error {
	if err := w.store.DeleteFile(w.mutationContext(ctx), w.id, rel); err != nil {
		return protocol.AsError(err)
	}
	w.indexMu.Lock()
	w.generations[rel]++
	delete(w.known, rel)
	delete(w.pending, rel)
	delete(w.failed, rel)
	w.updateIndexStateLocked()
	w.indexMu.Unlock()
	return nil
}

// invalidateAndQueue is the shared watcher/query/edit write barrier. The
// persisted rows are invalidated before the generation is advanced and before
// replacement work becomes visible to the worker.
func (w *Workspace) invalidateAndQueue(ctx context.Context, rel string, admit bool) error {
	if w.store == nil {
		return nil
	}
	if _, ok := w.cfg.LanguageServerForFile(rel); !ok {
		return w.deleteIndexedPath(ctx, rel)
	}
	unlock, err := w.lockDoc(ctx, rel)
	if err != nil {
		return err
	}
	w.cacheMu.Lock()
	job, err := w.invalidatePathLocked(ctx, rel, admit)
	w.cacheMu.Unlock()
	unlock()
	if err != nil {
		return err
	}
	if job != nil {
		w.queueIndexJob(*job)
	}
	return nil
}

func (w *Workspace) deleteIndexedPath(ctx context.Context, rel string) error {
	if w.store == nil {
		return nil
	}
	unlock, err := w.lockDoc(ctx, rel)
	if err != nil {
		return err
	}
	w.cacheMu.Lock()
	err = w.deletePathLocked(ctx, rel)
	w.cacheMu.Unlock()
	unlock()
	return err
}

func (w *Workspace) queueIndexJob(job indexJob) {
	w.jobMu.Lock()
	w.jobQueue = append(w.jobQueue, job)
	w.jobMu.Unlock()
	select {
	case w.indexWake <- struct{}{}:
	default:
	}
}

func (w *Workspace) runIndexWorker() {
	for {
		select {
		case <-w.indexCtx.Done():
			return
		case <-w.indexWake:
			for {
				job, ok := w.takeIndexJob()
				if !ok {
					break
				}
				select {
				case <-w.stagingDone:
				case <-w.indexCtx.Done():
					return
				}
				for {
					next, err := w.indexOne(job)
					if err != nil {
						structured := protocol.AsError(err)
						if structured.Code == protocol.ErrNotReady && w.indexCtx.Err() == nil {
							delay := time.Duration(structured.RetryAfterMS) * time.Millisecond
							if delay <= 0 {
								delay = 250 * time.Millisecond
							}
							if w.clock.Sleep(w.indexCtx, delay) != nil {
								return
							}
							continue
						}
						if w.indexCtx.Err() == nil {
							w.failIndexJob(job, err)
						}
						break
					}
					if next == nil {
						break
					}
					job = *next
				}
			}
		}
	}
}

func (w *Workspace) takeIndexJob() (indexJob, bool) {
	w.jobMu.Lock()
	defer w.jobMu.Unlock()
	if w.jobHead >= len(w.jobQueue) {
		w.jobQueue = w.jobQueue[:0]
		w.jobHead = 0
		return indexJob{}, false
	}
	job := w.jobQueue[w.jobHead]
	w.jobQueue[w.jobHead] = indexJob{}
	w.jobHead++
	if w.jobHead == len(w.jobQueue) {
		w.jobQueue = w.jobQueue[:0]
		w.jobHead = 0
	} else if w.jobHead >= 1024 && w.jobHead*2 >= len(w.jobQueue) {
		remaining := copy(w.jobQueue, w.jobQueue[w.jobHead:])
		clear(w.jobQueue[remaining:])
		w.jobQueue = w.jobQueue[:remaining]
		w.jobHead = 0
	}
	return job, true
}

func (w *Workspace) triggerIndexHeal() {
	if w.store == nil || w.indexCtx.Err() != nil {
		return
	}
	select {
	case w.healWake <- struct{}{}:
	default:
	}
}

func (w *Workspace) runIndexHealer() {
	for {
		select {
		case <-w.indexCtx.Done():
			return
		case <-w.healWake:
			w.indexMu.Lock()
			paths := make([]string, 0, len(w.known))
			for path := range w.known {
				paths = append(paths, path)
			}
			w.indexMu.Unlock()
			sort.Strings(paths)
			for _, path := range paths {
				if w.indexCtx.Err() != nil {
					return
				}
				if err := w.resumeOrQueue(w.indexCtx, path); err != nil {
					w.failIndexPath(path, err)
				}
			}
		}
	}
}

func (w *Workspace) indexOne(job indexJob) (*indexJob, error) {
	unlock, err := w.lockDoc(w.indexCtx, job.path)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if !w.currentIndexJob(job) {
		return nil, nil
	}

	abs := w.absPath(job.path)
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		w.cacheMu.Lock()
		if err := w.store.DeleteFile(w.indexCtx, w.id, job.path); err != nil {
			w.cacheMu.Unlock()
			return nil, protocol.AsError(err)
		}
		w.completeDeletedJob(job)
		w.cacheMu.Unlock()
		return nil, nil
	}
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "stating %s for indexing: %v", job.path, err)
	}
	if info.Size() > maxDocumentBytes {
		return nil, protocol.NewErrorf(protocol.ErrUnsupported,
			"%s is %d bytes; langer does not index files over %d bytes",
			job.path, info.Size(), maxDocumentBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "reading %s for indexing: %v", job.path, err)
	}
	text := string(data)
	firstHash := hashBytes(data)

	srv, err := w.serverFor(w.indexCtx, job.path)
	if err != nil {
		return nil, err
	}
	referenceGeneration, err := w.store.ReferenceGeneration(w.indexCtx, w.id)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	semantics, err := w.readIndexedSemantics(srv, job.path, text)
	if err != nil {
		return nil, err
	}

	info, err = os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		w.cacheMu.Lock()
		if err := w.store.DeleteFile(w.indexCtx, w.id, job.path); err != nil {
			w.cacheMu.Unlock()
			return nil, protocol.AsError(err)
		}
		w.completeDeletedJob(job)
		w.cacheMu.Unlock()
		return nil, nil
	}
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal,
			"restating %s before index commit: %v", job.path, err)
	}
	secondHash, err := w.scanner.Hash(abs)
	if err != nil {
		return nil, protocol.AsError(err)
	}
	if secondHash != firstHash {
		return w.rescheduleLocked(job)
	}
	w.cacheMu.Lock()
	if !w.currentIndexJob(job) {
		w.cacheMu.Unlock()
		return nil, nil
	}

	record := index.FileRecord{
		Path:         job.path,
		AbsolutePath: abs,
		LanguageID:   w.languageIDFor(job.path),
		ContentHash:  secondHash,
		SizeBytes:    info.Size(),
		ModTime:      info.ModTime(),
		Symbols:      semantics.symbols,
		Diagnostics:  semantics.diagnostics,
	}
	if err := w.store.PutFile(w.indexCtx, w.id, record); err != nil {
		w.cacheMu.Unlock()
		return nil, protocol.AsError(err)
	}
	for key, refs := range semantics.references {
		committed, err := w.store.ReplaceReferencesBySymbolKey(
			w.indexCtx,
			w.id,
			key,
			referenceGeneration,
			refs,
		)
		if err != nil {
			w.cacheMu.Unlock()
			return nil, protocol.AsError(err)
		}
		if !committed {
			w.log.Debug("discarding raced reference snapshot",
				"path", job.path,
				"symbol_key", key,
				"reference_generation", referenceGeneration)
		}
	}
	w.completeIndexJob(job)
	w.cacheMu.Unlock()
	return nil, nil
}

func (w *Workspace) readIndexedSemantics(srv lsp.Server, rel, text string) (indexedSemantics, error) {
	result := indexedSemantics{references: map[string][]index.Reference{}}
	err := srv.WithDiskText(w.indexCtx, rel, w.languageIDFor(rel), text,
		func(ctx context.Context, epoch uint64) error {
			symbols, err := srv.DocumentSymbolsForIndex(ctx, rel)
			if err != nil {
				return protocol.AsError(err)
			}

			result.symbols = make([]index.SymbolRecord, 0, len(symbols))
			keyCounts := make(map[string]int, len(symbols))
			validSelection := make(map[string]bool, len(symbols))
			for _, symbol := range symbols {
				symbol.Symbol.Path = rel
				stableKey := index.StableKey(symbol.Symbol)
				symbolKey := index.SymbolKey(w.repoNamespace, rel, stableKey)
				keyCounts[symbolKey]++
				if symbol.HasSelectionRange {
					validSelection[symbolKey] = true
				}
				result.symbols = append(result.symbols, index.SymbolRecord{
					Symbol:         symbol.Symbol,
					SelectionRange: symbol.SelectionRange,
					StableKey:      stableKey,
					SymbolKey:      symbolKey,
				})
			}

			if srv.Supports(lsp.CapPushDiagnostics) || srv.Supports(lsp.CapPullDiagnostics) {
				diagnostics, stale, err := srv.Diagnostics(ctx, rel, epoch)
				if err != nil {
					return protocol.AsError(err)
				}
				if stale {
					return protocol.NewErrorf(protocol.ErrNotReady,
						"diagnostics for %s did not settle during indexing", rel).WithRetryAfterMS(250)
				}
				result.diagnostics = diagnostics
			}

			if srv.Supports(lsp.CapReferences) {
				for _, symbol := range result.symbols {
					if keyCounts[symbol.SymbolKey] != 1 || !validSelection[symbol.SymbolKey] {
						continue
					}
					locations, err := srv.References(ctx, rel, symbol.SelectionRange.Start, true)
					if err != nil {
						return protocol.AsError(err)
					}
					refs := make([]index.Reference, 0, len(locations))
					for _, location := range locations {
						refPath, err := w.relPath(location.Path)
						if err != nil {
							continue
						}
						eligible, err := w.cacheEligibleFile(refPath)
						if err != nil || !eligible {
							continue
						}
						refs = append(refs, index.Reference{
							Path:         refPath,
							Range:        location.Range,
							IsDefinition: location.IsDefinition,
						})
					}
					result.references[symbol.SymbolKey] = refs
				}
			}
			return nil
		})
	if err != nil {
		return indexedSemantics{}, protocol.AsError(err)
	}
	return result, nil
}

func (w *Workspace) rescheduleLocked(job indexJob) (*indexJob, error) {
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	if !w.currentIndexJob(job) {
		return nil, nil
	}
	if err := w.store.InvalidateFile(w.indexCtx, w.id, job.path); err != nil {
		return nil, protocol.AsError(err)
	}
	w.indexMu.Lock()
	if w.pending[job.path] != job.generation {
		w.indexMu.Unlock()
		return nil, nil
	}
	w.generations[job.path]++
	next := &indexJob{path: job.path, generation: w.generations[job.path]}
	w.pending[job.path] = next.generation
	w.indexState = protocol.IndexIndexing
	w.indexError = nil
	w.indexMu.Unlock()
	return next, nil
}

func (w *Workspace) currentIndexJob(job indexJob) bool {
	w.indexMu.Lock()
	defer w.indexMu.Unlock()
	generation, pending := w.pending[job.path]
	_, known := w.known[job.path]
	return pending && known && generation == job.generation
}

func (w *Workspace) completeIndexJob(job indexJob) {
	w.indexMu.Lock()
	if w.pending[job.path] == job.generation {
		delete(w.pending, job.path)
		delete(w.failed, job.path)
	}
	w.updateIndexStateLocked()
	w.indexMu.Unlock()
}

func (w *Workspace) completeDeletedJob(job indexJob) {
	w.indexMu.Lock()
	if w.pending[job.path] == job.generation {
		delete(w.pending, job.path)
		delete(w.failed, job.path)
		delete(w.known, job.path)
		w.generations[job.path]++
	}
	w.updateIndexStateLocked()
	w.indexMu.Unlock()
}

func (w *Workspace) failIndex(err error, fatal bool) {
	if err == nil || errors.Is(err, context.Canceled) || w.indexCtx != nil && w.indexCtx.Err() != nil {
		return
	}
	w.cacheMu.Lock()
	w.indexMu.Lock()
	w.indexState = protocol.IndexFailed
	w.indexError = protocol.AsError(err)
	if fatal {
		w.indexFatal = true
		w.scanComplete = true
	}
	w.indexMu.Unlock()
	w.cacheMu.Unlock()
	w.log.Error("workspace indexing failed", "error", err)
}

func (w *Workspace) failIndexJob(job indexJob, err error) {
	if err == nil || errors.Is(err, context.Canceled) || w.indexCtx.Err() != nil {
		return
	}
	unlock, lockErr := w.lockDoc(w.indexCtx, job.path)
	if lockErr != nil {
		return
	}
	w.cacheMu.Lock()
	if w.currentIndexJob(job) {
		// Make a partially written PutFile unobservable as fresh. The original
		// error remains primary if SQLite is itself failing here.
		_ = w.store.InvalidateFile(w.indexCtx, w.id, job.path)
		w.indexMu.Lock()
		delete(w.pending, job.path)
		w.failed[job.path] = protocol.AsError(err)
		w.indexState = protocol.IndexFailed
		w.indexError = protocol.AsError(err)
		w.indexMu.Unlock()
	}
	w.cacheMu.Unlock()
	unlock()
	w.log.Error("workspace file indexing failed", "path", job.path, "error", err)
}

func (w *Workspace) failIndexPath(path string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || w.indexCtx.Err() != nil {
		return
	}
	unlock, lockErr := w.lockDoc(w.indexCtx, path)
	if lockErr != nil {
		return
	}
	w.cacheMu.Lock()
	_ = w.store.InvalidateFile(w.indexCtx, w.id, path)
	w.indexMu.Lock()
	delete(w.pending, path)
	w.failed[path] = protocol.AsError(err)
	w.indexState = protocol.IndexFailed
	w.indexError = protocol.AsError(err)
	w.indexMu.Unlock()
	w.cacheMu.Unlock()
	unlock()
	w.log.Error("workspace path indexing failed", "path", path, "error", err)
}

func (w *Workspace) updateIndexStateLocked() {
	if w.indexFatal {
		w.indexState = protocol.IndexFailed
		return
	}
	if len(w.failed) > 0 {
		w.indexState = protocol.IndexFailed
		for _, err := range w.failed {
			w.indexError = err
			break
		}
		return
	}
	if !w.scanComplete || len(w.pending) > 0 {
		w.indexState = protocol.IndexIndexing
		return
	}
	w.indexState = protocol.IndexReady
	w.indexError = nil
}

func (w *Workspace) requireReady() error {
	w.indexMu.Lock()
	if w.indexState == protocol.IndexReady {
		w.indexMu.Unlock()
		return nil
	}
	heal := w.indexState == protocol.IndexFailed && !w.indexFatal
	w.indexMu.Unlock()
	if heal {
		w.triggerIndexHeal()
	}
	return protocol.NewError(protocol.ErrNotReady,
		"workspace index is not ready; inspect index_status for progress or failure details").
		WithRetryAfterMS(250)
}
