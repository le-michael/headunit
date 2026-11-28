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

// Option configures optional settings for New.
type Option func(*serviceConfig)

type serviceConfig struct {
	uploadsDir string
}

// WithUploadsDir configures an optional second library root for uploaded tracks
// (store.SourceUploads). An empty or whitespace-only dir disables the uploads root.
func WithUploadsDir(dir string) Option {
	return func(c *serviceConfig) {
		c.uploadsDir = strings.TrimSpace(dir)
	}
}

type scanRoot struct {
	source string
	dir    string
}

// Service coordinates startup scanning, periodic reconciliation, live
// filesystem watching, and synchronous upload ingestion for Headunit's library
// root directories. All exported methods are safe for concurrent use, subject to
// the Start and Wait lifecycle rules.
type Service struct {
	musicDir     string
	uploadsDir   string
	roots        []scanRoot
	scanInterval time.Duration
	store        *store.Store
	logger       *slog.Logger

	// scanMu serializes SQLite store writes and cached count refreshes; it is a
	// strict leaf lock and must never be nested with watcher.watchMu or watcher.pendingMu.
	scanMu     sync.Mutex
	scanning   atomic.Bool
	count      atomic.Int64
	lastScanNs atomic.Int64

	startOnce      sync.Once
	lifecycleWG    sync.WaitGroup
	musicWatcher   atomic.Pointer[watcher]
	uploadsWatcher atomic.Pointer[watcher]

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

// New constructs a Service for musicDir (and optional configured roots such as
// WithUploadsDir) backed by st. It validates inputs and resolves root paths to
// clean absolute paths without touching disk or starting goroutines. A
// scanInterval of 0 disables periodic reconciliation; a nil logger defaults to
// slog.New(slog.DiscardHandler).
func New(musicDir string, scanInterval time.Duration, st *store.Store, logger *slog.Logger, opts ...Option) (*Service, error) {
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
	absMusicDir, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("scanner: resolve music directory %q: %w", musicDir, err)
	}

	var cfg serviceConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	roots := []scanRoot{{source: store.SourceMusic, dir: absMusicDir}}
	var absUploadsDir string
	if cfg.uploadsDir != "" {
		absUploadsDir, err = filepath.Abs(cfg.uploadsDir)
		if err != nil {
			return nil, fmt.Errorf("scanner: resolve uploads directory %q: %w", cfg.uploadsDir, err)
		}
		if isSameOrDescendant(absMusicDir, absUploadsDir) || isSameOrDescendant(absUploadsDir, absMusicDir) {
			return nil, fmt.Errorf("scanner: music directory %q and uploads directory %q must not overlap", absMusicDir, absUploadsDir)
		}
		roots = append(roots, scanRoot{source: store.SourceUploads, dir: absUploadsDir})
	}

	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Service{
		musicDir:             absMusicDir,
		uploadsDir:           absUploadsDir,
		roots:                roots,
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

// ScanNow performs a synchronous full-library reconciliation across all
// configured roots serialized against live watcher mutations, IngestUpload, and
// concurrent ScanNow calls. It is safe to call concurrently and standalone
// without Start.
//
// ScanNow evaluates each root independently under a two-tier error contract:
//   - Hard errors (ctx cancellation, missing/unreadable/non-directory root, or
//     SQLite errors) return a wrapped non-nil error without advancing LastScan.
//     If another root already committed in the same pass (without ctx
//     cancellation), Count is still refreshed to match SQLite.
//   - Recoverable safety-guard triggers (non-ENOENT subdirectory/entry walk or
//     extraction errors, or 0 audio files discovered while SQLite holds > 0
//     tracks in that root) are isolated per root: they log a loud slog.Warn,
//     skip stale-row deletions only for the affected root while committing valid
//     upserts, refresh Count and LastScan, and return nil.
func (s *Service) ScanNow(ctx context.Context) error {
	if ctx == nil {
		return errors.New("scanner: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan library: %w", err)
	}

	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan library: %w", err)
	}

	s.scanning.Store(true)
	defer s.scanning.Store(false)

	var (
		errs      []error
		committed bool
	)
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			if committed {
				s.refreshCountLocked(context.WithoutCancel(ctx))
			}
			return fmt.Errorf("scanner: scan %s directory: %w", root.source, err)
		}
		if err := s.scanSingleRootLocked(ctx, root); err != nil {
			if ctx.Err() != nil {
				if committed {
					s.refreshCountLocked(context.WithoutCancel(ctx))
				}
				return err
			}
			errs = append(errs, err)
			continue
		}
		committed = true
	}

	if committed {
		// Refresh cached count using an uncanceled context so Status().Count always
		// matches the already-committed SQLite state.
		s.refreshCountLocked(context.WithoutCancel(ctx))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	s.lastScanNs.Store(time.Now().UTC().UnixNano())
	return nil
}

