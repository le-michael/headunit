package scanner

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"headunit/internal/metadata"
)

type pendingEntry struct {
	timer *time.Timer
	gen   uint64
}

// watcher coordinates recursive fsnotify monitoring, debounced stability
// checks, and metadata ingestion for a resolved music root directory.
//
// Concurrency & Lock-Ordering Invariant:
// Service.scanMu, watcher.watchMu, and watcher.pendingMu are strict leaf locks
// that must never be nested in any order:
//   - Service.scanMu serializes SQLite store writes and cached count refreshes.
//   - watchMu protects watchedDirs and fsWatcher.Add/Remove synchronization.
//   - pendingMu protects pending, closed, nextGen, and workerWG timer accounting.
//
// Timer Accounting Contract:
// Under pendingMu, workerWG.Add(1) is called only while !closed immediately
// before creating each time.AfterFunc timer. Stopping or replacing a timer
// calls workerWG.Done() if and only if timer.Stop() returns true; otherwise the
// timer callback's defer balances workerWG while closed and entry.gen fence any
// stale or canceled firing before semaphore acquisition or worker execution.
type watcher struct {
	svc       *Service
	rootDir   string
	fsWatcher *fsnotify.Watcher
	events    chan fsnotify.Event
	rescan    chan struct{}
	workerSem chan struct{}

	watchMu     sync.Mutex
	watchedDirs map[string]os.FileInfo

	pendingMu sync.Mutex
	closed    bool
	nextGen   uint64
	pending   map[string]pendingEntry

	eventWG    sync.WaitGroup
	dispatchWG sync.WaitGroup
	workerWG   sync.WaitGroup

	cancel      context.CancelFunc
	limitWarned atomic.Bool
}

func (s *Service) startWatcher(ctx context.Context, walkRoot string) *watcher {
	fw, err := s.newWatcherFn()
	if err != nil {
		if isWatcherLimitError(err) {
			s.logger.Warn("scanner: live watcher disabled due to OS watch limit; increase fs.inotify.max_user_watches or fs.inotify.max_user_instances",
				"music_dir", walkRoot,
				"error", err,
			)
		} else {
			s.logger.Warn("scanner: live watcher disabled; failed to create fsnotify watcher",
				"music_dir", walkRoot,
				"error", err,
			)
		}
		return nil
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	w := &watcher{
		svc:         s,
		rootDir:     walkRoot,
		fsWatcher:   fw,
		events:      make(chan fsnotify.Event, s.maxEventQueue),
		rescan:      make(chan struct{}, 1),
		workerSem:   make(chan struct{}, workerLimit),
		watchedDirs: make(map[string]os.FileInfo),
		pending:     make(map[string]pendingEntry),
		cancel:      watchCancel,
	}

	// Start eventLoop before registering recursive watches so fsnotify.Events
	// never stalls if filesystem activity begins during the initial directory walk.
	w.eventWG.Add(1)
	go func() {
		defer w.eventWG.Done()
		w.eventLoop(watchCtx)
	}()

	w.addRecursiveWatches(watchCtx, walkRoot, false)

	w.dispatchWG.Add(1)
	go func() {
		defer w.dispatchWG.Done()
		w.dispatchLoop(watchCtx)
	}()

	return w
}

// close shuts down the watcher in strict raw-FD-safe order:
// 1. Cancel loop context (stops dispatchLoop/workers and switches eventLoop to silent drain).
// 2. Under pendingMu, set closed = true and stop all debounce timers before unlocking.
// 3. Wait for dispatchLoop to exit so zero fsWatcher.Add/Remove calls remain.
// 4. Close fsWatcher (deadlock-free because eventLoop keeps draining Events/Errors until closed).
// 5. Wait for eventLoop to finish draining and close events.
// 6. Wait for all in-flight stability/ingestion workers to finish.
func (w *watcher) close() {
	w.cancel()

	w.pendingMu.Lock()
	w.closed = true
	for absPath, entry := range w.pending {
		if entry.timer.Stop() {
			w.workerWG.Done()
		}
		delete(w.pending, absPath)
	}
	w.pendingMu.Unlock()

	w.dispatchWG.Wait()
	if err := w.fsWatcher.Close(); err != nil && !errors.Is(err, fsnotify.ErrClosed) {
		w.svc.logger.Warn("scanner: failed to close fsnotify watcher", "error", err)
	}
	w.eventWG.Wait()
	w.workerWG.Wait()
}

func (w *watcher) eventLoop(ctx context.Context) {
	defer close(w.events)

	eventsCh := w.fsWatcher.Events
	errorsCh := w.fsWatcher.Errors

	// Keep draining both fsnotify channels until fsWatcher.Close() closes them;
	// returning early on ctx.Done() can block fsnotify's internal reader while it
	// holds its mutex in sendError, deadlocking Close().
	for eventsCh != nil || errorsCh != nil {
		select {
		case ev, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
				continue
			}
			if ctx.Err() != nil {
				continue
			}
			// Drop Chmod-only and empty events immediately so chmod/rsync bursts
			// cannot backpressure event reading or trigger extraction.
			if !ev.Has(fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename) {
				continue
			}
			select {
			case w.events <- ev:
			default:
				w.triggerRescan()
			}
		case err, ok := <-errorsCh:
			if !ok {
				errorsCh = nil
				continue
			}
			if ctx.Err() != nil {
				continue
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.svc.logger.Warn("scanner: fsnotify event queue overflow; scheduling full reconciliation", "error", err)
				w.triggerRescan()
				continue
			}
			if isWatcherLimitError(err) {
				w.warnWatcherLimit(err)
				continue
			}
			w.svc.logger.Warn("scanner: fsnotify watcher error", "error", err)
		}
	}
}

