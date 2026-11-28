package stream_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"headunit/internal/store"
	"headunit/internal/stream"
	"headunit/internal/testutil"
)

type spyResponseWriter struct {
	rec              *httptest.ResponseRecorder
	headerCalls      int
	writeHeaderCalls int
	writeCalls       int
}

func newSpyResponseWriter() *spyResponseWriter {
	return &spyResponseWriter{rec: httptest.NewRecorder()}
}

func (s *spyResponseWriter) Header() http.Header {
	s.headerCalls++
	return s.rec.Header()
}

func (s *spyResponseWriter) Write(p []byte) (int, error) {
	s.writeCalls++
	return s.rec.Write(p)
}

func (s *spyResponseWriter) WriteHeader(statusCode int) {
	s.writeHeaderCalls++
	s.rec.WriteHeader(statusCode)
}

func (s *spyResponseWriter) assertUntouched(t *testing.T) {
	t.Helper()
	if s.headerCalls != 0 || s.writeHeaderCalls != 0 || s.writeCalls != 0 {
		t.Fatalf("response writer mutated on error: headerCalls=%d writeHeaderCalls=%d writeCalls=%d, want 0/0/0",
			s.headerCalls, s.writeHeaderCalls, s.writeCalls)
	}
}

func TestNewDirectStreamer(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	validDir := filepath.Join(baseDir, "valid")
	if err := os.Mkdir(validDir, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", validDir, err)
	}

	symlinkDir := filepath.Join(baseDir, "symlink-valid")
	if err := os.Symlink(validDir, symlinkDir); err != nil {
		t.Fatalf("os.Symlink(%q, %q): %v", validDir, symlinkDir, err)
	}

	regularFile := filepath.Join(baseDir, "not-a-dir.mp3")
	testutil.WriteMinimalMP3(t, regularFile)

	unreadableDir := filepath.Join(baseDir, "unreadable")
	if err := os.Mkdir(unreadableDir, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", unreadableDir, err)
	}

	tests := []struct {
		name       string
		musicDir   string
		setup      func(t *testing.T)
		wantErr    bool
		wantTarget error
		skipIfRoot bool
	}{
		{
			name:     "valid directory",
			musicDir: validDir,
		},
		{
			name:     "symlinked valid directory",
			musicDir: symlinkDir,
		},
		{
			name:     "empty string",
			musicDir: "",
			wantErr:  true,
		},
		{
			name:     "whitespace only",
			musicDir: "   \t\n ",
			wantErr:  true,
		},
		{
			name:       "non-existent directory",
			musicDir:   filepath.Join(baseDir, "missing"),
			wantErr:    true,
			wantTarget: os.ErrNotExist,
		},
		{
			name:     "regular file path",
			musicDir: regularFile,
			wantErr:  true,
		},
		{
			name:       "unreadable directory",
			musicDir:   unreadableDir,
			skipIfRoot: true,
			setup: func(t *testing.T) {
				t.Helper()
				if err := os.Chmod(unreadableDir, 0o000); err != nil {
					t.Fatalf("os.Chmod(%q, 0o000): %v", unreadableDir, err)
				}
				t.Cleanup(func() {
					_ = os.Chmod(unreadableDir, 0o755)
				})
			},
			wantErr:    true,
			wantTarget: os.ErrPermission,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipIfRoot && os.Geteuid() == 0 {
				t.Skip("skipping permission test when running as root")
			}
			if tc.setup != nil {
				tc.setup(t)
			}

			ds, err := stream.NewDirectStreamer(tc.musicDir)
			if tc.wantErr {
				if err == nil {
					if ds != nil {
						_ = ds.Close()
					}
					t.Fatalf("NewDirectStreamer(%q) = nil error, want error", tc.musicDir)
				}
				if tc.wantTarget != nil && !errors.Is(err, tc.wantTarget) {
					t.Fatalf("NewDirectStreamer(%q) error = %v, want errors.Is(..., %v)", tc.musicDir, err, tc.wantTarget)
				}
				return
			}

			if err != nil {
				t.Fatalf("NewDirectStreamer(%q) unexpected error: %v", tc.musicDir, err)
			}
			if ds == nil {
				t.Fatalf("NewDirectStreamer(%q) returned nil streamer", tc.musicDir)
			}
			if err := ds.Close(); err != nil {
				t.Fatalf("ds.Close() unexpected error: %v", err)
			}
		})
	}
}

func TestDirectStreamer_FullStreamAndFormats(t *testing.T) {
	t.Parallel()

	musicDir := t.TempDir()
	ds, err := stream.NewDirectStreamer(musicDir)
	if err != nil {
		t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
	}
	t.Cleanup(func() {
		if err := ds.Close(); err != nil {
			t.Errorf("ds.Close(): %v", err)
		}
	})

	htmlLike := []byte("<html><body>not html</body></html>")

	tests := []struct {
		name            string
		relPath         string
		trackID         string
		opts            stream.StreamOptions
		write           func(t testing.TB, fullPath string)
		wantContentType string
	}{
		{
			name:            "mp3",
			relPath:         "track.mp3",
			trackID:         "tr_00000000000000000000000000000001",
			write:           testutil.WriteMinimalMP3,
			wantContentType: "audio/mpeg",
		},
		{
			name:            "flac",
			relPath:         "track.flac",
			trackID:         "tr_00000000000000000000000000000002",
			write:           testutil.WriteMinimalFLAC,
			wantContentType: "audio/flac",
		},
		{
			name:            "wav",
			relPath:         "track.wav",
			trackID:         "tr_00000000000000000000000000000003",
			write:           testutil.WriteMinimalWAV,
			wantContentType: "audio/wav",
		},
		{
			name:    "m4a",
			relPath: "track.m4a",
			trackID: "tr_00000000000000000000000000000004",
			write: func(t testing.TB, p string) {
				testutil.WriteBytes(t, p, []byte("m4a-payload"))
			},
			wantContentType: "audio/mp4",
		},
		{
			name:    "aac",
			relPath: "track.aac",
			trackID: "tr_00000000000000000000000000000005",
			write: func(t testing.TB, p string) {
				testutil.WriteBytes(t, p, []byte("aac-payload"))
			},
			wantContentType: "audio/aac",
		},
		{
			name:    "ogg",
			relPath: "track.ogg",
			trackID: "tr_00000000000000000000000000000006",
			write: func(t testing.TB, p string) {
				testutil.WriteBytes(t, p, []byte("ogg-payload"))
			},
			wantContentType: "audio/ogg",
		},
		{
			name:    "opus",
			relPath: "track.opus",
			trackID: "tr_00000000000000000000000000000007",
			write: func(t testing.TB, p string) {
				testutil.WriteBytes(t, p, []byte("opus-payload"))
			},
			wantContentType: "audio/ogg",
		},
		{
			name:    "uppercase OPUS",
			relPath: "upper-opus.OPUS",
			trackID: "tr_00000000000000000000000000000008",
			write: func(t testing.TB, p string) {
				testutil.WriteBytes(t, p, []byte("upper-opus"))
			},
			wantContentType: "audio/ogg",
		},
		{
			name:            "uppercase FLAC",
			relPath:         "upper-flac.FLAC",
			trackID:         "tr_00000000000000000000000000000009",
			write:           testutil.WriteMinimalFLAC,
			wantContentType: "audio/flac",
		},
		{
			name:            "nested path with StreamOptions ignored",
			relPath:         "artist/album/01.wav",
			trackID:         "tr_0000000000000000000000000000000a",
			opts:            stream.StreamOptions{MaxBitRate: 128, Format: "mp3"},
			write:           testutil.WriteMinimalWAV,
			wantContentType: "audio/wav",
		},
		{
			name:    "unknown extension with HTML payload prevents sniffing",
			relPath: "payload.bin",
			trackID: "tr_0000000000000000000000000000000b",
			write: func(t testing.TB, p string) {
				testutil.WriteBytes(t, p, htmlLike)
			},
			wantContentType: "application/octet-stream",
		},
		{
			name:    "zero-byte regular mp3",
			relPath: "empty.mp3",
			trackID: "tr_0000000000000000000000000000000c",
			write: func(t testing.TB, p string) {
				testutil.WriteBytes(t, p, nil)
			},
			wantContentType: "audio/mpeg",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fullPath := filepath.Join(musicDir, filepath.FromSlash(tc.relPath))
			tc.write(t, fullPath)
			wantBytes, err := os.ReadFile(fullPath)
			if err != nil {
				t.Fatalf("os.ReadFile(%q): %v", fullPath, err)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/rest/stream?id="+tc.trackID, nil)
			track := &store.Track{ID: tc.trackID, RelPath: tc.relPath}

			if err := ds.Stream(rec, req, track, tc.opts); err != nil {
				t.Fatalf("ds.Stream() error: %v", err)
			}

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantContentType {
				t.Errorf("Content-Type = %q, want %q", got, tc.wantContentType)
			}
			if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
				t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			wantLen := fmt.Sprintf("%d", len(wantBytes))
			if got := rec.Header().Get("Content-Length"); got != wantLen {
				t.Errorf("Content-Length = %q, want %q", got, wantLen)
			}
			etag := rec.Header().Get("ETag")
			if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || strings.HasPrefix(etag, `W/`) {
				t.Errorf("ETag = %q, want strong quoted ETag", etag)
			}
			if got := rec.Header().Get("Last-Modified"); got == "" {
				t.Error("Last-Modified header is empty, want non-empty")
			}
			if !bytes.Equal(rec.Body.Bytes(), wantBytes) {
				t.Errorf("body len = %d, want %d (content mismatch)", rec.Body.Len(), len(wantBytes))
			}
		})
	}
}

