package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.senan.xyz/taglib"

	"headunit/internal/metadata"
	"headunit/internal/store"
	"headunit/internal/testutil"
)

const testWaitTimeout = 10 * time.Second

type capturedLog struct {
	Level   slog.Level
	Message string
	Text    string
}

type logCapture struct {
	mu      sync.Mutex
	entries []capturedLog
}

func newLogCapture() (*logCapture, *slog.Logger) {
	c := &logCapture{}
	return c, slog.New(c)
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool {
	return true
}

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(a.Value.String())
		return true
	})

	c.mu.Lock()
	c.entries = append(c.entries, capturedLog{
		Level:   r.Level,
		Message: r.Message,
		Text:    b.String(),
	})
	c.mu.Unlock()
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) countWarningsContaining(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	var count int
	for _, entry := range c.entries {
		if entry.Level == slog.LevelWarn && strings.Contains(entry.Text, substr) {
			count++
		}
	}
	return count
}

func (c *logCapture) allText() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var b strings.Builder
	for _, entry := range c.entries {
		b.WriteString(entry.Level.String())
		b.WriteByte(' ')
		b.WriteString(entry.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "headunit.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open(%q) unexpected error: %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close() unexpected error: %v", err)
		}
	})
	return st
}

func newTestService(t *testing.T, musicDir string, scanInterval time.Duration, st *store.Store, logger *slog.Logger, opts ...Option) *Service {
	t.Helper()

	svc, err := New(musicDir, scanInterval, st, logger, opts...)
	if err != nil {
		t.Fatalf("New(%q) unexpected error: %v", musicDir, err)
	}
	svc.debounceDelay = 25 * time.Millisecond
	svc.stabilityDelay = 15 * time.Millisecond
	return svc
}

func startAndWaitReady(t *testing.T, svc *Service) (context.Context, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		svc.Wait()
	})

	svc.Start(ctx)
	waitForChan(t, svc.watcherReady, testWaitTimeout, "watcherReady")
	waitForChan(t, svc.initialScanDone, testWaitTimeout, "initialScanDone")
	return ctx, cancel
}

func waitForChan(t *testing.T, ch <-chan struct{}, timeout time.Duration, name string) {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("timed out after %v waiting for %s", timeout, name)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for condition: %s", timeout, desc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func loadSnapshotsMap(t *testing.T, st *store.Store) map[string]store.TrackSnapshot {
	t.Helper()
	return loadSourceSnapshotsMap(t, st, store.SourceMusic)
}

func loadSourceSnapshotsMap(t *testing.T, st *store.Store, source string) map[string]store.TrackSnapshot {
	t.Helper()

	snaps, err := st.LoadSnapshots(context.Background(), source)
	if err != nil {
		t.Fatalf("LoadSnapshots(%q) unexpected error: %v", source, err)
	}
	return snaps
}

func mustGetTrackByRelPath(t *testing.T, st *store.Store, relPath string) store.Track {
	t.Helper()
	return mustGetTrackBySourceRelPath(t, st, store.SourceMusic, relPath)
}

func mustGetTrackBySourceRelPath(t *testing.T, st *store.Store, source, relPath string) store.Track {
	t.Helper()

	snaps := loadSourceSnapshotsMap(t, st, source)
	snap, ok := snaps[relPath]
	if !ok {
		t.Fatalf("snapshot for (%s, %q) not found in store (keys=%v)", source, relPath, mapKeys(snaps))
	}
	tr, err := st.GetTrack(context.Background(), snap.ID)
	if err != nil {
		t.Fatalf("GetTrack(%q) unexpected error: %v", snap.ID, err)
	}
	return *tr
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestNew_Validation(t *testing.T) {
	st := openTestStore(t)
	_, logger := newLogCapture()

	tests := []struct {
		name         string
		musicDir     string
		scanInterval time.Duration
		store        *store.Store
		logger       *slog.Logger
		wantErr      bool
	}{
		{
			name:         "empty musicDir rejected",
			musicDir:     "",
			scanInterval: time.Minute,
			store:        st,
			logger:       logger,
			wantErr:      true,
		},
		{
			name:         "whitespace musicDir rejected",
			musicDir:     "  \t\n ",
			scanInterval: time.Minute,
			store:        st,
			logger:       logger,
			wantErr:      true,
		},
		{
			name:         "negative scanInterval rejected",
			musicDir:     t.TempDir(),
			scanInterval: -1 * time.Millisecond,
			store:        st,
			logger:       logger,
			wantErr:      true,
		},
		{
			name:         "nil store rejected",
			musicDir:     t.TempDir(),
			scanInterval: time.Minute,
			store:        nil,
			logger:       logger,
			wantErr:      true,
		},
		{
			name:         "zero scanInterval and nil logger succeed",
			musicDir:     t.TempDir(),
			scanInterval: 0,
			store:        st,
			logger:       nil,
			wantErr:      false,
		},
		{
			name:         "relative non-existent musicDir normalized to absolute",
			musicDir:     " ./relative-music-dir ",
			scanInterval: 5 * time.Minute,
			store:        st,
			logger:       nil,
			wantErr:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(tc.musicDir, tc.scanInterval, tc.store, tc.logger)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%q, %v) error = %v, wantErr = %v", tc.musicDir, tc.scanInterval, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !filepath.IsAbs(svc.musicDir) {
				t.Errorf("svc.musicDir = %q, want absolute path", svc.musicDir)
			}
			if svc.logger == nil {
				t.Errorf("svc.logger = nil, want non-nil default logger")
			}
			if st := svc.Status(); st.Scanning || st.Count != 0 || !st.LastScan.IsZero() {
				t.Errorf("initial Status() = %+v, want zero status", st)
			}
		})
	}
}

func TestScanNow_RecursiveAndIgnored(t *testing.T) {
	t.Run("recursive flattening, formats, audio-named dir, and ignored entries", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()

		mp3Path := filepath.Join(musicDir, "Daft Punk", "Discovery", "01 - One More Time.mp3")
		testutil.WriteMinimalMP3(t, mp3Path)
		testutil.WriteTags(t, mp3Path, map[string][]string{
			taglib.Title:       {"One More Time"},
			taglib.Artist:      {"Daft Punk"},
			taglib.AlbumArtist: {"Daft Punk"},
			taglib.Album:       {"Discovery"},
			taglib.TrackNumber: {"1/14"},
			taglib.Date:        {"2001"},
			taglib.Genre:       {"Electronic"},
		})

		taggedWAV := filepath.Join(musicDir, "Various", "02 - Studio.wav")
		testutil.WriteMinimalWAV(t, taggedWAV)
		testutil.WriteTags(t, taggedWAV, map[string][]string{
			taglib.Title:       {"Studio Session"},
			taglib.Artist:      {"Wave Guest"},
			taglib.AlbumArtist: {"Various Artists"},
			taglib.Album:       {"Sessions"},
		})

		untaggedWAV := filepath.Join(musicDir, "Loose", "03 - Raw Wave.wav")
		testutil.WriteMinimalWAV(t, untaggedWAV)

		corruptMP3 := filepath.Join(musicDir, "Loose", "04 - Corrupt Audio.mp3")
		testutil.WriteBytes(t, corruptMP3, []byte("not real mp3 audio bytes"))

		audioNamedDirTrack := filepath.Join(musicDir, "MyMix.mp3", "01 - Inside.wav")
		testutil.WriteMinimalWAV(t, audioNamedDirTrack)

		// Ignored files, hidden dirs, unsupported formats, 0-byte audio, and symlinks.
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, ".DS_Store"))
		testutil.WriteMinimalMP3(t, filepath.Join(musicDir, "._Track.mp3"))
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, ".hidden", "song.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "song.flac.part"))
		testutil.WriteMinimalMP3(t, filepath.Join(musicDir, "song.mp3.tmp"))
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "song.wav.filepart"))
		testutil.WriteMinimalMP3(t, filepath.Join(musicDir, "song.m4a.crdownload"))
		testutil.WriteBytes(t, filepath.Join(musicDir, "notes.txt"), []byte("liner notes"))
		testutil.WriteBytes(t, filepath.Join(musicDir, "empty.wav"), nil)

		outsideTarget := filepath.Join(t.TempDir(), "outside.wav")
		testutil.WriteMinimalWAV(t, outsideTarget)
		if err := os.Symlink(outsideTarget, filepath.Join(musicDir, "linked-file.wav")); err != nil {
			t.Fatalf("os.Symlink(file): %v", err)
		}
		outsideDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(outsideDir, "inside-linked-dir.wav"))
		if err := os.Symlink(outsideDir, filepath.Join(musicDir, "linked-dir")); err != nil {
			t.Fatalf("os.Symlink(dir): %v", err)
		}
		if err := os.Symlink(filepath.Join(musicDir, "missing-target.mp3"), filepath.Join(musicDir, "broken-link.mp3")); err != nil {
			t.Fatalf("os.Symlink(broken): %v", err)
		}

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() unexpected error: %v", err)
		}

		wantRelPaths := []string{
			"Daft Punk/Discovery/01 - One More Time.mp3",
			"Loose/03 - Raw Wave.wav",
			"Loose/04 - Corrupt Audio.mp3",
			"MyMix.mp3/01 - Inside.wav",
			"Various/02 - Studio.wav",
		}
		snaps := loadSnapshotsMap(t, st)
		gotRelPaths := mapKeys(snaps)
		if !slices.Equal(gotRelPaths, wantRelPaths) {
			t.Fatalf("indexed relPaths = %v, want %v", gotRelPaths, wantRelPaths)
		}
		if got := svc.Status().Count; got != int64(len(wantRelPaths)) {
			t.Errorf("Status().Count = %d, want %d", got, len(wantRelPaths))
		}
		if logs.countWarningsContaining("") != 0 {
			t.Errorf("unexpected warning logs:\n%s", logs.allText())
		}

		mp3Track := mustGetTrackByRelPath(t, st, "Daft Punk/Discovery/01 - One More Time.mp3")
		if mp3Track.Title != "One More Time" || mp3Track.Artist != "Daft Punk" || mp3Track.Album != "Discovery" ||
			mp3Track.TrackNum != 1 || mp3Track.Year != 2001 || mp3Track.Genre != "Electronic" ||
			mp3Track.DurationSec != 2 || mp3Track.BitRate != 128 {
			t.Errorf("tagged MP3 track = %+v, want expected tags and properties", mp3Track)
		}

		untaggedTrack := mustGetTrackByRelPath(t, st, "Loose/03 - Raw Wave.wav")
		if untaggedTrack.Title != "Raw Wave" || untaggedTrack.Artist != "Unknown Artist" ||
			untaggedTrack.Album != "Unknown Album" || untaggedTrack.DurationSec != 2 {
			t.Errorf("untagged WAV track = %+v, want filename fallback + duration", untaggedTrack)
		}

		corruptTrack := mustGetTrackByRelPath(t, st, "Loose/04 - Corrupt Audio.mp3")
		if corruptTrack.Title != "Corrupt Audio" || corruptTrack.Artist != "Unknown Artist" ||
			corruptTrack.Album != "Unknown Album" {
			t.Errorf("corrupt MP3 track = %+v, want filename fallback", corruptTrack)
		}
	})

	t.Run("dot-prefixed root directory and symlinked root directory", func(t *testing.T) {
		st := openTestStore(t)
		parent := t.TempDir()
		dotRoot := filepath.Join(parent, ".music-root")
		testutil.WriteMinimalWAV(t, filepath.Join(dotRoot, "Sub", "01 - Dot Root Track.wav"))

		rootLink := filepath.Join(parent, "music-symlink")
		if err := os.Symlink(dotRoot, rootLink); err != nil {
			t.Fatalf("os.Symlink(%q, %q): %v", dotRoot, rootLink, err)
		}

		svcDot := newTestService(t, dotRoot, 0, st, nil)
		if err := svcDot.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow(dotRoot) unexpected error: %v", err)
		}
		want := []string{"Sub/01 - Dot Root Track.wav"}
		if got := mapKeys(loadSnapshotsMap(t, st)); !slices.Equal(got, want) {
			t.Fatalf("dotRoot indexed = %v, want %v", got, want)
		}

		// Scanning via symlinked root keeps exact same RelPath and IDs.
		orig := mustGetTrackByRelPath(t, st, "Sub/01 - Dot Root Track.wav")
		svcLink := newTestService(t, rootLink, 0, st, nil)
		if err := svcLink.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow(rootLink) unexpected error: %v", err)
		}
		after := mustGetTrackByRelPath(t, st, "Sub/01 - Dot Root Track.wav")
		if after.ID != orig.ID || after.RelPath != orig.RelPath {
			t.Errorf("after symlink root scan = (%q, %q), want (%q, %q)", after.ID, after.RelPath, orig.ID, orig.RelPath)
		}
	})
}

