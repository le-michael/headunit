package scanner

import (
	"context"
	"encoding/binary"
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

func newTestService(t *testing.T, musicDir string, scanInterval time.Duration, st *store.Store, logger *slog.Logger) *Service {
	t.Helper()

	svc, err := New(musicDir, scanInterval, st, logger)
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

	snaps, err := st.LoadSnapshots(context.Background())
	if err != nil {
		t.Fatalf("LoadSnapshots() unexpected error: %v", err)
	}
	return snaps
}

func mustGetTrackByRelPath(t *testing.T, st *store.Store, relPath string) store.Track {
	t.Helper()

	snaps := loadSnapshotsMap(t, st)
	snap, ok := snaps[relPath]
	if !ok {
		t.Fatalf("snapshot for %q not found in store (keys=%v)", relPath, mapKeys(snaps))
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

func minimalWAVBytes() []byte {
	const (
		channels      uint16 = 1
		sampleRate    uint32 = 8000
		bitsPerSample uint16 = 16
		blockAlign           = channels * bitsPerSample / 8
		byteRate             = sampleRate * uint32(blockAlign)
		dataSize      uint32 = byteRate * 2 // 32,000 bytes = 2 seconds
	)

	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataSize)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataSize)
	return buf
}

func writeMinimalWAV(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, minimalWAVBytes(), 0o600); err != nil {
		t.Fatalf("writeMinimalWAV(%q): %v", path, err)
	}
}

func writeMinimalMP3(t *testing.T, path string) {
	t.Helper()

	const (
		frameSize  = 417
		frameCount = 77
	)

	buf := make([]byte, frameSize*frameCount)
	for i := range frameCount {
		off := i * frameSize
		buf[off] = 0xFF
		buf[off+1] = 0xFB
		buf[off+2] = 0x90
		buf[off+3] = 0x00
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("writeMinimalMP3(%q): %v", path, err)
	}
}

func writeTags(t *testing.T, path string, tags map[string][]string) {
	t.Helper()
	if err := taglib.WriteTags(path, tags, taglib.Clear); err != nil {
		t.Fatalf("taglib.WriteTags(%q): %v", path, err)
	}
}

