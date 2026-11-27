//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"testing"

	"go.senan.xyz/taglib"

	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

func TestWatcher(t *testing.T) {
	h := newHarness(t)
	h.waitForCleanLibrary(t)

	var flacBytes []byte
	var flacPath, wavPath string
	var flacTrackID, wavTrackID string

	if !t.Run("LiveDropIndexingAndStream", func(t *testing.T) {
		flacBytes = buildTaggedFLACBytes(t, "01 - Live Track.flac", map[string][]string{
			taglib.Title:       {"Live Track"},
			taglib.AlbumArtist: {"Live Artist"},
			taglib.Artist:      {"Live Artist"},
			taglib.Album:       {"Live Session"},
			taglib.TrackNumber: {"1"},
			taglib.DiscNumber:  {"1"},
			taglib.Date:        {"2026"},
			taglib.Genre:       {"Electronic"},
		})
		flacPath = h.publishDirWithFile(t, "watcher-album", "01 - Live Track.flac", flacBytes)
		wavPath = h.publishFile(t, "02 - Live Signal.wav", testutil.MinimalWAVBytes())

		h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 2
		})

		searchEnv, _ := h.getJSON(t, h.withDefaultAuth(t, "/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500"))
		assertJSONResponseEnvelope(t, searchEnv, "ok")
		if searchEnv.SearchResult3 == nil {
			t.Fatalf("search3 SearchResult3 = nil, want non-nil")
		}

		flacSong, ok := findSongByPath(searchEnv.SearchResult3.Song, "watcher-album/01 - Live Track.flac")
		if !ok {
			t.Fatalf("search3 missing song %q", "watcher-album/01 - Live Track.flac")
		}
		flacTrackID = flacSong.ID

		wavSong, ok := findSongByPath(searchEnv.SearchResult3.Song, "02 - Live Signal.wav")
		if !ok {
			t.Fatalf("search3 missing song %q", "02 - Live Signal.wav")
		}
		wavTrackID = wavSong.ID

		resp, body := h.doRequest(t, http.MethodGet, h.withDefaultAuth(t, "/rest/stream?id="+url.QueryEscape(flacTrackID)), nil, nil)
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream live FLAC status = %d, want %d (body=%s)", got, want, string(body))
		}
		if !bytes.Equal(body, flacBytes) {
			t.Errorf("live FLAC stream body len = %d, want %d exact bytes", len(body), len(flacBytes))
		}
	}) {
		return
	}

	t.Run("LiveDeleteReconciliation", func(t *testing.T) {
		if err := os.Remove(wavPath); err != nil {
			t.Fatalf("os.Remove(%q): %v", wavPath, err)
		}
		h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 1
		})
		h.assertStreamNotFound(t, wavTrackID)

		if err := os.Remove(flacPath); err != nil {
			t.Fatalf("os.Remove(%q): %v", flacPath, err)
		}
		h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 0
		})
		h.assertStreamNotFound(t, flacTrackID)
	})
}