func TestScanNow_FastPathAndIncremental(t *testing.T) {
	st := openTestStore(t)
	musicDir := t.TempDir()

	p1 := filepath.Join(musicDir, "01 - Alpha.wav")
	p2 := filepath.Join(musicDir, "02 - Beta.wav")
	testutil.WriteMinimalWAV(t, p1)
	testutil.WriteMinimalWAV(t, p2)
	testutil.WriteTags(t, p1, map[string][]string{taglib.Title: {"Alpha v1"}})
	testutil.WriteTags(t, p2, map[string][]string{taglib.Title: {"Beta v1"}})

	svc := newTestService(t, musicDir, 0, st, nil)
	var extractCalls atomic.Int32
	svc.extractFn = func(path string) (metadata.Info, error) {
		extractCalls.Add(1)
		return metadata.Extract(path)
	}

	if got := svc.Status(); !got.LastScan.IsZero() || got.Count != 0 || got.Scanning {
		t.Fatalf("initial Status() = %+v, want zero state", got)
	}

	// First scan extracts both tracks.
	if err := svc.ScanNow(context.Background()); err != nil {
		t.Fatalf("first ScanNow() unexpected error: %v", err)
	}
	if got := extractCalls.Load(); got != 2 {
		t.Errorf("first scan extractCalls = %d, want 2", got)
	}
	status1 := svc.Status()
	if status1.Count != 2 || status1.LastScan.IsZero() || status1.LastScan.Location() != time.UTC {
		t.Errorf("after first scan Status() = %+v, want Count=2 and UTC LastScan", status1)
	}

	// Second unchanged scan hits fast-path and calls extractFn 0 times.
	extractCalls.Store(0)
	if err := svc.ScanNow(context.Background()); err != nil {
		t.Fatalf("second ScanNow() unexpected error: %v", err)
	}
	if got := extractCalls.Load(); got != 0 {
		t.Errorf("unchanged scan extractCalls = %d, want 0", got)
	}

	// Backdate track 1 CreatedAt directly via store while matching size/mtime, then
	// verify in-place edit re-extracts only track 1 and preserves ID + CreatedAt.
	orig1 := mustGetTrackByRelPath(t, st, "01 - Alpha.wav")
	backdated := orig1
	backdated.CreatedAt = time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	if _, err := st.LoadSnapshots(context.Background(), store.SourceMusic); err != nil {
		t.Fatal(err)
	}
	// Replace row with explicit older CreatedAt to test preservation across edits.
	if err := st.DeleteByRelPath(context.Background(), store.SourceMusic, orig1.RelPath); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrack(context.Background(), &backdated); err != nil {
		t.Fatal(err)
	}
	orig1 = mustGetTrackByRelPath(t, st, "01 - Alpha.wav")

	testutil.WriteTags(t, p1, map[string][]string{taglib.Title: {"Alpha v2 Updated"}})
	extractCalls.Store(0)
	if err := svc.ScanNow(context.Background()); err != nil {
		t.Fatalf("incremental edit ScanNow() unexpected error: %v", err)
	}
	if got := extractCalls.Load(); got != 1 {
		t.Errorf("incremental edit extractCalls = %d, want 1", got)
	}
	updated1 := mustGetTrackByRelPath(t, st, "01 - Alpha.wav")
	if updated1.ID != orig1.ID || !updated1.CreatedAt.Equal(orig1.CreatedAt) {
		t.Errorf("preserved identity = (ID=%q, CreatedAt=%v), want (%q, %v)",
			updated1.ID, updated1.CreatedAt, orig1.ID, orig1.CreatedAt)
	}
	if updated1.Title != "Alpha v2 Updated" {
		t.Errorf("updated Title = %q, want %q", updated1.Title, "Alpha v2 Updated")
	}

	// Delete 1 of 2 files and verify only the deleted row is pruned.
	if err := os.Remove(p2); err != nil {
		t.Fatalf("os.Remove(%q): %v", p2, err)
	}
	extractCalls.Store(0)
	if err := svc.ScanNow(context.Background()); err != nil {
		t.Fatalf("delete ScanNow() unexpected error: %v", err)
	}
	if got := extractCalls.Load(); got != 0 {
		t.Errorf("delete scan extractCalls = %d, want 0", got)
	}
	if got := mapKeys(loadSnapshotsMap(t, st)); !slices.Equal(got, []string{"01 - Alpha.wav"}) {
		t.Errorf("remaining tracks = %v, want [01 - Alpha.wav]", got)
	}
	if got := svc.Status().Count; got != 1 {
		t.Errorf("Status().Count = %d, want 1", got)
	}
}

func TestScanNow_SafetyGuards(t *testing.T) {
	t.Run("empty-mount guard preserves DB tracks and logs WARN", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		p1 := filepath.Join(musicDir, "01 - One.wav")
		p2 := filepath.Join(musicDir, "02 - Two.wav")
		testutil.WriteMinimalWAV(t, p1)
		testutil.WriteMinimalWAV(t, p2)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		// Simulate unmounted empty directory.
		if err := os.Remove(p1); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p2); err != nil {
			t.Fatal(err)
		}

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("empty-mount ScanNow() unexpected error: %v", err)
		}
		if got := len(loadSnapshotsMap(t, st)); got != 2 {
			t.Errorf("store track count = %d, want 2 preserved", got)
		}
		if got := svc.Status().Count; got != 2 {
			t.Errorf("Status().Count = %d, want 2", got)
		}
		if logs.countWarningsContaining("zero audio files") != 1 {
			t.Errorf("expected empty-mount WARN log, got:\n%s", logs.allText())
		}
	})

	t.Run("empty DB and empty directory succeeds without warning", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() on empty library unexpected error: %v", err)
		}
		if got := svc.Status(); got.Count != 0 || got.LastScan.IsZero() {
			t.Errorf("Status() = %+v, want Count=0 and non-zero LastScan", got)
		}
		if logs.countWarningsContaining("") != 0 {
			t.Errorf("expected zero WARN logs on empty library, got:\n%s", logs.allText())
		}
	})

	t.Run("walk error chmod 0000 subdir skips deletions while committing healthy upserts", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0o000 walk error test skipped when running as root (UID 0)")
		}

		st := openTestStore(t)
		musicDir := t.TempDir()
		blockedDir := filepath.Join(musicDir, "blocked")
		testutil.WriteMinimalWAV(t, filepath.Join(blockedDir, "01 - HiddenByPerm.wav"))
		removedFile := filepath.Join(musicDir, "02 - StaleIfHealthy.wav")
		testutil.WriteMinimalWAV(t, removedFile)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		if err := os.Chmod(blockedDir, 0o000); err != nil {
			t.Fatalf("os.Chmod(%q): %v", blockedDir, err)
		}
		t.Cleanup(func() { _ = os.Chmod(blockedDir, 0o755) })

		if err := os.Remove(removedFile); err != nil {
			t.Fatal(err)
		}
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "03 - HealthyNew.wav"))

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() with chmod 0000 subdir unexpected error: %v", err)
		}

		got := mapKeys(loadSnapshotsMap(t, st))
		want := []string{
			"02 - StaleIfHealthy.wav",
			"03 - HealthyNew.wav",
			"blocked/01 - HiddenByPerm.wav",
		}
		if !slices.Equal(got, want) {
			t.Errorf("tracks = %v, want %v", got, want)
		}
		if logs.countWarningsContaining("skipping stale track deletions") != 1 {
			t.Errorf("expected pruning-skipped WARN log, got:\n%s", logs.allText())
		}
	})

	t.Run("extractFn OS error commits healthy upserts and skips deletions", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()

		badFile := filepath.Join(musicDir, "01 - ReadErr.wav")
		removedFile := filepath.Join(musicDir, "02 - Removed.wav")
		testutil.WriteMinimalWAV(t, badFile)
		testutil.WriteMinimalWAV(t, removedFile)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		if err := os.Remove(removedFile); err != nil {
			t.Fatal(err)
		}
		// Touch badFile so fast-path does not skip it, and add a healthy new track.
		testutil.WriteMinimalWAV(t, badFile)
		testutil.WriteTags(t, badFile, map[string][]string{taglib.Title: {"Changed"}})
		healthyNew := filepath.Join(musicDir, "03 - Healthy.wav")
		testutil.WriteMinimalWAV(t, healthyNew)

		svc.extractFn = func(path string) (metadata.Info, error) {
			if path == badFile {
				return metadata.Info{}, fmt.Errorf("read %q: %w", path, syscall.EIO)
			}
			return metadata.Extract(path)
		}

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() with extract EIO unexpected error: %v", err)
		}
		got := mapKeys(loadSnapshotsMap(t, st))
		want := []string{"01 - ReadErr.wav", "02 - Removed.wav", "03 - Healthy.wav"}
		if !slices.Equal(got, want) {
			t.Errorf("tracks = %v, want %v", got, want)
		}
		if logs.countWarningsContaining("skipping stale track deletions") != 1 {
			t.Errorf("expected pruning-skipped WARN log, got:\n%s", logs.allText())
		}
	})

	t.Run("mid-walk ENOENT does not trip hadWalkErrors and allows pruning", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()

		keepFile := filepath.Join(musicDir, "01 - Keep.wav")
		vanishingFile := filepath.Join(musicDir, "02 - Vanish.wav")
		testutil.WriteMinimalWAV(t, keepFile)
		testutil.WriteMinimalWAV(t, vanishingFile)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		// Modify vanishingFile so extractFn is invoked, then delete it during extractFn.
		testutil.WriteTags(t, vanishingFile, map[string][]string{taglib.Title: {"Modified Before Vanish"}})
		svc.extractFn = func(path string) (metadata.Info, error) {
			if path == vanishingFile {
				_ = os.Remove(vanishingFile)
				return metadata.Info{}, fmt.Errorf("open %q: %w", path, os.ErrNotExist)
			}
			return metadata.Extract(path)
		}

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() with mid-walk ENOENT unexpected error: %v", err)
		}
		if got := mapKeys(loadSnapshotsMap(t, st)); !slices.Equal(got, []string{"01 - Keep.wav"}) {
			t.Errorf("tracks = %v, want [01 - Keep.wav]", got)
		}
		if logs.countWarningsContaining("") != 0 {
			t.Errorf("expected zero WARN logs on concurrent ENOENT, got:\n%s", logs.allText())
		}
	})

	t.Run("missing or non-directory root returns error without mutating DB or LastScan", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - Track.wav"))

		svc := newTestService(t, musicDir, 0, st, nil)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}
		before := svc.Status()

		if err := os.RemoveAll(musicDir); err != nil {
			t.Fatal(err)
		}
		if err := svc.ScanNow(context.Background()); err == nil {
			t.Fatalf("ScanNow() on missing root succeeded, want error")
		}

		// Also verify non-directory path returns error.
		fileRoot := filepath.Join(t.TempDir(), "not-a-dir")
		testutil.WriteBytes(t, fileRoot, []byte("file"))
		svcFile := newTestService(t, fileRoot, 0, st, nil)
		if err := svcFile.ScanNow(context.Background()); err == nil {
			t.Fatalf("ScanNow() on non-directory root succeeded, want error")
		}

		after := svc.Status()
		if !after.LastScan.Equal(before.LastScan) || after.Count != before.Count || len(loadSnapshotsMap(t, st)) != 1 {
			t.Errorf("after failed ScanNow Status=%+v, want unchanged %+v", after, before)
		}
	})

	t.Run("pre-canceled and mid-walk canceled ctx abort without mutating DB or LastScan", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - Seed.wav"))

		svc := newTestService(t, musicDir, 0, st, nil)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}
		before := svc.Status()

		// 1. Pre-canceled context fails immediately without scanning new files.
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "02 - Unscanned.wav"))
		preCanceledCtx, preCancel := context.WithCancel(context.Background())
		preCancel()
		if err := svc.ScanNow(preCanceledCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("ScanNow(preCanceledCtx) error = %v, want context.Canceled", err)
		}
		afterPre := svc.Status()
		if afterPre.Scanning || afterPre.Count != before.Count || !afterPre.LastScan.Equal(before.LastScan) {
			t.Errorf("after pre-canceled ScanNow Status=%+v, want Scanning=false and unchanged %+v", afterPre, before)
		}
		if got := mapKeys(loadSnapshotsMap(t, st)); !slices.Equal(got, []string{"01 - Seed.wav"}) {
			t.Errorf("after pre-canceled ScanNow tracks = %v, want [01 - Seed.wav]", got)
		}

		// 2. Mid-walk cancellation during extractFn aborts before ApplyScanBatch.
		walkCtx, walkCancel := context.WithCancel(context.Background())
		defer walkCancel()
		svc.extractFn = func(path string) (metadata.Info, error) {
			if filepath.Base(path) == "02 - Unscanned.wav" {
				walkCancel()
			}
			return metadata.Extract(path)
		}
		if err := svc.ScanNow(walkCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("ScanNow(mid-walk canceled) error = %v, want context.Canceled", err)
		}
		afterWalk := svc.Status()
		if afterWalk.Scanning || afterWalk.Count != before.Count || !afterWalk.LastScan.Equal(before.LastScan) {
			t.Errorf("after mid-walk canceled ScanNow Status=%+v, want Scanning=false and unchanged %+v", afterWalk, before)
		}
		if got := mapKeys(loadSnapshotsMap(t, st)); !slices.Equal(got, []string{"01 - Seed.wav"}) {
			t.Errorf("after mid-walk canceled ScanNow tracks = %v, want [01 - Seed.wav]", got)
		}
	})
}