// scanRoot reconciles a single root (invoked by watcher overflow recovery),
// preserving scanMu serialization, scanning indicator, count refresh, and
// LastScan advancement on success.
func (s *Service) scanRoot(ctx context.Context, root scanRoot) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan %s directory: %w", root.source, err)
	}

	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan %s directory: %w", root.source, err)
	}

	s.scanning.Store(true)
	defer s.scanning.Store(false)

	if err := s.scanSingleRootLocked(ctx, root); err != nil {
		return err
	}

	s.refreshCountLocked(context.WithoutCancel(ctx))
	s.lastScanNs.Store(time.Now().UTC().UnixNano())
	return nil
}

func (s *Service) scanSingleRootLocked(ctx context.Context, root scanRoot) error {
	walkRoot, err := resolveRootDir(root.source, root.dir)
	if err != nil {
		return err
	}

	snapshots, err := s.store.LoadSnapshots(ctx, root.source)
	if err != nil {
		return fmt.Errorf("scanner: load %s snapshots: %w", root.source, err)
	}

	upserts, seen, hadWalkErrors, err := s.collectScanChanges(ctx, root.source, walkRoot, snapshots)
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scanner: scan %s directory %q: %w", root.source, walkRoot, err)
	}

	deleteRelPaths := s.selectStaleRelPaths(root.source, walkRoot, snapshots, seen, hadWalkErrors)
	if err := s.store.ApplyScanBatch(ctx, root.source, upserts, deleteRelPaths); err != nil {
		return fmt.Errorf("scanner: apply %s scan batch: %w", root.source, err)
	}
	return nil
}

// IngestUpload validates relPath under the configured uploads root, extracts
// audio metadata, upserts the track with Source == store.SourceUploads under
// scanMu, refreshes the cached count, and returns the authoritative persisted Track.
func (s *Service) IngestUpload(ctx context.Context, relPath string) (*store.Track, error) {
	if s == nil {
		return nil, errors.New("scanner: service must not be nil")
	}
	if ctx == nil {
		return nil, errors.New("scanner: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("scanner: ingest upload %q: %w", relPath, err)
	}
	if s.uploadsDir == "" {
		return nil, errors.New("scanner: uploads directory is not configured")
	}
	if err := validateUploadRelPath(relPath); err != nil {
		return nil, err
	}

	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("scanner: ingest upload %q: %w", relPath, err)
	}

	walkRoot, err := resolveRootDir(store.SourceUploads, s.uploadsDir)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(walkRoot)
	if err != nil {
		return nil, fmt.Errorf("scanner: open uploads root %q: %w", walkRoot, err)
	}
	defer root.Close()

	osRelPath := filepath.FromSlash(relPath)
	info, err := root.Lstat(osRelPath)
	if err != nil {
		return nil, fmt.Errorf("scanner: stat uploaded file %q: %w", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("scanner: uploaded path %q is not a regular file", relPath)
	}
	if info.Size() <= 0 {
		return nil, fmt.Errorf("scanner: uploaded file %q is empty", relPath)
	}

	absPath := filepath.Join(walkRoot, osRelPath)
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, fmt.Errorf("scanner: resolve uploaded file %q: %w", relPath, err)
	}
	if resolvedPath != absPath {
		return nil, fmt.Errorf("scanner: uploaded path %q must not traverse symlinks", relPath)
	}

	meta, err := s.extractFn(absPath)
	if err != nil {
		return nil, fmt.Errorf("scanner: extract uploaded audio metadata %q: %w", relPath, err)
	}

	tr := buildTrack(store.SourceUploads, relPath, info.Size(), info.ModTime().UnixNano(), meta)
	if err := s.store.UpsertTrack(ctx, &tr); err != nil {
		return nil, fmt.Errorf("scanner: upsert uploaded track %q: %w", relPath, err)
	}

	postCtx := context.WithoutCancel(ctx)
	s.refreshCountLocked(postCtx)

	persisted, err := s.store.GetTrackBySourceRelPath(postCtx, store.SourceUploads, relPath)
	if err != nil {
		return nil, fmt.Errorf("scanner: reload uploaded track %q: %w", relPath, err)
	}
	return persisted, nil
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

func (s *Service) watcherPtr(source string) *atomic.Pointer[watcher] {
	switch source {
	case store.SourceUploads:
		return &s.uploadsWatcher
	default:
		return &s.musicWatcher
	}
}

func (s *Service) runLifecycle(ctx context.Context) {
	defer s.closeWatcherReady()
	defer s.closeInitialScanDone()

	var started []*watcher
	if ctx.Err() == nil {
		for _, root := range s.roots {
			if ctx.Err() != nil {
				break
			}
			walkRoot, err := resolveRootDir(root.source, root.dir)
			if err != nil {
				s.logger.Warn("scanner: live watcher disabled; "+root.source+" directory unavailable",
					"source", root.source,
					"dir", root.dir,
					"error", err,
				)
				continue
			}
			if w := s.startWatcher(ctx, root.source, root.dir, walkRoot); w != nil {
				s.watcherPtr(root.source).Store(w)
				started = append(started, w)
			}
		}
	}

	if len(started) > 0 {
		defer func() {
			for _, w := range started {
				w.close()
				s.watcherPtr(w.source).Store(nil)
			}
		}()
	}

	s.closeWatcherReady()

	if ctx.Err() == nil {
		if err := s.ScanNow(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
			s.logger.Warn("scanner: initial scan failed", "music_dir", s.musicDir, "uploads_dir", s.uploadsDir, "error", err)
		}
	}

	s.closeInitialScanDone()

	if len(started) == 0 {
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
				s.logger.Warn("scanner: periodic scan failed", "music_dir", s.musicDir, "uploads_dir", s.uploadsDir, "error", err)
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
	source string,
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
				return fmt.Errorf("scanner: walk %s directory %q: %w", source, walkRoot, entryErr)
			}
			// Ignore ENOENT on non-root entries because files/dirs can disappear mid-walk.
			if errors.Is(entryErr, os.ErrNotExist) {
				return nil
			}
			hadWalkErrors = true
			s.logger.Warn("scanner: walk entry error", "source", source, "path", path, "error", entryErr)
			return nil
		}

		if path == walkRoot {
			if !d.IsDir() {
				return fmt.Errorf("scanner: %s path %q is not a directory", source, walkRoot)
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
			s.logger.Warn("scanner: stat audio file failed", "source", source, "path", path, "error", err)
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
			s.logger.Warn("scanner: extract audio metadata failed", "source", source, "path", path, "error", err)
			return nil
		}

		upserts = append(upserts, buildTrack(source, relPath, size, mtimeNs, meta))
		return nil
	})
	if walkErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, false, fmt.Errorf("scanner: scan %s directory %q: %w", source, walkRoot, ctxErr)
		}
		return nil, nil, false, walkErr
	}

	return upserts, seen, hadWalkErrors, nil
}