func TestDirectStreamer_RangeRequests(t *testing.T) {
	t.Parallel()

	musicDir := t.TempDir()
	relPath := "album/song.wav"
	testutil.WriteMinimalWAV(t, filepath.Join(musicDir, filepath.FromSlash(relPath)))
	wantAll := testutil.MinimalWAVBytes()
	total := len(wantAll)

	ds, err := stream.NewDirectStreamer(musicDir)
	if err != nil {
		t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
	}
	t.Cleanup(func() {
		if err := ds.Close(); err != nil {
			t.Errorf("ds.Close(): %v", err)
		}
	})

	track := &store.Track{
		ID:      "tr_11111111111111111111111111111111",
		RelPath: relPath,
	}

	tests := []struct {
		name             string
		rangeHeader      string
		wantStatus       int
		wantBody         []byte
		wantContentRange string
	}{
		{
			name:             "AppleCoreMedia 2-byte probe",
			rangeHeader:      "bytes=0-1",
			wantStatus:       http.StatusPartialContent,
			wantBody:         wantAll[0:2],
			wantContentRange: fmt.Sprintf("bytes 0-1/%d", total),
		},
		{
			name:             "middle slice",
			rangeHeader:      "bytes=44-143",
			wantStatus:       http.StatusPartialContent,
			wantBody:         wantAll[44:144],
			wantContentRange: fmt.Sprintf("bytes 44-143/%d", total),
		},
		{
			name:             "open-ended tail",
			rangeHeader:      "bytes=32000-",
			wantStatus:       http.StatusPartialContent,
			wantBody:         wantAll[32000:],
			wantContentRange: fmt.Sprintf("bytes 32000-%d/%d", total-1, total),
		},
		{
			name:             "suffix range",
			rangeHeader:      "bytes=-16",
			wantStatus:       http.StatusPartialContent,
			wantBody:         wantAll[total-16:],
			wantContentRange: fmt.Sprintf("bytes %d-%d/%d", total-16, total-1, total),
		},
		{
			name:             "out-of-bounds range",
			rangeHeader:      "bytes=999999-",
			wantStatus:       http.StatusRequestedRangeNotSatisfiable,
			wantContentRange: fmt.Sprintf("bytes */%d", total),
		},
		{
			name:        "malformed range",
			rangeHeader: "bytes=invalid",
			wantStatus:  http.StatusRequestedRangeNotSatisfiable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/rest/stream?id="+track.ID, nil)
			req.Header.Set("Range", tc.rangeHeader)

			if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
				t.Fatalf("ds.Stream() error: %v", err)
			}

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			if tc.wantContentRange != "" {
				if got := rec.Header().Get("Content-Range"); got != tc.wantContentRange {
					t.Errorf("Content-Range = %q, want %q", got, tc.wantContentRange)
				}
			}
			if tc.wantStatus == http.StatusPartialContent {
				if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
					t.Errorf("Content-Type = %q, want %q", got, "audio/wav")
				}
				if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
					t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
				}
				wantLen := fmt.Sprintf("%d", len(tc.wantBody))
				if got := rec.Header().Get("Content-Length"); got != wantLen {
					t.Errorf("Content-Length = %q, want %q", got, wantLen)
				}
				if !bytes.Equal(rec.Body.Bytes(), tc.wantBody) {
					t.Errorf("body mismatch: got %d bytes, want %d bytes", rec.Body.Len(), len(tc.wantBody))
				}
			}
		})
	}
}

func TestDirectStreamer_HTTPMethods(t *testing.T) {
	t.Parallel()

	musicDir := t.TempDir()
	relPath := "song.wav"
	testutil.WriteMinimalWAV(t, filepath.Join(musicDir, relPath))
	wantAll := testutil.MinimalWAVBytes()

	ds, err := stream.NewDirectStreamer(musicDir)
	if err != nil {
		t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
	}
	t.Cleanup(func() {
		if err := ds.Close(); err != nil {
			t.Errorf("ds.Close(): %v", err)
		}
	})

	track := &store.Track{
		ID:      "tr_22222222222222222222222222222222",
		RelPath: relPath,
	}

	t.Run("HEAD full", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/rest/stream?id="+track.ID, nil)
		if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream() error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
			t.Errorf("Content-Type = %q, want %q", got, "audio/wav")
		}
		if got := rec.Header().Get("Content-Length"); got != fmt.Sprintf("%d", len(wantAll)) {
			t.Errorf("Content-Length = %q, want %d", got, len(wantAll))
		}
		if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
		}
		if got := rec.Header().Get("ETag"); got == "" {
			t.Error("ETag is empty, want non-empty")
		}
		if got := rec.Header().Get("Last-Modified"); got == "" {
			t.Error("Last-Modified is empty, want non-empty")
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
		}
	})

	t.Run("HEAD with Range", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/rest/stream?id="+track.ID, nil)
		req.Header.Set("Range", "bytes=0-1")
		if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream() error: %v", err)
		}
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Length"); got != "2" {
			t.Errorf("Content-Length = %q, want %q", got, "2")
		}
		if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-1/%d", len(wantAll)) {
			t.Errorf("Content-Range = %q, want bytes 0-1/%d", got, len(wantAll))
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
		}
	})

	t.Run("POST full", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/rest/stream", strings.NewReader("id="+track.ID))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream() error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !bytes.Equal(rec.Body.Bytes(), wantAll) {
			t.Errorf("POST body mismatch: got %d bytes, want %d", rec.Body.Len(), len(wantAll))
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
		}
	})

	t.Run("POST with Range", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/rest/stream", strings.NewReader("id="+track.ID))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Range", "bytes=0-15")
		if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream() error: %v", err)
		}
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
		}
		if !bytes.Equal(rec.Body.Bytes(), wantAll[0:16]) {
			t.Errorf("POST range body mismatch: got %d bytes, want 16", rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-15/%d", len(wantAll)) {
			t.Errorf("Content-Range = %q, want bytes 0-15/%d", got, len(wantAll))
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
		}
	})
}