func TestWatcher_LiveLifecycle(t *testing.T) {
	st := openTestStore(t)
	musicDir := t.TempDir()
	logs, logger := newLogCapture()

	svc := newTestService(t, musicDir, 0, st, logger)

	var extractCalls atomic.Int32
	svc.extractFn = func(path string) (metadata.Info, error) {
		extractCalls.Add(1)
		return metadata.Extract(path)
	}

	var (
		fwMu       sync.Mutex
		capturedFW *fsnotify.Watcher
	)
	svc.newWatcherFn = func() (*fsnotify.Watcher, error) {
		fw, err := fsnotify.NewWatcher()
		if err == nil {
			fwMu.Lock()
			capturedFW = fw
			fwMu.Unlock()
		}
		return fw, err
	}

	ctx, _ := startAndWaitReady(t, svc)
	walkRoot, err := filepath.EvalSymlinks(musicDir)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Nested mkdir -p + immediate file drop.
	nestedFile := filepath.Join(musicDir, "a", "b", "01 - Nested.wav")
	if err := os.MkdirAll(filepath.Dir(nestedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.WriteMinimalWAV(t, nestedFile)
	waitForCondition(t, testWaitTimeout, "nested track indexed", func() bool {
		_, ok := loadSnapshotsMap(t, st)["a/b/01 - Nested.wav"]
		return ok && svc.Status().Count == 1
	})

	// 2. Chmod-only event is a no-op (no extra extraction).
	beforeChmod := extractCalls.Load()
	if err := os.Chmod(nestedFile, 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Atomic .tmp write, ignored names/dirs, and in-tree symlinks are ignored until renamed to .mp3.
	tmpFile := filepath.Join(musicDir, "02 - Atomic.mp3.tmp")
	finalMP3 := filepath.Join(musicDir, "02 - Atomic.mp3")
	testutil.WriteMinimalMP3(t, tmpFile)
	testutil.WriteTags(t, tmpFile, map[string][]string{taglib.Title: {"Atomic Title"}})
	testutil.WriteMinimalMP3(t, filepath.Join(musicDir, "._Ignored.mp3"))
	testutil.WriteMinimalWAV(t, filepath.Join(musicDir, ".hidden", "ignored.wav"))

	outsideFile := filepath.Join(t.TempDir(), "outside.wav")
	testutil.WriteMinimalWAV(t, outsideFile)
	if err := os.Symlink(outsideFile, filepath.Join(musicDir, "linked-file.wav")); err != nil {
		t.Fatalf("os.Symlink(file): %v", err)
	}
	outsideDir := t.TempDir()
	testutil.WriteMinimalWAV(t, filepath.Join(outsideDir, "outside.wav"))
	if err := os.Symlink(outsideDir, filepath.Join(musicDir, "linked-dir")); err != nil {
		t.Fatalf("os.Symlink(dir): %v", err)
	}

	if got := extractCalls.Load(); got != beforeChmod {
		t.Errorf("extractCalls after chmod + ignored/symlink writes = %d, want %d", got, beforeChmod)
	}
	if err := os.Rename(tmpFile, finalMP3); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, testWaitTimeout, "atomic renamed MP3 indexed", func() bool {
		_, ok := loadSnapshotsMap(t, st)["02 - Atomic.mp3"]
		return ok && svc.Status().Count == 2
	})
	if got := extractCalls.Load(); got != beforeChmod+1 {
		t.Errorf("extractCalls after atomic MP3 index = %d, want %d (chmod, .tmp, ignored, and symlinks should not extract)", got, beforeChmod+1)
	}
	snapsAfterAtomic := loadSnapshotsMap(t, st)
	for _, unexpected := range []string{"._Ignored.mp3", ".hidden/ignored.wav", "linked-file.wav", "linked-dir/outside.wav"} {
		if _, exists := snapsAfterAtomic[unexpected]; exists {
			t.Errorf("unexpected ignored/symlinked track indexed: %q", unexpected)
		}
	}
	watched, wl := svc.watchSnapshot()
	for _, unexpectedDir := range []string{filepath.Join(walkRoot, ".hidden"), filepath.Join(walkRoot, "linked-dir")} {
		if slices.Contains(watched, unexpectedDir) || slices.Contains(wl, unexpectedDir) {
			t.Errorf("unexpected watch registered for %q (watched=%v, wl=%v)", unexpectedDir, watched, wl)
		}
	}
	if tr := mustGetTrackByRelPath(t, st, "02 - Atomic.mp3"); tr.Title != "Atomic Title" {
		t.Errorf("atomic MP3 Title = %q, want %q", tr.Title, "Atomic Title")
	}

	// 4. In-place edit preserves ID and CreatedAt.
	origMP3 := mustGetTrackByRelPath(t, st, "02 - Atomic.mp3")
	testutil.WriteTags(t, finalMP3, map[string][]string{taglib.Title: {"Atomic Title Edited"}})
	waitForCondition(t, testWaitTimeout, "in-place edit reflected in store", func() bool {
		return mustGetTrackByRelPath(t, st, "02 - Atomic.mp3").Title == "Atomic Title Edited"
	})
	editedMP3 := mustGetTrackByRelPath(t, st, "02 - Atomic.mp3")
	if editedMP3.ID != origMP3.ID || !editedMP3.CreatedAt.Equal(origMP3.CreatedAt) {
		t.Errorf("edited identity = (%q, %v), want (%q, %v)",
			editedMP3.ID, editedMP3.CreatedAt, origMP3.ID, origMP3.CreatedAt)
	}

	// 5. Audio-named directory created and removed (renamed out of tree -> DeleteByDirPrefix).
	audioNamedDir := filepath.Join(musicDir, "AudioNamedDir.mp3")
	audioNamedChild := filepath.Join(audioNamedDir, "01 - Track.wav")
	testutil.WriteMinimalWAV(t, audioNamedChild)
	waitForCondition(t, testWaitTimeout, "audio-named dir child indexed", func() bool {
		snaps := loadSnapshotsMap(t, st)
		_, childOK := snaps["AudioNamedDir.mp3/01 - Track.wav"]
		_, dirOK := snaps["AudioNamedDir.mp3"]
		return childOK && !dirOK && svc.Status().Count == 3
	})
	movedOut := filepath.Join(t.TempDir(), "AudioNamedDir.mp3")
	if err := os.Rename(audioNamedDir, movedOut); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, testWaitTimeout, "audio-named dir removed via DeleteByDirPrefix", func() bool {
		_, childOK := loadSnapshotsMap(t, st)["AudioNamedDir.mp3/01 - Track.wav"]
		return !childOK && svc.Status().Count == 2
	})

	// 6. Synthetic root Remove/Rename guard preserves DB rows and logs WARN.
	fwMu.Lock()
	fw := capturedFW
	fwMu.Unlock()
	if fw == nil {
		t.Fatalf("capturedFW is nil")
	}
	fw.Events <- fsnotify.Event{Name: walkRoot, Op: fsnotify.Remove}
	fw.Events <- fsnotify.Event{Name: walkRoot, Op: fsnotify.Rename}
	waitForCondition(t, testWaitTimeout, "root remove/rename warnings logged", func() bool {
		return logs.countWarningsContaining("music root directory") >= 2
	})
	if got := svc.Status().Count; got != 2 {
		t.Errorf("Count after synthetic root Remove/Rename = %d, want 2", got)
	}

	// 7. Last-file deletions prune cleanly down to 0, and subsequent ScanNow stays 0.
	if err := os.Remove(nestedFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(finalMP3); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, testWaitTimeout, "last-file deletions prune count to 0", func() bool {
		return svc.Status().Count == 0 && len(loadSnapshotsMap(t, st)) == 0
	})
	if err := svc.ScanNow(ctx); err != nil {
		t.Fatalf("ScanNow() after last-file deletion unexpected error: %v", err)
	}
	if got := svc.Status().Count; got != 0 {
		t.Errorf("Status().Count after ScanNow = %d, want 0", got)
	}
	if got := logs.countWarningsContaining("zero audio files"); got != 0 {
		t.Errorf("unexpected empty-mount warning after live last-file deletion (%d)", got)
	}
}

func TestWatcher_PartialWriteStability(t *testing.T) {
	t.Run("growing file waits until stable", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		svc := newTestService(t, musicDir, 0, st, nil)
		svc.debounceDelay = 30 * time.Millisecond
		svc.stabilityDelay = 15 * time.Millisecond

		fullWAV := testutil.MinimalWAVBytes()
		chunk1End := 44 + 8000
		chunk2End := 44 + 20000

		var (
			statCalls          atomic.Int32
			indexedWhileSaving atomic.Bool
		)
		svc.onStabilityStat = func(absPath string, _ int) {
			n := statCalls.Add(1)
			if n == 1 || n == 2 {
				cnt, err := st.TrackCount(context.Background())
				if err == nil && cnt != 0 {
					indexedWhileSaving.Store(true)
				}
				var chunk []byte
				if n == 1 {
					chunk = fullWAV[chunk1End:chunk2End]
				} else {
					chunk = fullWAV[chunk2End:]
				}
				f, err := os.OpenFile(absPath, os.O_WRONLY|os.O_APPEND, 0o600)
				if err == nil {
					_, _ = f.Write(chunk)
					_ = f.Close()
				}
			}
		}

		startAndWaitReady(t, svc)

		growingPath := filepath.Join(musicDir, "growing.wav")
		testutil.WriteBytes(t, growingPath, fullWAV[:chunk1End])

		waitForCondition(t, testWaitTimeout, "growing file indexed once stable", func() bool {
			snap, ok := loadSnapshotsMap(t, st)["growing.wav"]
			return ok && snap.FileSize == int64(len(fullWAV)) && svc.Status().Count == 1
		})

		if indexedWhileSaving.Load() {
			t.Errorf("file was indexed while still growing")
		}
		if got := statCalls.Load(); got < 3 {
			t.Errorf("onStabilityStat calls = %d, want >= 3", got)
		}
	})

	t.Run("zero-byte file hits maxStabilityAttempts then indexes after valid write", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		logs, logger := newLogCapture()

		svc := newTestService(t, musicDir, 0, st, logger)
		svc.debounceDelay = 20 * time.Millisecond
		svc.stabilityDelay = 10 * time.Millisecond
		svc.maxStabilityAttempts = 2

		startAndWaitReady(t, svc)

		zeroPath := filepath.Join(musicDir, "01 - Zero.wav")
		testutil.WriteBytes(t, zeroPath, nil)

		waitForCondition(t, testWaitTimeout, "zero-byte file hits maxStabilityAttempts", func() bool {
			return logs.countWarningsContaining("did not stabilize") >= 1
		})
		if got := svc.Status().Count; got != 0 || len(loadSnapshotsMap(t, st)) != 0 {
			t.Fatalf("zero-byte file was indexed unexpectedly: count=%d", got)
		}

		// Writing valid WAV bytes fires a fresh Write event resetting attempts to 0.
		testutil.WriteMinimalWAV(t, zeroPath)
		waitForCondition(t, testWaitTimeout, "zero-byte file indexed after valid WAV write", func() bool {
			snap, ok := loadSnapshotsMap(t, st)["01 - Zero.wav"]
			return ok && snap.FileSize == int64(len(testutil.MinimalWAVBytes())) && svc.Status().Count == 1
		})
	})

	t.Run("mid-parse mutation re-schedules unlocked and indexes final metadata", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		svc := newTestService(t, musicDir, 0, st, nil)

		staged := filepath.Join(musicDir, "01 - Mutate.wav.tmp")
		target := filepath.Join(musicDir, "01 - Mutate.wav")
		testutil.WriteMinimalWAV(t, staged)
		testutil.WriteTags(t, staged, map[string][]string{taglib.Title: {"Before Mutation"}})

		firstIngestDone := make(chan struct{})
		var firstIngestOnce sync.Once
		svc.onIngestDone = func(relPath string) {
			if relPath == "01 - Mutate.wav" {
				firstIngestOnce.Do(func() {
					close(firstIngestDone)
				})
			}
		}

		allowSecondExtract := make(chan struct{})
		var allowOnce sync.Once
		closeAllow := func() {
			allowOnce.Do(func() {
				close(allowSecondExtract)
			})
		}

		var extractCalls atomic.Int32
		svc.extractFn = func(path string) (metadata.Info, error) {
			if filepath.Base(path) == "01 - Mutate.wav" {
				switch extractCalls.Add(1) {
				case 1:
					info, err := metadata.Extract(path)
					if err != nil {
						return metadata.Info{}, err
					}
					// Rewrite file with different tags and byte length while extractFn runs.
					if err := taglib.WriteTags(path, map[string][]string{taglib.Title: {"After Mid-Parse Mutation"}}, taglib.Clear); err != nil {
						return metadata.Info{}, err
					}
					return info, nil
				case 2:
					<-allowSecondExtract
				}
			}
			return metadata.Extract(path)
		}

		startAndWaitReady(t, svc)
		// Registered after startAndWaitReady so LIFO cleanup releases a gated worker
		// before cancel() + svc.Wait() if an assertion fails early.
		t.Cleanup(closeAllow)

		if err := os.Rename(staged, target); err != nil {
			t.Fatalf("os.Rename(%q, %q): %v", staged, target, err)
		}

		waitForChan(t, firstIngestDone, testWaitTimeout, "first ingestFile completion")
		if got := len(loadSnapshotsMap(t, st)); got != 0 {
			t.Fatalf("store snapshots after aborted mid-parse ingest = %d, want 0 (Before Mutation must not be upserted)", got)
		}
		if got := svc.Status().Count; got != 0 {
			t.Fatalf("Status().Count after aborted mid-parse ingest = %d, want 0", got)
		}

		closeAllow()

		waitForCondition(t, testWaitTimeout, "final mutated metadata indexed", func() bool {
			if extractCalls.Load() < 2 {
				return false
			}
			snaps := loadSnapshotsMap(t, st)
			if _, ok := snaps["01 - Mutate.wav"]; !ok {
				return false
			}
			return svc.Status().Count == 1 && mustGetTrackByRelPath(t, st, "01 - Mutate.wav").Title == "After Mid-Parse Mutation"
		})
	})

	t.Run("mid-parse deletion aborts upsert with zero ghost row", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		svc := newTestService(t, musicDir, 0, st, nil)

		ingestDone := make(chan string, 4)
		svc.onIngestDone = func(relPath string) {
			ingestDone <- relPath
		}
		svc.extractFn = func(path string) (metadata.Info, error) {
			if filepath.Base(path) == "01 - Vanish.wav" {
				if err := os.Remove(path); err != nil {
					return metadata.Info{}, err
				}
				return metadata.Info{
					Title:       "Ghost",
					Artist:      "Ghost Artist",
					AlbumArtist: "Ghost Artist",
					Album:       "Ghost Album",
				}, nil
			}
			return metadata.Extract(path)
		}

		startAndWaitReady(t, svc)

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - Vanish.wav"))

		timer := time.NewTimer(testWaitTimeout)
		defer timer.Stop()
		select {
		case gotRel := <-ingestDone:
			if gotRel != "01 - Vanish.wav" {
				t.Fatalf("ingestDone relPath = %q, want %q", gotRel, "01 - Vanish.wav")
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for ingestFile completion")
		}

		if got := len(loadSnapshotsMap(t, st)); got != 0 {
			t.Errorf("store snapshots count = %d, want 0 (no ghost row)", got)
		}
		if got := svc.Status().Count; got != 0 {
			t.Errorf("Status().Count = %d, want 0", got)
		}
	})
}