func (w *watcher) dispatchLoop(ctx context.Context) {
	for {
		// Prioritize rescan so overflow reconciliation drains stale queued events
		// before handling newer incremental notifications.
		select {
		case <-ctx.Done():
			return
		case <-w.rescan:
			w.handleRescan(ctx)
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-w.rescan:
			w.handleRescan(ctx)
		case ev, ok := <-w.events:
			if !ok {
				return
			}
			if w.svc.beforeDispatch != nil {
				w.svc.beforeDispatch(ctx)
			}
			// Re-check rescan after waking in case both channels were ready.
			select {
			case <-ctx.Done():
				return
			case <-w.rescan:
				w.handleRescan(ctx)
			default:
				w.handleEvent(ctx, ev)
			}
		}
	}
}

func (w *watcher) triggerRescan() {
	select {
	case w.rescan <- struct{}{}:
		w.svc.rescanCount.Add(1)
	default:
	}
}

func (w *watcher) handleRescan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

drainLoop:
	for {
		select {
		case _, ok := <-w.events:
			if !ok {
				break drainLoop
			}
		default:
			break drainLoop
		}
	}

	w.pruneMissingWatches()
	w.addRecursiveWatches(ctx, w.rootDir, false)

	if err := w.svc.ScanNow(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		w.svc.logger.Warn("scanner: overflow reconciliation scan failed", "music_dir", w.rootDir, "error", err)
	}
}

type watchedDirEntry struct {
	path string
	info os.FileInfo
}

func (w *watcher) pruneMissingWatches() {
	watchList := w.fsWatcher.WatchList()
	live := make(map[string]struct{}, len(watchList))
	for _, dir := range watchList {
		live[dir] = struct{}{}
	}

	w.watchMu.Lock()
	entries := make([]watchedDirEntry, 0, len(w.watchedDirs))
	for dir, info := range w.watchedDirs {
		entries = append(entries, watchedDirEntry{path: dir, info: info})
	}
	w.watchMu.Unlock()

	for _, entry := range entries {
		if _, ok := live[entry.path]; !ok {
			w.removeWatchesUnder(entry.path)
			continue
		}
		curInfo, err := os.Lstat(entry.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				w.removeWatchesUnder(entry.path)
			}
			continue
		}
		if !curInfo.IsDir() || !os.SameFile(entry.info, curInfo) {
			w.removeWatchesUnder(entry.path)
		}
	}
}

