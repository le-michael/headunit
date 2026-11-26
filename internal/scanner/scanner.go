// Package scanner indexes local audio files into Headunit's SQLite store via
// startup reconciliation, live recursive fsnotify watching, and periodic scans.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"headunit/internal/metadata"
	"headunit/internal/store"
)

const (
	defaultDebounceDelay        = 1 * time.Second
	defaultStabilityDelay       = 500 * time.Millisecond
	defaultMaxStabilityAttempts = 10
	defaultMaxEventQueue        = 4096
	workerLimit                 = 4
)

// ScanStatus describes a point-in-time snapshot of library scanning state.
type ScanStatus struct {
	// Scanning reports whether a full library scan is currently executing.
	Scanning bool

	// Count is the latest known number of indexed tracks in the store.
	Count int64

	// LastScan is the UTC completion time of the most recent successful full
	// scan, or the zero time if no full scan has completed yet.
	LastScan time.Time
}

// Service coordinates startup scanning, periodic reconciliation, and live
// filesystem watching for a music library root directory. All exported methods
// are safe for concurrent use, subject to the Start and Wait lifecycle rules.
type Service struct {
	musicDir     string
	scanInterval time.Duration
	store        *store.Store
	logger       *slog.Logger

	// scanMu serializes SQLite store writes and cached count refreshes; it is a
	// strict leaf lock and must never be nested with watcher.watchMu or watcher.pendingMu.
	scanMu     sync.Mutex
	scanning   atomic.Bool
	count      atomic.Int64
	lastScanNs atomic.Int64

	startOnce     sync.Once
	lifecycleWG   sync.WaitGroup
	activeWatcher atomic.Pointer[watcher]

	// Unexported test seams initialized to production defaults by New.
	debounceDelay        time.Duration
	stabilityDelay       time.Duration
	maxStabilityAttempts int
	maxEventQueue        int
	extractFn            func(string) (metadata.Info, error)
	newWatcherFn         func() (*fsnotify.Watcher, error)
	addWatchFn           func(*fsnotify.Watcher, string) error
	onStabilityStat      func(absPath string, attempt int)
	onIngestDone         func(relPath string)
	beforeDispatch       func(ctx context.Context)
	rescanCount          atomic.Int64
	watcherReady         chan struct{}
	watcherReadyOnce     sync.Once
	initialScanDone      chan struct{}
	initialScanDoneOnce  sync.Once
}

// New constructs a Service for musicDir backed by st. It validates inputs and
// resolves musicDir to a clean absolute path without touching disk or starting
// goroutines. A scanInterval of 0 disables periodic reconciliation; a nil
// logger defaults to slog.New(slog.DiscardHandler).
func New(musicDir string, scanInterval time.Duration, st *store.Store, logger *slog.Logger) (*Service, error) {
	trimmed := strings.TrimSpace(musicDir)
	if trimmed == "" {
		return nil, errors.New("scanner: music directory must not be empty")
	}
	if scanInterval < 0 {
		return nil, fmt.Errorf("scanner: scan interval %v must not be negative", scanInterval)
	}
	if st == nil {
		return nil, errors.New("scanner: store must not be nil")
	}
	absDir, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("scanner: resolve music directory %q: %w", musicDir, err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Service{
		musicDir:             absDir,
		scanInterval:         scanInterval,
		store:                st,
		logger:               logger,
		debounceDelay:        defaultDebounceDelay,
		stabilityDelay:       defaultStabilityDelay,
		maxStabilityAttempts: defaultMaxStabilityAttempts,
		maxEventQueue:        defaultMaxEventQueue,
		extractFn:            metadata.Extract,
		newWatcherFn:         fsnotify.NewWatcher,
		addWatchFn:           func(fw *fsnotify.Watcher, dir string) error { return fw.Add(dir) },
		watcherReady:         make(chan struct{}),
		initialScanDone:      make(chan struct{}),
	}, nil
}

// Start seeds the track count from SQLite and asynchronously launches the live
// watcher, initial full scan, and optional periodic reconciliation loop.
// Start is single-use and idempotent: subsequent calls are no-ops. Non-fatal
// startup degradation (such as an unavailable musicDir or OS watcher limits) is
// logged via slog.Warn rather than returned so HTTP server startup remains
// non-blocking. Cancel ctx and call Wait after Start returns to drain
// background goroutines before closing the store.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		s.seedTrackCount(ctx)

		s.lifecycleWG.Add(1)
		go func() {
			defer s.lifecycleWG.Done()
			s.runLifecycle(ctx)
		}()

		if s.scanInterval > 0 {
			s.lifecycleWG.Add(1)
			go func() {
				defer s.lifecycleWG.Done()
				s.runPeriodicScan(ctx)
			}()
		}
	})
}

// Wait blocks until all background goroutines launched by Start have exited and
// watcher resources have been closed. Call Wait only after Start has returned
// (or when Start was never called, in which case Wait returns immediately).
// Wait does not wait on direct external ScanNow calls.
func (s *Service) Wait() {
	s.lifecycleWG.Wait()
}