func TestDirectStreamer_ConditionalAndETag(t *testing.T) {
	t.Parallel()

	musicDir := t.TempDir()
	ds, err := stream.NewDirectStreamer(musicDir)
	if err != nil {
		t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
	}
	t.Cleanup(func() {
		if err := ds.Close(); err != nil {
			t.Errorf("ds.Close(): %v", err)
		}
	})

	relPath := "conditional.wav"
	fullPath := filepath.Join(musicDir, relPath)
	testutil.WriteMinimalWAV(t, fullPath)
	t0 := time.Unix(1700000000, 100000000)
	if err := os.Chtimes(fullPath, t0, t0); err != nil {
		t.Fatalf("os.Chtimes(%q): %v", fullPath, err)
	}

	track := &store.Track{
		ID:      "tr_33333333333333333333333333333333",
		RelPath: relPath,
	}

	// Fetch initial ETag.
	initRec := httptest.NewRecorder()
	if err := ds.Stream(initRec, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), track, stream.StreamOptions{}); err != nil {
		t.Fatalf("initial ds.Stream() error: %v", err)
	}
	etag1 := initRec.Header().Get("ETag")
	if etag1 == "" {
		t.Fatal("initial ETag is empty")
	}

	t.Run("If-None-Match matching returns 304", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
		req.Header.Set("If-None-Match", etag1)
		if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream() error: %v", err)
		}
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("304 body len = %d, want 0", rec.Body.Len())
		}
		if got := rec.Header().Get("ETag"); got != etag1 {
			t.Errorf("ETag = %q, want %q", got, etag1)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
		}
	})

	t.Run("If-None-Match non-matching returns 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
		req.Header.Set("If-None-Match", `"stale-etag"`)
		if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream() error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !bytes.Equal(rec.Body.Bytes(), testutil.MinimalWAVBytes()) {
			t.Errorf("body mismatch: got %d bytes", rec.Body.Len())
		}
	})

	t.Run("If-Range matching strong ETag returns 206", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
		req.Header.Set("Range", "bytes=0-1")
		req.Header.Set("If-Range", etag1)
		if err := ds.Stream(rec, req, track, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream() error: %v", err)
		}
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
		}
		if !bytes.Equal(rec.Body.Bytes(), testutil.MinimalWAVBytes()[0:2]) {
			t.Errorf("body = %v, want %v", rec.Body.Bytes(), testutil.MinimalWAVBytes()[0:2])
		}
	})

	t.Run("same-second replacement invalidates ETag so stale If-Range returns 200 full body", func(t *testing.T) {
		updatedPath := filepath.Join(musicDir, "same-second.wav")
		updatedTrack := &store.Track{
			ID:      "tr_44444444444444444444444444444444",
			RelPath: "same-second.wav",
		}
		initialPayload := testutil.MinimalWAVBytes()
		testutil.WriteBytes(t, updatedPath, initialPayload)
		if err := os.Chtimes(updatedPath, t0, t0); err != nil {
			t.Fatalf("os.Chtimes(%q): %v", updatedPath, err)
		}

		firstRec := httptest.NewRecorder()
		if err := ds.Stream(firstRec, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), updatedTrack, stream.StreamOptions{}); err != nil {
			t.Fatalf("first ds.Stream() error: %v", err)
		}
		firstETag := firstRec.Header().Get("ETag")

		// Replace within the exact same Unix second with different size and sub-second ns.
		newPayload := append(testutil.MinimalWAVBytes(), 0xAA, 0xBB, 0xCC)
		testutil.WriteBytes(t, updatedPath, newPayload)
		t1 := time.Unix(1700000000, 200000000)
		if err := os.Chtimes(updatedPath, t1, t1); err != nil {
			t.Fatalf("os.Chtimes(%q): %v", updatedPath, err)
		}

		req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
		req.Header.Set("Range", "bytes=0-1")
		req.Header.Set("If-Range", firstETag)
		secondRec := httptest.NewRecorder()
		if err := ds.Stream(secondRec, req, updatedTrack, stream.StreamOptions{}); err != nil {
			t.Fatalf("second ds.Stream() error: %v", err)
		}
		if secondRec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (stale ETag should fall back to full 200)", secondRec.Code, http.StatusOK)
		}
		if !bytes.Equal(secondRec.Body.Bytes(), newPayload) {
			t.Errorf("body len = %d, want %d", secondRec.Body.Len(), len(newPayload))
		}
		if secondRec.Header().Get("ETag") == firstETag {
			t.Errorf("ETag did not change after same-second replacement: %q", firstETag)
		}
	})

	t.Run("empty and unsafe track IDs produce valid RFC 9110 strong ETags", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			trackID string
		}{
			{name: "empty ID", trackID: ""},
			{name: "unsafe ID with quotes and CRLF", trackID: "bad \"id\"\r\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tr := &store.Track{ID: tc.trackID, RelPath: relPath}
				rec := httptest.NewRecorder()
				if err := ds.Stream(rec, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), tr, stream.StreamOptions{}); err != nil {
					t.Fatalf("ds.Stream() error: %v", err)
				}
				etag := rec.Header().Get("ETag")
				if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || strings.HasPrefix(etag, `W/`) {
					t.Fatalf("ETag = %q, want strong quoted ETag", etag)
				}
				inner := etag[1 : len(etag)-1]
				if strings.ContainsAny(inner, "\"\r\n ") {
					t.Fatalf("ETag %q contains invalid characters", etag)
				}

				// Round-trip If-None-Match -> 304.
				inmRec := httptest.NewRecorder()
				inmReq := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
				inmReq.Header.Set("If-None-Match", etag)
				if err := ds.Stream(inmRec, inmReq, tr, stream.StreamOptions{}); err != nil {
					t.Fatalf("If-None-Match ds.Stream() error: %v", err)
				}
				if inmRec.Code != http.StatusNotModified {
					t.Errorf("If-None-Match status = %d, want %d", inmRec.Code, http.StatusNotModified)
				}

				// Round-trip If-Range -> 206.
				irRec := httptest.NewRecorder()
				irReq := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
				irReq.Header.Set("Range", "bytes=0-1")
				irReq.Header.Set("If-Range", etag)
				if err := ds.Stream(irRec, irReq, tr, stream.StreamOptions{}); err != nil {
					t.Fatalf("If-Range ds.Stream() error: %v", err)
				}
				if irRec.Code != http.StatusPartialContent {
					t.Errorf("If-Range status = %d, want %d", irRec.Code, http.StatusPartialContent)
				}
			})
		}
	})
}