func (w *watcher) handleEvent(ctx context.Context, ev fsnotify.Event) {
	absPath := filepath.Clean(ev.Name)
	if absPath == w.rootDir {
		if ev.Has(fsnotify.Remove | fsnotify.Rename) {
			w.svc.logger.Warn("scanner: ignoring remove/rename event on music root directory",
				"path", absPath,
				"op", ev.Op.String(),
			)
		}
		return
	}

	relPath, ok := toRelPath(w.rootDir, absPath)
	if !ok || hasIgnoredSegment(relPath) {
		return
	}

	if ev.Has(fsnotify.Remove | fsnotify.Rename) {
		w.handleRemoveOrRename(ctx, absPath, relPath)
		if !ev.Has(fsnotify.Create | fsnotify.Write) {
			return
		}
	}

	if !ev.Has(fsnotify.Create | fsnotify.Write) {
		return
	}

	info, err := os.Lstat(absPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.svc.logger.Warn("scanner: failed to stat changed path", "path", absPath, "error", err)
		}
		return
	}

	if info.IsDir() {
		if ev.Has(fsnotify.Create) {
			w.handleDirCreated(ctx, absPath)
		}
		return
	}

	// Never index symlinks, FIFOs, sockets, or unsupported non-audio files.
	if !info.Mode().IsRegular() {
		return
	}
	if _, _, ok := metadata.LookupFormat(absPath); !ok {
		return
	}

	w.scheduleFile(ctx, absPath, 0)
}

func (w *watcher) handleRemoveOrRename(ctx context.Context, absPath, relPath string) {
	if w.removeWatchesUnder(absPath) {
		w.cancelPendingUnder(absPath)
		w.deleteDirTracks(ctx, relPath)
		return
	}

	w.cancelPending(absPath)
	if _, _, isAudio := metadata.LookupFormat(absPath); isAudio {
		w.deleteFileTrack(ctx, relPath)
	}
}

func (w *watcher) deleteDirTracks(ctx context.Context, relDir string) {
	if ctx.Err() != nil {
		return
	}

	w.svc.scanMu.Lock()
	defer w.svc.scanMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	if err := w.svc.store.DeleteByDirPrefix(ctx, relDir); err != nil {
		if ctx.Err() == nil {
			w.svc.logger.Warn("scanner: failed to delete tracks under removed directory",
				"rel_dir", relDir,
				"error", err,
			)
		}
		return
	}
	w.svc.refreshCountLocked(context.WithoutCancel(ctx))
}

func (w *watcher) deleteFileTrack(ctx context.Context, relPath string) {
	if ctx.Err() != nil {
		return
	}

	w.svc.scanMu.Lock()
	defer w.svc.scanMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	if err := w.svc.store.DeleteByRelPath(ctx, relPath); err != nil {
		if ctx.Err() == nil {
			w.svc.logger.Warn("scanner: failed to delete removed track",
				"rel_path", relPath,
				"error", err,
			)
		}
		return
	}
	w.svc.refreshCountLocked(context.WithoutCancel(ctx))
}

func (w *watcher) handleDirCreated(ctx context.Context, absDir string) {
	w.addRecursiveWatches(ctx, absDir, true)
}