func TestWatcher_NestedDirectoryRename(t *testing.T) {
	t.Run("nested directory rename updates watches, deletes old prefix, and protects sibling prefix", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		walkRoot, err := filepath.EvalSymlinks(musicDir)
		if err != nil {
			t.Fatal(err)
		}

		dirA := filepath.Join(musicDir, "dirA")
		dirASub := filepath.Join(dirA, "sub")
		dirAB := filepath.Join(musicDir, "dirAB")
		testutil.WriteMinimalWAV(t, filepath.Join(dirASub, "track.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(dirAB, "keep.wav"))

		svc := newTestService(t, musicDir, 0, st, nil)
		startAndWaitReady(t, svc)

		waitForCondition(t, testWaitTimeout, "initial dirA/sub and dirAB watches active", func() bool {
			watched, wl := svc.watchSnapshot()
			return slices.Contains(watched, filepath.Join(walkRoot, "dirA", "sub")) &&
				slices.Contains(wl, filepath.Join(walkRoot, "dirA", "sub"))
		})

		dirB := filepath.Join(musicDir, "dirB")
		if err := os.Rename(dirA, dirB); err != nil {
			t.Fatalf("os.Rename(%q, %q): %v", dirA, dirB, err)
		}

		waitForCondition(t, testWaitTimeout, "renamed directory tracks and watches updated", func() bool {
			snaps := loadSnapshotsMap(t, st)
			_, oldGone := snaps["dirA/sub/track.wav"]
			_, newOK := snaps["dirB/sub/track.wav"]
			_, siblingOK := snaps["dirAB/keep.wav"]
			if oldGone || !newOK || !siblingOK || svc.Status().Count != 2 {
				return false
			}
			watched, wl := svc.watchSnapshot()
			oldDirPath := filepath.Join(walkRoot, "dirA")
			oldSubPath := filepath.Join(walkRoot, "dirA", "sub")
			newDirPath := filepath.Join(walkRoot, "dirB")
			newSubPath := filepath.Join(walkRoot, "dirB", "sub")
			return !slices.Contains(watched, oldDirPath) &&
				!slices.Contains(watched, oldSubPath) &&
				!slices.Contains(wl, oldDirPath) &&
				!slices.Contains(wl, oldSubPath) &&
				slices.Contains(watched, newDirPath) &&
				slices.Contains(watched, newSubPath) &&
				slices.Contains(wl, newDirPath) &&
				slices.Contains(wl, newSubPath)
		})

		// New file written inside renamed nested directory is watched live.
		testutil.WriteMinimalWAV(t, filepath.Join(dirB, "sub", "new.wav"))
		waitForCondition(t, testWaitTimeout, "new file under dirB/sub indexed live", func() bool {
			_, ok := loadSnapshotsMap(t, st)["dirB/sub/new.wav"]
			return ok && svc.Status().Count == 3
		})
	})

	t.Run("os.Rename replacing empty watched directory keeps live watching active", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		walkRoot, err := filepath.EvalSymlinks(musicDir)
		if err != nil {
			t.Fatal(err)
		}

		dirA := filepath.Join(musicDir, "dirA")
		dirB := filepath.Join(musicDir, "dirB")
		if err := os.MkdirAll(dirA, 0o755); err != nil {
			t.Fatal(err)
		}
		testutil.WriteMinimalWAV(t, filepath.Join(dirB, "seed.wav"))

		svc := newTestService(t, musicDir, 0, st, nil)
		startAndWaitReady(t, svc)

		dirAWatch := filepath.Join(walkRoot, "dirA")
		dirBWatch := filepath.Join(walkRoot, "dirB")
		waitForCondition(t, testWaitTimeout, "initial dirA and dirB watches active", func() bool {
			watched, wl := svc.watchSnapshot()
			return slices.Contains(watched, dirAWatch) &&
				slices.Contains(wl, dirAWatch) &&
				slices.Contains(watched, dirBWatch) &&
				slices.Contains(wl, dirBWatch) &&
				svc.Status().Count == 1
		})

		// Use syscall.Rename (POSIX rename(2)) because Go's os.Rename rejects
		// overwriting an existing empty directory with EEXIST.
		if err := syscall.Rename(dirB, dirA); err != nil {
			t.Fatalf("syscall.Rename(%q, %q): %v", dirB, dirA, err)
		}

		waitForCondition(t, testWaitTimeout, "replaced dirA watches and tracks reconciled", func() bool {
			snaps := loadSnapshotsMap(t, st)
			_, oldGone := snaps["dirB/seed.wav"]
			_, newOK := snaps["dirA/seed.wav"]
			if oldGone || !newOK || svc.Status().Count != 1 {
				return false
			}
			watched, wl := svc.watchSnapshot()
			return !slices.Contains(watched, dirBWatch) &&
				!slices.Contains(wl, dirBWatch) &&
				slices.Contains(watched, dirAWatch) &&
				slices.Contains(wl, dirAWatch)
		})

		testutil.WriteMinimalWAV(t, filepath.Join(dirA, "after-replace.wav"))
		waitForCondition(t, testWaitTimeout, "new file in replaced dirA indexed live", func() bool {
			_, ok := loadSnapshotsMap(t, st)["dirA/after-replace.wav"]
			return ok && svc.Status().Count == 2
		})
	})
}

func TestWatcher_OverflowReconcile(t *testing.T) {
	t.Run("events channel overflow reconciles watches and DB", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		walkRoot, err := filepath.EvalSymlinks(musicDir)
		if err != nil {
			t.Fatal(err)
		}

		oldDir := filepath.Join(musicDir, "olddir")
		testutil.WriteMinimalWAV(t, filepath.Join(oldDir, "old.wav"))

		svc := newTestService(t, musicDir, 0, st, nil)
		svc.maxEventQueue = 2

		dispatchedFirst := make(chan struct{})
		releaseDispatch := make(chan struct{})
		var blockOnce sync.Once
		var releaseOnce sync.Once
		closeRelease := func() {
			releaseOnce.Do(func() {
				close(releaseDispatch)
			})
		}
		svc.beforeDispatch = func(ctx context.Context) {
			blockOnce.Do(func() {
				close(dispatchedFirst)
				select {
				case <-releaseDispatch:
				case <-ctx.Done():
				}
			})
		}

		startAndWaitReady(t, svc)
		// Registered after startAndWaitReady so LIFO cleanup unblocks dispatchLoop
		// before cancel() + svc.Wait() if an assertion fails early.
		t.Cleanup(closeRelease)

		// Trigger first dispatch and hold dispatchLoop while flooding >2 events.
		if err := os.RemoveAll(oldDir); err != nil {
			t.Fatal(err)
		}
		waitForChan(t, dispatchedFirst, testWaitTimeout, "dispatchedFirst")

		// Create an unwatched directory tree (which handleRescan must watch and index)
		// plus writes directly in the already-watched root so inotify overflows maxEventQueue=2.
		newDir := filepath.Join(musicDir, "newdir")
		for _, name := range []string{"a.wav", "b.wav", "c.wav", "d.wav"} {
			testutil.WriteMinimalWAV(t, filepath.Join(newDir, name))
		}
		for i := range 5 {
			testutil.WriteBytes(t, filepath.Join(musicDir, fmt.Sprintf("burst-%d.txt", i)), []byte("overflow"))
		}

		waitForCondition(t, testWaitTimeout, "events queue overflow triggered rescan", func() bool {
			return svc.rescanCount.Load() >= 1
		})
		closeRelease()

		waitForCondition(t, testWaitTimeout, "overflow reconciliation updated DB and watches", func() bool {
			snaps := loadSnapshotsMap(t, st)
			_, oldExists := snaps["olddir/old.wav"]
			_, aOK := snaps["newdir/a.wav"]
			_, dOK := snaps["newdir/d.wav"]
			if oldExists || !aOK || !dOK || svc.Status().Count != 4 {
				return false
			}
			watched, wl := svc.watchSnapshot()
			oldWatch := filepath.Join(walkRoot, "olddir")
			newWatch := filepath.Join(walkRoot, "newdir")
			return !slices.Contains(watched, oldWatch) &&
				!slices.Contains(wl, oldWatch) &&
				slices.Contains(watched, newWatch) &&
				slices.Contains(wl, newWatch)
		})

		// Subsequent live events keep working.
		testutil.WriteMinimalWAV(t, filepath.Join(newDir, "live.wav"))
		waitForCondition(t, testWaitTimeout, "live event after overflow indexed", func() bool {
			_, ok := loadSnapshotsMap(t, st)["newdir/live.wav"]
			return ok && svc.Status().Count == 5
		})
	})

	t.Run("fsnotify.ErrEventOverflow injection reconciles and continues watching", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - Initial.wav"))

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)

		var (
			fwMu       sync.Mutex
			capturedFW *fsnotify.Watcher
		)
		svc.newWatcherFn = func() (*fsnotify.Watcher, error) {
			fw, err := fsnotify.NewWatcher()
			if err == nil {
				fwMu.Lock()
				capturedFW = fw
				fwMu.Unlock()
			}
			return fw, err
		}

		startAndWaitReady(t, svc)
		beforeScan := svc.Status().LastScan

		fwMu.Lock()
		fw := capturedFW
		fwMu.Unlock()
		fw.Errors <- fsnotify.ErrEventOverflow

		waitForCondition(t, testWaitTimeout, "ErrEventOverflow logged and reconciled", func() bool {
			return logs.countWarningsContaining("overflow") >= 1 && svc.Status().LastScan.After(beforeScan)
		})

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "after-overflow", "02 - Live.wav"))
		waitForCondition(t, testWaitTimeout, "live event after ErrEventOverflow indexed", func() bool {
			_, ok := loadSnapshotsMap(t, st)["after-overflow/02 - Live.wav"]
			return ok && svc.Status().Count == 2
		})
	})

	t.Run("rename-and-recreate directory during overflow watches both old and new trees", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		walkRoot, err := filepath.EvalSymlinks(musicDir)
		if err != nil {
			t.Fatal(err)
		}

		dirA := filepath.Join(musicDir, "dirA")
		dirB := filepath.Join(musicDir, "dirB")
		testutil.WriteMinimalWAV(t, filepath.Join(dirA, "sub", "orig.wav"))

		svc := newTestService(t, musicDir, 0, st, nil)
		svc.maxEventQueue = 2

		dispatchedFirst := make(chan struct{})
		releaseDispatch := make(chan struct{})
		var blockOnce sync.Once
		var releaseOnce sync.Once
		closeRelease := func() {
			releaseOnce.Do(func() {
				close(releaseDispatch)
			})
		}
		svc.beforeDispatch = func(ctx context.Context) {
			blockOnce.Do(func() {
				close(dispatchedFirst)
				select {
				case <-releaseDispatch:
				case <-ctx.Done():
				}
			})
		}

		startAndWaitReady(t, svc)
		t.Cleanup(closeRelease)

		// Pause dispatchLoop, then rename dirA -> dirB and recreate dirA/sub during queue overflow.
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "trigger.wav"))
		waitForChan(t, dispatchedFirst, testWaitTimeout, "dispatchedFirst")

		if err := os.Rename(dirA, dirB); err != nil {
			t.Fatalf("os.Rename(%q, %q): %v", dirA, dirB, err)
		}
		testutil.WriteMinimalWAV(t, filepath.Join(dirA, "sub", "recreated.wav"))
		for i := range 5 {
			testutil.WriteBytes(t, filepath.Join(musicDir, fmt.Sprintf("burst-%d.txt", i)), []byte("overflow"))
		}

		waitForCondition(t, testWaitTimeout, "overflow rescan triggered during rename-and-recreate", func() bool {
			return svc.rescanCount.Load() >= 1
		})
		closeRelease()

		wantWatches := []string{
			filepath.Join(walkRoot, "dirA"),
			filepath.Join(walkRoot, "dirA", "sub"),
			filepath.Join(walkRoot, "dirB"),
			filepath.Join(walkRoot, "dirB", "sub"),
		}
		waitForCondition(t, testWaitTimeout, "rename-and-recreate reconciled DB and watches", func() bool {
			snaps := loadSnapshotsMap(t, st)
			_, origAtB := snaps["dirB/sub/orig.wav"]
			_, recreatedAtA := snaps["dirA/sub/recreated.wav"]
			_, trigOK := snaps["trigger.wav"]
			if !origAtB || !recreatedAtA || !trigOK || svc.Status().Count != 3 {
				return false
			}
			watched, wl := svc.watchSnapshot()
			for _, wdir := range wantWatches {
				if !slices.Contains(watched, wdir) || !slices.Contains(wl, wdir) {
					return false
				}
			}
			return true
		})

		// Verify live watches work in BOTH recreated dirA/sub and moved dirB/sub.
		testutil.WriteMinimalWAV(t, filepath.Join(dirA, "sub", "live-a.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(dirB, "sub", "live-b.wav"))
		waitForCondition(t, testWaitTimeout, "live events in both dirA/sub and dirB/sub indexed", func() bool {
			snaps := loadSnapshotsMap(t, st)
			_, liveA := snaps["dirA/sub/live-a.wav"]
			_, liveB := snaps["dirB/sub/live-b.wav"]
			return liveA && liveB && svc.Status().Count == 5
		})
	})
}

