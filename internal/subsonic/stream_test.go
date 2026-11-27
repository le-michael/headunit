package subsonic_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"headunit/internal/auth"
	"headunit/internal/store"
	"headunit/internal/stream"
	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

func newDirectStreamerForTest(t *testing.T, musicDir string) *stream.DirectStreamer {
	t.Helper()
	ds, err := stream.NewDirectStreamer(musicDir)
	if err != nil {
		t.Fatalf("NewDirectStreamer(%q) error = %v", musicDir, err)
	}
	t.Cleanup(func() {
		if err := ds.Close(); err != nil {
			t.Errorf("DirectStreamer.Close() error = %v", err)
		}
	})
	return ds
}

func doHTTPRequest(t *testing.T, client *http.Client, method, urlStr, formBody string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if formBody != "" {
		bodyReader = strings.NewReader(formBody)
	}
	req, err := http.NewRequest(method, urlStr, bodyReader)
	if err != nil {
		t.Fatalf("http.NewRequest(%s, %s) error = %v", method, urlStr, err)
	}
	if formBody != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do(%s, %s) error = %v", method, urlStr, err)
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	return res, payload
}

func assertNoAudioHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for _, forbidden := range []string{"ETag", "Accept-Ranges", "Content-Range"} {
		if got := h.Get(forbidden); got != "" {
			t.Errorf("unexpected audio header %s = %q on error response", forbidden, got)
		}
	}
	if ct := h.Get("Content-Type"); strings.HasPrefix(ct, "audio/") {
		t.Errorf("unexpected audio Content-Type %q on error response", ct)
	}
}

type spyStreamer struct {
	mu       sync.Mutex
	calls    int
	lastOpts stream.StreamOptions
	lastID   string
	err      error
}

func (s *spyStreamer) Stream(_ http.ResponseWriter, _ *http.Request, track *store.Track, opts stream.StreamOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if track != nil {
		s.lastID = track.ID
	}
	s.lastOpts = opts
	return s.err
}

func (*spyStreamer) Close() error {
	return nil
}