// ScanNow performs a synchronous full-library reconciliation serialized against
// live watcher mutations and concurrent ScanNow calls. It is safe to call
// concurrently and standalone without Start.
//
// ScanNow follows a two-tier error contract:
//   - Hard errors (ctx cancellation, missing/unreadable/non-directory musicDir,
//     or SQLite errors) return a wrapped non-nil error without mutating tracks
//     or updating LastScan.
//   - Recoverable safety-guard triggers (non-ENOENT subdirectory/entry walk or
//     extraction errors, or 0 audio files discovered while SQLite holds > 0
//     tracks) log a loud slog.Warn, skip all stale-row deletions while
//     committing valid upserts, refresh Count and LastScan, and return nil.
func (s *Service) ScanNow(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan music directory: %w", err)
	}

	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan music directory: %w", err)
	}

	s.scanning.Store(true)
	defer s.scanning.Store(false)

	walkRoot, err := resolveRootDir(s.musicDir)
	if err != nil {
		return err
	}

	snapshots, err := s.store.LoadSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("scanner: load snapshots: %w", err)
	}

	upserts, seen, hadWalkErrors, err := s.collectScanChanges(ctx, walkRoot, snapshots)
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan music directory %q: %w", walkRoot, err)
	}

	deleteRelPaths := s.selectStaleRelPaths(walkRoot, snapshots, seen, hadWalkErrors)
	if err := s.store.ApplyScanBatch(ctx, upserts, deleteRelPaths); err != nil {
		return fmt.Errorf("scanner: apply scan batch: %w", err)
	}

	// Refresh cached count using an uncanceled context so Status().Count always
	// matches the already-committed SQLite state.
	s.refreshCountLocked(context.WithoutCancel(ctx))
	s.lastScanNs.Store(time.Now().UTC().UnixNano())
	return nil
}

// Status returns a non-blocking, lock-free snapshot of current scan state
// assembled from individual atomic fields (fields are read independently
// without cross-field locking).
func (s *Service) Status() ScanStatus {
	var lastScan time.Time
	if ns := s.lastScanNs.Load(); ns != 0 {
		lastScan = time.Unix(0, ns).UTC()
	}
	return ScanStatus{
		Scanning: s.scanning.Load(),
		Count:    s.count.Load(),
		LastScan: lastScan,
	}
}

func (s *Service) seedTrackCount(ctx context.Context) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	s.refreshCountLocked(ctx)
}

func (s *Service) refreshCountLocked(ctx context.Context) {
	count, err := s.store.TrackCount(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("scanner: failed to refresh track count", "error", err)
		}
		return
	}
	s.count.Store(count)
}

func (s *Service) runLifecycle(ctx context.Context) {
	defer s.closeWatcherReady()
	defer s.closeInitialScanDone()

	var w *watcher
	if ctx.Err() == nil {
		if walkRoot, err := resolveRootDir(s.musicDir); err != nil {
			s.logger.Warn("scanner: live watcher disabled; music directory unavailable",
				"music_dir", s.musicDir,
				"error", err,
			)
		} else {
			w = s.startWatcher(ctx, walkRoot)
		}
	}

	if w != nil {
		s.activeWatcher.Store(w)
		defer func() {
			w.close()
			s.activeWatcher.Store(nil)
		}()
	}

	s.closeWatcherReady()

	if ctx.Err() == nil {
		if err := s.ScanNow(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
			s.logger.Warn("scanner: initial scan failed", "music_dir", s.musicDir, "error", err)
		}
	}

	s.closeInitialScanDone()

	if w == nil {
		return
	}
	<-ctx.Done()
}

func (s *Service) runPeriodicScan(ctx context.Context) {
	ticker := time.NewTicker(s.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.ScanNow(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
				s.logger.Warn("scanner: periodic scan failed", "music_dir", s.musicDir, "error", err)
			}
		}
	}
}

func (s *Service) closeWatcherReady() {
	s.watcherReadyOnce.Do(func() {
		close(s.watcherReady)
	})
}

func (s *Service) closeInitialScanDone() {
	s.initialScanDoneOnce.Do(func() {
		close(s.initialScanDone)
	})
}