func TestDirectStreamer_ErrorContractAndConfinement(t *testing.T) {
	t.Parallel()

	outerDir := t.TempDir()
	musicDir := filepath.Join(outerDir, "music")
	if err := os.Mkdir(musicDir, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", musicDir, err)
	}

	outsideFile := filepath.Join(outerDir, "secret.wav")
	testutil.WriteMinimalWAV(t, outsideFile)

	// Regular file inside musicDir.
	testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "song.wav"))
	testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "nested", "real.wav"))

	// Directory path inside musicDir.
	if err := os.Mkdir(filepath.Join(musicDir, "albumdir"), 0o755); err != nil {
		t.Fatalf("os.Mkdir albumdir: %v", err)
	}

	// Dangling symlink.
	if err := os.Symlink("does-not-exist.wav", filepath.Join(musicDir, "dangling.wav")); err != nil {
		t.Fatalf("os.Symlink dangling: %v", err)
	}

	// Escaping relative symlink.
	if err := os.Symlink("../secret.wav", filepath.Join(musicDir, "escape-rel.wav")); err != nil {
		t.Fatalf("os.Symlink escape-rel: %v", err)
	}

	// Absolute symlink pointing outside root.
	if err := os.Symlink(outsideFile, filepath.Join(musicDir, "escape-abs-out.wav")); err != nil {
		t.Fatalf("os.Symlink escape-abs-out: %v", err)
	}

	// Absolute symlink pointing inside root (still rejected by os.Root).
	if err := os.Symlink(filepath.Join(musicDir, "song.wav"), filepath.Join(musicDir, "escape-abs-in.wav")); err != nil {
		t.Fatalf("os.Symlink escape-abs-in: %v", err)
	}

	// Symlink loop.
	if err := os.Symlink("loop-b.wav", filepath.Join(musicDir, "loop-a.wav")); err != nil {
		t.Fatalf("os.Symlink loop-a: %v", err)
	}
	if err := os.Symlink("loop-a.wav", filepath.Join(musicDir, "loop-b.wav")); err != nil {
		t.Fatalf("os.Symlink loop-b: %v", err)
	}

	// Valid intra-root relative symlink.
	if err := os.Symlink("nested/real.wav", filepath.Join(musicDir, "ok-link.wav")); err != nil {
		t.Fatalf("os.Symlink ok-link: %v", err)
	}

	// FIFO (named pipe) inside musicDir.
	fifoPath := filepath.Join(musicDir, "pipe.wav")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("syscall.Mkfifo(%q): %v", fifoPath, err)
	}

	// Unreadable file and unreadable parent dir (permission tests).
	unreadableFile := filepath.Join(musicDir, "unreadable.wav")
	testutil.WriteMinimalWAV(t, unreadableFile)

	unreadableParent := filepath.Join(musicDir, "locked-dir")
	testutil.WriteMinimalWAV(t, filepath.Join(unreadableParent, "hidden.wav"))

	ds, err := stream.NewDirectStreamer(musicDir)
	if err != nil {
		t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
	}
	t.Cleanup(func() {
		if err := ds.Close(); err != nil {
			t.Errorf("ds.Close(): %v", err)
		}
	})

	// First verify valid intra-root relative symlink succeeds.
	t.Run("valid intra-root relative symlink succeeds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
		tr := &store.Track{ID: "tr_55555555555555555555555555555555", RelPath: "ok-link.wav"}
		if err := ds.Stream(rec, req, tr, stream.StreamOptions{}); err != nil {
			t.Fatalf("ds.Stream(ok-link.wav) unexpected error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !bytes.Equal(rec.Body.Bytes(), testutil.MinimalWAVBytes()) {
			t.Errorf("body mismatch for valid intra-root symlink")
		}
	})

	closedDS, err := stream.NewDirectStreamer(musicDir)
	if err != nil {
		t.Fatalf("NewDirectStreamer for closedDS: %v", err)
	}
	if err := closedDS.Close(); err != nil {
		t.Fatalf("closedDS.Close(): %v", err)
	}

	var nilDS *stream.DirectStreamer
	zeroDS := &stream.DirectStreamer{}

	tests := []struct {
		name          string
		streamer      *stream.DirectStreamer
		useNilWriter  bool
		useNilRequest bool
		track         *store.Track
		setup         func(t *testing.T)
		skipIfRoot    bool
		wantNotFound  bool
		wantTargetErr error
	}{
		{
			name:          "missing file",
			streamer:      ds,
			track:         &store.Track{ID: "tr_1", RelPath: "missing.wav"},
			wantNotFound:  true,
			wantTargetErr: os.ErrNotExist,
		},
		{
			name:          "dangling symlink",
			streamer:      ds,
			track:         &store.Track{ID: "tr_2", RelPath: "dangling.wav"},
			wantNotFound:  true,
			wantTargetErr: os.ErrNotExist,
		},
		{
			name:         "directory path",
			streamer:     ds,
			track:        &store.Track{ID: "tr_3", RelPath: "albumdir"},
			wantNotFound: true,
		},
		{
			name:          "non-directory intermediate component",
			streamer:      ds,
			track:         &store.Track{ID: "tr_4", RelPath: "song.wav/sub.wav"},
			wantNotFound:  true,
			wantTargetErr: syscall.ENOTDIR,
		},
		{
			name:         "invalid relPath empty",
			streamer:     ds,
			track:        &store.Track{ID: "tr_5", RelPath: ""},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath dot",
			streamer:     ds,
			track:        &store.Track{ID: "tr_6", RelPath: "."},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath dotdot",
			streamer:     ds,
			track:        &store.Track{ID: "tr_7", RelPath: ".."},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath parent traversal",
			streamer:     ds,
			track:        &store.Track{ID: "tr_8", RelPath: "../outside.wav"},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath leading slash",
			streamer:     ds,
			track:        &store.Track{ID: "tr_9", RelPath: "/etc/passwd"},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath trailing slash",
			streamer:     ds,
			track:        &store.Track{ID: "tr_10", RelPath: "sub/"},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath double slash",
			streamer:     ds,
			track:        &store.Track{ID: "tr_11", RelPath: "a//b.wav"},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath nested dotdot",
			streamer:     ds,
			track:        &store.Track{ID: "tr_12", RelPath: "sub/../a.wav"},
			wantNotFound: true,
		},
		{
			name:         "invalid relPath NUL byte",
			streamer:     ds,
			track:        &store.Track{ID: "tr_13", RelPath: "a\x00.wav"},
			wantNotFound: true,
		},
		{
			name:         "escaping relative symlink",
			streamer:     ds,
			track:        &store.Track{ID: "tr_14", RelPath: "escape-rel.wav"},
			wantNotFound: true,
		},
		{
			name:         "absolute symlink outside root",
			streamer:     ds,
			track:        &store.Track{ID: "tr_15", RelPath: "escape-abs-out.wav"},
			wantNotFound: true,
		},
		{
			name:         "absolute symlink inside root",
			streamer:     ds,
			track:        &store.Track{ID: "tr_16", RelPath: "escape-abs-in.wav"},
			wantNotFound: true,
		},
		{
			name:          "symlink loop",
			streamer:      ds,
			track:         &store.Track{ID: "tr_17", RelPath: "loop-a.wav"},
			wantNotFound:  true,
			wantTargetErr: syscall.ELOOP,
		},
		{
			name:         "FIFO rejected without blocking",
			streamer:     ds,
			track:        &store.Track{ID: "tr_18", RelPath: "pipe.wav"},
			wantNotFound: true,
		},
		{
			name:       "unreadable file returns EACCES",
			streamer:   ds,
			track:      &store.Track{ID: "tr_19", RelPath: "unreadable.wav"},
			skipIfRoot: true,
			setup: func(t *testing.T) {
				t.Helper()
				if err := os.Chmod(unreadableFile, 0o000); err != nil {
					t.Fatalf("os.Chmod(%q, 0o000): %v", unreadableFile, err)
				}
				t.Cleanup(func() {
					_ = os.Chmod(unreadableFile, 0o600)
				})
			},
			wantNotFound:  false,
			wantTargetErr: os.ErrPermission,
		},
		{
			name:       "unreadable parent dir returns EACCES",
			streamer:   ds,
			track:      &store.Track{ID: "tr_20", RelPath: "locked-dir/hidden.wav"},
			skipIfRoot: true,
			setup: func(t *testing.T) {
				t.Helper()
				if err := os.Chmod(unreadableParent, 0o000); err != nil {
					t.Fatalf("os.Chmod(%q, 0o000): %v", unreadableParent, err)
				}
				t.Cleanup(func() {
					_ = os.Chmod(unreadableParent, 0o755)
				})
			},
			wantNotFound:  false,
			wantTargetErr: os.ErrPermission,
		},
		{
			name:          "nil DirectStreamer",
			streamer:      nilDS,
			track:         &store.Track{ID: "tr_21", RelPath: "song.wav"},
			wantNotFound:  false,
			wantTargetErr: os.ErrClosed,
		},
		{
			name:          "zero-value DirectStreamer",
			streamer:      zeroDS,
			track:         &store.Track{ID: "tr_22", RelPath: "song.wav"},
			wantNotFound:  false,
			wantTargetErr: os.ErrClosed,
		},
		{
			name:          "closed DirectStreamer",
			streamer:      closedDS,
			track:         &store.Track{ID: "tr_23", RelPath: "song.wav"},
			wantNotFound:  false,
			wantTargetErr: os.ErrClosed,
		},
		{
			name:         "nil ResponseWriter",
			streamer:     ds,
			useNilWriter: true,
			track:        &store.Track{ID: "tr_24", RelPath: "song.wav"},
			wantNotFound: false,
		},
		{
			name:          "nil Request",
			streamer:      ds,
			useNilRequest: true,
			track:         &store.Track{ID: "tr_25", RelPath: "song.wav"},
			wantNotFound:  false,
		},
		{
			name:         "nil Track",
			streamer:     ds,
			track:        nil,
			wantNotFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipIfRoot && os.Geteuid() == 0 {
				t.Skip("skipping permission test when running as root")
			}
			if tc.setup != nil {
				tc.setup(t)
			}

			spy := newSpyResponseWriter()
			// Simulate pre-existing middleware header (e.g., CORS) that must remain untouched.
			spy.rec.Header().Set("Access-Control-Allow-Origin", "*")

			var w http.ResponseWriter = spy
			if tc.useNilWriter {
				w = nil
			}
			var r *http.Request
			if !tc.useNilRequest {
				r = httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
			}

			err := tc.streamer.Stream(w, r, tc.track, stream.StreamOptions{})
			if err == nil {
				t.Fatalf("Stream() = nil, want non-nil error")
			}
			if got := errors.Is(err, stream.ErrFileNotFound); got != tc.wantNotFound {
				t.Errorf("errors.Is(err, ErrFileNotFound) = %v, want %v (err=%v)", got, tc.wantNotFound, err)
			}
			if tc.wantTargetErr != nil && !errors.Is(err, tc.wantTargetErr) {
				t.Errorf("errors.Is(err, %v) = false (err=%v)", tc.wantTargetErr, err)
			}
			if strings.Contains(err.Error(), musicDir) {
				t.Errorf("error %q leaked host musicDir %q", err.Error(), musicDir)
			}

			spy.assertUntouched(t)
			if got := spy.rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("pre-existing CORS header = %q, want %q", got, "*")
			}
		})
	}
}