func TestStreamDirectEndToEnd(t *testing.T) {
	musicDir := t.TempDir()
	mp3Path := filepath.Join(musicDir, "artist/album/track.mp3")
	flacPath := filepath.Join(musicDir, "artist/album/track.flac")
	wavPath := filepath.Join(musicDir, "artist/album/track.wav")

	testutil.WriteMinimalMP3(t, mp3Path)
	testutil.WriteMinimalFLAC(t, flacPath)
	testutil.WriteMinimalWAV(t, wavPath)

	mp3Bytes, err := os.ReadFile(mp3Path)
	if err != nil {
		t.Fatalf("ReadFile(mp3) error = %v", err)
	}
	flacBytes, err := os.ReadFile(flacPath)
	if err != nil {
		t.Fatalf("ReadFile(flac) error = %v", err)
	}
	wavBytes, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatalf("ReadFile(wav) error = %v", err)
	}

	st := openTestStore(t)
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	seedTracks(t, st,
		store.Track{
			ID: "tr_mp3", RelPath: "artist/album/track.mp3", FileSize: int64(len(mp3Bytes)), MtimeNs: 1,
			Title: "MP3 Track", Artist: "Tester", AlbumArtist: "Tester", Album: "Fixtures", CreatedAt: now,
		},
		store.Track{
			ID: "tr_flac", RelPath: "artist/album/track.flac", FileSize: int64(len(flacBytes)), MtimeNs: 2,
			Title: "FLAC Track", Artist: "Tester", AlbumArtist: "Tester", Album: "Fixtures", CreatedAt: now,
		},
		store.Track{
			ID: "tr_wav", RelPath: "artist/album/track.wav", FileSize: int64(len(wavBytes)), MtimeNs: 3,
			Title: "WAV Track", Artist: "Tester", AlbumArtist: "Tester", Album: "Fixtures", CreatedAt: now,
		},
	)

	ds := newDirectStreamerForTest(t, musicDir)
	h, err := subsonic.NewHandler(subsonic.Dependencies{
		Auth:     auth.NoopAuthenticator{},
		Scanner:  &atomicScanStatusProvider{},
		Store:    st,
		Users:    st,
		Streamer: ds,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	srv := httptest.NewServer(h)
	defer srv.Close()
	client := srv.Client()

	t.Run("GET full payloads for MP3, FLAC, and WAV on both routes", func(t *testing.T) {
		fixtures := []struct {
			id       string
			route    string
			wantMIME string
			wantBody []byte
		}{
			{id: "tr_mp3", route: "/rest/stream", wantMIME: "audio/mpeg", wantBody: mp3Bytes},
			{id: "tr_flac", route: "/rest/stream.view", wantMIME: "audio/flac", wantBody: flacBytes},
			{id: "tr_wav", route: "/rest/stream", wantMIME: "audio/wav", wantBody: wavBytes},
		}
		for _, fx := range fixtures {
			res, body := doHTTPRequest(t, client, http.MethodGet, srv.URL+fx.route+"?id="+fx.id, "", nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("%s status = %d, want 200", fx.id, res.StatusCode)
			}
			assertCORSHeaders(t, res.Header)
			if got := res.Header.Get("Content-Type"); got != fx.wantMIME {
				t.Errorf("%s Content-Type = %q, want %q", fx.id, got, fx.wantMIME)
			}
			if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
				t.Errorf("%s Accept-Ranges = %q, want bytes", fx.id, got)
			}
			if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s X-Content-Type-Options = %q, want nosniff", fx.id, got)
			}
			if res.Header.Get("ETag") == "" {
				t.Errorf("%s ETag is empty", fx.id)
			}
			if !bytes.Equal(body, fx.wantBody) {
				t.Errorf("%s body mismatch: got %d bytes, want %d bytes", fx.id, len(body), len(fx.wantBody))
			}
		}
	})

	t.Run("Range 206, mid-file Range, 416, HEAD, If-Range, POST, and OPTIONS", func(t *testing.T) {
		// Probe ETag first.
		headRes, headBody := doHTTPRequest(t, client, http.MethodHead, srv.URL+"/rest/stream?id=tr_mp3", "", nil)
		if headRes.StatusCode != http.StatusOK {
			t.Fatalf("HEAD status = %d, want 200", headRes.StatusCode)
		}
		if len(headBody) != 0 {
			t.Errorf("HEAD body len = %d, want 0", len(headBody))
		}
		if got, want := headRes.Header.Get("Content-Length"), strconv.Itoa(len(mp3Bytes)); got != want {
			t.Errorf("HEAD Content-Length = %q, want %q", got, want)
		}
		etag := headRes.Header.Get("ETag")
		if etag == "" {
			t.Fatalf("HEAD ETag is empty")
		}

		// Range: bytes=0-1 -> 206 Partial Content.
		r01Res, r01Body := doHTTPRequest(t, client, http.MethodGet, srv.URL+"/rest/stream?id=tr_mp3", "",
			map[string]string{"Range": "bytes=0-1"})
		if r01Res.StatusCode != http.StatusPartialContent {
			t.Fatalf("Range 0-1 status = %d, want 206", r01Res.StatusCode)
		}
		wantCR := fmt.Sprintf("bytes 0-1/%d", len(mp3Bytes))
		if got := r01Res.Header.Get("Content-Range"); got != wantCR {
			t.Errorf("Content-Range = %q, want %q", got, wantCR)
		}
		if !bytes.Equal(r01Body, mp3Bytes[0:2]) {
			t.Errorf("Range 0-1 bytes = %x, want %x", r01Body, mp3Bytes[0:2])
		}

		// Mid-file Range: bytes=10-25.
		midRes, midBody := doHTTPRequest(t, client, http.MethodGet, srv.URL+"/rest/stream?id=tr_mp3", "",
			map[string]string{"Range": "bytes=10-25"})
		if midRes.StatusCode != http.StatusPartialContent {
			t.Fatalf("Range 10-25 status = %d, want 206", midRes.StatusCode)
		}
		if !bytes.Equal(midBody, mp3Bytes[10:26]) {
			t.Errorf("Range 10-25 bytes mismatch")
		}

		// Out-of-bounds Range -> 416.
		oobRes, _ := doHTTPRequest(t, client, http.MethodGet, srv.URL+"/rest/stream?id=tr_mp3", "",
			map[string]string{"Range": "bytes=9999999-"})
		if oobRes.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("OOB Range status = %d, want 416", oobRes.StatusCode)
		}

		// If-Range with matching ETag -> 206.
		ifMatchRes, ifMatchBody := doHTTPRequest(t, client, http.MethodGet, srv.URL+"/rest/stream?id=tr_mp3", "",
			map[string]string{"Range": "bytes=0-3", "If-Range": etag})
		if ifMatchRes.StatusCode != http.StatusPartialContent {
			t.Fatalf("If-Range matching status = %d, want 206", ifMatchRes.StatusCode)
		}
		if !bytes.Equal(ifMatchBody, mp3Bytes[0:4]) {
			t.Errorf("If-Range matching bytes mismatch")
		}

		// If-Range with stale ETag -> 200 full payload.
		staleRes, staleBody := doHTTPRequest(t, client, http.MethodGet, srv.URL+"/rest/stream?id=tr_mp3", "",
			map[string]string{"Range": "bytes=0-3", "If-Range": `"stale-etag"`})
		if staleRes.StatusCode != http.StatusOK {
			t.Fatalf("If-Range stale status = %d, want 200", staleRes.StatusCode)
		}
		if !bytes.Equal(staleBody, mp3Bytes) {
			t.Errorf("If-Range stale body len = %d, want %d", len(staleBody), len(mp3Bytes))
		}

		// POST form body.
		postRes, postBody := doHTTPRequest(t, client, http.MethodPost, srv.URL+"/rest/stream.view", "id=tr_mp3", nil)
		if postRes.StatusCode != http.StatusOK {
			t.Fatalf("POST stream status = %d, want 200", postRes.StatusCode)
		}
		if !bytes.Equal(postBody, mp3Bytes) {
			t.Errorf("POST stream body len = %d, want %d", len(postBody), len(mp3Bytes))
		}

		// OPTIONS preflight skips auth and returns 204 + CORS.
		optRes, _ := doHTTPRequest(t, client, http.MethodOptions, srv.URL+"/rest/stream", "", nil)
		if optRes.StatusCode != http.StatusNoContent {
			t.Fatalf("OPTIONS status = %d, want 204", optRes.StatusCode)
		}
		assertCORSHeaders(t, optRes.Header)
	})
}