func (w *watcher) addRecursiveWatches(ctx context.Context, startDir string, enqueueFiles bool) {
	walkErr := filepath.WalkDir(startDir, func(path string, d fs.DirEntry, entryErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entryErr != nil {
			if errors.Is(entryErr, os.ErrNotExist) {
				return nil
			}
			w.svc.logger.Warn("scanner: watch walk error", "path", path, "error", entryErr)
			return nil
		}

		if d.IsDir() {
			if path != w.rootDir && metadata.IsIgnoredName(d.Name()) {
				return filepath.SkipDir
			}
			// Register the watch before WalkDir calls os.ReadDir(path) so newly
			// created files inside nested mkdir -p trees are not missed.
			if err := w.addWatch(path); err != nil {
				if errors.Is(err, fsnotify.ErrClosed) {
					return err
				}
				if errors.Is(err, os.ErrNotExist) {
					return filepath.SkipDir
				}
				if isWatcherLimitError(err) {
					w.warnWatcherLimit(err)
					return nil
				}
				w.svc.logger.Warn("scanner: failed to add directory watch", "dir", path, "error", err)
			}
			return nil
		}

		if !enqueueFiles || !d.Type().IsRegular() {
			return nil
		}
		if metadata.IsIgnoredName(d.Name()) {
			return nil
		}
		if _, _, ok := metadata.LookupFormat(d.Name()); !ok {
			return nil
		}

		relPath, ok := toRelPath(w.rootDir, path)
		if !ok || hasIgnoredSegment(relPath) {
			return nil
		}
		w.scheduleFile(ctx, path, 0)
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) && !errors.Is(walkErr, fsnotify.ErrClosed) && ctx.Err() == nil {
		w.svc.logger.Warn("scanner: failed walking directory for watches", "dir", startDir, "error", walkErr)
	}
}

func (w *watcher) addWatch(absDir string) error {
	curInfo, err := os.Lstat(absDir)
	if err != nil {
		return err
	}
	if !curInfo.IsDir() {
		return nil
	}

	w.watchMu.Lock()
	defer w.watchMu.Unlock()

	if prevInfo, exists := w.watchedDirs[absDir]; exists {
		if os.SameFile(prevInfo, curInfo) {
			return nil
		}
		// Directory was replaced at the same path; drop stale watch state on
		// the unlinked inode before registering a watch on the new directory.
		w.removeWatchesUnderLocked(absDir)
	}
	if err := w.svc.addWatchFn(w.fsWatcher, absDir); err != nil {
		return err
	}
	w.watchedDirs[absDir] = curInfo
	return nil
}

func (w *watcher) removeWatchesUnder(absPath string) bool {
	w.watchMu.Lock()
	defer w.watchMu.Unlock()
	return w.removeWatchesUnderLocked(absPath)
}

// removeWatchesUnderLocked removes absPath and all watched descendants under it
// from both watchedDirs and fsWatcher while w.watchMu is held, returning true if
// at least one directory watch was tracked. Separating prefixes with
// filepath.Separator prevents "/music/Rock" from matching "/music/Rockabilly".
func (w *watcher) removeWatchesUnderLocked(absPath string) bool {
	prefix := absPath + string(filepath.Separator)

	var toRemove []string
	for dir := range w.watchedDirs {
		if dir == absPath || strings.HasPrefix(dir, prefix) {
			toRemove = append(toRemove, dir)
		}
	}
	if len(toRemove) == 0 {
		return false
	}

	for _, dir := range toRemove {
		delete(w.watchedDirs, dir)
		if err := w.fsWatcher.Remove(dir); err != nil && !isIgnorableWatchRemoveErr(err) {
			w.svc.logger.Warn("scanner: failed to remove directory watch", "dir", dir, "error", err)
		}
	}
	return true
}

func (w *watcher) scheduleFile(ctx context.Context, absPath string, attempt int) {
	w.schedule(ctx, absPath, attempt, true)
}

// scheduleFileIfIdle avoids replacing an already-armed debounce timer so a
// fresh Write event's attempt=0 timer is preserved across stability/post-parse retries.
func (w *watcher) scheduleFileIfIdle(ctx context.Context, absPath string, attempt int) {
	w.schedule(ctx, absPath, attempt, false)
}

func (w *watcher) schedule(ctx context.Context, absPath string, attempt int, replaceExisting bool) {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()

	if w.closed || ctx.Err() != nil {
		return
	}
	if prev, exists := w.pending[absPath]; exists {
		if !replaceExisting {
			return
		}
		if prev.timer.Stop() {
			w.workerWG.Done()
		}
	}

	w.nextGen++
	gen := w.nextGen

	// Increment workerWG under pendingMu before starting the timer so
	// w.close() always observes every scheduled timer before workerWG.Wait().
	w.workerWG.Add(1)
	timer := time.AfterFunc(w.svc.debounceDelay, func() {
		defer w.workerWG.Done()
		w.onTimerFired(ctx, absPath, gen, attempt)
	})
	w.pending[absPath] = pendingEntry{
		timer: timer,
		gen:   gen,
	}
}

func (w *watcher) cancelPending(absPath string) {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()

	if entry, exists := w.pending[absPath]; exists {
		if entry.timer.Stop() {
			w.workerWG.Done()
		}
		delete(w.pending, absPath)
	}
}

func (w *watcher) cancelPendingUnder(absDir string) {
	prefix := absDir + string(filepath.Separator)

	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()

	for absPath, entry := range w.pending {
		if absPath == absDir || strings.HasPrefix(absPath, prefix) {
			if entry.timer.Stop() {
				w.workerWG.Done()
			}
			delete(w.pending, absPath)
		}
	}
}

func (w *watcher) onTimerFired(ctx context.Context, absPath string, gen uint64, attempt int) {
	w.pendingMu.Lock()
	entry, exists := w.pending[absPath]
	if w.closed || !exists || entry.gen != gen {
		w.pendingMu.Unlock()
		return
	}
	delete(w.pending, absPath)
	w.pendingMu.Unlock()

	select {
	case <-ctx.Done():
		return
	case w.workerSem <- struct{}{}:
		defer func() { <-w.workerSem }()
	}

	w.runStabilityAndIngest(ctx, absPath, attempt)
}

func (w *watcher) runStabilityAndIngest(ctx context.Context, absPath string, attempt int) {
	if ctx.Err() != nil {
		return
	}

	relPath, ok := toRelPath(w.rootDir, absPath)
	if !ok || hasIgnoredSegment(relPath) {
		return
	}
	if _, _, ok := metadata.LookupFormat(absPath); !ok {
		return
	}

	info1, err := os.Lstat(absPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.svc.logger.Warn("scanner: failed to stat file before stability check", "path", absPath, "error", err)
		}
		return
	}
	if !info1.Mode().IsRegular() {
		return
	}

	if w.svc.onStabilityStat != nil {
		w.svc.onStabilityStat(absPath, attempt)
	}

	if !sleepContext(ctx, w.svc.stabilityDelay) {
		return
	}

	info2, err := os.Lstat(absPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.svc.logger.Warn("scanner: failed to stat file after stability check", "path", absPath, "error", err)
		}
		return
	}
	if !info2.Mode().IsRegular() {
		return
	}

	if info2.Size() <= 0 || info2.Size() != info1.Size() || info2.ModTime().UnixNano() != info1.ModTime().UnixNano() {
		nextAttempt := attempt + 1
		if nextAttempt >= w.svc.maxStabilityAttempts {
			w.svc.logger.Warn("scanner: file did not stabilize within max attempts",
				"path", absPath,
				"attempts", nextAttempt,
				"size_bytes", info2.Size(),
			)
			return
		}
		w.scheduleFileIfIdle(ctx, absPath, nextAttempt)
		return
	}

	w.ingestFile(ctx, absPath, relPath, info2)
}

