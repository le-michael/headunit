//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"net/url"
	"testing"

	"go.senan.xyz/taglib"

	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

func TestStream(t *testing.T) {
	h := newHarness(t)
	h.waitForCleanLibrary(t)

	flacBytes := buildTaggedFLACBytes(t, "01 - Stream Track.flac", map[string][]string{
		taglib.Title:       {"Stream Track"},
		taglib.AlbumArtist: {"Stream Artist"},
		taglib.Artist:      {"Stream Artist"},
		taglib.Album:       {"Stream Session"},
		taglib.TrackNumber: {"1"},
		taglib.DiscNumber:  {"1"},
		taglib.Date:        {"2026"},
		taglib.Genre:       {"Electronic"},
	})
	h.publishDirWithFile(t, "stream-album", "01 - Stream Track.flac", flacBytes)

	wavBytes := testutil.MinimalWAVBytes()
	if got, want := len(wavBytes), 32044; got != want {
		t.Fatalf("len(MinimalWAVBytes()) = %d, want %d", got, want)
	}
	h.publishFile(t, "02 - Range Signal.wav", wavBytes)

	h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 2
	})

	searchEnv, _ := h.getJSON(t, h.withDefaultAuth(t, "/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500"))
	assertJSONResponseEnvelope(t, searchEnv, "ok")
	if searchEnv.SearchResult3 == nil {
		t.Fatalf("search3 SearchResult3 = nil, want non-nil")
	}

	flacSong, ok := findSongByPath(searchEnv.SearchResult3.Song, "stream-album/01 - Stream Track.flac")
	if !ok {
		t.Fatalf("search3 missing song %q", "stream-album/01 - Stream Track.flac")
	}
	wavSong, ok := findSongByPath(searchEnv.SearchResult3.Song, "02 - Range Signal.wav")
	if !ok {
		t.Fatalf("search3 missing song %q", "02 - Range Signal.wav")
	}

	t.Run("FullFLAC", func(t *testing.T) {
		resp, body := h.doRequest(t, http.MethodGet, h.withTokenAuth(t, "/rest/stream?id="+url.QueryEscape(flacSong.ID)), nil, nil)
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream status = %d, want %d (body=%s)", got, want, string(body))
		}
		if got, want := resp.Header.Get("Content-Type"), "audio/flac"; got != want {
			t.Errorf("FLAC Content-Type = %q, want %q", got, want)
		}
		if got, want := resp.Header.Get("Accept-Ranges"), "bytes"; got != want {
			t.Errorf("FLAC Accept-Ranges = %q, want %q", got, want)
		}
		if got := resp.Header.Get("ETag"); got == "" {
			t.Errorf("FLAC ETag = empty, want non-empty")
		}
		if got, want := resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
			t.Errorf("FLAC X-Content-Type-Options = %q, want %q", got, want)
		}
		if !bytes.Equal(body, flacBytes) {
			t.Errorf("FLAC stream body len = %d, want %d exact bytes", len(body), len(flacBytes))
		}
	})

	t.Run("PartialWAVRange", func(t *testing.T) {
		resp, body := h.doRequest(
			t,
			http.MethodGet,
			h.withAPIKeyAuth(t, "/rest/stream?id="+url.QueryEscape(wavSong.ID)),
			nil,
			map[string]string{"Range": "bytes=0-3"},
		)
		if got, want := resp.StatusCode, http.StatusPartialContent; got != want {
			t.Fatalf("GET /rest/stream Range status = %d, want %d (body=%s)", got, want, string(body))
		}
		if got, want := resp.Header.Get("Content-Type"), "audio/wav"; got != want {
			t.Errorf("WAV Range Content-Type = %q, want %q", got, want)
		}
		if got, want := resp.Header.Get("Content-Range"), "bytes 0-3/32044"; got != want {
			t.Errorf("WAV Content-Range = %q, want %q", got, want)
		}
		if got, want := string(body), "RIFF"; got != want {
			t.Errorf("WAV Range body = %q, want %q", got, want)
		}
	})

	t.Run("HEAD", func(t *testing.T) {
		resp, body := h.doRequest(t, http.MethodHead, h.withTokenAuth(t, "/rest/stream?id="+url.QueryEscape(wavSong.ID)), nil, nil)
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("HEAD /rest/stream status = %d, want %d", got, want)
		}
		if got, want := resp.Header.Get("Content-Type"), "audio/wav"; got != want {
			t.Errorf("HEAD Content-Type = %q, want %q", got, want)
		}
		if got, want := resp.Header.Get("Accept-Ranges"), "bytes"; got != want {
			t.Errorf("HEAD Accept-Ranges = %q, want %q", got, want)
		}
		if got, want := resp.Header.Get("Content-Length"), "32044"; got != want {
			t.Errorf("HEAD Content-Length = %q, want %q", got, want)
		}
		if got := len(body); got != 0 {
			t.Errorf("HEAD body len = %d, want 0", got)
		}
	})

	t.Run("UnsatisfiableRange", func(t *testing.T) {
		resp, body := h.doRequest(
			t,
			http.MethodGet,
			h.withAPIKeyAuth(t, "/rest/stream?id="+url.QueryEscape(wavSong.ID)),
			nil,
			map[string]string{"Range": "bytes=9999999-"},
		)
		if got, want := resp.StatusCode, http.StatusRequestedRangeNotSatisfiable; got != want {
			t.Fatalf("GET /rest/stream unsatisfiable Range status = %d, want %d (body=%s)", got, want, string(body))
		}
		if got, want := resp.Header.Get("Content-Range"), "bytes */32044"; got != want {
			t.Errorf("416 Content-Range = %q, want %q", got, want)
		}
	})

	t.Run("UnknownTrackNotFound", func(t *testing.T) {
		h.assertStreamNotFound(t, "tr_unknown")
	})
}