func TestStreamErrorsPreHeader(t *testing.T) {
	musicDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.mp3")
	testutil.WriteMinimalMP3(t, outsideFile)

	symlinkPath := filepath.Join(musicDir, "escape.mp3")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatalf("os.Symlink() error = %v", err)
	}

	st := openTestStore(t)
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	seedTracks(t, st,
		store.Track{
			ID: "tr_deleted", RelPath: "missing/file.mp3", FileSize: 100, MtimeNs: 1,
			Title: "Deleted", Artist: "A", AlbumArtist: "A", Album: "B", CreatedAt: now,
		},
		store.Track{
			ID: "tr_symlink", RelPath: "escape.mp3", FileSize: 100, MtimeNs: 2,
			Title: "Symlink", Artist: "A", AlbumArtist: "A", Album: "B", CreatedAt: now,
		},
	)
	ds := newDirectStreamerForTest(t, musicDir)

	type errCase struct {
		name      string
		store     subsonic.LibraryStore
		streamer  stream.Streamer
		query     string
		wantCode  int
		wantMsg   string
		secretLog string
	}

	const secretText = "internal secret leak guard /var/lib/headunit"

	cases := []errCase{
		{
			name:     "missing id",
			store:    st,
			streamer: ds,
			query:    "",
			wantCode: 10,
		},
		{
			name:     "blank id",
			store:    st,
			streamer: ds,
			query:    "id=%20%20",
			wantCode: 10,
		},
		{
			name:     "unknown id",
			store:    st,
			streamer: ds,
			query:    "id=tr_unknown",
			wantCode: 70,
		},
		{
			name:     "nil track guard",
			store:    &spyLibraryStore{track: nil},
			streamer: ds,
			query:    "id=tr_any",
			wantCode: 70,
		},
		{
			name:     "deleted backing file",
			store:    st,
			streamer: ds,
			query:    "id=tr_deleted",
			wantCode: 70,
		},
		{
			name:     "symlink escaping music root",
			store:    st,
			streamer: ds,
			query:    "id=tr_symlink",
			wantCode: 70,
		},
		{
			name:      "unexpected store error sanitized",
			store:     &spyLibraryStore{err: errors.New(secretText)},
			streamer:  ds,
			query:     "id=tr_any",
			wantCode:  0,
			wantMsg:   "Internal server error",
			secretLog: secretText,
		},
		{
			name: "unexpected streamer error sanitized",
			store: &spyLibraryStore{track: &store.Track{
				ID: "tr_ok", RelPath: "ok.mp3",
			}},
			streamer:  &spyStreamer{err: errors.New(secretText)},
			query:     "id=tr_ok",
			wantCode:  0,
			wantMsg:   "Internal server error",
			secretLog: secretText,
		},
	}

	for _, tc := range cases {
		for _, format := range []string{"xml", "json"} {
			t.Run(tc.name+"_"+format, func(t *testing.T) {
				var logBuf bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logBuf, nil))
				h, err := subsonic.NewHandler(subsonic.Dependencies{
					Auth:     auth.NoopAuthenticator{},
					Scanner:  &atomicScanStatusProvider{},
					Store:    tc.store,
					Users:    &stubUserStore{},
					Streamer: tc.streamer,
					Logger:   logger,
				})
				if err != nil {
					t.Fatalf("NewHandler() error = %v", err)
				}

				q := tc.query
				if format == "json" {
					if q != "" {
						q += "&"
					}
					q += "f=json"
				}
				target := "/rest/stream"
				if q != "" {
					target += "?" + q
				}

				res, body := execSubsonicRequest(t, h, http.MethodGet, target, "")
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", res.StatusCode)
				}
				assertNoAudioHeaders(t, res.Header)
				if tc.secretLog != "" && strings.Contains(string(body), tc.secretLog) {
					t.Fatalf("secret error leaked in body: %s", body)
				}

				if format == "json" {
					if got := res.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
						t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
					}
					resp := decodeJSON(t, body)
					assertJSONEnvelopeFields(t, resp, "failed")
					errObj := mustJSONMap(t, resp, "error")
					if got := int(mustJSONFloat(t, errObj, "code")); got != tc.wantCode {
						t.Errorf("code = %d, want %d", got, tc.wantCode)
					}
					if tc.wantMsg != "" {
						if got := mustJSONString(t, errObj, "message"); got != tc.wantMsg {
							t.Errorf("message = %q, want %q", got, tc.wantMsg)
						}
					}
				} else {
					if got := res.Header.Get("Content-Type"); got != "application/xml; charset=utf-8" {
						t.Errorf("Content-Type = %q, want application/xml; charset=utf-8", got)
					}
					env := decodeXML(t, body)
					assertEnvelopeFields(t, env, "failed")
					if env.Error == nil || env.Error.Code != tc.wantCode {
						t.Errorf("error = %+v, want code %d", env.Error, tc.wantCode)
					}
					if tc.wantMsg != "" && env.Error.Message != tc.wantMsg {
						t.Errorf("message = %q, want %q", env.Error.Message, tc.wantMsg)
					}
				}

				if tc.secretLog != "" && !strings.Contains(logBuf.String(), tc.secretLog) {
					t.Errorf("expected slog output to contain %q, got %q", tc.secretLog, logBuf.String())
				}
			})
		}
	}
}