func (s *Service) collectScanChanges(
	ctx context.Context,
	walkRoot string,
	snapshots map[string]store.TrackSnapshot,
) ([]store.Track, map[string]struct{}, bool, error) {
	var (
		upserts       []store.Track
		hadWalkErrors bool
	)
	seen := make(map[string]struct{}, len(snapshots))

	walkErr := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, entryErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entryErr != nil {
			if path == walkRoot {
				return fmt.Errorf("scanner: walk music directory %q: %w", walkRoot, entryErr)
			}
			// Ignore ENOENT on non-root entries because files/dirs can disappear mid-walk.
			if errors.Is(entryErr, os.ErrNotExist) {
				return nil
			}
			hadWalkErrors = true
			s.logger.Warn("scanner: walk entry error", "path", path, "error", entryErr)
			return nil
		}

		if path == walkRoot {
			if !d.IsDir() {
				return fmt.Errorf("scanner: music path %q is not a directory", walkRoot)
			}
			// Exempt the configured root itself from metadata.IsIgnoredName so
			// dot-prefixed roots (e.g., /tmp/.music) are still scanned.
			return nil
		}

		if d.IsDir() {
			if metadata.IsIgnoredName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip in-tree symlinks, FIFOs, sockets, and device nodes silently.
		if !d.Type().IsRegular() {
			return nil
		}
		if metadata.IsIgnoredName(d.Name()) {
			return nil
		}
		if _, _, ok := metadata.LookupFormat(d.Name()); !ok {
			return nil
		}

		relPath, ok := toRelPath(walkRoot, path)
		if !ok {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			hadWalkErrors = true
			s.logger.Warn("scanner: stat audio file failed", "path", path, "error", err)
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return nil
		}

		seen[relPath] = struct{}{}

		size := info.Size()
		mtimeNs := info.ModTime().UnixNano()
		if snap, exists := snapshots[relPath]; exists && snap.FileSize == size && snap.MtimeNs == mtimeNs {
			return nil
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		meta, err := s.extractFn(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				delete(seen, relPath)
				return nil
			}
			// Keep relPath in seen and mark walk errors so a transient read
			// failure never prunes an existing track row.
			hadWalkErrors = true
			s.logger.Warn("scanner: extract audio metadata failed", "path", path, "error", err)
			return nil
		}

		upserts = append(upserts, buildTrack(relPath, size, mtimeNs, meta))
		return nil
	})
	if walkErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, false, fmt.Errorf("scanner: scan music directory %q: %w", walkRoot, ctxErr)
		}
		return nil, nil, false, walkErr
	}

	return upserts, seen, hadWalkErrors, nil
}

func (s *Service) selectStaleRelPaths(
	walkRoot string,
	snapshots map[string]store.TrackSnapshot,
	seen map[string]struct{},
	hadWalkErrors bool,
) []string {
	if hadWalkErrors {
		s.logger.Warn("scanner: skipping stale track deletions due to walk or extraction errors",
			"music_dir", walkRoot,
			"indexed_tracks", len(snapshots),
			"discovered_tracks", len(seen),
		)
		return nil
	}
	if len(seen) == 0 && len(snapshots) > 0 {
		s.logger.Warn("scanner: skipping stale track deletions because music directory yielded zero audio files",
			"music_dir", walkRoot,
			"indexed_tracks", len(snapshots),
		)
		return nil
	}

	var deleteRelPaths []string
	for relPath := range snapshots {
		if _, ok := seen[relPath]; !ok {
			deleteRelPaths = append(deleteRelPaths, relPath)
		}
	}
	return deleteRelPaths
}

// watchSnapshot returns copies of tracked watch directories and fsnotify's
// underlying WatchList for white-box tests without nesting locks.
func (s *Service) watchSnapshot() ([]string, []string) {
	w := s.activeWatcher.Load()
	if w == nil {
		return nil, nil
	}

	w.watchMu.Lock()
	dirs := make([]string, 0, len(w.watchedDirs))
	for dir := range w.watchedDirs {
		dirs = append(dirs, dir)
	}
	w.watchMu.Unlock()

	return dirs, w.fsWatcher.WatchList()
}

// pendingCount returns the number of pending debounce timers for white-box
// tests without nesting locks.
func (s *Service) pendingCount() int {
	w := s.activeWatcher.Load()
	if w == nil {
		return 0
	}

	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	return len(w.pending)
}

func resolveRootDir(musicDir string) (string, error) {
	info, err := os.Stat(musicDir)
	if err != nil {
		return "", fmt.Errorf("scanner: stat music directory %q: %w", musicDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("scanner: music path %q is not a directory", musicDir)
	}

	resolved, err := filepath.EvalSymlinks(musicDir)
	if err != nil {
		return "", fmt.Errorf("scanner: resolve music directory %q: %w", musicDir, err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("scanner: stat resolved music directory %q: %w", resolved, err)
	}
	if !resolvedInfo.IsDir() {
		return "", fmt.Errorf("scanner: resolved music path %q is not a directory", resolved)
	}
	return resolved, nil
}

func toRelPath(rootDir, absPath string) (string, bool) {
	rel, err := filepath.Rel(rootDir, absPath)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || !fs.ValidPath(rel) {
		return "", false
	}
	return rel, true
}

func hasIgnoredSegment(relPath string) bool {
	for seg := range strings.SplitSeq(relPath, "/") {
		if metadata.IsIgnoredName(seg) {
			return true
		}
	}
	return false
}

func buildTrack(relPath string, fileSize, mtimeNs int64, info metadata.Info) store.Track {
	return store.Track{
		RelPath:     relPath,
		FileSize:    fileSize,
		MtimeNs:     mtimeNs,
		Title:       info.Title,
		Artist:      info.Artist,
		AlbumArtist: info.AlbumArtist,
		Album:       info.Album,
		TrackNum:    info.TrackNum,
		DiscNum:     info.DiscNum,
		Year:        info.Year,
		Genre:       info.Genre,
		DurationSec: info.DurationSec,
		BitRate:     info.BitRate,
	}
}