func (s *Service) selectStaleRelPaths(
	source string,
	walkRoot string,
	snapshots map[string]store.TrackSnapshot,
	seen map[string]struct{},
	hadWalkErrors bool,
) []string {
	if hadWalkErrors {
		s.logger.Warn("scanner: skipping stale track deletions due to walk or extraction errors",
			"source", source,
			"dir", walkRoot,
			"indexed_tracks", len(snapshots),
			"discovered_tracks", len(seen),
		)
		return nil
	}
	if len(seen) == 0 && len(snapshots) > 0 {
		s.logger.Warn("scanner: skipping stale track deletions because "+source+" directory yielded zero audio files",
			"source", source,
			"dir", walkRoot,
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
// underlying WatchList for the music watcher in white-box tests without nesting locks.
func (s *Service) watchSnapshot() ([]string, []string) {
	return s.watchSnapshotFor(store.SourceMusic)
}

func (s *Service) watchSnapshotFor(source string) ([]string, []string) {
	w := s.watcherPtr(source).Load()
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

// pendingCount returns the total number of pending debounce timers across active
// watchers for white-box tests without nesting locks.
func (s *Service) pendingCount() int {
	var total int
	for _, ptr := range []*atomic.Pointer[watcher]{&s.musicWatcher, &s.uploadsWatcher} {
		w := ptr.Load()
		if w == nil {
			continue
		}
		w.pendingMu.Lock()
		total += len(w.pending)
		w.pendingMu.Unlock()
	}
	return total
}

func isSameOrDescendant(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func resolveRootDir(source, dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("scanner: stat %s directory %q: %w", source, dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("scanner: %s path %q is not a directory", source, dir)
	}

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("scanner: resolve %s directory %q: %w", source, dir, err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("scanner: stat resolved %s directory %q: %w", source, resolved, err)
	}
	if !resolvedInfo.IsDir() {
		return "", fmt.Errorf("scanner: resolved %s path %q is not a directory", source, resolved)
	}
	return resolved, nil
}

func validateUploadRelPath(relPath string) error {
	if !fs.ValidPath(relPath) || relPath == "." || strings.ContainsRune(relPath, '\x00') {
		return fmt.Errorf("scanner: invalid upload relative path %q", relPath)
	}
	if hasIgnoredSegment(relPath) {
		return fmt.Errorf("scanner: upload relative path %q contains an ignored segment", relPath)
	}
	if _, _, ok := metadata.LookupFormat(relPath); !ok {
		return fmt.Errorf("scanner: upload relative path %q has unsupported audio format", relPath)
	}
	return nil
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

func buildTrack(source, relPath string, fileSize, mtimeNs int64, info metadata.Info) store.Track {
	return store.Track{
		Source:      source,
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