func TestWatcher_LimitDegradation(t *testing.T) {
	t.Run("newWatcherFn EMFILE logs once and falls back to periodic polling", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - Seed.wav"))

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 25*time.Millisecond, st, logger)
		svc.newWatcherFn = func() (*fsnotify.Watcher, error) {
			return nil, fmt.Errorf("inotify_init1: %w", syscall.EMFILE)
		}

		startAndWaitReady(t, svc)

		if got := svc.Status().Count; got != 1 {
			t.Fatalf("initial Status().Count = %d, want 1", got)
		}
		if got := logs.countWarningsContaining("fs.inotify.max_user_watches"); got != 1 {
			t.Errorf("limit WARN count = %d, want 1\nlogs:\n%s", got, logs.allText())
		}

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "02 - Polled.wav"))
		waitForCondition(t, testWaitTimeout, "periodic polling indexes new track on EMFILE fallback", func() bool {
			_, ok := loadSnapshotsMap(t, st)["02 - Polled.wav"]
			return ok && svc.Status().Count == 2
		})
		if got := logs.countWarningsContaining("fs.inotify.max_user_watches"); got != 1 {
			t.Errorf("limit WARN count after polling = %d, want 1", got)
		}
	})

	t.Run("addWatchFn ENOSPC across multiple subdirs logs once and falls back to periodic polling", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "sub1", "01 - One.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "sub2", "02 - Two.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "sub3", "03 - Three.wav"))

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 25*time.Millisecond, st, logger)
		svc.addWatchFn = func(_ *fsnotify.Watcher, dir string) error {
			return fmt.Errorf("inotify_add_watch %q: %w", dir, syscall.ENOSPC)
		}

		startAndWaitReady(t, svc)

		if got := svc.Status().Count; got != 3 {
			t.Fatalf("initial Status().Count = %d, want 3", got)
		}
		if got := logs.countWarningsContaining("fs.inotify.max_user_watches"); got != 1 {
			t.Errorf("limit WARN count across subdirs = %d, want 1\nlogs:\n%s", got, logs.allText())
		}

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "sub2", "04 - Polled.wav"))
		waitForCondition(t, testWaitTimeout, "periodic polling indexes new track on ENOSPC fallback", func() bool {
			_, ok := loadSnapshotsMap(t, st)["sub2/04 - Polled.wav"]
			return ok && svc.Status().Count == 4
		})
		if got := logs.countWarningsContaining("fs.inotify.max_user_watches"); got != 1 {
			t.Errorf("limit WARN count after polling = %d, want 1", got)
		}
	})
}

func TestService_ConcurrentScanAndWatcher(t *testing.T) {
	st := openTestStore(t)
	musicDir := t.TempDir()
	testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "base", "00 - Anchor.wav"))

	svc := newTestService(t, musicDir, 30*time.Millisecond, st, nil)
	svc.debounceDelay = 15 * time.Millisecond
	svc.stabilityDelay = 10 * time.Millisecond
	ctx, _ := startAndWaitReady(t, svc)

	var (
		workWG     sync.WaitGroup
		statusWG   sync.WaitGroup
		stopStatus = make(chan struct{})
		writerErrs = make(chan error, 1)
	)

	// Concurrent filesystem writer performing creates, rewrites, renames, and deletes.
	workWG.Add(1)
	go func() {
		defer workWG.Done()
		dir := filepath.Join(musicDir, "churn")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writerErrs <- err
			return
		}
		wavData := testutil.MinimalWAVBytes()
		for i := range 15 {
			tmpPath := filepath.Join(dir, fmt.Sprintf("track-%d.wav.tmp", i))
			finalPath := filepath.Join(dir, fmt.Sprintf("track-%d.wav", i))
			if err := os.WriteFile(tmpPath, wavData, 0o600); err != nil {
				writerErrs <- err
				return
			}
			if err := os.Rename(tmpPath, finalPath); err != nil {
				writerErrs <- err
				return
			}
			if i%3 == 0 {
				if err := taglib.WriteTags(finalPath, map[string][]string{taglib.Title: {fmt.Sprintf("Title %d", i)}}, taglib.Clear); err != nil {
					writerErrs <- err
					return
				}
			}
			if i%4 == 0 {
				if err := os.Remove(finalPath); err != nil {
					writerErrs <- err
					return
				}
			}
		}
	}()

	// Concurrent ScanNow callers.
	for range 3 {
		workWG.Add(1)
		go func() {
			defer workWG.Done()
			for range 10 {
				_ = svc.ScanNow(ctx)
			}
		}()
	}

	// Concurrent Status readers.
	for range 2 {
		statusWG.Add(1)
		go func() {
			defer statusWG.Done()
			for {
				select {
				case <-stopStatus:
					return
				default:
					_ = svc.Status()
				}
			}
		}()
	}

	workWG.Wait()
	close(stopStatus)
	statusWG.Wait()

	select {
	case err := <-writerErrs:
		t.Fatalf("concurrent filesystem writer failed: %v", err)
	default:
	}

	// Reconcile final disk state after churn stops and assert exact DB-vs-disk parity.
	if err := svc.ScanNow(ctx); err != nil {
		t.Fatalf("final ScanNow() unexpected error: %v", err)
	}

	var wantPaths []string
	walkErr := filepath.WalkDir(musicDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		if _, _, ok := metadata.LookupFormat(d.Name()); !ok || metadata.IsIgnoredName(d.Name()) {
			return nil
		}
		rel, ok := toRelPath(musicDir, path)
		if ok {
			wantPaths = append(wantPaths, rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("WalkDir(%q): %v", musicDir, walkErr)
	}
	sort.Strings(wantPaths)

	waitForCondition(t, testWaitTimeout, "store matches final disk state", func() bool {
		gotPaths := mapKeys(loadSnapshotsMap(t, st))
		return slices.Equal(gotPaths, wantPaths) && svc.Status().Count == int64(len(wantPaths))
	})
}

func TestService_StartupCountAndCleanShutdown(t *testing.T) {
	t.Run("startup count seeded before scan, idempotent double Start, clean shutdown mid-debounce", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()

		// Pre-populate 2 tracks in SQLite and matching files on disk, plus a 3rd new file
		// whose extraction waits on scanGate so we can inspect pre-scan Status().
		p1 := filepath.Join(musicDir, "01 - SeedOne.wav")
		p2 := filepath.Join(musicDir, "02 - SeedTwo.wav")
		p3 := filepath.Join(musicDir, "03 - PendingInitial.wav")
		testutil.WriteMinimalWAV(t, p1)
		testutil.WriteMinimalWAV(t, p2)
		testutil.WriteMinimalWAV(t, p3)

		for _, p := range []string{p1, p2} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			rel, ok := toRelPath(musicDir, p)
			if !ok {
				t.Fatalf("toRelPath(%q) failed", p)
			}
			tr := buildTrack(store.SourceMusic, rel, info.Size(), info.ModTime().UnixNano(), metadata.FallbackFromFilename(p))
			if err := st.UpsertTrack(context.Background(), &tr); err != nil {
				t.Fatal(err)
			}
		}

		svc := newTestService(t, musicDir, 20*time.Millisecond, st, nil)
		// Long debounce so shutdown catches a pending timer mid-debounce.
		svc.debounceDelay = 5 * time.Second

		ctx, cancel := context.WithCancel(context.Background())
		scanEntered := make(chan struct{})
		scanGate := make(chan struct{})
		var gateOnce sync.Once
		closeScanGate := func() {
			gateOnce.Do(func() {
				close(scanGate)
			})
		}
		var enteredOnce sync.Once
		var p3Extracts atomic.Int32
		svc.extractFn = func(path string) (metadata.Info, error) {
			if filepath.Base(path) == "03 - PendingInitial.wav" {
				p3Extracts.Add(1)
				enteredOnce.Do(func() {
					close(scanEntered)
					select {
					case <-scanGate:
					case <-ctx.Done():
					}
				})
			}
			return metadata.Extract(path)
		}

		var (
			fwMu       sync.Mutex
			capturedFW *fsnotify.Watcher
		)
		svc.newWatcherFn = func() (*fsnotify.Watcher, error) {
			fw, err := fsnotify.NewWatcher()
			if err == nil {
				fwMu.Lock()
				capturedFW = fw
				fwMu.Unlock()
			}
			return fw, err
		}

		// Register LIFO cleanups: closeScanGate runs before cancel()+svc.Wait().
		t.Cleanup(func() {
			cancel()
			svc.Wait()
		})
		t.Cleanup(closeScanGate)

		// Concurrent double Start must be safe and idempotent.
		var startWG sync.WaitGroup
		for range 2 {
			startWG.Add(1)
			go func() {
				defer startWG.Done()
				svc.Start(ctx)
			}()
		}
		startWG.Wait()

		waitForChan(t, svc.watcherReady, testWaitTimeout, "watcherReady")
		waitForChan(t, scanEntered, testWaitTimeout, "scanEntered")

		// Immediately after Start and prior to initial scan completion: Scanning == true, Count == 2, LastScan.IsZero().
		preScan := svc.Status()
		if !preScan.Scanning || preScan.Count != 2 || !preScan.LastScan.IsZero() {
			t.Errorf("pre-initial-scan Status() = %+v, want Scanning=true, Count=2, and zero LastScan", preScan)
		}

		closeScanGate()
		waitForChan(t, svc.initialScanDone, testWaitTimeout, "initialScanDone")

		if got := p3Extracts.Load(); got != 1 {
			t.Errorf("p3 extract calls = %d, want 1 (idempotent Start)", got)
		}
		postScan := svc.Status()
		if postScan.Scanning || postScan.Count != 3 || postScan.LastScan.IsZero() {
			t.Errorf("post-initial-scan Status() = %+v, want Scanning=false, Count=3, and non-zero LastScan", postScan)
		}

		// Trigger a pending debounce timer, wait until registered, then cancel and Wait cleanly.
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "04 - MidDebounce.wav"))
		waitForCondition(t, testWaitTimeout, "debounce timer pending before shutdown", func() bool {
			return svc.pendingCount() >= 1
		})
		cancel()
		svc.Wait()

		if got := svc.pendingCount(); got != 0 {
			t.Errorf("pendingCount after Wait() = %d, want 0", got)
		}

		fwMu.Lock()
		fw := capturedFW
		fwMu.Unlock()
		if fw == nil {
			t.Fatalf("capturedFW is nil")
		}
		if err := fw.Add(musicDir); !errors.Is(err, fsnotify.ErrClosed) {
			t.Errorf("fsWatcher.Add after Wait() error = %v, want fsnotify.ErrClosed", err)
		}
	})

	t.Run("Wait without Start returns immediately", func(t *testing.T) {
		st := openTestStore(t)
		svc := newTestService(t, t.TempDir(), 0, st, nil)

		done := make(chan struct{})
		go func() {
			svc.Wait()
			close(done)
		}()
		waitForChan(t, done, time.Second, "Wait without Start")
	})

	t.Run("Start with already-canceled context closes ready channels and drains cleanly", func(t *testing.T) {
		st := openTestStore(t)
		svc := newTestService(t, t.TempDir(), 50*time.Millisecond, st, nil)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		svc.Start(ctx)
		waitForChan(t, svc.watcherReady, testWaitTimeout, "watcherReady on canceled ctx")
		waitForChan(t, svc.initialScanDone, testWaitTimeout, "initialScanDone on canceled ctx")
		svc.Wait()
	})

	t.Run("missing musicDir at Start degrades watcher and recovers via periodic polling", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := filepath.Join(t.TempDir(), "not-mounted-yet")
		logs, logger := newLogCapture()

		svc := newTestService(t, musicDir, 25*time.Millisecond, st, logger)
		startAndWaitReady(t, svc)

		if got := logs.countWarningsContaining("music directory unavailable"); got != 1 {
			t.Errorf("unavailable musicDir WARN count = %d, want 1\nlogs:\n%s", got, logs.allText())
		}
		if got := svc.Status(); got.Count != 0 || !got.LastScan.IsZero() {
			t.Errorf("Status() while musicDir missing = %+v, want Count=0 and zero LastScan", got)
		}

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - MountedLater.wav"))
		waitForCondition(t, testWaitTimeout, "periodic polling recovers when musicDir appears", func() bool {
			_, ok := loadSnapshotsMap(t, st)["01 - MountedLater.wav"]
			return ok && svc.Status().Count == 1 && !svc.Status().LastScan.IsZero()
		})
	})

	t.Run("clean shutdown mid-stability cancels sleep promptly and closes watcher", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()

		svc := newTestService(t, musicDir, 0, st, nil)
		svc.debounceDelay = 5 * time.Millisecond
		svc.stabilityDelay = 5 * time.Second

		stabilityEntered := make(chan struct{})
		var enteredOnce sync.Once
		svc.onStabilityStat = func(string, int) {
			enteredOnce.Do(func() {
				close(stabilityEntered)
			})
		}

		var (
			fwMu       sync.Mutex
			capturedFW *fsnotify.Watcher
		)
		svc.newWatcherFn = func() (*fsnotify.Watcher, error) {
			fw, err := fsnotify.NewWatcher()
			if err == nil {
				fwMu.Lock()
				capturedFW = fw
				fwMu.Unlock()
			}
			return fw, err
		}

		_, cancel := startAndWaitReady(t, svc)
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - MidStability.wav"))
		waitForChan(t, stabilityEntered, testWaitTimeout, "stabilityEntered")

		cancel()
		waitDone := make(chan struct{})
		go func() {
			svc.Wait()
			close(waitDone)
		}()
		waitForChan(t, waitDone, 2*time.Second, "Wait returns promptly during stability sleep")

		if got := svc.Status().Count; got != 0 || len(loadSnapshotsMap(t, st)) != 0 {
			t.Errorf("after mid-stability shutdown Count=%d snapshots=%d, want 0", got, len(loadSnapshotsMap(t, st)))
		}
		fwMu.Lock()
		fw := capturedFW
		fwMu.Unlock()
		if fw == nil {
			t.Fatalf("capturedFW is nil")
		}
		if err := fw.Add(musicDir); !errors.Is(err, fsnotify.ErrClosed) {
			t.Errorf("fsWatcher.Add after Wait() error = %v, want fsnotify.ErrClosed", err)
		}
	})
}

