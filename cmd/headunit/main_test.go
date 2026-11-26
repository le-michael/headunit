package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.senan.xyz/taglib"

	"headunit/internal/config"
	"headunit/internal/store"
	"headunit/internal/stream"
	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

const (
	e2ePollInterval = 50 * time.Millisecond
	e2ePollTimeout  = 15 * time.Second
)

type jsonSubsonicEnvelope struct {
	SubsonicResponse subsonic.Response `json:"subsonic-response"`
}

func TestFullStackLifecycleAndOpenSubsonicE2E(t *testing.T) {
	musicDir := t.TempDir()
	dataDir := t.TempDir()

	// Seed 3 valid audio files across nested folders plus 2 ignored files.
	mp3Path := filepath.Join(musicDir, "dirA", "sub1", "01 - Opening.mp3")
	testutil.WriteMinimalMP3(t, mp3Path)
	testutil.WriteTags(t, mp3Path, map[string][]string{
		taglib.Title:       {"Opening"},
		taglib.AlbumArtist: {"The Alpha Band"},
		taglib.Artist:      {"The Alpha Band feat. Guest"},
		taglib.Album:       {"First Light"},
		taglib.TrackNumber: {"1"},
		taglib.DiscNumber:  {"1"},
		taglib.Date:        {"2024"},
		taglib.Genre:       {"Indie"},
	})
	mp3Bytes := mustReadFile(t, mp3Path)

	flacPath := filepath.Join(musicDir, "dirB", "02 - Second.flac")
	testutil.WriteMinimalFLAC(t, flacPath)
	testutil.WriteTags(t, flacPath, map[string][]string{
		taglib.Title:       {"Second"},
		taglib.AlbumArtist: {"The Alpha Band"},
		taglib.Artist:      {"The Alpha Band"},
		taglib.Album:       {"First Light"},
		taglib.TrackNumber: {"2"},
		taglib.DiscNumber:  {"1"},
		taglib.Date:        {"2024"},
		taglib.Genre:       {"Indie"},
	})
	flacBytes := mustReadFile(t, flacPath)

	wavPath := filepath.Join(musicDir, "misc", "03 - Raw Signal.wav")
	testutil.WriteMinimalWAV(t, wavPath)
	wavBytes := mustReadFile(t, wavPath)

	testutil.WriteBytes(t, filepath.Join(musicDir, ".DS_Store"), []byte("ignored"))
	testutil.WriteBytes(t, filepath.Join(musicDir, "dirA", "partial.mp3.part"), []byte("partial"))

	cfg := config.Config{
		Port:         "4533",
		MusicDir:     musicDir,
		DataDir:      dataDir,
		ScanInterval: 0,
	}

	srv, err := newServer(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("newServer() unexpected error: %v", err)
	}

	// Verify production HTTP server timeout and context invariants.
	if got, want := srv.httpSrv.ReadHeaderTimeout, 10*time.Second; got != want {
		t.Errorf("httpSrv.ReadHeaderTimeout = %v, want %v", got, want)
	}
	if got, want := srv.httpSrv.IdleTimeout, 120*time.Second; got != want {
		t.Errorf("httpSrv.IdleTimeout = %v, want %v", got, want)
	}
	if got := srv.httpSrv.WriteTimeout; got != 0 {
		t.Errorf("httpSrv.WriteTimeout = %v, want 0", got)
	}
	if srv.httpSrv.ErrorLog == nil {
		t.Errorf("httpSrv.ErrorLog = nil, want non-nil slog logger")
	}
	if srv.httpSrv.BaseContext != nil {
		t.Errorf("httpSrv.BaseContext != nil, want nil so in-flight requests drain cleanly")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = srv.closeResources()
		t.Fatalf("net.Listen() unexpected error: %v", err)
	}
	listenAddr := ln.Addr().String()
	baseURL := "http://" + listenAddr

	runCtx, runCancel := context.WithCancel(context.Background())
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.serve(runCtx, ln)
	}()

	var (
		waitOnce     sync.Once
		waitServeErr error
		waitTimedOut bool
	)
	stopAndWait := func(timeout time.Duration) (error, bool) {
		runCancel()
		waitOnce.Do(func() {
			select {
			case waitServeErr = <-serveErrCh:
			case <-time.After(timeout):
				waitTimedOut = true
			}
		})
		return waitServeErr, waitTimedOut
	}
	t.Cleanup(func() {
		err, timedOut := stopAndWait(10 * time.Second)
		if timedOut {
			t.Errorf("cleanup: timed out waiting for server.serve to exit")
			return
		}
		if err != nil {
			t.Errorf("cleanup: server.serve returned unexpected error: %v", err)
		}
	})

	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	var mp3TrackID, flacTrackID, wavTrackID string

	if !t.Run("StartupScanStatus", func(t *testing.T) {
		status := pollScanStatus(t, client, baseURL, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 3 && s.LastScan != ""
		})
		if _, err := time.Parse(time.RFC3339, status.LastScan); err != nil {
			t.Errorf("scanStatus.lastScan = %q, want valid RFC3339 timestamp: %v", status.LastScan, err)
		}
	}) {
		return
	}

	t.Run("SystemDualRoutesAndMethods", func(t *testing.T) {
		// GET /rest/ping (default XML)
		resp, body := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/ping", nil, nil)
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

		// GET /rest/ping.view?f=json
		jsonEnv, _ := getJSONResponse(t, client, baseURL+"/rest/ping.view?f=json")
		assertJSONResponseEnvelope(t, jsonEnv, "ok")

		// HEAD /rest/ping
		headResp, headBody := doHTTPRequest(t, client, http.MethodHead, baseURL+"/rest/ping", nil, nil)
		if got, want := headResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("HEAD /rest/ping status = %d, want %d", got, want)
		}
		if got := len(headBody); got != 0 {
			t.Errorf("HEAD /rest/ping body len = %d, want 0", got)
		}
		cl, err := strconv.Atoi(headResp.Header.Get("Content-Length"))
		if err != nil || cl <= 0 {
			t.Errorf("HEAD /rest/ping Content-Length = %q, want integer > 0", headResp.Header.Get("Content-Length"))
		}

		// OPTIONS /rest/ping
		optResp, optBody := doHTTPRequest(t, client, http.MethodOptions, baseURL+"/rest/ping", nil, nil)
		if got, want := optResp.StatusCode, http.StatusNoContent; got != want {
			t.Fatalf("OPTIONS /rest/ping status = %d, want %d", got, want)
		}
		if got := len(optBody); got != 0 {
			t.Errorf("OPTIONS /rest/ping body len = %d, want 0", got)
		}
		if got, want := optResp.Header.Get("Access-Control-Allow-Origin"), "*"; got != want {
			t.Errorf("CORS Allow-Origin = %q, want %q", got, want)
		}
		if got, want := optResp.Header.Get("Access-Control-Allow-Methods"), "GET, POST, HEAD, OPTIONS"; got != want {
			t.Errorf("CORS Allow-Methods = %q, want %q", got, want)
		}
		if got := optResp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Range") {
			t.Errorf("CORS Allow-Headers = %q, want to include Range", got)
		}
		if got := optResp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Content-Range") {
			t.Errorf("CORS Expose-Headers = %q, want to include Content-Range", got)
		}

		// POST /rest/getUser.view with form body f=json&u=alice
		formValues := url.Values{"f": {"json"}, "u": {"alice"}}
		postHeaders := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		userResp, userBody := doHTTPRequest(t, client, http.MethodPost, baseURL+"/rest/getUser.view", strings.NewReader(formValues.Encode()), postHeaders)
		if got, want := userResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("POST /rest/getUser.view status = %d, want %d", got, want)
		}
		userEnv := decodeJSONResponse(t, userBody)
		assertJSONResponseEnvelope(t, userEnv, "ok")
		if userEnv.User == nil {
			t.Fatalf("POST /rest/getUser.view missing user payload: %s", string(userBody))
		}
		if got, want := userEnv.User.Username, "alice"; got != want {
			t.Errorf("user.username = %q, want %q", got, want)
		}
		if !userEnv.User.StreamRole {
			t.Errorf("user.streamRole = false, want true")
		}
		if userEnv.User.DownloadRole {
			t.Errorf("user.downloadRole = true, want false")
		}
		if !slices.Equal(userEnv.User.Folder, []int{1}) {
			t.Errorf("user.folder = %v, want [1]", userEnv.User.Folder)
		}

		// GET /rest/getLicense?f=json
		licEnv, _ := getJSONResponse(t, client, baseURL+"/rest/getLicense?f=json")
		assertJSONResponseEnvelope(t, licEnv, "ok")
		if licEnv.License == nil || !licEnv.License.Valid {
			t.Errorf("getLicense license = %+v, want valid=true", licEnv.License)
		}

		// GET /rest/getOpenSubsonicExtensions.view?f=json
		extEnv, extRaw := getJSONResponse(t, client, baseURL+"/rest/getOpenSubsonicExtensions.view?f=json")
		assertJSONResponseEnvelope(t, extEnv, "ok")
		if !strings.Contains(string(extRaw), `"openSubsonicExtensions":[]`) {
			t.Errorf("getOpenSubsonicExtensions raw JSON = %s, want substring %q", string(extRaw), `"openSubsonicExtensions":[]`)
		}
	})

	if !t.Run("LibraryBrowseAndSearch3", func(t *testing.T) {
		// getMusicFolders
		foldersEnv, _ := getJSONResponse(t, client, baseURL+"/rest/getMusicFolders?f=json")
		assertJSONResponseEnvelope(t, foldersEnv, "ok")
		if foldersEnv.MusicFolders == nil {
			t.Fatalf("getMusicFolders missing musicFolders payload")
		}
		wantFolders := []subsonic.MusicFolder{{ID: 1, Name: "Music"}}
		if !slices.Equal(foldersEnv.MusicFolders.MusicFolder, wantFolders) {
			t.Errorf("musicFolders = %+v, want %+v", foldersEnv.MusicFolders.MusicFolder, wantFolders)
		}

		// getArtists: verifies ignoredArticles ("The Alpha Band" -> "A") and fallback ("Unknown Artist" -> "U").
		artistsEnv, _ := getJSONResponse(t, client, baseURL+"/rest/getArtists?f=json")
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
		idxA := artistsEnv.Artists.Index[0]
		if idxA.Name != "A" || len(idxA.Artist) != 1 || idxA.Artist[0].Name != "The Alpha Band" {
			t.Fatalf("index[0] = %+v, want bucket A containing 'The Alpha Band'", idxA)
		}
		idxU := artistsEnv.Artists.Index[1]
		if idxU.Name != "U" || len(idxU.Artist) != 1 || idxU.Artist[0].Name != "Unknown Artist" {
			t.Fatalf("index[1] = %+v, want bucket U containing 'Unknown Artist'", idxU)
		}
		alphaArtistID := idxA.Artist[0].ID

		// getArtist
		artistEnv, _ := getJSONResponse(t, client, baseURL+"/rest/getArtist?f=json&id="+url.QueryEscape(alphaArtistID))
		assertJSONResponseEnvelope(t, artistEnv, "ok")
		if artistEnv.Artist == nil {
			t.Fatalf("getArtist missing artist payload")
		}
		if got, want := artistEnv.Artist.Name, "The Alpha Band"; got != want {
			t.Errorf("artist.name = %q, want %q", got, want)
		}
		if got, want := len(artistEnv.Artist.Album), 1; got != want {
			t.Fatalf("len(artist.album) = %d, want %d", got, want)
		}
		firstLight := artistEnv.Artist.Album[0]
		if firstLight.Name != "First Light" || firstLight.SongCount != 2 || firstLight.Duration != 4 {
			t.Errorf("artist.album[0] = %+v, want name='First Light' songCount=2 duration=4", firstLight)
		}

		// getAlbum: 2 tracks ordered by track number across separate folders, zero coverArt key.
		albumEnv, albumRaw := getJSONResponse(t, client, baseURL+"/rest/getAlbum?f=json&id="+url.QueryEscape(firstLight.ID))
		assertJSONResponseEnvelope(t, albumEnv, "ok")
		if strings.Contains(string(albumRaw), `"coverArt"`) {
			t.Errorf("getAlbum raw JSON unexpectedly contains coverArt key: %s", string(albumRaw))
		}
		if albumEnv.Album == nil || len(albumEnv.Album.Song) != 2 {
			t.Fatalf("getAlbum songs = %+v, want 2 songs", albumEnv.Album)
		}
		s0, s1 := albumEnv.Album.Song[0], albumEnv.Album.Song[1]
		if s0.Track != 1 || s0.Title != "Opening" || s0.Artist != "The Alpha Band feat. Guest" || s0.Path != "dirA/sub1/01 - Opening.mp3" {
			t.Errorf("album.song[0] = %+v, want track 1 'Opening' in dirA/sub1", s0)
		}
		if s1.Track != 2 || s1.Title != "Second" || s1.Artist != "The Alpha Band" || s1.Path != "dirB/02 - Second.flac" {
			t.Errorf("album.song[1] = %+v, want track 2 'Second' in dirB", s1)
		}
		mp3TrackID = s0.ID
		flacTrackID = s1.ID

		// getAlbumList2?type=alphabeticalByName
		albumListEnv, _ := getJSONResponse(t, client, baseURL+"/rest/getAlbumList2?f=json&type=alphabeticalByName")
		assertJSONResponseEnvelope(t, albumListEnv, "ok")
		if albumListEnv.AlbumList2 == nil || len(albumListEnv.AlbumList2.Album) != 2 {
			t.Fatalf("getAlbumList2 albums = %+v, want 2 albums", albumListEnv.AlbumList2)
		}
		gotAlbumNames := []string{albumListEnv.AlbumList2.Album[0].Name, albumListEnv.AlbumList2.Album[1].Name}
		wantAlbumNames := []string{"First Light", "Unknown Album"}
		if !slices.Equal(gotAlbumNames, wantAlbumNames) {
			t.Errorf("albumList2 names = %v, want %v", gotAlbumNames, wantAlbumNames)
		}

		// getSong (JSON)
		songJSONEnv, _ := getJSONResponse(t, client, baseURL+"/rest/getSong?f=json&id="+url.QueryEscape(mp3TrackID))
		assertJSONResponseEnvelope(t, songJSONEnv, "ok")
		if songJSONEnv.Song == nil || songJSONEnv.Song.ID != mp3TrackID || songJSONEnv.Song.Title != "Opening" {
			t.Errorf("getSong JSON song = %+v, want id=%q title='Opening'", songJSONEnv.Song, mp3TrackID)
		}

		// getSong (XML)
		songXMLResp, songXMLBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/getSong?id="+url.QueryEscape(mp3TrackID), nil, nil)
		if got, want := songXMLResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/getSong XML status = %d, want %d", got, want)
		}
		songXMLEnv := decodeXMLResponse(t, songXMLBody)
		assertXMLResponseEnvelope(t, songXMLEnv, "ok")
		if songXMLEnv.Song == nil || songXMLEnv.Song.ID != mp3TrackID || songXMLEnv.Song.Title != "Opening" || songXMLEnv.Song.ContentType != "audio/mpeg" {
			t.Errorf("getSong XML song = %+v, want id=%q title='Opening' contentType='audio/mpeg'", songXMLEnv.Song, mp3TrackID)
		}

		// search3 bulk sync (literal "")
		searchEnv, _ := getJSONResponse(t, client, baseURL+"/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500")
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
		if got, want := len(sr.Song), 3; got != want {
			t.Fatalf("len(search3.song) = %d, want %d", got, want)
		}
		wavSong, ok := findSongByPath(sr.Song, "misc/03 - Raw Signal.wav")
		if !ok {
			t.Fatalf("search3 songs missing untagged WAV track: %+v", sr.Song)
		}
		if wavSong.Title != "Raw Signal" || wavSong.Artist != "Unknown Artist" || wavSong.Album != "Unknown Album" {
			t.Errorf("untagged WAV fallback metadata = (%q, %q, %q), want ('Raw Signal', 'Unknown Artist', 'Unknown Album')",
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

	t.Run("DirectRangeStreaming", func(t *testing.T) {
		// 200 full stream MP3
		mp3Resp, mp3Body := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/stream?id="+url.QueryEscape(mp3TrackID), nil, nil)
		if got, want := mp3Resp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream MP3 status = %d, want %d", got, want)
		}
		if got, want := mp3Resp.Header.Get("Content-Type"), "audio/mpeg"; got != want {
			t.Errorf("MP3 Content-Type = %q, want %q", got, want)
		}
		if got, want := mp3Resp.Header.Get("Accept-Ranges"), "bytes"; got != want {
			t.Errorf("MP3 Accept-Ranges = %q, want %q", got, want)
		}
		if got := mp3Resp.Header.Get("ETag"); got == "" {
			t.Errorf("MP3 ETag = empty, want non-empty ETag")
		}
		if got, want := mp3Resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
			t.Errorf("MP3 X-Content-Type-Options = %q, want %q", got, want)
		}
		if !bytes.Equal(mp3Body, mp3Bytes) {
			t.Errorf("MP3 stream body mismatch: got %d bytes, want %d bytes", len(mp3Body), len(mp3Bytes))
		}

		// 206 partial content FLAC bytes=0-1 -> "fL"
		rangeHeaders := map[string]string{"Range": "bytes=0-1"}
		flacResp, flacBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/stream?id="+url.QueryEscape(flacTrackID), nil, rangeHeaders)
		if got, want := flacResp.StatusCode, http.StatusPartialContent; got != want {
			t.Fatalf("GET /rest/stream FLAC range status = %d, want %d", got, want)
		}
		if got, want := flacResp.Header.Get("Content-Type"), "audio/flac"; got != want {
			t.Errorf("FLAC Content-Type = %q, want %q", got, want)
		}
		wantRange := fmt.Sprintf("bytes 0-1/%d", len(flacBytes))
		if got := flacResp.Header.Get("Content-Range"); got != wantRange {
			t.Errorf("FLAC Content-Range = %q, want %q", got, wantRange)
		}
		if got, want := string(flacBody), "fL"; got != want {
			t.Errorf("FLAC range body = %q, want %q", got, want)
		}

		// 416 unsatisfiable range WAV
		badRangeHeaders := map[string]string{"Range": "bytes=9999999-"}
		wav416Resp, _ := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/stream?id="+url.QueryEscape(wavTrackID), nil, badRangeHeaders)
		if got, want := wav416Resp.StatusCode, http.StatusRequestedRangeNotSatisfiable; got != want {
			t.Errorf("GET /rest/stream WAV invalid range status = %d, want %d", got, want)
		}
		want416Range := fmt.Sprintf("bytes */%d", len(wavBytes))
		if got := wav416Resp.Header.Get("Content-Range"); got != want416Range {
			t.Errorf("WAV 416 Content-Range = %q, want %q", got, want416Range)
		}

		// HEAD /rest/stream WAV
		wavHeadResp, wavHeadBody := doHTTPRequest(t, client, http.MethodHead, baseURL+"/rest/stream?id="+url.QueryEscape(wavTrackID), nil, nil)
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

		// Unknown track ID -> HTTP 200 Subsonic error 70 with zero audio headers
		errResp, errBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/stream?id=tr_unknown&f=json", nil, nil)
		if got, want := errResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream unknown id status = %d, want %d", got, want)
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
	})

	t.Run("CompatibilityStubs", func(t *testing.T) {
		plEnv, plRaw := getJSONResponse(t, client, baseURL+"/rest/getPlaylists?f=json")
		assertJSONResponseEnvelope(t, plEnv, "ok")
		if !strings.Contains(string(plRaw), `"playlist":[]`) {
			t.Errorf("getPlaylists raw JSON = %s, want non-null empty array", string(plRaw))
		}

		genreEnv, genreRaw := getJSONResponse(t, client, baseURL+"/rest/getGenres?f=json")
		assertJSONResponseEnvelope(t, genreEnv, "ok")
		if !strings.Contains(string(genreRaw), `"genre":[]`) {
			t.Errorf("getGenres raw JSON = %s, want non-null empty array", string(genreRaw))
		}

		starEnv, starRaw := getJSONResponse(t, client, baseURL+"/rest/getStarred2?f=json")
		assertJSONResponseEnvelope(t, starEnv, "ok")
		for _, wantKey := range []string{`"artist":[]`, `"album":[]`, `"song":[]`} {
			if !strings.Contains(string(starRaw), wantKey) {
				t.Errorf("getStarred2 raw JSON = %s, want substring %q", string(starRaw), wantKey)
			}
		}

		coverEnv, _ := getJSONResponse(t, client, baseURL+"/rest/getCoverArt?f=json&id=al_missing")
		assertJSONResponseEnvelope(t, coverEnv, "failed")
		if coverEnv.Error == nil || coverEnv.Error.Code != 70 {
			t.Errorf("getCoverArt error = %+v, want code 70", coverEnv.Error)
		}

		scrobbleEnv, _ := getJSONResponse(t, client, baseURL+"/rest/scrobble?f=json&id="+url.QueryEscape(mp3TrackID))
		assertJSONResponseEnvelope(t, scrobbleEnv, "ok")
	})

	t.Run("LiveFilesystemDropAndDelete", func(t *testing.T) {
		// Prepare and tag the new FLAC outside musicDir first so the live drop
		// writes final tagged bytes in a single operation.
		stagedPath := filepath.Join(t.TempDir(), "01 - Live Drop.flac")
		testutil.WriteMinimalFLAC(t, stagedPath)
		testutil.WriteTags(t, stagedPath, map[string][]string{
			taglib.Title:       {"Live Drop"},
			taglib.AlbumArtist: {"The Beta Quartet"},
			taglib.Artist:      {"The Beta Quartet"},
			taglib.Album:       {"Live Session"},
			taglib.TrackNumber: {"1"},
		})
		liveBytes := mustReadFile(t, stagedPath)

		livePath := filepath.Join(musicDir, "live-added", "nested", "01 - Live Drop.flac")
		testutil.WriteBytes(t, livePath, liveBytes)

		pollScanStatus(t, client, baseURL, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 4
		})

		searchEnv, _ := getJSONResponse(t, client, baseURL+"/rest/search3?f=json&query=Live+Drop")
		assertJSONResponseEnvelope(t, searchEnv, "ok")
		if searchEnv.SearchResult3 == nil {
			t.Fatalf("search3 Live Drop missing searchResult3 payload")
		}
		liveSong, ok := findSongByPath(searchEnv.SearchResult3.Song, "live-added/nested/01 - Live Drop.flac")
		if !ok {
			t.Fatalf("search3 Live Drop songs = %+v, want live-added/nested/01 - Live Drop.flac", searchEnv.SearchResult3.Song)
		}
		if got, want := liveSong.Title, "Live Drop"; got != want {
			t.Errorf("liveSong.Title = %q, want %q", got, want)
		}

		streamResp, streamBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/stream?id="+url.QueryEscape(liveSong.ID), nil, nil)
		if got, want := streamResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream live track status = %d, want %d", got, want)
		}
		if !bytes.Equal(streamBody, liveBytes) {
			t.Errorf("live track stream body mismatch: got %d bytes, want %d bytes", len(streamBody), len(liveBytes))
		}

		if err := os.Remove(livePath); err != nil {
			t.Fatalf("os.Remove(%q) unexpected error: %v", livePath, err)
		}

		pollScanStatus(t, client, baseURL, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 3
		})

		deletedEnv, _ := getJSONResponse(t, client, baseURL+"/rest/stream?id="+url.QueryEscape(liveSong.ID)+"&f=json")
		assertJSONResponseEnvelope(t, deletedEnv, "failed")
		if deletedEnv.Error == nil || deletedEnv.Error.Code != 70 {
			t.Errorf("stream deleted track error = %+v, want code 70", deletedEnv.Error)
		}
	})

	t.Run("GracefulShutdownAndWALReopen", func(t *testing.T) {
		serveErr, timedOut := stopAndWait(10 * time.Second)
		if timedOut {
			t.Fatalf("timed out waiting for graceful shutdown")
		}
		if serveErr != nil {
			t.Fatalf("server.serve() error = %v, want nil", serveErr)
		}

		conn, err := net.DialTimeout("tcp", listenAddr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("tcp dial %q succeeded after shutdown, want connection refused", listenAddr)
		}

		reopened, err := store.Open(context.Background(), filepath.Join(dataDir, dbFilename))
		if err != nil {
			t.Fatalf("reopen store after shutdown: %v", err)
		}
		defer func() {
			if closeErr := reopened.Close(); closeErr != nil {
				t.Errorf("reopened.Close() unexpected error: %v", closeErr)
			}
		}()

		count, err := reopened.TrackCount(context.Background())
		if err != nil {
			t.Fatalf("reopened.TrackCount() unexpected error: %v", err)
		}
		if got, want := count, int64(3); got != want {
			t.Errorf("reopened TrackCount = %d, want %d", got, want)
		}
	})
}