func TestDirectStreamer_CloseAndConcurrency(t *testing.T) {
	t.Parallel()

	t.Run("nil, zero, and repeated Close", func(t *testing.T) {
		var nilDS *stream.DirectStreamer
		if err := nilDS.Close(); err != nil {
			t.Fatalf("nilDS.Close() = %v, want nil", err)
		}

		zeroDS := &stream.DirectStreamer{}
		if err := zeroDS.Close(); err != nil {
			t.Fatalf("zeroDS.Close() = %v, want nil", err)
		}

		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "song.wav"))
		ds, err := stream.NewDirectStreamer(musicDir)
		if err != nil {
			t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
		}
		if err := ds.Close(); err != nil {
			t.Fatalf("first Close() = %v, want nil", err)
		}
		if err := ds.Close(); err != nil {
			t.Fatalf("second Close() = %v, want nil", err)
		}

		spy := newSpyResponseWriter()
		err = ds.Stream(spy, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), &store.Track{
			ID:      "tr_closed",
			RelPath: "song.wav",
		}, stream.StreamOptions{})
		if !errors.Is(err, os.ErrClosed) || errors.Is(err, stream.ErrFileNotFound) {
			t.Fatalf("post-Close Stream() error = %v, want os.ErrClosed and not ErrFileNotFound", err)
		}
		spy.assertUntouched(t)
	})

	t.Run("32 concurrent streams", func(t *testing.T) {
		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "song.wav"))
		wantWAV := testutil.MinimalWAVBytes()

		ds, err := stream.NewDirectStreamer(musicDir)
		if err != nil {
			t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
		}
		t.Cleanup(func() {
			if err := ds.Close(); err != nil {
				t.Errorf("ds.Close(): %v", err)
			}
		})

		const workers = 32
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := range workers {
			go func(idx int) {
				defer wg.Done()
				switch idx % 4 {
				case 0:
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
					if err := ds.Stream(rec, req, &store.Track{ID: "tr_ok", RelPath: "song.wav"}, stream.StreamOptions{}); err != nil {
						t.Errorf("worker %d GET error: %v", idx, err)
						return
					}
					if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wantWAV) {
						t.Errorf("worker %d GET mismatch: status=%d len=%d", idx, rec.Code, rec.Body.Len())
					}
				case 1:
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
					req.Header.Set("Range", "bytes=0-1")
					if err := ds.Stream(rec, req, &store.Track{ID: "tr_ok", RelPath: "song.wav"}, stream.StreamOptions{}); err != nil {
						t.Errorf("worker %d Range error: %v", idx, err)
						return
					}
					if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), wantWAV[:2]) {
						t.Errorf("worker %d Range mismatch: status=%d body=%v", idx, rec.Code, rec.Body.Bytes())
					}
				case 2:
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodHead, "/rest/stream", nil)
					if err := ds.Stream(rec, req, &store.Track{ID: "tr_ok", RelPath: "song.wav"}, stream.StreamOptions{}); err != nil {
						t.Errorf("worker %d HEAD error: %v", idx, err)
						return
					}
					if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
						t.Errorf("worker %d HEAD mismatch: status=%d len=%d", idx, rec.Code, rec.Body.Len())
					}
				case 3:
					spy := newSpyResponseWriter()
					req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
					err := ds.Stream(spy, req, &store.Track{ID: "tr_missing", RelPath: "missing.wav"}, stream.StreamOptions{})
					if !errors.Is(err, stream.ErrFileNotFound) {
						t.Errorf("worker %d missing file err = %v, want ErrFileNotFound", idx, err)
					}
					if spy.headerCalls != 0 || spy.writeHeaderCalls != 0 || spy.writeCalls != 0 {
						t.Errorf("worker %d mutated spy on error", idx)
					}
				}
			}(i)
		}
		wg.Wait()
	})

	t.Run("concurrent Stream and Close under -race", func(t *testing.T) {
		musicDir := t.TempDir()
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "song.wav"))
		wantWAV := testutil.MinimalWAVBytes()

		ds, err := stream.NewDirectStreamer(musicDir)
		if err != nil {
			t.Fatalf("NewDirectStreamer(%q): %v", musicDir, err)
		}

		const readers = 16
		var wg sync.WaitGroup
		wg.Add(readers + 2)

		for range readers {
			go func() {
				defer wg.Done()
				for range 5 {
					spy := newSpyResponseWriter()
					req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
					err := ds.Stream(spy, req, &store.Track{ID: "tr_race", RelPath: "song.wav"}, stream.StreamOptions{})
					if err == nil {
						if spy.rec.Code != http.StatusOK || !bytes.Equal(spy.rec.Body.Bytes(), wantWAV) {
							t.Errorf("concurrent Stream mismatch: status=%d len=%d", spy.rec.Code, spy.rec.Body.Len())
						}
						continue
					}
					if !errors.Is(err, os.ErrClosed) || errors.Is(err, stream.ErrFileNotFound) {
						t.Errorf("concurrent closed Stream err = %v, want os.ErrClosed and not ErrFileNotFound", err)
					}
					if spy.headerCalls != 0 || spy.writeHeaderCalls != 0 || spy.writeCalls != 0 {
						t.Errorf("concurrent closed Stream mutated spy")
					}
				}
			}()
		}

		for range 2 {
			go func() {
				defer wg.Done()
				if err := ds.Close(); err != nil {
					t.Errorf("concurrent Close() = %v, want nil", err)
				}
			}()
		}

		wg.Wait()
	})
}