func TestNew_MultiRootValidation(t *testing.T) {
	st := openTestStore(t)
	baseDir := t.TempDir()
	musicDir := filepath.Join(baseDir, "music")
	nestedUploads := filepath.Join(musicDir, "uploads")
	parentDir := baseDir
	siblingUploads := filepath.Join(baseDir, "music-uploads")
	relUploads := " ./relative-uploads "

	tests := []struct {
		name           string
		musicDir       string
		opts           []Option
		wantErr        bool
		wantUploadsSet bool
	}{
		{
			name:           "valid sibling uploadsDir normalized to absolute",
			musicDir:       musicDir,
			opts:           []Option{WithUploadsDir(siblingUploads)},
			wantErr:        false,
			wantUploadsSet: true,
		},
		{
			name:           "relative non-existent uploadsDir normalized without touching disk",
			musicDir:       musicDir,
			opts:           []Option{WithUploadsDir(relUploads)},
			wantErr:        false,
			wantUploadsSet: true,
		},
		{
			name:           "empty and whitespace WithUploadsDir ignored",
			musicDir:       musicDir,
			opts:           []Option{WithUploadsDir(""), WithUploadsDir("  \t\n "), nil},
			wantErr:        false,
			wantUploadsSet: false,
		},
		{
			name:           "subsequent empty WithUploadsDir clears prior option",
			musicDir:       musicDir,
			opts:           []Option{WithUploadsDir(siblingUploads), WithUploadsDir(" ")},
			wantErr:        false,
			wantUploadsSet: false,
		},
		{
			name:     "identical musicDir and uploadsDir rejected",
			musicDir: musicDir,
			opts:     []Option{WithUploadsDir(musicDir + string(filepath.Separator) + ".")},
			wantErr:  true,
		},
		{
			name:     "uploadsDir inside musicDir rejected",
			musicDir: musicDir,
			opts:     []Option{WithUploadsDir(nestedUploads)},
			wantErr:  true,
		},
		{
			name:     "musicDir inside uploadsDir rejected",
			musicDir: musicDir,
			opts:     []Option{WithUploadsDir(parentDir)},
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(tc.musicDir, 0, st, nil, tc.opts...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%q) error = %v, wantErr = %v", tc.musicDir, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.wantUploadsSet {
				if !filepath.IsAbs(svc.uploadsDir) {
					t.Errorf("svc.uploadsDir = %q, want non-empty absolute path", svc.uploadsDir)
				}
				if len(svc.roots) != 2 {
					t.Errorf("len(svc.roots) = %d, want 2", len(svc.roots))
				}
			} else {
				if svc.uploadsDir != "" {
					t.Errorf("svc.uploadsDir = %q, want empty", svc.uploadsDir)
				}
				if len(svc.roots) != 1 {
					t.Errorf("len(svc.roots) = %d, want 1", len(svc.roots))
				}
			}
		})
	}
}

func TestMultiRoot_ScanAndSafetyIsolation(t *testing.T) {
	t.Run("coexisting identical rel_path across roots and empty uploadsDir", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		sharedRel := filepath.Join("Artist", "Album", "01 - Shared.wav")
		musicShared := filepath.Join(musicDir, sharedRel)
		musicOnly := filepath.Join(musicDir, "music-only.wav")
		testutil.WriteMinimalWAV(t, musicShared)
		testutil.WriteTags(t, musicShared, map[string][]string{taglib.Title: {"Music Shared"}})
		testutil.WriteMinimalWAV(t, musicOnly)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger, WithUploadsDir(uploadsDir))
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() with empty uploadsDir unexpected error: %v", err)
		}
		if logs.countWarningsContaining("zero audio files") != 0 {
			t.Errorf("unexpected zero-file warning when uploadsDir is empty:\n%s", logs.allText())
		}
		if got := svc.Status(); got.Count != 2 || got.LastScan.IsZero() {
			t.Errorf("Status() = %+v, want Count=2 and non-zero LastScan", got)
		}

		// Now populate uploadsDir with identical rel_path and an uploads-only file.
		uploadsShared := filepath.Join(uploadsDir, sharedRel)
		uploadsOnly := filepath.Join(uploadsDir, "uploads-only.wav")
		testutil.WriteMinimalWAV(t, uploadsShared)
		testutil.WriteTags(t, uploadsShared, map[string][]string{taglib.Title: {"Uploads Shared"}})
		testutil.WriteMinimalWAV(t, uploadsOnly)

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() with populated both roots unexpected error: %v", err)
		}

		musicSnaps := loadSourceSnapshotsMap(t, st, store.SourceMusic)
		uploadsSnaps := loadSourceSnapshotsMap(t, st, store.SourceUploads)
		if got, want := mapKeys(musicSnaps), []string{"Artist/Album/01 - Shared.wav", "music-only.wav"}; !slices.Equal(got, want) {
			t.Errorf("music snapshots = %v, want %v", got, want)
		}
		if got, want := mapKeys(uploadsSnaps), []string{"Artist/Album/01 - Shared.wav", "uploads-only.wav"}; !slices.Equal(got, want) {
			t.Errorf("uploads snapshots = %v, want %v", got, want)
		}
		if got := svc.Status().Count; got != 4 {
			t.Errorf("Status().Count = %d, want 4", got)
		}

		mTr := mustGetTrackBySourceRelPath(t, st, store.SourceMusic, "Artist/Album/01 - Shared.wav")
		uTr := mustGetTrackBySourceRelPath(t, st, store.SourceUploads, "Artist/Album/01 - Shared.wav")
		if mTr.ID == uTr.ID || mTr.Source != store.SourceMusic || uTr.Source != store.SourceUploads {
			t.Errorf("shared rel_path identity = (%q/%s vs %q/%s), want distinct IDs and sources",
				mTr.ID, mTr.Source, uTr.ID, uTr.Source)
		}
		if mTr.Title != "Music Shared" || uTr.Title != "Uploads Shared" {
			t.Errorf("shared rel_path titles = (%q, %q), want (%q, %q)",
				mTr.Title, uTr.Title, "Music Shared", "Uploads Shared")
		}
	})

	t.Run("zero-file guard isolated per root", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		mKeep := filepath.Join(musicDir, "m-keep.wav")
		mDrop := filepath.Join(musicDir, "m-drop.wav")
		uOne := filepath.Join(uploadsDir, "u-one.wav")
		testutil.WriteMinimalWAV(t, mKeep)
		testutil.WriteMinimalWAV(t, mDrop)
		testutil.WriteMinimalWAV(t, uOne)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger, WithUploadsDir(uploadsDir))
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		// Empty uploadsDir while deleting m-drop.wav in musicDir:
		// uploads must keep u-one.wav (guard tripped) while music must prune m-drop.wav.
		if err := os.Remove(uOne); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(mDrop); err != nil {
			t.Fatal(err)
		}
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() unexpected error: %v", err)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceMusic)), []string{"m-keep.wav"}; !slices.Equal(got, want) {
			t.Errorf("music snapshots = %v, want %v", got, want)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceUploads)), []string{"u-one.wav"}; !slices.Equal(got, want) {
			t.Errorf("uploads snapshots = %v, want %v", got, want)
		}
		if logs.countWarningsContaining("uploads directory yielded zero audio files") != 1 {
			t.Errorf("expected uploads zero-file warning, got:\n%s", logs.allText())
		}
		if logs.countWarningsContaining("music directory yielded zero audio files") != 0 {
			t.Errorf("unexpected music zero-file warning, got:\n%s", logs.allText())
		}

		// Restore uOne, add uTwo then remove mKeep & uOne:
		// music must trip zero-file guard while uploads must prune uOne and index uTwo.
		testutil.WriteMinimalWAV(t, uOne)
		uTwo := filepath.Join(uploadsDir, "u-two.wav")
		testutil.WriteMinimalWAV(t, uTwo)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(mKeep); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(uOne); err != nil {
			t.Fatal(err)
		}
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() reverse guard unexpected error: %v", err)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceMusic)), []string{"m-keep.wav"}; !slices.Equal(got, want) {
			t.Errorf("music snapshots after reverse guard = %v, want %v", got, want)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceUploads)), []string{"u-two.wav"}; !slices.Equal(got, want) {
			t.Errorf("uploads snapshots after reverse guard = %v, want %v", got, want)
		}
		if logs.countWarningsContaining("music directory yielded zero audio files") != 1 {
			t.Errorf("expected music zero-file warning, got:\n%s", logs.allText())
		}
	})

	t.Run("walk error chmod 0000 in uploadsDir skips deletions only in uploads", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod 0o000 walk error test skipped when running as root (UID 0)")
		}

		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		mKeep := filepath.Join(musicDir, "m-keep.wav")
		mDrop := filepath.Join(musicDir, "m-drop.wav")
		testutil.WriteMinimalWAV(t, mKeep)
		testutil.WriteMinimalWAV(t, mDrop)

		uBlocked := filepath.Join(uploadsDir, "blocked")
		testutil.WriteMinimalWAV(t, filepath.Join(uBlocked, "hidden.wav"))
		uDrop := filepath.Join(uploadsDir, "u-drop.wav")
		testutil.WriteMinimalWAV(t, uDrop)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger, WithUploadsDir(uploadsDir))
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		if err := os.Chmod(uBlocked, 0o000); err != nil {
			t.Fatalf("os.Chmod(%q): %v", uBlocked, err)
		}
		t.Cleanup(func() { _ = os.Chmod(uBlocked, 0o755) })

		if err := os.Remove(mDrop); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(uDrop); err != nil {
			t.Fatal(err)
		}
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "u-new.wav"))

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() with chmod 0000 in uploads unexpected error: %v", err)
		}

		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceMusic)), []string{"m-keep.wav"}; !slices.Equal(got, want) {
			t.Errorf("music tracks = %v, want %v (stale music track should still prune)", got, want)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceUploads)), []string{"blocked/hidden.wav", "u-drop.wav", "u-new.wav"}; !slices.Equal(got, want) {
			t.Errorf("uploads tracks = %v, want %v", got, want)
		}
		if logs.countWarningsContaining("skipping stale track deletions") != 1 {
			t.Errorf("expected 1 pruning-skipped WARN log, got:\n%s", logs.allText())
		}
	})

	t.Run("extractFn error in uploadsDir skips deletions only in uploads", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		mKeep := filepath.Join(musicDir, "m-keep.wav")
		mDrop := filepath.Join(musicDir, "m-drop.wav")
		testutil.WriteMinimalWAV(t, mKeep)
		testutil.WriteMinimalWAV(t, mDrop)

		uBad := filepath.Join(uploadsDir, "u-bad.wav")
		uDrop := filepath.Join(uploadsDir, "u-drop.wav")
		testutil.WriteMinimalWAV(t, uBad)
		testutil.WriteMinimalWAV(t, uDrop)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger, WithUploadsDir(uploadsDir))
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		if err := os.Remove(mDrop); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(uDrop); err != nil {
			t.Fatal(err)
		}
		testutil.WriteMinimalWAV(t, uBad)
		testutil.WriteTags(t, uBad, map[string][]string{taglib.Title: {"Changed"}})
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "u-new.wav"))

		svc.extractFn = func(path string) (metadata.Info, error) {
			if path == uBad {
				return metadata.Info{}, fmt.Errorf("read %q: %w", path, syscall.EIO)
			}
			return metadata.Extract(path)
		}

		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() unexpected error: %v", err)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceMusic)), []string{"m-keep.wav"}; !slices.Equal(got, want) {
			t.Errorf("music tracks = %v, want %v", got, want)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceUploads)), []string{"u-bad.wav", "u-drop.wav", "u-new.wav"}; !slices.Equal(got, want) {
			t.Errorf("uploads tracks = %v, want %v", got, want)
		}
		if logs.countWarningsContaining("skipping stale track deletions") != 1 {
			t.Errorf("expected 1 pruning-skipped WARN log, got:\n%s", logs.allText())
		}
	})

	t.Run("hard error on one root reconciles healthy root without advancing LastScan", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "m1.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "u1.wav"))

		svc := newTestService(t, musicDir, 0, st, nil, WithUploadsDir(uploadsDir))
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}
		before := svc.Status()

		// Remove musicDir while adding a new file in uploadsDir.
		if err := os.RemoveAll(musicDir); err != nil {
			t.Fatal(err)
		}
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "u2.wav"))

		err := svc.ScanNow(context.Background())
		if err == nil {
			t.Fatalf("ScanNow() with missing musicDir succeeded, want error")
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceUploads)), []string{"u1.wav", "u2.wav"}; !slices.Equal(got, want) {
			t.Errorf("uploads tracks = %v, want %v", got, want)
		}
		if got, want := mapKeys(loadSourceSnapshotsMap(t, st, store.SourceMusic)), []string{"m1.wav"}; !slices.Equal(got, want) {
			t.Errorf("music tracks = %v, want %v (preserved on hard error)", got, want)
		}
		after := svc.Status()
		if after.Count != 3 || !after.LastScan.Equal(before.LastScan) || after.Scanning {
			t.Errorf("Status() after partial failure = %+v, want Count=3, Scanning=false, and unchanged LastScan=%v", after, before.LastScan)
		}
	})

	t.Run("mid-walk cancellation on second root preserves first root count without advancing LastScan", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "m1.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "u1.wav"))

		svc := newTestService(t, musicDir, 0, st, nil, WithUploadsDir(uploadsDir))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		svc.extractFn = func(path string) (metadata.Info, error) {
			if filepath.Base(path) == "u1.wav" {
				cancel()
			}
			return metadata.Extract(path)
		}

		err := svc.ScanNow(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ScanNow() error = %v, want context.Canceled", err)
		}
		if got := svc.Status(); got.Count != 1 || !got.LastScan.IsZero() || got.Scanning {
			t.Errorf("Status() after second-root cancel = %+v, want Count=1, zero LastScan, Scanning=false", got)
		}
	})
}

