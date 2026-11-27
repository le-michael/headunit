//go:build e2e

package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.senan.xyz/taglib"

	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

type dockerE2EConfig struct {
	baseURL     string
	musicDir    string
	scanTimeout time.Duration
}

func TestDockerComposeE2E(t *testing.T) {
	cfg := loadDockerE2EConfig(t)
	client := newProxyBypassHTTPClient(t)

	albumDir := filepath.Join(cfg.musicDir, "live-album")
	wavPath := filepath.Join(cfg.musicDir, "02 - Smoke Signal.wav")
	ignoredPath := filepath.Join(cfg.musicDir, "ignored.mp3.part")

	// Register cleanup on the root test so fixtures remain present across
	// dependent subtests and are still cleaned up if an earlier phase fails.
	t.Cleanup(func() {
		if err := os.RemoveAll(albumDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("cleanup os.RemoveAll(%q): %v", albumDir, err)
		}
		for _, path := range []string{wavPath, ignoredPath} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("cleanup os.Remove(%q): %v", path, err)
			}
		}
	})

	var (
		flacTrackID string
		wavTrackID  string
		flacBytes   []byte
		wavBytes    []byte
		flacPath    string
	)

	if !t.Run("SystemAndStartupScan", func(t *testing.T) {
		resp, body := doHTTPRequest(t, client, http.MethodGet, cfg.baseURL+"/rest/ping", nil, nil)
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/ping status = %d, want %d", got, want)
		}
		if got, want := resp.Header.Get("Content-Type"), "application/xml; charset=utf-8"; got != want {
			t.Errorf("GET /rest/ping Content-Type = %q, want %q", got, want)
		}
		if got, want := resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
			t.Errorf("GET /rest/ping X-Content-Type-Options = %q, want %q", got, want)
		}
		if !bytes.HasPrefix(body, []byte(xml.Header)) {
			t.Errorf("GET /rest/ping XML body missing %q header: %q", xml.Header, string(body))
		}
		xmlEnv := decodeXMLResponse(t, body)
		assertXMLResponseEnvelope(t, xmlEnv, "ok")
		if xmlEnv.Error != nil {
			t.Errorf("GET /rest/ping error = %+v, want nil", xmlEnv.Error)
		}

		jsonEnv, _ := getJSONResponse(t, client, cfg.baseURL+"/rest/ping.view?f=json")
		assertJSONResponseEnvelope(t, jsonEnv, "ok")

		extEnv, extRaw := getJSONResponse(t, client, cfg.baseURL+"/rest/getOpenSubsonicExtensions.view?f=json")
		assertJSONResponseEnvelope(t, extEnv, "ok")
		if !strings.Contains(string(extRaw), `"openSubsonicExtensions":[]`) {
			t.Errorf("getOpenSubsonicExtensions raw JSON = %s, want substring %q", string(extRaw), `"openSubsonicExtensions":[]`)
		}

		foldersEnv, _ := getJSONResponse(t, client, cfg.baseURL+"/rest/getMusicFolders?f=json")
		assertJSONResponseEnvelope(t, foldersEnv, "ok")
		if foldersEnv.MusicFolders == nil {
			t.Fatalf("getMusicFolders missing musicFolders payload")
		}
		wantFolders := []subsonic.MusicFolder{{ID: 1, Name: "Music"}}
		if !slices.Equal(foldersEnv.MusicFolders.MusicFolder, wantFolders) {
			t.Errorf("musicFolders = %+v, want %+v", foldersEnv.MusicFolders.MusicFolder, wantFolders)
		}

		status := pollScanStatus(t, client, cfg.baseURL, cfg.scanTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.LastScan != ""
		})
		if got, want := status.Count, int64(0); got != want {
			t.Errorf("startup scanStatus.count = %d, want %d", got, want)
		}
		if _, err := time.Parse(time.RFC3339, status.LastScan); err != nil {
			t.Errorf("startup scanStatus.lastScan = %q, want valid RFC3339 timestamp: %v", status.LastScan, err)
		}
	}) {
		return
	}

	if !t.Run("LiveDropBrowseAndSearch3", func(t *testing.T) {
		stagedFLAC := filepath.Join(t.TempDir(), "01 - Docker Track.flac")
		testutil.WriteMinimalFLAC(t, stagedFLAC)
		testutil.WriteTags(t, stagedFLAC, map[string][]string{
			taglib.Title:       {"Docker Track"},
			taglib.AlbumArtist: {"The Container Band"},
			taglib.Artist:      {"The Container Band"},
			taglib.Album:       {"Compose Session"},
			taglib.TrackNumber: {"1"},
			taglib.DiscNumber:  {"1"},
			taglib.Date:        {"2026"},
			taglib.Genre:       {"Electronic"},
		})
		flacBytes = mustReadFile(t, stagedFLAC)
		flacPath = publishReadableDirWithFile(t, cfg.musicDir, "live-album", "01 - Docker Track.flac", flacBytes)

		wavBytes = testutil.MinimalWAVBytes()
		if got, want := len(wavBytes), 32044; got != want {
			t.Fatalf("len(MinimalWAVBytes()) = %d, want %d", got, want)
		}
		wavPath = publishReadableFile(t, cfg.musicDir, "02 - Smoke Signal.wav", wavBytes)

		publishReadableFile(t, cfg.musicDir, "ignored.mp3.part", []byte("partial"))

		pollScanStatus(t, client, cfg.baseURL, cfg.scanTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 2
		})

		artistsEnv, _ := getJSONResponse(t, client, cfg.baseURL+"/rest/getArtists?f=json")
		assertJSONResponseEnvelope(t, artistsEnv, "ok")
		if artistsEnv.Artists == nil {
			t.Fatalf("getArtists missing artists payload")
		}
		if got, want := artistsEnv.Artists.IgnoredArticles, "The El La Los Las Le Les"; got != want {
			t.Errorf("artists.ignoredArticles = %q, want %q", got, want)
		}
		if got, want := len(artistsEnv.Artists.Index), 2; got != want {
			t.Fatalf("len(artists.index) = %d, want %d (indexes=%+v)", got, want, artistsEnv.Artists.Index)
		}
		idxC := artistsEnv.Artists.Index[0]
		if idxC.Name != "C" || len(idxC.Artist) != 1 || idxC.Artist[0].Name != "The Container Band" {
			t.Fatalf("index[0] = %+v, want bucket C containing 'The Container Band'", idxC)
		}
		idxU := artistsEnv.Artists.Index[1]
		if idxU.Name != "U" || len(idxU.Artist) != 1 || idxU.Artist[0].Name != "Unknown Artist" {
			t.Fatalf("index[1] = %+v, want bucket U containing 'Unknown Artist'", idxU)
		}
		artistID := idxC.Artist[0].ID

		artistEnv, _ := getJSONResponse(t, client, cfg.baseURL+"/rest/getArtist?f=json&id="+url.QueryEscape(artistID))
		assertJSONResponseEnvelope(t, artistEnv, "ok")
		if artistEnv.Artist == nil {
			t.Fatalf("getArtist missing artist payload")
		}
		if got, want := artistEnv.Artist.Name, "The Container Band"; got != want {
			t.Errorf("artist.name = %q, want %q", got, want)
		}
		if got, want := len(artistEnv.Artist.Album), 1; got != want {
			t.Fatalf("len(artist.album) = %d, want %d", got, want)
		}
		albumSummary := artistEnv.Artist.Album[0]
		if albumSummary.Name != "Compose Session" || albumSummary.SongCount != 1 || albumSummary.Duration != 2 {
			t.Errorf("artist.album[0] = %+v, want name='Compose Session' songCount=1 duration=2", albumSummary)
		}

		albumEnv, albumRaw := getJSONResponse(t, client, cfg.baseURL+"/rest/getAlbum?f=json&id="+url.QueryEscape(albumSummary.ID))
		assertJSONResponseEnvelope(t, albumEnv, "ok")
		if strings.Contains(string(albumRaw), `"coverArt"`) {
			t.Errorf("getAlbum raw JSON unexpectedly contains coverArt key: %s", string(albumRaw))
		}
		if albumEnv.Album == nil || len(albumEnv.Album.Song) != 1 {
			t.Fatalf("getAlbum songs = %+v, want 1 song", albumEnv.Album)
		}
		flacSong := albumEnv.Album.Song[0]
		if flacSong.Track != 1 || flacSong.Title != "Docker Track" || flacSong.Artist != "The Container Band" ||
			flacSong.Path != "live-album/01 - Docker Track.flac" || flacSong.ContentType != "audio/flac" {
			t.Errorf("album.song[0] = %+v, want track 1 'Docker Track' by 'The Container Band' at live-album/01 - Docker Track.flac (audio/flac)", flacSong)
		}
		flacTrackID = flacSong.ID

		searchEnv, _ := getJSONResponse(t, client, cfg.baseURL+"/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500")
		assertJSONResponseEnvelope(t, searchEnv, "ok")
		if searchEnv.SearchResult3 == nil {
			t.Fatalf("search3 missing searchResult3 payload")
		}
		sr := searchEnv.SearchResult3
		if got, want := len(sr.Artist), 2; got != want {
			t.Errorf("len(search3.artist) = %d, want %d", got, want)
		}
		if got, want := len(sr.Album), 2; got != want {
			t.Errorf("len(search3.album) = %d, want %d", got, want)
		}
		if got, want := len(sr.Song), 2; got != want {
			t.Fatalf("len(search3.song) = %d, want %d", got, want)
		}
		wavSong, ok := findSongByPath(sr.Song, "02 - Smoke Signal.wav")
		if !ok {
			t.Fatalf("search3 songs missing untagged WAV track: %+v", sr.Song)
		}
		if wavSong.Title != "Smoke Signal" || wavSong.Artist != "Unknown Artist" || wavSong.Album != "Unknown Album" {
			t.Errorf("untagged WAV fallback metadata = (%q, %q, %q), want ('Smoke Signal', 'Unknown Artist', 'Unknown Album')",
				wavSong.Title, wavSong.Artist, wavSong.Album)
		}
		if got, want := wavSong.Duration, 2; got != want {
			t.Errorf("untagged WAV duration = %d, want %d", got, want)
		}
		if got, want := wavSong.BitRate, 128; got != want {
			t.Errorf("untagged WAV bitRate = %d, want %d", got, want)
		}
		wavTrackID = wavSong.ID
	}) {
		return
	}

	if !t.Run("DirectRangeStreamingAndErrors", func(t *testing.T) {
		wavRangeHeaders := map[string]string{"Range": "bytes=0-3"}
		wavRangeResp, wavRangeBody := doHTTPRequest(t, client, http.MethodGet, cfg.baseURL+"/rest/stream?id="+url.QueryEscape(wavTrackID), nil, wavRangeHeaders)
		if got, want := wavRangeResp.StatusCode, http.StatusPartialContent; got != want {
			t.Fatalf("GET /rest/stream WAV range status = %d, want %d", got, want)
		}
		if got, want := wavRangeResp.Header.Get("Content-Type"), "audio/wav"; got != want {
			t.Errorf("WAV range Content-Type = %q, want %q", got, want)
		}
		wantRange := fmt.Sprintf("bytes 0-3/%d", len(wavBytes))
		if got := wavRangeResp.Header.Get("Content-Range"); got != wantRange {
			t.Errorf("WAV range Content-Range = %q, want %q", got, wantRange)
		}
		if got, want := string(wavRangeBody), "RIFF"; got != want {
			t.Errorf("WAV range body = %q, want %q", got, want)
		}

		wavHeadResp, wavHeadBody := doHTTPRequest(t, client, http.MethodHead, cfg.baseURL+"/rest/stream?id="+url.QueryEscape(wavTrackID), nil, nil)
		if got, want := wavHeadResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("HEAD /rest/stream WAV status = %d, want %d", got, want)
		}
		if got, want := wavHeadResp.Header.Get("Content-Type"), "audio/wav"; got != want {
			t.Errorf("HEAD /rest/stream Content-Type = %q, want %q", got, want)
		}
		if got, want := wavHeadResp.Header.Get("Accept-Ranges"), "bytes"; got != want {
			t.Errorf("HEAD /rest/stream Accept-Ranges = %q, want %q", got, want)
		}
		if got, want := wavHeadResp.Header.Get("Content-Length"), strconv.Itoa(len(wavBytes)); got != want {
			t.Errorf("HEAD /rest/stream Content-Length = %q, want %q", got, want)
		}
		if got := len(wavHeadBody); got != 0 {
			t.Errorf("HEAD /rest/stream body len = %d, want 0", got)
		}

		flacResp, flacBody := doHTTPRequest(t, client, http.MethodGet, cfg.baseURL+"/rest/stream?id="+url.QueryEscape(flacTrackID), nil, nil)
		if got, want := flacResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream FLAC status = %d, want %d", got, want)
		}
		if got, want := flacResp.Header.Get("Content-Type"), "audio/flac"; got != want {
			t.Errorf("FLAC Content-Type = %q, want %q", got, want)
		}
		if got, want := flacResp.Header.Get("Accept-Ranges"), "bytes"; got != want {
			t.Errorf("FLAC Accept-Ranges = %q, want %q", got, want)
		}
		if got := flacResp.Header.Get("ETag"); got == "" {
			t.Errorf("FLAC ETag = empty, want non-empty ETag")
		}
		if got, want := flacResp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
			t.Errorf("FLAC X-Content-Type-Options = %q, want %q", got, want)
		}
		if !bytes.Equal(flacBody, flacBytes) {
			t.Errorf("FLAC stream body mismatch: got %d bytes, want %d bytes", len(flacBody), len(flacBytes))
		}

		badRangeHeaders := map[string]string{"Range": "bytes=9999999-"}
		wav416Resp, _ := doHTTPRequest(t, client, http.MethodGet, cfg.baseURL+"/rest/stream?id="+url.QueryEscape(wavTrackID), nil, badRangeHeaders)
		if got, want := wav416Resp.StatusCode, http.StatusRequestedRangeNotSatisfiable; got != want {
			t.Errorf("GET /rest/stream WAV invalid range status = %d, want %d", got, want)
		}
		want416Range := fmt.Sprintf("bytes */%d", len(wavBytes))
		if got := wav416Resp.Header.Get("Content-Range"); got != want416Range {
			t.Errorf("WAV 416 Content-Range = %q, want %q", got, want416Range)
		}

		assertStreamErrorNotFound(t, client, cfg.baseURL, "tr_unknown")
	}) {
		return
	}

	if !t.Run("LiveDeleteReconciliation", func(t *testing.T) {
		if err := os.Remove(wavPath); err != nil {
			t.Fatalf("os.Remove(%q) unexpected error: %v", wavPath, err)
		}
		pollScanStatus(t, client, cfg.baseURL, cfg.scanTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 1
		})
		assertStreamErrorNotFound(t, client, cfg.baseURL, wavTrackID)

		if err := os.Remove(flacPath); err != nil {
			t.Fatalf("os.Remove(%q) unexpected error: %v", flacPath, err)
		}
		pollScanStatus(t, client, cfg.baseURL, cfg.scanTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 0
		})
		assertStreamErrorNotFound(t, client, cfg.baseURL, flacTrackID)
	}) {
		return
	}
}

