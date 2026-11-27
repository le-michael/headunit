//go:build e2e

package e2e

import (
	"net/url"
	"strings"
	"testing"

	"go.senan.xyz/taglib"

	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

func TestLibrary(t *testing.T) {
	h := newHarness(t)
	h.waitForCleanLibrary(t)

	flacBytes := buildTaggedFLACBytes(t, "01 - Docker Track.flac", map[string][]string{
		taglib.Title:       {"Docker Track"},
		taglib.AlbumArtist: {"The Container Band"},
		taglib.Artist:      {"The Container Band"},
		taglib.Album:       {"Compose Session"},
		taglib.TrackNumber: {"1"},
		taglib.DiscNumber:  {"1"},
		taglib.Date:        {"2026"},
		taglib.Genre:       {"Electronic"},
	})
	h.publishDirWithFile(t, "live-album", "01 - Docker Track.flac", flacBytes)

	wavBytes := testutil.MinimalWAVBytes()
	if got, want := len(wavBytes), 32044; got != want {
		t.Fatalf("len(MinimalWAVBytes()) = %d, want %d", got, want)
	}
	h.publishFile(t, "02 - Smoke Signal.wav", wavBytes)
	h.publishFile(t, "ignored.mp3.part", []byte("partial"))

	h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 2
	})

	t.Run("BrowseArtistsAndAlbums", func(t *testing.T) {
		artistsEnv, _ := h.getJSON(t, h.withTokenAuth(t, "/rest/getArtists?f=json"))
		assertJSONResponseEnvelope(t, artistsEnv, "ok")
		if artistsEnv.Artists == nil {
			t.Fatalf("getArtists Artists = nil, want non-nil")
		}
		if got, want := artistsEnv.Artists.IgnoredArticles, "The El La Los Las Le Les"; got != want {
			t.Errorf("getArtists IgnoredArticles = %q, want %q", got, want)
		}
		if got, want := len(artistsEnv.Artists.Index), 2; got != want {
			t.Fatalf("getArtists Index len = %d, want %d (%+v)", got, want, artistsEnv.Artists.Index)
		}

		var containerBandID string
		wantBuckets := map[string]string{
			"C": "The Container Band",
			"U": "Unknown Artist",
		}
		for _, bucket := range artistsEnv.Artists.Index {
			wantArtist, ok := wantBuckets[bucket.Name]
			if !ok {
				t.Errorf("unexpected getArtists bucket %q", bucket.Name)
				continue
			}
			delete(wantBuckets, bucket.Name)
			if got, want := len(bucket.Artist), 1; got != want {
				t.Errorf("bucket %q artist len = %d, want %d", bucket.Name, got, want)
				continue
			}
			if got := bucket.Artist[0].Name; got != wantArtist {
				t.Errorf("bucket %q artist name = %q, want %q", bucket.Name, got, wantArtist)
			}
			if bucket.Name == "C" {
				containerBandID = bucket.Artist[0].ID
			}
		}
		if len(wantBuckets) > 0 {
			t.Fatalf("missing expected getArtists buckets: %v", wantBuckets)
		}
		if containerBandID == "" {
			t.Fatalf("The Container Band ID = empty, want non-empty")
		}

		artistEnv, _ := h.getJSON(t, h.withTokenAuth(t, "/rest/getArtist?f=json&id="+url.QueryEscape(containerBandID)))
		assertJSONResponseEnvelope(t, artistEnv, "ok")
		if artistEnv.Artist == nil {
			t.Fatalf("getArtist Artist = nil, want non-nil")
		}
		if got, want := artistEnv.Artist.Name, "The Container Band"; got != want {
			t.Errorf("getArtist Name = %q, want %q", got, want)
		}
		if got, want := len(artistEnv.Artist.Album), 1; got != want {
			t.Fatalf("getArtist Album len = %d, want %d", got, want)
		}
		alb := artistEnv.Artist.Album[0]
		if got, want := alb.Name, "Compose Session"; got != want {
			t.Errorf("getArtist album Name = %q, want %q", got, want)
		}
		if got, want := alb.SongCount, 1; got != want {
			t.Errorf("getArtist album SongCount = %d, want %d", got, want)
		}
		if got, want := alb.Duration, 2; got != want {
			t.Errorf("getArtist album Duration = %d, want %d", got, want)
		}
		if alb.ID == "" {
			t.Fatalf("Compose Session ID = empty, want non-empty")
		}

		albumEnv, albumBody := h.getJSON(t, h.withTokenAuth(t, "/rest/getAlbum?f=json&id="+url.QueryEscape(alb.ID)))
		assertJSONResponseEnvelope(t, albumEnv, "ok")
		if strings.Contains(string(albumBody), `"coverArt"`) {
			t.Errorf("getAlbum body unexpectedly contains coverArt: %s", string(albumBody))
		}
		if albumEnv.Album == nil {
			t.Fatalf("getAlbum Album = nil, want non-nil")
		}
		if got, want := len(albumEnv.Album.Song), 1; got != want {
			t.Fatalf("getAlbum Song len = %d, want %d", got, want)
		}
		song := albumEnv.Album.Song[0]
		if got, want := song.Track, 1; got != want {
			t.Errorf("getAlbum song Track = %d, want %d", got, want)
		}
		if got, want := song.Title, "Docker Track"; got != want {
			t.Errorf("getAlbum song Title = %q, want %q", got, want)
		}
		if got, want := song.Artist, "The Container Band"; got != want {
			t.Errorf("getAlbum song Artist = %q, want %q", got, want)
		}
		if got, want := song.Path, "live-album/01 - Docker Track.flac"; got != want {
			t.Errorf("getAlbum song Path = %q, want %q", got, want)
		}
		if got, want := song.ContentType, "audio/flac"; got != want {
			t.Errorf("getAlbum song ContentType = %q, want %q", got, want)
		}
	})

	t.Run("Search3AndUntaggedWAVFallback", func(t *testing.T) {
		searchEnv, _ := h.getJSON(t, h.withAPIKeyAuth(t, "/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500"))
		assertJSONResponseEnvelope(t, searchEnv, "ok")
		if searchEnv.SearchResult3 == nil {
			t.Fatalf("search3 SearchResult3 = nil, want non-nil")
		}
		sr := searchEnv.SearchResult3
		if got, want := len(sr.Artist), 2; got != want {
			t.Errorf("search3 Artist len = %d, want %d", got, want)
		}
		if got, want := len(sr.Album), 2; got != want {
			t.Errorf("search3 Album len = %d, want %d", got, want)
		}
		if got, want := len(sr.Song), 2; got != want {
			t.Errorf("search3 Song len = %d, want %d", got, want)
		}

		wavSong, ok := findSongByPath(sr.Song, "02 - Smoke Signal.wav")
		if !ok {
			t.Fatalf("search3 missing song %q in %+v", "02 - Smoke Signal.wav", sr.Song)
		}
		if got, want := wavSong.Title, "Smoke Signal"; got != want {
			t.Errorf("WAV song Title = %q, want %q", got, want)
		}
		if got, want := wavSong.Artist, "Unknown Artist"; got != want {
			t.Errorf("WAV song Artist = %q, want %q", got, want)
		}
		if got, want := wavSong.Album, "Unknown Album"; got != want {
			t.Errorf("WAV song Album = %q, want %q", got, want)
		}
		if got, want := wavSong.Duration, 2; got != want {
			t.Errorf("WAV song Duration = %d, want %d", got, want)
		}
		if got, want := wavSong.BitRate, 128; got != want {
			t.Errorf("WAV song BitRate = %d, want %d", got, want)
		}
	})
}