func TestMultiRoot_WatcherIsolationAndOverflow(t *testing.T) {
	t.Run("live uploads watcher mutations stay isolated from identical rel_path in music and shutdown closes both watchers", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		sharedRel := filepath.Join("shared", "01 - Same.wav")
		musicShared := filepath.Join(musicDir, sharedRel)
		uploadsShared := filepath.Join(uploadsDir, sharedRel)
		testutil.WriteMinimalWAV(t, musicShared)
		testutil.WriteTags(t, musicShared, map[string][]string{taglib.Title: {"Music Original"}})
		testutil.WriteMinimalWAV(t, uploadsShared)
		testutil.WriteTags(t, uploadsShared, map[string][]string{taglib.Title: {"Upload Original"}})

		albumRel := filepath.Join("albumA", "track.wav")
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, albumRel))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, albumRel))

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger, WithUploadsDir(uploadsDir))

		var (
			fwMu        sync.Mutex
			capturedFWs []*fsnotify.Watcher
		)
		svc.newWatcherFn = func() (*fsnotify.Watcher, error) {
			fw, err := fsnotify.NewWatcher()
			if err == nil {
				fwMu.Lock()
				capturedFWs = append(capturedFWs, fw)
				fwMu.Unlock()
			}
			return fw, err
		}

		_, cancel := startAndWaitReady(t, svc)

		fwMu.Lock()
		fwCount := len(capturedFWs)
		fwMu.Unlock()
		if fwCount != 2 {
			t.Fatalf("captured watcher count = %d, want 2", fwCount)
		}

		origMusicShared := mustGetTrackBySourceRelPath(t, st, store.SourceMusic, "shared/01 - Same.wav")
		origUploadShared := mustGetTrackBySourceRelPath(t, st, store.SourceUploads, "shared/01 - Same.wav")
		if svc.Status().Count != 4 {
			t.Fatalf("initial Count = %d, want 4", svc.Status().Count)
		}

		// 1. Live add and edit in uploadsDir leave music rows intact.
		uploadNew := filepath.Join(uploadsDir, "shared", "02 - Fresh.wav")
		testutil.WriteMinimalWAV(t, uploadNew)
		testutil.WriteTags(t, uploadsShared, map[string][]string{taglib.Title: {"Upload Edited"}})
		waitForCondition(t, testWaitTimeout, "uploads add and edit reflected", func() bool {
			snaps := loadSourceSnapshotsMap(t, st, store.SourceUploads)
			if _, ok := snaps["shared/02 - Fresh.wav"]; !ok {
				return false
			}
			edited := mustGetTrackBySourceRelPath(t, st, store.SourceUploads, "shared/01 - Same.wav")
			return edited.Title == "Upload Edited" && svc.Status().Count == 5
		})

		afterEditUpload := mustGetTrackBySourceRelPath(t, st, store.SourceUploads, "shared/01 - Same.wav")
		if afterEditUpload.ID != origUploadShared.ID || !afterEditUpload.CreatedAt.Equal(origUploadShared.CreatedAt) {
			t.Errorf("uploads edited identity = (%q, %v), want (%q, %v)",
				afterEditUpload.ID, afterEditUpload.CreatedAt, origUploadShared.ID, origUploadShared.CreatedAt)
		}
		afterEditMusic := mustGetTrackBySourceRelPath(t, st, store.SourceMusic, "shared/01 - Same.wav")
		if afterEditMusic.Title != "Music Original" || afterEditMusic.ID != origMusicShared.ID {
			t.Errorf("music shared track mutated unexpectedly: %+v", afterEditMusic)
		}

		// 2. Live file delete in uploadsDir leaves identical music rel_path intact.
		if err := os.Remove(uploadsShared); err != nil {
			t.Fatal(err)
		}
		waitForCondition(t, testWaitTimeout, "uploads shared delete isolated", func() bool {
			_, uExists := loadSourceSnapshotsMap(t, st, store.SourceUploads)["shared/01 - Same.wav"]
			_, mExists := loadSourceSnapshotsMap(t, st, store.SourceMusic)["shared/01 - Same.wav"]
			return !uExists && mExists && svc.Status().Count == 4
		})

		// 3. Directory rename in uploadsDir updates only SourceUploads.
		if err := os.Rename(filepath.Join(uploadsDir, "albumA"), filepath.Join(uploadsDir, "albumB")); err != nil {
			t.Fatal(err)
		}
		waitForCondition(t, testWaitTimeout, "uploads dir rename isolated", func() bool {
			uSnaps := loadSourceSnapshotsMap(t, st, store.SourceUploads)
			mSnaps := loadSourceSnapshotsMap(t, st, store.SourceMusic)
			_, uOld := uSnaps["albumA/track.wav"]
			_, uNew := uSnaps["albumB/track.wav"]
			_, mOrig := mSnaps["albumA/track.wav"]
			return !uOld && uNew && mOrig && svc.Status().Count == 4
		})

		// 4. Synthetic root remove/rename on uploads watcher logs "uploads root directory".
		uw := svc.uploadsWatcher.Load()
		if uw == nil {
			t.Fatalf("uploadsWatcher is nil")
		}
		uploadsWalkRoot, err := filepath.EvalSymlinks(uploadsDir)
		if err != nil {
			t.Fatal(err)
		}
		uw.fsWatcher.Events <- fsnotify.Event{Name: uploadsWalkRoot, Op: fsnotify.Remove}
		waitForCondition(t, testWaitTimeout, "uploads root remove warning logged", func() bool {
			return logs.countWarningsContaining("uploads root directory") >= 1
		})

		// 5. Shutdown closes both watchers cleanly.
		cancel()
		svc.Wait()

		if got := svc.pendingCount(); got != 0 {
			t.Errorf("pendingCount after Wait() = %d, want 0", got)
		}
		fwMu.Lock()
		fws := slices.Clone(capturedFWs)
		fwMu.Unlock()
		for i, fw := range fws {
			if err := fw.Add(uploadsDir); !errors.Is(err, fsnotify.ErrClosed) {
				t.Errorf("capturedFW[%d].Add after Wait() = %v, want fsnotify.ErrClosed", i, err)
			}
		}
	})

	t.Run("uploads watcher overflow reconciles uploads and advances LastScan when musicDir is unavailable", func(t *testing.T) {
		st := openTestStore(t)
		parent := t.TempDir()
		missingMusicDir := filepath.Join(parent, "missing-music")
		uploadsDir := filepath.Join(parent, "uploads")
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "01 - Initial.wav"))

		// Pre-seed a SourceMusic track in SQLite to prove overflow rescan on uploads never touches music rows.
		musicSeed := buildTrack(store.SourceMusic, "01 - SeededMusic.wav", 1234, 5678, metadata.Info{
			Title:       "Seeded Music",
			Artist:      "Seed Artist",
			AlbumArtist: "Seed Artist",
			Album:       "Seed Album",
		})
		if err := st.UpsertTrack(context.Background(), &musicSeed); err != nil {
			t.Fatal(err)
		}

		logs, logger := newLogCapture()
		svc := newTestService(t, missingMusicDir, 0, st, logger, WithUploadsDir(uploadsDir))

		var (
			fwMu      sync.Mutex
			uploadsFW *fsnotify.Watcher
		)
		svc.newWatcherFn = func() (*fsnotify.Watcher, error) {
			fw, err := fsnotify.NewWatcher()
			if err == nil {
				fwMu.Lock()
				uploadsFW = fw
				fwMu.Unlock()
			}
			return fw, err
		}

		_, cancel := startAndWaitReady(t, svc)

		if logs.countWarningsContaining("music directory unavailable") != 1 {
			t.Errorf("expected music directory unavailable warning, got:\n%s", logs.allText())
		}
		if svc.musicWatcher.Load() != nil {
			t.Fatalf("musicWatcher = non-nil, want nil when musicDir is missing")
		}
		if svc.uploadsWatcher.Load() == nil {
			t.Fatalf("uploadsWatcher = nil, want active uploads watcher")
		}
		// Initial ScanNow failed on missingMusicDir while indexing uploadsDir, so LastScan is zero and Count == 2.
		before := svc.Status()
		if before.Count != 2 || !before.LastScan.IsZero() {
			t.Fatalf("initial Status() = %+v, want Count=2 and zero LastScan", before)
		}

		// Drop a file while temporarily suppressing or triggering overflow via ErrEventOverflow.
		// Using a non-audio or ignored file won't be indexed, so write a WAV then immediately inject overflow.
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "02 - Overflowed.wav"))
		fwMu.Lock()
		fw := uploadsFW
		fwMu.Unlock()
		if fw == nil {
			t.Fatalf("uploadsFW is nil")
		}
		fw.Errors <- fsnotify.ErrEventOverflow

		waitForCondition(t, testWaitTimeout, "uploads ErrEventOverflow reconciled and advanced LastScan", func() bool {
			if logs.countWarningsContaining("overflow") < 1 {
				return false
			}
			stNow := svc.Status()
			_, u2OK := loadSourceSnapshotsMap(t, st, store.SourceUploads)["02 - Overflowed.wav"]
			_, mOK := loadSourceSnapshotsMap(t, st, store.SourceMusic)["01 - SeededMusic.wav"]
			return u2OK && mOK && stNow.Count == 3 && stNow.LastScan.After(before.LastScan)
		})

		// Verify runLifecycle stayed blocked on <-ctx.Done() and live watching continues.
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "03 - PostOverflowLive.wav"))
		waitForCondition(t, testWaitTimeout, "live upload after overflow indexed", func() bool {
			_, ok := loadSourceSnapshotsMap(t, st, store.SourceUploads)["03 - PostOverflowLive.wav"]
			return ok && svc.Status().Count == 4
		})

		cancel()
		svc.Wait()
		if err := fw.Add(uploadsDir); !errors.Is(err, fsnotify.ErrClosed) {
			t.Errorf("uploadsFW.Add after Wait() = %v, want fsnotify.ErrClosed", err)
		}
	})

	t.Run("missing uploadsDir at Start degrades uploads watcher while keeping music watcher active", func(t *testing.T) {
		st := openTestStore(t)
		parent := t.TempDir()
		musicDir := filepath.Join(parent, "music")
		missingUploadsDir := filepath.Join(parent, "missing-uploads")
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "01 - Music.wav"))

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger, WithUploadsDir(missingUploadsDir))
		startAndWaitReady(t, svc)

		if logs.countWarningsContaining("uploads directory unavailable") != 1 {
			t.Errorf("expected uploads directory unavailable warning, got:\n%s", logs.allText())
		}
		if svc.musicWatcher.Load() == nil {
			t.Fatalf("musicWatcher = nil, want active music watcher")
		}
		if svc.uploadsWatcher.Load() != nil {
			t.Fatalf("uploadsWatcher = non-nil, want nil when uploadsDir is missing")
		}

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "02 - LiveMusic.wav"))
		waitForCondition(t, testWaitTimeout, "music watcher stays active when uploadsDir missing", func() bool {
			_, ok := loadSourceSnapshotsMap(t, st, store.SourceMusic)["02 - LiveMusic.wav"]
			return ok && svc.Status().Count == 2
		})
	})

	t.Run("uploads queue overflow reconciles uploads watches and DB without touching music", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		uploadsWalkRoot, err := filepath.EvalSymlinks(uploadsDir)
		if err != nil {
			t.Fatal(err)
		}
		musicWalkRoot, err := filepath.EvalSymlinks(musicDir)
		if err != nil {
			t.Fatal(err)
		}

		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "olddir", "shared.wav"))
		oldUploadsDir := filepath.Join(uploadsDir, "olddir")
		testutil.WriteMinimalWAV(t, filepath.Join(oldUploadsDir, "shared.wav"))

		svc := newTestService(t, musicDir, 0, st, nil, WithUploadsDir(uploadsDir))
		svc.maxEventQueue = 2

		dispatchedFirst := make(chan struct{})
		releaseDispatch := make(chan struct{})
		var blockOnce sync.Once
		var releaseOnce sync.Once
		closeRelease := func() {
			releaseOnce.Do(func() {
				close(releaseDispatch)
			})
		}
		svc.beforeDispatch = func(ctx context.Context) {
			blockOnce.Do(func() {
				close(dispatchedFirst)
				select {
				case <-releaseDispatch:
				case <-ctx.Done():
				}
			})
		}

		startAndWaitReady(t, svc)
		t.Cleanup(closeRelease)

		beforeScan := svc.Status().LastScan
		if err := os.RemoveAll(oldUploadsDir); err != nil {
			t.Fatal(err)
		}
		waitForChan(t, dispatchedFirst, testWaitTimeout, "dispatchedFirst")

		newUploadsDir := filepath.Join(uploadsDir, "newdir")
		for _, name := range []string{"a.wav", "b.wav", "c.wav", "d.wav"} {
			testutil.WriteMinimalWAV(t, filepath.Join(newUploadsDir, name))
		}
		for i := range 5 {
			testutil.WriteBytes(t, filepath.Join(uploadsDir, fmt.Sprintf("burst-%d.txt", i)), []byte("overflow"))
		}

		uw := svc.uploadsWatcher.Load()
		if uw == nil {
			t.Fatalf("uploadsWatcher = nil")
		}
		waitForCondition(t, testWaitTimeout, "uploads queue overflow triggered rescan", func() bool {
			return uw.rescanCount.Load() >= 1
		})
		closeRelease()

		waitForCondition(t, testWaitTimeout, "uploads overflow updated uploads DB/watches and kept music intact", func() bool {
			uSnaps := loadSourceSnapshotsMap(t, st, store.SourceUploads)
			mSnaps := loadSourceSnapshotsMap(t, st, store.SourceMusic)
			_, uOldExists := uSnaps["olddir/shared.wav"]
			_, uAOK := uSnaps["newdir/a.wav"]
			_, uDOK := uSnaps["newdir/d.wav"]
			_, mSharedOK := mSnaps["olddir/shared.wav"]
			if uOldExists || !uAOK || !uDOK || !mSharedOK || svc.Status().Count != 5 {
				return false
			}
			if !svc.Status().LastScan.After(beforeScan) {
				return false
			}
			uWatched, uWL := svc.watchSnapshotFor(store.SourceUploads)
			oldWatch := filepath.Join(uploadsWalkRoot, "olddir")
			newWatch := filepath.Join(uploadsWalkRoot, "newdir")
			mWatched, _ := svc.watchSnapshotFor(store.SourceMusic)
			return !slices.Contains(uWatched, oldWatch) &&
				!slices.Contains(uWL, oldWatch) &&
				slices.Contains(uWatched, newWatch) &&
				slices.Contains(uWL, newWatch) &&
				slices.Contains(mWatched, filepath.Join(musicWalkRoot, "olddir"))
		})
	})
}