func loadDockerE2EConfig(t *testing.T) dockerE2EConfig {
	t.Helper()

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("HEADUNIT_E2E_URL")), "/")
	if baseURL == "" {
		t.Fatalf("HEADUNIT_E2E_URL must be set when running -tags=e2e")
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		t.Fatalf("HEADUNIT_E2E_URL=%q must be a valid HTTP(S) base URL: %v", baseURL, err)
	}

	musicDir := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_MUSIC_DIR"))
	if musicDir == "" {
		t.Fatalf("HEADUNIT_E2E_MUSIC_DIR must be set when running -tags=e2e")
	}
	info, err := os.Stat(musicDir)
	if err != nil {
		t.Fatalf("HEADUNIT_E2E_MUSIC_DIR %q stat error: %v", musicDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("HEADUNIT_E2E_MUSIC_DIR %q is not a directory", musicDir)
	}

	scanTimeout := 30 * time.Second
	if rawTimeout := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_SCAN_TIMEOUT")); rawTimeout != "" {
		parsed, err := time.ParseDuration(rawTimeout)
		if err != nil || parsed <= 0 {
			t.Fatalf("HEADUNIT_E2E_SCAN_TIMEOUT=%q must be a positive duration: %v", rawTimeout, err)
		}
		scanTimeout = parsed
	}

	return dockerE2EConfig{
		baseURL:     baseURL,
		musicDir:    musicDir,
		scanTimeout: scanTimeout,
	}
}