func (w *watcher) ingestFile(ctx context.Context, absPath, relPath string, stableInfo os.FileInfo) {
	if w.svc.onIngestDone != nil {
		defer w.svc.onIngestDone(relPath)
	}
	if ctx.Err() != nil {
		return
	}

	meta, err := w.svc.extractFn(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		w.svc.logger.Warn("scanner: failed to extract audio metadata", "path", absPath, "error", err)
		return
	}

	w.svc.scanMu.Lock()
	if ctx.Err() != nil {
		w.svc.scanMu.Unlock()
		return
	}

	curInfo, err := os.Lstat(absPath)
	if err != nil {
		w.svc.scanMu.Unlock()
		if !errors.Is(err, os.ErrNotExist) {
			w.svc.logger.Warn("scanner: post-parse stat failed", "path", absPath, "error", err)
		}
		return
	}
	if !curInfo.Mode().IsRegular() || curInfo.Size() <= 0 {
		w.svc.scanMu.Unlock()
		return
	}
	if curInfo.Size() != stableInfo.Size() || curInfo.ModTime().UnixNano() != stableInfo.ModTime().UnixNano() {
		// Unlock scanMu before touching pendingMu to preserve the leaf-lock invariant.
		w.svc.scanMu.Unlock()
		w.scheduleFileIfIdle(ctx, absPath, 0)
		return
	}

	tr := buildTrack(relPath, curInfo.Size(), curInfo.ModTime().UnixNano(), meta)
	if err := w.svc.store.UpsertTrack(ctx, &tr); err != nil {
		w.svc.scanMu.Unlock()
		if ctx.Err() == nil {
			w.svc.logger.Warn("scanner: failed to upsert track", "rel_path", relPath, "error", err)
		}
		return
	}
	w.svc.refreshCountLocked(context.WithoutCancel(ctx))
	w.svc.scanMu.Unlock()
}

func (w *watcher) warnWatcherLimit(err error) {
	if w.limitWarned.CompareAndSwap(false, true) {
		w.svc.logger.Warn("scanner: OS watch limit reached; increase fs.inotify.max_user_watches or fs.inotify.max_user_instances",
			"music_dir", w.rootDir,
			"error", err,
		)
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func isWatcherLimitError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE)
}

func isIgnorableWatchRemoveErr(err error) bool {
	return errors.Is(err, fsnotify.ErrNonExistentWatch) ||
		errors.Is(err, fsnotify.ErrClosed) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.EINVAL)
}