func TestIngestUpload(t *testing.T) {
	t.Run("insert, overwrite preserving ID and CreatedAt, and coexistence with music root", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()

		relPath := "Artist/Album/01 - Upload.wav"
		musicFile := filepath.Join(musicDir, filepath.FromSlash(relPath))
		testutil.WriteMinimalWAV(t, musicFile)
		testutil.WriteTags(t, musicFile, map[string][]string{taglib.Title: {"Music Version"}})

		svc := newTestService(t, musicDir, 0, st, nil, WithUploadsDir(uploadsDir))
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("ScanNow() unexpected error: %v", err)
		}
		musicTrack := mustGetTrackBySourceRelPath(t, st, store.SourceMusic, relPath)

		uploadFile := filepath.Join(uploadsDir, filepath.FromSlash(relPath))
		testutil.WriteMinimalWAV(t, uploadFile)
		testutil.WriteTags(t, uploadFile, map[string][]string{
			taglib.Title:       {"Upload v1"},
			taglib.Artist:      {"Upload Artist"},
			taglib.AlbumArtist: {"Upload Album Artist"},
			taglib.Album:       {"Upload Album"},
			taglib.TrackNumber: {"1/10"},
			taglib.Date:        {"2024"},
			taglib.Genre:       {"Ambient"},
		})

		tr1, err := svc.IngestUpload(context.Background(), relPath)
		if err != nil {
			t.Fatalf("IngestUpload(v1) unexpected error: %v", err)
		}
		if tr1 == nil || !strings.HasPrefix(tr1.ID, "tr_") || tr1.Source != store.SourceUploads || tr1.RelPath != relPath {
			t.Fatalf("IngestUpload(v1) track = %+v, want SourceUploads, RelPath=%q, and tr_ ID", tr1, relPath)
		}
		if tr1.ID == musicTrack.ID {
			t.Errorf("upload track ID %q collided with music track ID", tr1.ID)
		}
		if tr1.Title != "Upload v1" || tr1.Artist != "Upload Artist" || tr1.AlbumArtist != "Upload Album Artist" ||
			tr1.Album != "Upload Album" || tr1.TrackNum != 1 || tr1.Year != 2024 || tr1.Genre != "Ambient" ||
			tr1.DurationSec != 2 || tr1.CreatedAt.IsZero() {
			t.Errorf("IngestUpload(v1) metadata = %+v", tr1)
		}
		if got := svc.Status().Count; got != 2 {
			t.Errorf("Status().Count after initial IngestUpload = %d, want 2", got)
		}

		// Backdate CreatedAt directly via store so overwrite preservation is observable beyond 1s resolution.
		backdated := *tr1
		backdated.CreatedAt = time.Date(2021, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := st.DeleteByRelPath(context.Background(), store.SourceUploads, relPath); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertTrack(context.Background(), &backdated); err != nil {
			t.Fatal(err)
		}

		// Overwrite upload file with new metadata and size.
		testutil.WriteTags(t, uploadFile, map[string][]string{
			taglib.Title:       {"Upload v2 Overwritten"},
			taglib.Artist:      {"Upload Artist"},
			taglib.AlbumArtist: {"Upload Album Artist"},
			taglib.Album:       {"Upload Album"},
		})
		info2, err := os.Stat(uploadFile)
		if err != nil {
			t.Fatal(err)
		}

		tr2, err := svc.IngestUpload(context.Background(), relPath)
		if err != nil {
			t.Fatalf("IngestUpload(v2) unexpected error: %v", err)
		}
		if tr2.ID != backdated.ID || !tr2.CreatedAt.Equal(backdated.CreatedAt) {
			t.Errorf("overwrite identity = (ID=%q, CreatedAt=%v), want (%q, %v)",
				tr2.ID, tr2.CreatedAt, backdated.ID, backdated.CreatedAt)
		}
		if tr2.Title != "Upload v2 Overwritten" || tr2.FileSize != info2.Size() || tr2.MtimeNs != info2.ModTime().UnixNano() {
			t.Errorf("overwrite fields = %+v, want Title=%q FileSize=%d MtimeNs=%d",
				tr2, "Upload v2 Overwritten", info2.Size(), info2.ModTime().UnixNano())
		}
		if got := svc.Status().Count; got != 2 {
			t.Errorf("Status().Count after overwrite = %d, want 2", got)
		}
		afterMusic := mustGetTrackBySourceRelPath(t, st, store.SourceMusic, relPath)
		if afterMusic.Title != "Music Version" || afterMusic.ID != musicTrack.ID {
			t.Errorf("music track modified by upload ingest: %+v", afterMusic)
		}
	})

	t.Run("uploadsDir reached through parent symlink still succeeds for regular files inside", func(t *testing.T) {
		st := openTestStore(t)
		parent := t.TempDir()
		musicDir := filepath.Join(parent, "music")
		realUploads := filepath.Join(parent, "real-uploads")
		linkUploads := filepath.Join(parent, "uploads-link")
		if err := os.MkdirAll(musicDir, 0o755); err != nil {
			t.Fatal(err)
		}
		testutil.WriteMinimalWAV(t, filepath.Join(realUploads, "Artist", "01.wav"))
		if err := os.Symlink(realUploads, linkUploads); err != nil {
			t.Fatalf("os.Symlink: %v", err)
		}

		svc := newTestService(t, musicDir, 0, st, nil, WithUploadsDir(linkUploads))
		tr, err := svc.IngestUpload(context.Background(), "Artist/01.wav")
		if err != nil {
			t.Fatalf("IngestUpload through symlinked root unexpected error: %v", err)
		}
		if tr.Source != store.SourceUploads || tr.RelPath != "Artist/01.wav" {
			t.Errorf("IngestUpload track = %+v, want uploads Artist/01.wav", tr)
		}
	})

	t.Run("validation, confinement, and error rejections do not mutate store", func(t *testing.T) {
		st := openTestStore(t)
		parent := t.TempDir()
		musicDir := filepath.Join(parent, "music")
		uploadsDir := filepath.Join(parent, "uploads")
		outsideDir := filepath.Join(parent, "outside")
		if err := os.MkdirAll(musicDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Targets for rejection cases.
		testutil.WriteBytes(t, filepath.Join(uploadsDir, "empty.wav"), nil)
		testutil.WriteBytes(t, filepath.Join(uploadsDir, "notes.txt"), []byte("not audio"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, ".DS_Store"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, ".hidden", "song.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "Artist", ".upload-123.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "Artist", "song.wav.tmp"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "Artist", "dir.tmp", "song.wav"))
		if err := os.MkdirAll(filepath.Join(uploadsDir, "dir.wav"), 0o755); err != nil {
			t.Fatal(err)
		}

		realInTree := filepath.Join(uploadsDir, "real", "song.wav")
		testutil.WriteMinimalWAV(t, realInTree)
		outsideTarget := filepath.Join(outsideDir, "outside.wav")
		testutil.WriteMinimalWAV(t, outsideTarget)

		if err := os.Symlink(realInTree, filepath.Join(uploadsDir, "leaf-in-tree.wav")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideTarget, filepath.Join(uploadsDir, "leaf-outside.wav")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(uploadsDir, "nonexistent.wav"), filepath.Join(uploadsDir, "leaf-broken.wav")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(uploadsDir, "real"), filepath.Join(uploadsDir, "dir-link-inside")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, filepath.Join(uploadsDir, "dir-link-outside")); err != nil {
			t.Fatal(err)
		}

		singleRootSvc := newTestService(t, musicDir, 0, st, nil)
		if _, err := singleRootSvc.IngestUpload(context.Background(), "song.wav"); err == nil {
			t.Errorf("IngestUpload on single-root service succeeded, want error")
		}

		svc := newTestService(t, musicDir, 0, st, nil, WithUploadsDir(uploadsDir))

		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := svc.IngestUpload(canceledCtx, "real/song.wav"); !errors.Is(err, context.Canceled) {
			t.Errorf("IngestUpload(canceledCtx) error = %v, want context.Canceled", err)
		}
		if _, err := svc.IngestUpload(nil, "real/song.wav"); err == nil {
			t.Errorf("IngestUpload(nil ctx) succeeded, want error")
		}

		badPaths := []string{
			"",
			".",
			"..",
			"../outside/outside.wav",
			"/abs.wav",
			"real//song.wav",
			"real/./song.wav",
			"real/../real/song.wav",
			"real/",
			"bad\x00name.wav",
			".DS_Store",
			".hidden/song.wav",
			"Artist/.upload-123.wav",
			"Artist/song.wav.tmp",
			"Artist/dir.tmp/song.wav",
			"notes.txt",
			"noext",
			"missing.wav",
			"empty.wav",
			"dir.wav",
			"leaf-in-tree.wav",
			"leaf-outside.wav",
			"leaf-broken.wav",
			"dir-link-inside/song.wav",
			"dir-link-outside/outside.wav",
		}
		for _, p := range badPaths {
			t.Run(fmt.Sprintf("reject_%q", p), func(t *testing.T) {
				tr, err := svc.IngestUpload(context.Background(), p)
				if err == nil {
					t.Fatalf("IngestUpload(%q) succeeded (%+v), want error", p, tr)
				}
			})
		}

		if got := len(loadSourceSnapshotsMap(t, st, store.SourceUploads)); got != 0 {
			t.Errorf("uploads snapshots after rejections = %d, want 0", got)
		}
		if got := svc.Status().Count; got != 0 {
			t.Errorf("Status().Count after rejections = %d, want 0", got)
		}
	})

	t.Run("concurrent IngestUpload and ScanNow serialize cleanly under race", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		uploadsDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "m0.wav"))

		svc := newTestService(t, musicDir, 0, st, nil, WithUploadsDir(uploadsDir))
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatal(err)
		}

		const nUploads = 8
		relPaths := make([]string, nUploads)
		for i := range nUploads {
			rel := fmt.Sprintf("batch/track-%02d.wav", i)
			relPaths[i] = rel
			testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, filepath.FromSlash(rel)))
		}

		var wg sync.WaitGroup
		errCh := make(chan error, nUploads+4)
		for _, rel := range relPaths {
			wg.Add(1)
			go func(r string) {
				defer wg.Done()
				tr, err := svc.IngestUpload(context.Background(), r)
				if err != nil {
					errCh <- fmt.Errorf("IngestUpload(%q): %w", r, err)
					return
				}
				if tr.Source != store.SourceUploads || tr.RelPath != r {
					errCh <- fmt.Errorf("IngestUpload(%q) unexpected track %+v", r, tr)
				}
			}(rel)
		}
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := svc.ScanNow(context.Background()); err != nil {
					errCh <- fmt.Errorf("ScanNow: %w", err)
				}
			}()
		}
		wg.Wait()
		close(errCh)

		for err := range errCh {
			t.Error(err)
		}
		if got := svc.Status().Count; got != nUploads+1 {
			t.Errorf("final Status().Count = %d, want %d", got, nUploads+1)
		}
	})
}