func newProxyBypassHTTPClient(t *testing.T) *http.Client {
	t.Helper()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func publishReadableFile(t *testing.T, dir, filename string, data []byte) string {
	t.Helper()

	tmpFile, err := os.CreateTemp(dir, ".e2e-stage-*.tmp")
	if err != nil {
		t.Fatalf("os.CreateTemp(%q): %v", dir, err)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		t.Fatalf("write temp file %q: %v", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file %q: %v", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		t.Fatalf("os.Chmod(%q, 0644): %v", tmpPath, err)
	}

	dstPath := filepath.Join(dir, filename)
	if err := os.Rename(tmpPath, dstPath); err != nil {
		t.Fatalf("os.Rename(%q, %q): %v", tmpPath, dstPath, err)
	}
	cleanupTmp = false
	return dstPath
}

func publishReadableDirWithFile(t *testing.T, musicDir, dirName, filename string, data []byte) string {
	t.Helper()

	stageDir, err := os.MkdirTemp(musicDir, ".e2e-dir-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp(%q): %v", musicDir, err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	if err := os.Chmod(stageDir, 0o755); err != nil {
		t.Fatalf("os.Chmod(%q, 0755): %v", stageDir, err)
	}

	stagedFile := filepath.Join(stageDir, filename)
	if err := os.WriteFile(stagedFile, data, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", stagedFile, err)
	}
	if err := os.Chmod(stagedFile, 0o644); err != nil {
		t.Fatalf("os.Chmod(%q, 0644): %v", stagedFile, err)
	}

	dstDir := filepath.Join(musicDir, dirName)
	if err := os.Rename(stageDir, dstDir); err != nil {
		t.Fatalf("os.Rename(%q, %q): %v", stageDir, dstDir, err)
	}
	cleanupStage = false
	return filepath.Join(dstDir, filename)
}

func assertStreamErrorNotFound(t *testing.T, client *http.Client, baseURL, trackID string) {
	t.Helper()

	errResp, errBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/stream?id="+url.QueryEscape(trackID)+"&f=json", nil, nil)
	if got, want := errResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /rest/stream?id=%s status = %d, want %d", trackID, got, want)
	}
	if got, want := errResp.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("stream error Content-Type = %q, want %q", got, want)
	}
	for _, hdr := range []string{"ETag", "Accept-Ranges", "Content-Range"} {
		if got := errResp.Header.Get(hdr); got != "" {
			t.Errorf("stream error header %s = %q, want empty", hdr, got)
		}
	}
	errEnv := decodeJSONResponse(t, errBody)
	assertJSONResponseEnvelope(t, errEnv, "failed")
	if errEnv.Error == nil || errEnv.Error.Code != 70 {
		t.Errorf("stream error payload = %+v, want code 70", errEnv.Error)
	}
}