func TestNewDirectStreamer_NoFDLeakOnUploadsFailure(t *testing.T) {
	// Intentionally sequential (no t.Parallel) so /proc/self/fd counts are not
	// perturbed by concurrent tests opening or closing file descriptors.
	if _, err := os.ReadDir("/proc/self/fd"); err != nil {
		t.Skipf("/proc/self/fd unavailable: %v", err)
	}

	baseDir := t.TempDir()
	musicDir := filepath.Join(baseDir, "music")
	if err := os.Mkdir(musicDir, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", musicDir, err)
	}
	missingUploads := filepath.Join(baseDir, "missing-uploads")

	beforeFDs, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("os.ReadDir(/proc/self/fd) before: %v", err)
	}

	ds, err := stream.NewDirectStreamer(musicDir, stream.WithUploadsDir(missingUploads))
	if err == nil {
		_ = ds.Close()
		t.Fatalf("NewDirectStreamer(%q, WithUploadsDir(%q)) = nil, want error", musicDir, missingUploads)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewDirectStreamer error = %v, want os.ErrNotExist", err)
	}

	afterFDs, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("os.ReadDir(/proc/self/fd) after: %v", err)
	}
	if len(afterFDs) != len(beforeFDs) {
		t.Fatalf("open FD count = %d after failed NewDirectStreamer, want %d (music root leaked)", len(afterFDs), len(beforeFDs))
	}
}