func writeBytes(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", path, err)
	}
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
		writeMinimalMP3(t, mp3Path)
		writeTags(t, mp3Path, map[string][]string{
			taglib.Title:       {"One More Time"},
			taglib.Artist:      {"Daft Punk"},
			taglib.AlbumArtist: {"Daft Punk"},
			taglib.Album:       {"Discovery"},
			taglib.TrackNumber: {"1/14"},
			taglib.Date:        {"2001"},
			taglib.Genre:       {"Electronic"},
		})

		taggedWAV := filepath.Join(musicDir, "Various", "02 - Studio.wav")
		writeMinimalWAV(t, taggedWAV)
		writeTags(t, taggedWAV, map[string][]string{
			taglib.Title:       {"Studio Session"},
			taglib.Artist:      {"Wave Guest"},
			taglib.AlbumArtist: {"Various Artists"},
			taglib.Album:       {"Sessions"},
		})

		untaggedWAV := filepath.Join(musicDir, "Loose", "03 - Raw Wave.wav")
		writeMinimalWAV(t, untaggedWAV)

		corruptMP3 := filepath.Join(musicDir, "Loose", "04 - Corrupt Audio.mp3")
		writeBytes(t, corruptMP3, []byte("not real mp3 audio bytes"))

		audioNamedDirTrack := filepath.Join(musicDir, "MyMix.mp3", "01 - Inside.wav")
		writeMinimalWAV(t, audioNamedDirTrack)

		// Ignored files, hidden dirs, unsupported formats, 0-byte audio, and symlinks.
		writeMinimalWAV(t, filepath.Join(musicDir, ".DS_Store"))
		writeMinimalMP3(t, filepath.Join(musicDir, "._Track.mp3"))
		writeMinimalWAV(t, filepath.Join(musicDir, ".hidden", "song.wav"))
		writeMinimalWAV(t, filepath.Join(musicDir, "song.flac.part"))
		writeMinimalMP3(t, filepath.Join(musicDir, "song.mp3.tmp"))
		writeMinimalWAV(t, filepath.Join(musicDir, "song.wav.filepart"))
		writeMinimalMP3(t, filepath.Join(musicDir, "song.m4a.crdownload"))
		writeBytes(t, filepath.Join(musicDir, "notes.txt"), []byte("liner notes"))
		writeBytes(t, filepath.Join(musicDir, "empty.wav"), nil)

		outsideTarget := filepath.Join(t.TempDir(), "outside.wav")
		writeMinimalWAV(t, outsideTarget)
		if err := os.Symlink(outsideTarget, filepath.Join(musicDir, "linked-file.wav")); err != nil {
			t.Fatalf("os.Symlink(file): %v", err)
		}
		outsideDir := t.TempDir()
		writeMinimalWAV(t, filepath.Join(outsideDir, "inside-linked-dir.wav"))
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
		writeMinimalWAV(t, filepath.Join(dotRoot, "Sub", "01 - Dot Root Track.wav"))

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
	writeMinimalWAV(t, p1)
	writeMinimalWAV(t, p2)
	writeTags(t, p1, map[string][]string{taglib.Title: {"Alpha v1"}})
	writeTags(t, p2, map[string][]string{taglib.Title: {"Beta v1"}})

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
	if _, err := st.LoadSnapshots(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Replace row with explicit older CreatedAt to test preservation across edits.
	if err := st.DeleteByRelPath(context.Background(), orig1.RelPath); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrack(context.Background(), &backdated); err != nil {
		t.Fatal(err)
	}
	orig1 = mustGetTrackByRelPath(t, st, "01 - Alpha.wav")

	writeTags(t, p1, map[string][]string{taglib.Title: {"Alpha v2 Updated"}})
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
		writeMinimalWAV(t, p1)
		writeMinimalWAV(t, p2)

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
		writeMinimalWAV(t, filepath.Join(blockedDir, "01 - HiddenByPerm.wav"))
		removedFile := filepath.Join(musicDir, "02 - StaleIfHealthy.wav")
		writeMinimalWAV(t, removedFile)

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
		writeMinimalWAV(t, filepath.Join(musicDir, "03 - HealthyNew.wav"))

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
		writeMinimalWAV(t, badFile)
		writeMinimalWAV(t, removedFile)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		if err := os.Remove(removedFile); err != nil {
			t.Fatal(err)
		}
		// Touch badFile so fast-path does not skip it, and add a healthy new track.
		writeMinimalWAV(t, badFile)
		writeTags(t, badFile, map[string][]string{taglib.Title: {"Changed"}})
		healthyNew := filepath.Join(musicDir, "03 - Healthy.wav")
		writeMinimalWAV(t, healthyNew)

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
		writeMinimalWAV(t, keepFile)
		writeMinimalWAV(t, vanishingFile)

		logs, logger := newLogCapture()
		svc := newTestService(t, musicDir, 0, st, logger)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}

		// Modify vanishingFile so extractFn is invoked, then delete it during extractFn.
		writeTags(t, vanishingFile, map[string][]string{taglib.Title: {"Modified Before Vanish"}})
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
		writeMinimalWAV(t, filepath.Join(musicDir, "01 - Track.wav"))

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
		writeBytes(t, fileRoot, []byte("file"))
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
		writeMinimalWAV(t, filepath.Join(musicDir, "01 - Seed.wav"))

		svc := newTestService(t, musicDir, 0, st, nil)
		if err := svc.ScanNow(context.Background()); err != nil {
			t.Fatalf("seed ScanNow() unexpected error: %v", err)
		}
		before := svc.Status()

		// 1. Pre-canceled context fails immediately without scanning new files.
		writeMinimalWAV(t, filepath.Join(musicDir, "02 - Unscanned.wav"))
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
	writeMinimalWAV(t, nestedFile)
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
	writeMinimalMP3(t, tmpFile)
	writeTags(t, tmpFile, map[string][]string{taglib.Title: {"Atomic Title"}})
	writeMinimalMP3(t, filepath.Join(musicDir, "._Ignored.mp3"))
	writeMinimalWAV(t, filepath.Join(musicDir, ".hidden", "ignored.wav"))

	outsideFile := filepath.Join(t.TempDir(), "outside.wav")
	writeMinimalWAV(t, outsideFile)
	if err := os.Symlink(outsideFile, filepath.Join(musicDir, "linked-file.wav")); err != nil {
		t.Fatalf("os.Symlink(file): %v", err)
	}
	outsideDir := t.TempDir()
	writeMinimalWAV(t, filepath.Join(outsideDir, "outside.wav"))
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
	writeTags(t, finalMP3, map[string][]string{taglib.Title: {"Atomic Title Edited"}})
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
	writeMinimalWAV(t, audioNamedChild)
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

		fullWAV := minimalWAVBytes()
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
		writeBytes(t, growingPath, fullWAV[:chunk1End])

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
		writeBytes(t, zeroPath, nil)

		waitForCondition(t, testWaitTimeout, "zero-byte file hits maxStabilityAttempts", func() bool {
			return logs.countWarningsContaining("did not stabilize") >= 1
		})
		if got := svc.Status().Count; got != 0 || len(loadSnapshotsMap(t, st)) != 0 {
			t.Fatalf("zero-byte file was indexed unexpectedly: count=%d", got)
		}

		// Writing valid WAV bytes fires a fresh Write event resetting attempts to 0.
		writeMinimalWAV(t, zeroPath)
		waitForCondition(t, testWaitTimeout, "zero-byte file indexed after valid WAV write", func() bool {
			snap, ok := loadSnapshotsMap(t, st)["01 - Zero.wav"]
			return ok && snap.FileSize == int64(len(minimalWAVBytes())) && svc.Status().Count == 1
		})
	})

	t.Run("mid-parse mutation re-schedules unlocked and indexes final metadata", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		svc := newTestService(t, musicDir, 0, st, nil)

		staged := filepath.Join(musicDir, "01 - Mutate.wav.tmp")
		target := filepath.Join(musicDir, "01 - Mutate.wav")
		writeMinimalWAV(t, staged)
		writeTags(t, staged, map[string][]string{taglib.Title: {"Before Mutation"}})

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

		writeMinimalWAV(t, filepath.Join(musicDir, "01 - Vanish.wav"))

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
		writeMinimalWAV(t, filepath.Join(dirASub, "track.wav"))
		writeMinimalWAV(t, filepath.Join(dirAB, "keep.wav"))

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
		writeMinimalWAV(t, filepath.Join(dirB, "sub", "new.wav"))
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
		writeMinimalWAV(t, filepath.Join(dirB, "seed.wav"))

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

		writeMinimalWAV(t, filepath.Join(dirA, "after-replace.wav"))
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
		writeMinimalWAV(t, filepath.Join(oldDir, "old.wav"))

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
			writeMinimalWAV(t, filepath.Join(newDir, name))
		}
		for i := range 5 {
			writeBytes(t, filepath.Join(musicDir, fmt.Sprintf("burst-%d.txt", i)), []byte("overflow"))
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
		writeMinimalWAV(t, filepath.Join(newDir, "live.wav"))
		waitForCondition(t, testWaitTimeout, "live event after overflow indexed", func() bool {
			_, ok := loadSnapshotsMap(t, st)["newdir/live.wav"]
			return ok && svc.Status().Count == 5
		})
	})

	t.Run("fsnotify.ErrEventOverflow injection reconciles and continues watching", func(t *testing.T) {
		st := openTestStore(t)
		musicDir := t.TempDir()
		writeMinimalWAV(t, filepath.Join(musicDir, "01 - Initial.wav"))

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

		writeMinimalWAV(t, filepath.Join(musicDir, "after-overflow", "02 - Live.wav"))
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
		writeMinimalWAV(t, filepath.Join(dirA, "sub", "orig.wav"))

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
		writeMinimalWAV(t, filepath.Join(musicDir, "trigger.wav"))
		waitForChan(t, dispatchedFirst, testWaitTimeout, "dispatchedFirst")

		if err := os.Rename(dirA, dirB); err != nil {
			t.Fatalf("os.Rename(%q, %q): %v", dirA, dirB, err)
		}
		writeMinimalWAV(t, filepath.Join(dirA, "sub", "recreated.wav"))
		for i := range 5 {
			writeBytes(t, filepath.Join(musicDir, fmt.Sprintf("burst-%d.txt", i)), []byte("overflow"))
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
		writeMinimalWAV(t, filepath.Join(dirA, "sub", "live-a.wav"))
		writeMinimalWAV(t, filepath.Join(dirB, "sub", "live-b.wav"))
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
		writeMinimalWAV(t, filepath.Join(musicDir, "01 - Seed.wav"))

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

		writeMinimalWAV(t, filepath.Join(musicDir, "02 - Polled.wav"))
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
		writeMinimalWAV(t, filepath.Join(musicDir, "sub1", "01 - One.wav"))
		writeMinimalWAV(t, filepath.Join(musicDir, "sub2", "02 - Two.wav"))
		writeMinimalWAV(t, filepath.Join(musicDir, "sub3", "03 - Three.wav"))

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

		writeMinimalWAV(t, filepath.Join(musicDir, "sub2", "04 - Polled.wav"))
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
	writeMinimalWAV(t, filepath.Join(musicDir, "base", "00 - Anchor.wav"))

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
		wavData := minimalWAVBytes()
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
		writeMinimalWAV(t, p1)
		writeMinimalWAV(t, p2)
		writeMinimalWAV(t, p3)

		for _, p := range []string{p1, p2} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			rel, ok := toRelPath(musicDir, p)
			if !ok {
				t.Fatalf("toRelPath(%q) failed", p)
			}
			tr := buildTrack(rel, info.Size(), info.ModTime().UnixNano(), metadata.FallbackFromFilename(p))
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
		writeMinimalWAV(t, filepath.Join(musicDir, "04 - MidDebounce.wav"))
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

		writeMinimalWAV(t, filepath.Join(musicDir, "01 - MountedLater.wav"))
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
		writeMinimalWAV(t, filepath.Join(musicDir, "01 - MidStability.wav"))
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