func TestStartupAndServeFailureCleanup(t *testing.T) {
	t.Run("missing music dir", func(t *testing.T) {
		dataDir := t.TempDir()
		cfg := config.Config{
			Port:         "4533",
			MusicDir:     filepath.Join(t.TempDir(), "does-not-exist"),
			DataDir:      dataDir,
			ScanInterval: 0,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err == nil {
			_ = srv.closeResources()
			t.Fatalf("newServer() error = nil, want error for missing MUSIC_DIR")
		}
		if srv != nil {
			t.Fatalf("newServer() server = %+v, want nil", srv)
		}

		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("data dir is regular file", func(t *testing.T) {
		musicDir := t.TempDir()
		dataFile := filepath.Join(t.TempDir(), "not-a-directory")
		testutil.WriteBytes(t, dataFile, []byte("file"))

		cfg := config.Config{
			Port:         "4533",
			MusicDir:     musicDir,
			DataDir:      dataFile,
			ScanInterval: 0,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err == nil {
			_ = srv.closeResources()
			t.Fatalf("newServer() error = nil, want error when DATA_DIR is a regular file")
		}
		if srv != nil {
			t.Fatalf("newServer() server = %+v, want nil", srv)
		}
	})

	t.Run("invalid env config", func(t *testing.T) {
		tests := []struct {
			name         string
			port         string
			scanInterval string
		}{
			{name: "PORT=0", port: "0", scanInterval: "0"},
			{name: "SCAN_INTERVAL=-1s", port: "4533", scanInterval: "-1s"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PORT", tc.port)
				t.Setenv("MUSIC_DIR", t.TempDir())
				t.Setenv("DATA_DIR", t.TempDir())
				t.Setenv("SCAN_INTERVAL", tc.scanInterval)

				if err := run(context.Background(), nil); err == nil {
					t.Fatalf("run() error = nil, want validation error for %s", tc.name)
				}
			})
		}
	})

	t.Run("occupied port", func(t *testing.T) {
		occupied, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("net.Listen(:0) unexpected error: %v", err)
		}
		defer occupied.Close()

		_, port, err := net.SplitHostPort(occupied.Addr().String())
		if err != nil {
			t.Fatalf("net.SplitHostPort(%q) unexpected error: %v", occupied.Addr().String(), err)
		}

		dataDir := t.TempDir()
		t.Setenv("PORT", port)
		t.Setenv("MUSIC_DIR", t.TempDir())
		t.Setenv("DATA_DIR", dataDir)
		t.Setenv("SCAN_INTERVAL", "0")

		err = run(context.Background(), slog.New(slog.DiscardHandler))
		if err == nil {
			t.Fatalf("run() error = nil, want EADDRINUSE error")
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("run() error = %v, want errors.Is(err, syscall.EADDRINUSE)", err)
		}

		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("closed listener", func(t *testing.T) {
		dataDir := t.TempDir()
		cfg := config.Config{
			Port:         "4533",
			MusicDir:     t.TempDir(),
			DataDir:      dataDir,
			ScanInterval: 0,
		}

		srv, err := newServer(context.Background(), cfg, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("newServer() unexpected error: %v", err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			_ = srv.closeResources()
			t.Fatalf("net.Listen() unexpected error: %v", err)
		}
		if err := ln.Close(); err != nil {
			_ = srv.closeResources()
			t.Fatalf("ln.Close() unexpected error: %v", err)
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- srv.serve(context.Background(), ln)
		}()

		select {
		case serveErr := <-errCh:
			if serveErr == nil {
				t.Fatalf("serve(closed listener) error = nil, want non-nil")
			}
			if !errors.Is(serveErr, net.ErrClosed) {
				t.Errorf("serve(closed listener) error = %v, want errors.Is(err, net.ErrClosed)", serveErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for serve(closed listener) to return")
		}

		assertResourcesClosed(t, srv)
		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("nil listener", func(t *testing.T) {
		dataDir := t.TempDir()
		cfg := config.Config{
			Port:         "4533",
			MusicDir:     t.TempDir(),
			DataDir:      dataDir,
			ScanInterval: 0,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err != nil {
			t.Fatalf("newServer() unexpected error: %v", err)
		}

		if err := srv.serve(context.Background(), nil); err == nil {
			t.Fatalf("serve(nil) error = nil, want non-nil")
		}

		assertResourcesClosed(t, srv)
		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})
}

func TestServeShutdownTimeoutForcesClose(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{
		Port:         "4533",
		MusicDir:     t.TempDir(),
		DataDir:      dataDir,
		ScanInterval: 0,
	}

	srv, err := newServer(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newServer() unexpected error: %v", err)
	}
	srv.shutdownTimeout = 100 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	var startOnce sync.Once

	inner := srv.httpSrv.Handler
	srv.httpSrv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-stream" {
			defer close(handlerDone)
			startOnce.Do(func() { close(started) })
			<-release
			return
		}
		inner.ServeHTTP(w, r)
	})

	// LIFO defer order closes release first, then waits for any started handler
	// goroutine to return before the test exits.
	defer func() {
		select {
		case <-started:
			<-handlerDone
		default:
		}
	}()
	defer close(release)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = srv.closeResources()
		t.Fatalf("net.Listen() unexpected error: %v", err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.serve(runCtx, ln)
	}()

	var (
		waitOnce     sync.Once
		waitServeErr error
		waitTimedOut bool
	)
	stopAndWait := func(timeout time.Duration) (error, bool) {
		runCancel()
		waitOnce.Do(func() {
			select {
			case waitServeErr = <-serveErrCh:
			case <-time.After(timeout):
				waitTimedOut = true
			}
		})
		return waitServeErr, waitTimedOut
	}
	t.Cleanup(func() {
		_, timedOut := stopAndWait(5 * time.Second)
		if timedOut {
			t.Errorf("cleanup: timed out waiting for server.serve to exit")
		}
	})

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	reqErrCh := make(chan error, 1)
	go func() {
		resp, reqErr := client.Get("http://" + ln.Addr().String() + "/slow-stream")
		if reqErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		reqErrCh <- reqErr
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for /slow-stream request to enter handler")
	}

	serveErr, timedOut := stopAndWait(2 * time.Second)
	if timedOut {
		t.Fatalf("timed out waiting for serve() to force-close and drain resources")
	}
	if !errors.Is(serveErr, context.DeadlineExceeded) {
		t.Fatalf("serve() error = %v, want errors.Is(err, context.DeadlineExceeded)", serveErr)
	}

	// Verify httpSrv.Close() terminated the active client socket before release
	// is closed.
	select {
	case reqErr := <-reqErrCh:
		if reqErr == nil {
			t.Fatalf("GET /slow-stream succeeded unexpectedly; want connection close error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for forced HTTP close to sever in-flight client request")
	}

	assertResourcesClosed(t, srv)
	assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) unexpected error: %v", path, err)
	}
	return data
}

func doHTTPRequest(
	t *testing.T,
	client *http.Client,
	method string,
	rawURL string,
	body io.Reader,
	headers map[string]string,
) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatalf("http.NewRequest(%s, %q) unexpected error: %v", method, rawURL, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s unexpected error: %v", method, rawURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s read body error: %v", method, rawURL, err)
	}
	return resp, respBody
}

func getJSONResponse(t *testing.T, client *http.Client, rawURL string) (subsonic.Response, []byte) {
	t.Helper()
	resp, body := doHTTPRequest(t, client, http.MethodGet, rawURL, nil, nil)
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET %s status = %d, want %d (body=%s)", rawURL, got, want, string(body))
	}
	if got, want := resp.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("GET %s Content-Type = %q, want %q", rawURL, got, want)
	}
	return decodeJSONResponse(t, body), body
}

func decodeJSONResponse(t *testing.T, body []byte) subsonic.Response {
	t.Helper()
	var env jsonSubsonicEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; body = %s", err, string(body))
	}
	return env.SubsonicResponse
}

func decodeXMLResponse(t *testing.T, body []byte) subsonic.Response {
	t.Helper()
	var env subsonic.Response
	if err := xml.Unmarshal(body, &env); err != nil {
		t.Fatalf("xml.Unmarshal() error = %v; body = %s", err, string(body))
	}
	return env
}

func assertXMLResponseEnvelope(t *testing.T, env subsonic.Response, wantStatus string) {
	t.Helper()
	if got, want := env.Xmlns, "http://subsonic.org/restapi"; got != want {
		t.Errorf("xml xmlns = %q, want %q", got, want)
	}
	assertCommonEnvelope(t, env, wantStatus)
}

func assertJSONResponseEnvelope(t *testing.T, env subsonic.Response, wantStatus string) {
	t.Helper()
	assertCommonEnvelope(t, env, wantStatus)
}

func assertCommonEnvelope(t *testing.T, env subsonic.Response, wantStatus string) {
	t.Helper()
	if got := env.Status; got != wantStatus {
		t.Errorf("status = %q, want %q", got, wantStatus)
	}
	if got, want := env.Version, "1.16.1"; got != want {
		t.Errorf("version = %q, want %q", got, want)
	}
	if got, want := env.Type, "headunit"; got != want {
		t.Errorf("type = %q, want %q", got, want)
	}
	if got, want := env.ServerVersion, "0.1.0"; got != want {
		t.Errorf("serverVersion = %q, want %q", got, want)
	}
	if !env.OpenSubsonic {
		t.Errorf("openSubsonic = false, want true")
	}
}

func pollScanStatus(
	t *testing.T,
	client *http.Client,
	baseURL string,
	timeout time.Duration,
	ready func(subsonic.ScanStatus) bool,
) subsonic.ScanStatus {
	t.Helper()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(e2ePollInterval)
	defer ticker.Stop()

	var last subsonic.ScanStatus
	for {
		env, _ := getJSONResponse(t, client, baseURL+"/rest/getScanStatus?f=json")
		assertJSONResponseEnvelope(t, env, "ok")
		if env.ScanStatus != nil {
			last = *env.ScanStatus
			if ready(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for scanStatus condition; last = %+v", timeout, last)
		}
		<-ticker.C
	}
}

func findSongByPath(songs []subsonic.Child, relPath string) (subsonic.Child, bool) {
	for _, s := range songs {
		if s.Path == relPath {
			return s, true
		}
	}
	return subsonic.Child{}, false
}

func assertResourcesClosed(t *testing.T, srv *server) {
	t.Helper()

	if _, err := srv.store.TrackCount(context.Background()); err == nil {
		t.Errorf("store.TrackCount() succeeded after shutdown, want closed-DB error")
	}

	req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
	rec := httptest.NewRecorder()
	err := srv.streamer.Stream(rec, req, &store.Track{ID: "tr_probe", RelPath: "probe.wav"}, stream.StreamOptions{})
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("streamer.Stream() after close error = %v, want errors.Is(err, os.ErrClosed)", err)
	}
}

func assertDBReopensCleanly(t *testing.T, dbPath string, wantCount int64) {
	t.Helper()

	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open(%q) unexpected error: %v", dbPath, err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("st.Close() unexpected error: %v", closeErr)
		}
	}()

	count, err := st.TrackCount(context.Background())
	if err != nil {
		t.Fatalf("TrackCount() unexpected error: %v", err)
	}
	if count != wantCount {
		t.Errorf("TrackCount() = %d, want %d", count, wantCount)
	}
}