func TestDirectStreamer_MultiRoot(t *testing.T) {
	t.Parallel()

	t.Run("WithUploadsDir constructor validation", func(t *testing.T) {
		t.Parallel()

		baseDir := t.TempDir()
		musicDir := filepath.Join(baseDir, "music")
		uploadsDir := filepath.Join(baseDir, "uploads")
		if err := os.Mkdir(musicDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", musicDir, err)
		}
		if err := os.Mkdir(uploadsDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", uploadsDir, err)
		}
		regularFile := filepath.Join(baseDir, "not-a-dir.wav")
		testutil.WriteMinimalWAV(t, regularFile)

		unreadableUploads := filepath.Join(baseDir, "unreadable-uploads")
		if err := os.Mkdir(unreadableUploads, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", unreadableUploads, err)
		}

		tests := []struct {
			name       string
			opts       []stream.DirectOption
			setup      func(t *testing.T)
			skipIfRoot bool
			wantErr    bool
			wantTarget error
		}{
			{
				name: "valid uploads dir",
				opts: []stream.DirectOption{stream.WithUploadsDir(uploadsDir)},
			},
			{
				name: "empty uploads dir ignored",
				opts: []stream.DirectOption{stream.WithUploadsDir("")},
			},
			{
				name: "whitespace uploads dir ignored",
				opts: []stream.DirectOption{stream.WithUploadsDir("  \t\n ")},
			},
			{
				name: "nil option ignored",
				opts: []stream.DirectOption{nil, stream.WithUploadsDir(uploadsDir)},
			},
			{
				name:       "missing uploads dir returns error",
				opts:       []stream.DirectOption{stream.WithUploadsDir(filepath.Join(baseDir, "missing"))},
				wantErr:    true,
				wantTarget: os.ErrNotExist,
			},
			{
				name:    "regular file uploads dir returns error",
				opts:    []stream.DirectOption{stream.WithUploadsDir(regularFile)},
				wantErr: true,
			},
			{
				name:       "unreadable uploads dir returns permission error",
				opts:       []stream.DirectOption{stream.WithUploadsDir(unreadableUploads)},
				skipIfRoot: true,
				setup: func(t *testing.T) {
					t.Helper()
					if err := os.Chmod(unreadableUploads, 0o000); err != nil {
						t.Fatalf("os.Chmod(%q, 0o000): %v", unreadableUploads, err)
					}
					t.Cleanup(func() {
						_ = os.Chmod(unreadableUploads, 0o755)
					})
				},
				wantErr:    true,
				wantTarget: os.ErrPermission,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if tc.skipIfRoot && os.Geteuid() == 0 {
					t.Skip("skipping permission test when running as root")
				}
				if tc.setup != nil {
					tc.setup(t)
				}

				ds, err := stream.NewDirectStreamer(musicDir, tc.opts...)
				if tc.wantErr {
					if err == nil {
						if ds != nil {
							_ = ds.Close()
						}
						t.Fatalf("NewDirectStreamer() = nil error, want error")
					}
					if tc.wantTarget != nil && !errors.Is(err, tc.wantTarget) {
						t.Fatalf("NewDirectStreamer() error = %v, want errors.Is(..., %v)", err, tc.wantTarget)
					}
					return
				}
				if err != nil {
					t.Fatalf("NewDirectStreamer() unexpected error: %v", err)
				}
				if err := ds.Close(); err != nil {
					t.Fatalf("ds.Close() unexpected error: %v", err)
				}
			})
		}
	})

	t.Run("identical relPath isolation, ranges, HEAD, and ETag disambiguation", func(t *testing.T) {
		t.Parallel()

		baseDir := t.TempDir()
		musicDir := filepath.Join(baseDir, "music")
		uploadsDir := filepath.Join(baseDir, "uploads")
		if err := os.Mkdir(musicDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", musicDir, err)
		}
		if err := os.Mkdir(uploadsDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", uploadsDir, err)
		}

		relPath := "Artist/Album/01.wav"
		musicBytes := append(testutil.MinimalWAVBytes(), []byte("MUSIC-ROOT-PAYLOAD")...)
		uploadsBytes := append(testutil.MinimalWAVBytes(), []byte("UPLOAD-ROOT-DATA!!")...)
		if len(musicBytes) != len(uploadsBytes) {
			t.Fatalf("test fixture sizes differ: %d vs %d", len(musicBytes), len(uploadsBytes))
		}

		musicFile := filepath.Join(musicDir, filepath.FromSlash(relPath))
		uploadsFile := filepath.Join(uploadsDir, filepath.FromSlash(relPath))
		testutil.WriteBytes(t, musicFile, musicBytes)
		testutil.WriteBytes(t, uploadsFile, uploadsBytes)

		// Pin identical mtime so ETag disambiguation depends solely on root source.
		fixedTime := time.Unix(1700000000, 123456000)
		for _, p := range []string{musicFile, uploadsFile} {
			if err := os.Chtimes(p, fixedTime, fixedTime); err != nil {
				t.Fatalf("os.Chtimes(%q): %v", p, err)
			}
		}

		ds, err := stream.NewDirectStreamer(musicDir, stream.WithUploadsDir(uploadsDir))
		if err != nil {
			t.Fatalf("NewDirectStreamer(): %v", err)
		}
		t.Cleanup(func() {
			if err := ds.Close(); err != nil {
				t.Errorf("ds.Close(): %v", err)
			}
		})

		cases := []struct {
			name   string
			source string
			want   []byte
		}{
			{name: "empty source defaults to music", source: "", want: musicBytes},
			{name: "explicit SourceMusic", source: store.SourceMusic, want: musicBytes},
			{name: "SourceUploads", source: store.SourceUploads, want: uploadsBytes},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tr := &store.Track{
					ID:      "tr_shared_" + tc.source,
					Source:  tc.source,
					RelPath: relPath,
				}

				// 200 full body
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
				if err := ds.Stream(rec, req, tr, stream.StreamOptions{}); err != nil {
					t.Fatalf("200 Stream() error: %v", err)
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("200 status = %d, want %d", rec.Code, http.StatusOK)
				}
				if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
					t.Errorf("Content-Type = %q, want %q", got, "audio/wav")
				}
				if !bytes.Equal(rec.Body.Bytes(), tc.want) {
					t.Errorf("200 body mismatch for source %q", tc.source)
				}

				// 206 byte range on distinct tail bytes
				start := len(tc.want) - 18
				end := len(tc.want) - 1
				rangeRec := httptest.NewRecorder()
				rangeReq := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
				rangeReq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
				if err := ds.Stream(rangeRec, rangeReq, tr, stream.StreamOptions{}); err != nil {
					t.Fatalf("206 Stream() error: %v", err)
				}
				if rangeRec.Code != http.StatusPartialContent {
					t.Fatalf("206 status = %d, want %d", rangeRec.Code, http.StatusPartialContent)
				}
				if !bytes.Equal(rangeRec.Body.Bytes(), tc.want[start:]) {
					t.Errorf("206 body = %q, want %q", rangeRec.Body.String(), string(tc.want[start:]))
				}

				// 416 out-of-bounds range
				oobRec := httptest.NewRecorder()
				oobReq := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
				oobReq.Header.Set("Range", "bytes=999999-")
				if err := ds.Stream(oobRec, oobReq, tr, stream.StreamOptions{}); err != nil {
					t.Fatalf("416 Stream() error: %v", err)
				}
				if oobRec.Code != http.StatusRequestedRangeNotSatisfiable {
					t.Fatalf("416 status = %d, want %d", oobRec.Code, http.StatusRequestedRangeNotSatisfiable)
				}
				wantContentRange := fmt.Sprintf("bytes */%d", len(tc.want))
				if got := oobRec.Header().Get("Content-Range"); got != wantContentRange {
					t.Errorf("416 Content-Range = %q, want %q", got, wantContentRange)
				}

				// HEAD
				headRec := httptest.NewRecorder()
				headReq := httptest.NewRequest(http.MethodHead, "/rest/stream", nil)
				if err := ds.Stream(headRec, headReq, tr, stream.StreamOptions{}); err != nil {
					t.Fatalf("HEAD Stream() error: %v", err)
				}
				if headRec.Code != http.StatusOK {
					t.Fatalf("HEAD status = %d, want %d", headRec.Code, http.StatusOK)
				}
				if headRec.Body.Len() != 0 {
					t.Errorf("HEAD body len = %d, want 0", headRec.Body.Len())
				}
				if got := headRec.Header().Get("Content-Length"); got != fmt.Sprintf("%d", len(tc.want)) {
					t.Errorf("HEAD Content-Length = %q, want %d", got, len(tc.want))
				}
			})
		}

		// ETag disambiguation when Track.ID is empty or invalid for RFC 9110 etagc.
		for _, idCase := range []struct {
			name    string
			trackID string
		}{
			{name: "empty track ID", trackID: ""},
			{name: "invalid RFC 9110 track ID", trackID: "bad \"id\"\r\n"},
		} {
			t.Run("ETag disambiguation: "+idCase.name, func(t *testing.T) {
				musicRec := httptest.NewRecorder()
				if err := ds.Stream(musicRec, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), &store.Track{
					ID:      idCase.trackID,
					Source:  store.SourceMusic,
					RelPath: relPath,
				}, stream.StreamOptions{}); err != nil {
					t.Fatalf("music Stream() error: %v", err)
				}

				uploadsRec := httptest.NewRecorder()
				if err := ds.Stream(uploadsRec, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), &store.Track{
					ID:      idCase.trackID,
					Source:  store.SourceUploads,
					RelPath: relPath,
				}, stream.StreamOptions{}); err != nil {
					t.Fatalf("uploads Stream() error: %v", err)
				}

				musicETag := musicRec.Header().Get("ETag")
				uploadsETag := uploadsRec.Header().Get("ETag")
				if musicETag == "" || uploadsETag == "" {
					t.Fatalf("expected non-empty ETags, got music=%q uploads=%q", musicETag, uploadsETag)
				}
				if musicETag == uploadsETag {
					t.Errorf("ETags collided across sources for same relPath: both %q", musicETag)
				}
			})
		}
	})

	t.Run("source errors, uploads confinement, and post-Close", func(t *testing.T) {
		t.Parallel()

		outerDir := t.TempDir()
		musicDir := filepath.Join(outerDir, "music")
		uploadsDir := filepath.Join(outerDir, "uploads")
		if err := os.Mkdir(musicDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", musicDir, err)
		}
		if err := os.Mkdir(uploadsDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", uploadsDir, err)
		}

		outsideSecret := filepath.Join(outerDir, "secret.wav")
		testutil.WriteMinimalWAV(t, outsideSecret)
		testutil.WriteMinimalWAV(t, filepath.Join(musicDir, "song.wav"))
		testutil.WriteMinimalWAV(t, filepath.Join(uploadsDir, "nested", "upload.wav"))

		// Symlinks inside uploadsDir.
		if err := os.Symlink("../secret.wav", filepath.Join(uploadsDir, "escape-rel.wav")); err != nil {
			t.Fatalf("os.Symlink escape-rel: %v", err)
		}
		if err := os.Symlink(outsideSecret, filepath.Join(uploadsDir, "escape-abs-out.wav")); err != nil {
			t.Fatalf("os.Symlink escape-abs-out: %v", err)
		}
		if err := os.Symlink(filepath.Join(uploadsDir, "nested", "upload.wav"), filepath.Join(uploadsDir, "escape-abs-in.wav")); err != nil {
			t.Fatalf("os.Symlink escape-abs-in: %v", err)
		}
		if err := os.Symlink("missing.wav", filepath.Join(uploadsDir, "dangling.wav")); err != nil {
			t.Fatalf("os.Symlink dangling: %v", err)
		}
		if err := os.Symlink("nested/upload.wav", filepath.Join(uploadsDir, "ok-link.wav")); err != nil {
			t.Fatalf("os.Symlink ok-link: %v", err)
		}

		singleRootDS, err := stream.NewDirectStreamer(musicDir)
		if err != nil {
			t.Fatalf("NewDirectStreamer(single): %v", err)
		}
		t.Cleanup(func() {
			if err := singleRootDS.Close(); err != nil {
				t.Errorf("singleRootDS.Close(): %v", err)
			}
		})

		multiRootDS, err := stream.NewDirectStreamer(musicDir, stream.WithUploadsDir(uploadsDir))
		if err != nil {
			t.Fatalf("NewDirectStreamer(multi): %v", err)
		}
		t.Cleanup(func() {
			if err := multiRootDS.Close(); err != nil {
				t.Errorf("multiRootDS.Close(): %v", err)
			}
		})

		// Valid intra-root symlink in uploadsDir succeeds.
		okRec := httptest.NewRecorder()
		if err := multiRootDS.Stream(okRec, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), &store.Track{
			ID:      "tr_ok_upload_link",
			Source:  store.SourceUploads,
			RelPath: "ok-link.wav",
		}, stream.StreamOptions{}); err != nil {
			t.Fatalf("uploads intra-root symlink Stream() error: %v", err)
		}
		if okRec.Code != http.StatusOK || !bytes.Equal(okRec.Body.Bytes(), testutil.MinimalWAVBytes()) {
			t.Fatalf("uploads intra-root symlink mismatch: status=%d len=%d", okRec.Code, okRec.Body.Len())
		}

		errorCases := []struct {
			name     string
			streamer *stream.DirectStreamer
			track    *store.Track
		}{
			{
				name:     "unknown source on multi-root streamer",
				streamer: multiRootDS,
				track:    &store.Track{ID: "tr_1", Source: "other", RelPath: "song.wav"},
			},
			{
				name:     "uppercase source rejected",
				streamer: multiRootDS,
				track:    &store.Track{ID: "tr_2", Source: "MUSIC", RelPath: "song.wav"},
			},
			{
				name:     "SourceUploads on single-root streamer",
				streamer: singleRootDS,
				track:    &store.Track{ID: "tr_3", Source: store.SourceUploads, RelPath: "nested/upload.wav"},
			},
			{
				name:     "SourceUploads on whitespace-ignored uploads streamer",
				streamer: singleRootDS,
				track:    &store.Track{ID: "tr_4", Source: store.SourceUploads, RelPath: "missing.wav"},
			},
			{
				name:     "uploads relative symlink escape",
				streamer: multiRootDS,
				track:    &store.Track{ID: "tr_5", Source: store.SourceUploads, RelPath: "escape-rel.wav"},
			},
			{
				name:     "uploads absolute symlink outside root",
				streamer: multiRootDS,
				track:    &store.Track{ID: "tr_6", Source: store.SourceUploads, RelPath: "escape-abs-out.wav"},
			},
			{
				name:     "uploads absolute symlink inside root",
				streamer: multiRootDS,
				track:    &store.Track{ID: "tr_7", Source: store.SourceUploads, RelPath: "escape-abs-in.wav"},
			},
			{
				name:     "uploads dangling symlink",
				streamer: multiRootDS,
				track:    &store.Track{ID: "tr_8", Source: store.SourceUploads, RelPath: "dangling.wav"},
			},
			{
				name:     "uploads path traversal",
				streamer: multiRootDS,
				track:    &store.Track{ID: "tr_9", Source: store.SourceUploads, RelPath: "../music/song.wav"},
			},
		}

		for _, tc := range errorCases {
			t.Run(tc.name, func(t *testing.T) {
				spy := newSpyResponseWriter()
				spy.rec.Header().Set("Access-Control-Allow-Origin", "*")
				req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)

				err := tc.streamer.Stream(spy, req, tc.track, stream.StreamOptions{})
				if !errors.Is(err, stream.ErrFileNotFound) {
					t.Fatalf("Stream() error = %v, want errors.Is(..., ErrFileNotFound)", err)
				}
				if strings.Contains(err.Error(), musicDir) || strings.Contains(err.Error(), uploadsDir) || strings.Contains(err.Error(), outerDir) {
					t.Errorf("error %q leaked host path", err.Error())
				}
				spy.assertUntouched(t)
				if got := spy.rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
					t.Errorf("pre-existing CORS header = %q, want %q", got, "*")
				}
			})
		}

		t.Run("post-Close returns os.ErrClosed for both roots", func(t *testing.T) {
			ds, err := stream.NewDirectStreamer(musicDir, stream.WithUploadsDir(uploadsDir))
			if err != nil {
				t.Fatalf("NewDirectStreamer(): %v", err)
			}
			if err := ds.Close(); err != nil {
				t.Fatalf("first Close(): %v", err)
			}
			if err := ds.Close(); err != nil {
				t.Fatalf("second Close(): %v", err)
			}

			for _, tr := range []*store.Track{
				{ID: "tr_m", Source: store.SourceMusic, RelPath: "song.wav"},
				{ID: "tr_u", Source: store.SourceUploads, RelPath: "nested/upload.wav"},
			} {
				spy := newSpyResponseWriter()
				err := ds.Stream(spy, httptest.NewRequest(http.MethodGet, "/rest/stream", nil), tr, stream.StreamOptions{})
				if !errors.Is(err, os.ErrClosed) || errors.Is(err, stream.ErrFileNotFound) {
					t.Errorf("post-Close Stream(%q) = %v, want os.ErrClosed and not ErrFileNotFound", tr.Source, err)
				}
				spy.assertUntouched(t)
			}
		})
	})

	t.Run("concurrent multi-root Stream and Close under -race", func(t *testing.T) {
		t.Parallel()

		baseDir := t.TempDir()
		musicDir := filepath.Join(baseDir, "music")
		uploadsDir := filepath.Join(baseDir, "uploads")
		if err := os.Mkdir(musicDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", musicDir, err)
		}
		if err := os.Mkdir(uploadsDir, 0o755); err != nil {
			t.Fatalf("os.Mkdir(%q): %v", uploadsDir, err)
		}

		musicWAV := append(testutil.MinimalWAVBytes(), []byte("MUSIC")...)
		uploadsWAV := append(testutil.MinimalWAVBytes(), []byte("UPLOADS")...)
		testutil.WriteBytes(t, filepath.Join(musicDir, "song.wav"), musicWAV)
		testutil.WriteBytes(t, filepath.Join(uploadsDir, "song.wav"), uploadsWAV)

		ds, err := stream.NewDirectStreamer(musicDir, stream.WithUploadsDir(uploadsDir))
		if err != nil {
			t.Fatalf("NewDirectStreamer(): %v", err)
		}

		const readers = 16
		var wg sync.WaitGroup
		wg.Add(readers + 2)

		for i := range readers {
			go func(idx int) {
				defer wg.Done()
				source := store.SourceMusic
				want := musicWAV
				if idx%2 == 1 {
					source = store.SourceUploads
					want = uploadsWAV
				}
				for range 5 {
					spy := newSpyResponseWriter()
					req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
					err := ds.Stream(spy, req, &store.Track{
						ID:      fmt.Sprintf("tr_%d", idx),
						Source:  source,
						RelPath: "song.wav",
					}, stream.StreamOptions{})
					if err == nil {
						if spy.rec.Code != http.StatusOK || !bytes.Equal(spy.rec.Body.Bytes(), want) {
							t.Errorf("worker %d (%s) mismatch: status=%d len=%d want=%d", idx, source, spy.rec.Code, spy.rec.Body.Len(), len(want))
						}
						continue
					}
					if !errors.Is(err, os.ErrClosed) || errors.Is(err, stream.ErrFileNotFound) {
						t.Errorf("worker %d (%s) closed Stream err = %v, want os.ErrClosed and not ErrFileNotFound", idx, source, err)
					}
					if spy.headerCalls != 0 || spy.writeHeaderCalls != 0 || spy.writeCalls != 0 {
						t.Errorf("worker %d (%s) mutated spy on error", idx, source)
					}
				}
			}(i)
		}

		for range 2 {
			go func() {
				defer wg.Done()
				if err := ds.Close(); err != nil {
					t.Errorf("concurrent Close() = %v, want nil", err)
				}
			}()
		}

		wg.Wait()
	})
}