func TestStreamOptionsForwardingAndAuth(t *testing.T) {
	track := &store.Track{ID: "tr_opt", RelPath: "song.mp3"}

	t.Run("forwards StreamOptions and normalizes invalid or negative maxBitRate", func(t *testing.T) {
		for _, tc := range []struct {
			query string
			want  stream.StreamOptions
		}{
			{query: "id=tr_opt&maxBitRate=128&format=mp3", want: stream.StreamOptions{MaxBitRate: 128, Format: "mp3"}},
			{query: "id=tr_opt&maxBitRate=-50&format=%20raw%20", want: stream.StreamOptions{MaxBitRate: 0, Format: "raw"}},
			{query: "id=tr_opt&maxBitRate=bad", want: stream.StreamOptions{MaxBitRate: 0, Format: ""}},
		} {
			spyStr := &spyStreamer{}
			spySt := &spyLibraryStore{track: track}
			h, err := subsonic.NewHandler(subsonic.Dependencies{
				Auth:     auth.NoopAuthenticator{},
				Scanner:  &atomicScanStatusProvider{},
				Store:    spySt,
				Users:    &stubUserStore{},
				Streamer: spyStr,
			})
			if err != nil {
				t.Fatalf("NewHandler() error = %v", err)
			}
			_, _ = execSubsonicRequest(t, h, http.MethodGet, "/rest/stream?"+tc.query, "")
			if spyStr.calls != 1 {
				t.Fatalf("%q: streamer calls = %d, want 1", tc.query, spyStr.calls)
			}
			if spyStr.lastOpts != tc.want {
				t.Errorf("%q: opts = %+v, want %+v", tc.query, spyStr.lastOpts, tc.want)
			}
		}
	})

	t.Run("failing auth returns error 40 without calling store or streamer", func(t *testing.T) {
		spyStr := &spyStreamer{}
		spySt := &spyLibraryStore{track: track}
		failingAuth := &stubAuthenticator{err: errors.New("bad credentials")}
		h, err := subsonic.NewHandler(subsonic.Dependencies{
			Auth:     failingAuth,
			Scanner:  &atomicScanStatusProvider{},
			Store:    spySt,
			Users:    &stubUserStore{},
			Streamer: spyStr,
		})
		if err != nil {
			t.Fatalf("NewHandler() error = %v", err)
		}

		res, body := execSubsonicRequest(t, h, http.MethodGet, "/rest/stream?id=tr_opt&f=json", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		assertNoAudioHeaders(t, res.Header)
		resp := decodeJSON(t, body)
		assertJSONEnvelopeFields(t, resp, "failed")
		errObj := mustJSONMap(t, resp, "error")
		if got := int(mustJSONFloat(t, errObj, "code")); got != 40 {
			t.Errorf("error code = %d, want 40", got)
		}
		if spyStr.calls != 0 {
			t.Errorf("streamer called %d times on auth failure, want 0", spyStr.calls)
		}
	})
}

func TestConcurrentLibraryAndStream(t *testing.T) {
	musicDir := t.TempDir()
	mp3Path := filepath.Join(musicDir, "conc/album/01.mp3")
	testutil.WriteMinimalMP3(t, mp3Path)
	mp3Bytes, err := os.ReadFile(mp3Path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	st := openTestStore(t)
	seedTracks(t, st, store.Track{
		ID:          "tr_conc",
		RelPath:     "conc/album/01.mp3",
		FileSize:    int64(len(mp3Bytes)),
		MtimeNs:     1,
		Title:       "Concurrent Track",
		Artist:      "The Concurrency",
		AlbumArtist: "The Concurrency",
		Album:       "Race Free",
		TrackNum:    1,
		DiscNum:     1,
		Year:        2026,
		Genre:       "Synth",
		DurationSec: 2,
		BitRate:     128,
		CreatedAt:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	artistID := store.ArtistID("The Concurrency")
	albumID := store.AlbumID("The Concurrency", "Race Free")
	ds := newDirectStreamerForTest(t, musicDir)

	h, err := subsonic.NewHandler(subsonic.Dependencies{
		Auth:     auth.NoopAuthenticator{},
		Scanner:  &atomicScanStatusProvider{},
		Store:    st,
		Users:    st,
		Streamer: ds,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	srv := httptest.NewServer(h)
	defer srv.Close()
	client := srv.Client()

	requests := []struct {
		method   string
		path     string
		formBody string
		headers  map[string]string
		wantCode int
	}{
		{method: http.MethodGet, path: "/rest/getMusicFolders?f=json", wantCode: http.StatusOK},
		{method: http.MethodGet, path: "/rest/getArtists", wantCode: http.StatusOK},
		{method: http.MethodGet, path: "/rest/getArtist?f=json&id=" + artistID, wantCode: http.StatusOK},
		{method: http.MethodGet, path: "/rest/getAlbum?id=" + albumID, wantCode: http.StatusOK},
		{method: http.MethodGet, path: "/rest/getAlbumList2?type=newest&f=json", wantCode: http.StatusOK},
		{method: http.MethodGet, path: "/rest/getSong?id=tr_conc", wantCode: http.StatusOK},
		{method: http.MethodPost, path: "/rest/search3", formBody: "query=Concurrent&f=json", wantCode: http.StatusOK},
		{method: http.MethodGet, path: "/rest/stream?id=tr_conc", headers: map[string]string{"Range": "bytes=0-1"}, wantCode: http.StatusPartialContent},
		{method: http.MethodHead, path: "/rest/stream.view?id=tr_conc", wantCode: http.StatusOK},
	}

	const workers = 12
	var wg sync.WaitGroup
	errCh := make(chan string, workers*len(requests))

	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i, rq := range requests {
				var bodyReader io.Reader
				if rq.formBody != "" {
					bodyReader = strings.NewReader(rq.formBody)
				}
				req, err := http.NewRequest(rq.method, srv.URL+rq.path, bodyReader)
				if err != nil {
					errCh <- fmt.Sprintf("w=%d i=%d NewRequest: %v", worker, i, err)
					return
				}
				if rq.formBody != "" {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				for k, v := range rq.headers {
					req.Header.Set(k, v)
				}
				res, err := client.Do(req)
				if err != nil {
					errCh <- fmt.Sprintf("w=%d i=%d Do: %v", worker, i, err)
					return
				}
				body, readErr := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if readErr != nil {
					errCh <- fmt.Sprintf("w=%d i=%d ReadAll: %v", worker, i, readErr)
					return
				}
				if res.StatusCode != rq.wantCode {
					errCh <- fmt.Sprintf("w=%d i=%d status=%d want=%d", worker, i, res.StatusCode, rq.wantCode)
					return
				}
				if rq.wantCode == http.StatusPartialContent && !bytes.Equal(body, mp3Bytes[0:2]) {
					errCh <- fmt.Sprintf("w=%d i=%d range bytes mismatch", worker, i)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
}
