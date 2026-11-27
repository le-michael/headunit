package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
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

	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("AUTH_SECRET_KEY", "")

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

	const (
		alicePass = "alice-super-secret-1!"
		bobPass   = "bob-standard-secret-2@"
	)
	var (
		bobKeyID    string
		bobAPIKey   string
		mp3TrackID  string
		flacTrackID string
		wavTrackID  string
	)

	aliceAuth := func(salt string) url.Values {
		return saltedMD5Params("alice", alicePass, salt)
	}
	bobAuth := func(salt string) url.Values {
		return saltedMD5Params("bob", bobPass, salt)
	}
	bobKeyAuth := func() url.Values {
		return apiKeyParams(bobAPIKey)
	}

	if !t.Run("ZeroUserFailClosedAndPublicEndpoints", func(t *testing.T) {
		// /healthz returns 200 ok\n
		hzResp, hzBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/healthz", nil, nil)
		if got, want := hzResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /healthz status = %d, want %d", got, want)
		}
		if got, want := string(hzBody), "ok\n"; got != want {
			t.Errorf("GET /healthz body = %q, want %q", got, want)
		}

		// Zero-user /rest/ping fails closed with Subsonic error 40 (XML & JSON).
		pingXMLResp, pingXMLBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/ping", nil, nil)
		if got, want := pingXMLResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/ping status = %d, want %d", got, want)
		}
		pingXMLEnv := decodeXMLResponse(t, pingXMLBody)
		assertXMLResponseEnvelope(t, pingXMLEnv, "failed")
		if pingXMLEnv.Error == nil || pingXMLEnv.Error.Code != 40 {
			t.Errorf("GET /rest/ping XML error = %+v, want code 40", pingXMLEnv.Error)
		}

		pingJSONEnv, _ := getJSONResponse(t, client, baseURL+"/rest/ping?f=json")
		assertJSONResponseEnvelope(t, pingJSONEnv, "failed")
		if pingJSONEnv.Error == nil || pingJSONEnv.Error.Code != 40 {
			t.Errorf("GET /rest/ping JSON error = %+v, want code 40", pingJSONEnv.Error)
		}

		// Unauthenticated /rest/getOpenSubsonicExtensions advertises apiKeyAuthentication v1 (XML & JSON).
		extXMLResp, extXMLBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/rest/getOpenSubsonicExtensions", nil, nil)
		if got, want := extXMLResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/getOpenSubsonicExtensions XML status = %d, want %d", got, want)
		}
		extXMLEnv := decodeXMLResponse(t, extXMLBody)
		assertXMLResponseEnvelope(t, extXMLEnv, "ok")
		assertAPIKeyExtensionV1(t, extXMLEnv)

		extJSONEnv, extJSONRaw := getJSONResponse(t, client, baseURL+"/rest/getOpenSubsonicExtensions.view?f=json")
		assertJSONResponseEnvelope(t, extJSONEnv, "ok")
		assertAPIKeyExtensionV1(t, extJSONEnv)
		if !strings.Contains(string(extJSONRaw), `"openSubsonicExtensions":[{"name":"apiKeyAuthentication","versions":[1]}]`) {
			t.Errorf("getOpenSubsonicExtensions raw JSON = %s, want apiKeyAuthentication v1", string(extJSONRaw))
		}

		// Verify <dataDir>/auth.key exists as a regular file with mode 0600.
		keyPath := filepath.Join(dataDir, "auth.key")
		info, err := os.Lstat(keyPath)
		if err != nil {
			t.Fatalf("os.Lstat(%q) unexpected error: %v", keyPath, err)
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%q mode = %v, want regular file", keyPath, info.Mode())
		}
		if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
			t.Errorf("%q perm = %o, want %o", keyPath, got, want)
		}
	}) {
		return
	}

	if !t.Run("LiveCLIProvisioning", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if err := runCLI(
			context.Background(),
			[]string{"user", "create", "--username", "alice", "--admin", "--password-stdin"},
			strings.NewReader(alicePass+"\n"),
			&stdout,
			&stderr,
		); err != nil {
			t.Fatalf("runCLI(user create alice) unexpected error: %v (stderr=%s)", err, stderr.String())
		}

		stdout.Reset()
		stderr.Reset()
		t.Setenv("HEADUNIT_TEST_BOB_PW", bobPass)
		if err := runCLI(
			context.Background(),
			[]string{"user", "create", "--username", "bob", "--password-env", "HEADUNIT_TEST_BOB_PW"},
			nil,
			&stdout,
			&stderr,
		); err != nil {
			t.Fatalf("runCLI(user create bob) unexpected error: %v (stderr=%s)", err, stderr.String())
		}

		stdout.Reset()
		stderr.Reset()
		if err := runCLI(
			context.Background(),
			[]string{"apikey", "create", "--username", "bob", "--name", "main-e2e-key"},
			nil,
			&stdout,
			&stderr,
		); err != nil {
			t.Fatalf("runCLI(apikey create bob) unexpected error: %v (stderr=%s)", err, stderr.String())
		}
		createdKey := parseAPIKeyCreateOutput(t, stdout.String())
		bobKeyID = createdKey["id"]
		bobAPIKey = createdKey["api_key"]
		if bobKeyID == "" || !strings.HasPrefix(bobAPIKey, "hu_") {
			t.Fatalf("created key = %+v, want non-empty id and hu_ api_key", createdKey)
		}
	}) {
		return
	}

	if !t.Run("StartupScanStatus", func(t *testing.T) {
		status := pollScanStatus(t, client, baseURL, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 3 && s.LastScan != ""
		})
		if _, err := time.Parse(time.RFC3339, status.LastScan); err != nil {
			t.Errorf("scanStatus.lastScan = %q, want valid RFC3339 timestamp: %v", status.LastScan, err)
		}
	}) {
		return
	}

	t.Run("SystemDualRoutesAndMethods", func(t *testing.T) {
		// GET /rest/ping (default XML, salted MD5)
		resp, body := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/ping", aliceAuth("ping-xml-1")), nil, nil)
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

		// GET /rest/ping.view?f=json (apiKey)
		jsonEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/ping.view?f=json", bobKeyAuth()))
		assertJSONResponseEnvelope(t, jsonEnv, "ok")

		// HEAD /rest/ping
		headResp, headBody := doHTTPRequest(t, client, http.MethodHead, withQueryParams(baseURL+"/rest/ping", aliceAuth("ping-head-1")), nil, nil)
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

		// POST /rest/getUser.view with form body f=json and salted MD5 credentials for alice
		formValues := aliceAuth("post-user-1")
		formValues.Set("f", "json")
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
		if !userEnv.User.AdminRole {
			t.Errorf("user.adminRole = false, want true")
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
		licEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getLicense?f=json", bobKeyAuth()))
		assertJSONResponseEnvelope(t, licEnv, "ok")
		if licEnv.License == nil || !licEnv.License.Valid {
			t.Errorf("getLicense license = %+v, want valid=true", licEnv.License)
		}
	})

	t.Run("TokenInfoAndGetUserRoles", func(t *testing.T) {
		// tokenInfo with apiKey succeeds and returns owning username.
		tokEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/tokenInfo?f=json", bobKeyAuth()))
		assertJSONResponseEnvelope(t, tokEnv, "ok")
		if tokEnv.TokenInfo == nil || tokEnv.TokenInfo.Username != "bob" {
			t.Errorf("tokenInfo = %+v, want username=bob", tokEnv.TokenInfo)
		}

		// tokenInfo with salted MD5 token returns Subsonic error 42.
		tokFailEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/tokenInfo?f=json", aliceAuth("tok-fail-1")))
		assertJSONResponseEnvelope(t, tokFailEnv, "failed")
		if tokFailEnv.Error == nil || tokFailEnv.Error.Code != 42 {
			t.Errorf("tokenInfo(token auth) error = %+v, want code 42", tokFailEnv.Error)
		}

		// Standard user bob: self (omitted & case-insensitive) succeeds with adminRole=false;
		// querying another existing or unknown user returns 50.
		bobSelfEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getUser?f=json", bobKeyAuth()))
		assertJSONResponseEnvelope(t, bobSelfEnv, "ok")
		if bobSelfEnv.User == nil || bobSelfEnv.User.Username != "bob" || bobSelfEnv.User.AdminRole || !bobSelfEnv.User.StreamRole {
			t.Errorf("bob getUser(self) = %+v, want username=bob adminRole=false streamRole=true", bobSelfEnv.User)
		}

		bobCaseParams := bobAuth("bob-case-1")
		bobCaseParams.Set("username", "BOB")
		bobCaseEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getUser?f=json", bobCaseParams))
		assertJSONResponseEnvelope(t, bobCaseEnv, "ok")
		if bobCaseEnv.User == nil || bobCaseEnv.User.Username != "bob" || bobCaseEnv.User.AdminRole {
			t.Errorf("bob getUser(BOB) = %+v, want canonical username=bob adminRole=false", bobCaseEnv.User)
		}

		for _, target := range []string{"alice", "ghost"} {
			params := bobAuth("bob-other-" + target)
			params.Set("username", target)
			env, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getUser?f=json", params))
			assertJSONResponseEnvelope(t, env, "failed")
			if env.Error == nil || env.Error.Code != 50 {
				t.Errorf("non-admin bob getUser(%q) error = %+v, want code 50", target, env.Error)
			}
		}

		// Admin alice: querying bob succeeds; querying unknown user returns 70.
		aliceBobParams := aliceAuth("alice-get-bob")
		aliceBobParams.Set("username", "bob")
		aliceBobEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getUser?f=json", aliceBobParams))
		assertJSONResponseEnvelope(t, aliceBobEnv, "ok")
		if aliceBobEnv.User == nil || aliceBobEnv.User.Username != "bob" || aliceBobEnv.User.AdminRole {
			t.Errorf("admin getUser(bob) = %+v, want username=bob adminRole=false", aliceBobEnv.User)
		}

		aliceGhostParams := aliceAuth("alice-get-ghost")
		aliceGhostParams.Set("username", "ghost")
		aliceGhostEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getUser?f=json", aliceGhostParams))
		assertJSONResponseEnvelope(t, aliceGhostEnv, "failed")
		if aliceGhostEnv.Error == nil || aliceGhostEnv.Error.Code != 70 {
			t.Errorf("admin getUser(ghost) error = %+v, want code 70", aliceGhostEnv.Error)
		}
	})

	t.Run("NegativeAuthMatrix", func(t *testing.T) {
		tests := []struct {
			name     string
			params   url.Values
			wantCode int
		}{
			{
				name:     "deprecated plaintext password",
				params:   url.Values{"u": {"alice"}, "p": {"secret"}},
				wantCode: 42,
			},
			{
				name:     "conflicting apiKey and u",
				params:   url.Values{"apiKey": {bobAPIKey}, "u": {"alice"}},
				wantCode: 43,
			},
			{
				name:     "invalid apiKey",
				params:   url.Values{"apiKey": {"hu_0000000000000000000000000000000000000000000000000000000000000000"}},
				wantCode: 44,
			},
			{
				name:     "wrong password token",
				params:   saltedMD5Params("alice", "wrong-password", "bad-salt-1"),
				wantCode: 40,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				env, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/ping?f=json", tc.params))
				assertJSONResponseEnvelope(t, env, "failed")
				if env.Error == nil || env.Error.Code != tc.wantCode {
					t.Errorf("%s: error = %+v, want code %d", tc.name, env.Error, tc.wantCode)
				}
			})
		}
	})

	if !t.Run("LibraryBrowseAndSearch3", func(t *testing.T) {
		// getMusicFolders (salted MD5)
		foldersEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getMusicFolders?f=json", aliceAuth("browse-1")))
		assertJSONResponseEnvelope(t, foldersEnv, "ok")
		if foldersEnv.MusicFolders == nil {
			t.Fatalf("getMusicFolders missing musicFolders payload")
		}
		wantFolders := []subsonic.MusicFolder{{ID: 1, Name: "Music"}}
		if !slices.Equal(foldersEnv.MusicFolders.MusicFolder, wantFolders) {
			t.Errorf("musicFolders = %+v, want %+v", foldersEnv.MusicFolders.MusicFolder, wantFolders)
		}

		// getArtists: verifies ignoredArticles ("The Alpha Band" -> "A") and fallback ("Unknown Artist" -> "U").
		artistsEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getArtists?f=json", aliceAuth("browse-2")))
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

		// getArtist (apiKey)
		artistEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getArtist?f=json&id="+url.QueryEscape(alphaArtistID), bobKeyAuth()))
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
		albumEnv, albumRaw := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getAlbum?f=json&id="+url.QueryEscape(firstLight.ID), aliceAuth("browse-3")))
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

		// getAlbumList2?type=alphabeticalByName (apiKey)
		albumListEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getAlbumList2?f=json&type=alphabeticalByName", bobKeyAuth()))
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
		songJSONEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getSong?f=json&id="+url.QueryEscape(mp3TrackID), bobKeyAuth()))
		assertJSONResponseEnvelope(t, songJSONEnv, "ok")
		if songJSONEnv.Song == nil || songJSONEnv.Song.ID != mp3TrackID || songJSONEnv.Song.Title != "Opening" {
			t.Errorf("getSong JSON song = %+v, want id=%q title='Opening'", songJSONEnv.Song, mp3TrackID)
		}

		// getSong (XML)
		songXMLResp, songXMLBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/getSong?id="+url.QueryEscape(mp3TrackID), aliceAuth("browse-4")), nil, nil)
		if got, want := songXMLResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/getSong XML status = %d, want %d", got, want)
		}
		songXMLEnv := decodeXMLResponse(t, songXMLBody)
		assertXMLResponseEnvelope(t, songXMLEnv, "ok")
		if songXMLEnv.Song == nil || songXMLEnv.Song.ID != mp3TrackID || songXMLEnv.Song.Title != "Opening" || songXMLEnv.Song.ContentType != "audio/mpeg" {
			t.Errorf("getSong XML song = %+v, want id=%q title='Opening' contentType='audio/mpeg'", songXMLEnv.Song, mp3TrackID)
		}

		// search3 bulk sync (literal "", apiKey)
		searchEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500", bobKeyAuth()))
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
		// 200 full stream MP3 (salted MD5)
		mp3Resp, mp3Body := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(mp3TrackID), aliceAuth("stream-1")), nil, nil)
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

		// 206 partial content FLAC bytes=0-1 -> "fL" (apiKey)
		rangeHeaders := map[string]string{"Range": "bytes=0-1"}
		flacResp, flacBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(flacTrackID), bobKeyAuth()), nil, rangeHeaders)
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

		// 416 unsatisfiable range WAV (apiKey)
		badRangeHeaders := map[string]string{"Range": "bytes=9999999-"}
		wav416Resp, _ := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(wavTrackID), bobKeyAuth()), nil, badRangeHeaders)
		if got, want := wav416Resp.StatusCode, http.StatusRequestedRangeNotSatisfiable; got != want {
			t.Errorf("GET /rest/stream WAV invalid range status = %d, want %d", got, want)
		}
		want416Range := fmt.Sprintf("bytes */%d", len(wavBytes))
		if got := wav416Resp.Header.Get("Content-Range"); got != want416Range {
			t.Errorf("WAV 416 Content-Range = %q, want %q", got, want416Range)
		}

		// HEAD /rest/stream WAV (salted MD5)
		wavHeadResp, wavHeadBody := doHTTPRequest(t, client, http.MethodHead, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(wavTrackID), aliceAuth("stream-head")), nil, nil)
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
		errResp, errBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id=tr_unknown&f=json", bobKeyAuth()), nil, nil)
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
		plEnv, plRaw := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getPlaylists?f=json", aliceAuth("stub-1")))
		assertJSONResponseEnvelope(t, plEnv, "ok")
		if !strings.Contains(string(plRaw), `"playlist":[]`) {
			t.Errorf("getPlaylists raw JSON = %s, want non-null empty array", string(plRaw))
		}

		genreEnv, genreRaw := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getGenres?f=json", bobKeyAuth()))
		assertJSONResponseEnvelope(t, genreEnv, "ok")
		if !strings.Contains(string(genreRaw), `"genre":[]`) {
			t.Errorf("getGenres raw JSON = %s, want non-null empty array", string(genreRaw))
		}

		starEnv, starRaw := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getStarred2?f=json", aliceAuth("stub-2")))
		assertJSONResponseEnvelope(t, starEnv, "ok")
		for _, wantKey := range []string{`"artist":[]`, `"album":[]`, `"song":[]`} {
			if !strings.Contains(string(starRaw), wantKey) {
				t.Errorf("getStarred2 raw JSON = %s, want substring %q", string(starRaw), wantKey)
			}
		}

		coverEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getCoverArt?f=json&id=al_missing", bobKeyAuth()))
		assertJSONResponseEnvelope(t, coverEnv, "failed")
		if coverEnv.Error == nil || coverEnv.Error.Code != 70 {
			t.Errorf("getCoverArt error = %+v, want code 70", coverEnv.Error)
		}

		scrobbleEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/scrobble?f=json&id="+url.QueryEscape(mp3TrackID), aliceAuth("stub-3")))
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

		pollScanStatus(t, client, baseURL, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 4
		})

		searchEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/search3?f=json&query=Live+Drop", bobKeyAuth()))
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

		streamResp, streamBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(liveSong.ID), bobKeyAuth()), nil, nil)
		if got, want := streamResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream live track status = %d, want %d", got, want)
		}
		if !bytes.Equal(streamBody, liveBytes) {
			t.Errorf("live track stream body mismatch: got %d bytes, want %d bytes", len(streamBody), len(liveBytes))
		}

		if err := os.Remove(livePath); err != nil {
			t.Fatalf("os.Remove(%q) unexpected error: %v", livePath, err)
		}

		pollScanStatus(t, client, baseURL, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 3
		})

		deletedEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(liveSong.ID)+"&f=json", aliceAuth("watch-del")))
		assertJSONResponseEnvelope(t, deletedEnv, "failed")
		if deletedEnv.Error == nil || deletedEnv.Error.Code != 70 {
			t.Errorf("stream deleted track error = %+v, want code 70", deletedEnv.Error)
		}
	})

	t.Run("LiveRevocationAndDisable", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if err := runCLI(
			context.Background(),
			[]string{"apikey", "revoke", "--id", bobKeyID},
			nil,
			&stdout,
			&stderr,
		); err != nil {
			t.Fatalf("runCLI(apikey revoke) unexpected error: %v (stderr=%s)", err, stderr.String())
		}

		revokedEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/ping?f=json", bobKeyAuth()))
		assertJSONResponseEnvelope(t, revokedEnv, "failed")
		if revokedEnv.Error == nil || revokedEnv.Error.Code != 44 {
			t.Errorf("ping after apikey revoke error = %+v, want code 44", revokedEnv.Error)
		}

		// Bob's token auth remains active until bob is disabled.
		bobStillOK, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/ping?f=json", bobAuth("bob-before-disable")))
		assertJSONResponseEnvelope(t, bobStillOK, "ok")

		stdout.Reset()
		stderr.Reset()
		if err := runCLI(
			context.Background(),
			[]string{"user", "disable", "--username", "bob"},
			nil,
			&stdout,
			&stderr,
		); err != nil {
			t.Fatalf("runCLI(user disable bob) unexpected error: %v (stderr=%s)", err, stderr.String())
		}

		disabledEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/ping?f=json", bobAuth("bob-after-disable")))
		assertJSONResponseEnvelope(t, disabledEnv, "failed")
		if disabledEnv.Error == nil || disabledEnv.Error.Code != 40 {
			t.Errorf("ping after user disable error = %+v, want code 40", disabledEnv.Error)
		}

		// Admin alice remains authenticated.
		aliceStillOK, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/ping?f=json", aliceAuth("alice-still-ok")))
		assertJSONResponseEnvelope(t, aliceStillOK, "ok")
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

	t.Run("uploads dir is regular file", func(t *testing.T) {
		musicDir := t.TempDir()
		dataDir := t.TempDir()
		uploadsFile := filepath.Join(dataDir, uploadsDirname)
		testutil.WriteBytes(t, uploadsFile, []byte("not-a-directory"))

		cfg := config.Config{
			Port:         "4533",
			MusicDir:     musicDir,
			DataDir:      dataDir,
			ScanInterval: 0,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err == nil {
			_ = srv.closeResources()
			t.Fatalf("newServer() error = nil, want error when uploads dir is a regular file")
		}
		if srv != nil {
			t.Fatalf("newServer() server = %+v, want nil", srv)
		}

		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("negative upload max file bytes", func(t *testing.T) {
		dataDir := t.TempDir()
		cfg := config.Config{
			Port:               "4533",
			MusicDir:           t.TempDir(),
			DataDir:            dataDir,
			ScanInterval:       0,
			UploadsEnabled:     true,
			UploadMaxFileBytes: -1,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err == nil {
			_ = srv.closeResources()
			t.Fatalf("newServer() error = nil, want error for negative UploadMaxFileBytes")
		}
		if srv != nil {
			t.Fatalf("newServer() server = %+v, want nil", srv)
		}

		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("corrupt auth key", func(t *testing.T) {
		dataDir := t.TempDir()
		secretMaterial := "not-valid-hex-secret-material"
		keyPath := filepath.Join(dataDir, "auth.key")
		if err := os.WriteFile(keyPath, []byte(secretMaterial+"\n"), 0o600); err != nil {
			t.Fatalf("os.WriteFile(%q) unexpected error: %v", keyPath, err)
		}

		cfg := config.Config{
			Port:         "4533",
			MusicDir:     t.TempDir(),
			DataDir:      dataDir,
			ScanInterval: 0,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err == nil {
			_ = srv.closeResources()
			t.Fatalf("newServer() error = nil, want error for corrupt auth.key")
		}
		if srv != nil {
			t.Fatalf("newServer() server = %+v, want nil", srv)
		}
		if strings.Contains(err.Error(), secretMaterial) {
			t.Errorf("newServer() error leaked key file contents: %v", err)
		}

		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("symlink auth key", func(t *testing.T) {
		dataDir := t.TempDir()
		target := filepath.Join(t.TempDir(), "real.key")
		testutil.WriteBytes(t, target, []byte(strings.Repeat("ab", 32)+"\n"))
		keyPath := filepath.Join(dataDir, "auth.key")
		if err := os.Symlink(target, keyPath); err != nil {
			t.Fatalf("os.Symlink() unexpected error: %v", err)
		}

		cfg := config.Config{
			Port:         "4533",
			MusicDir:     t.TempDir(),
			DataDir:      dataDir,
			ScanInterval: 0,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err == nil {
			_ = srv.closeResources()
			t.Fatalf("newServer() error = nil, want error for symlink auth.key")
		}
		if srv != nil {
			t.Fatalf("newServer() server = %+v, want nil", srv)
		}

		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("invalid AUTH_SECRET_KEY", func(t *testing.T) {
		dataDir := t.TempDir()
		secretMaterial := "deadbeef-invalid-secret-material"
		cfg := config.Config{
			Port:          "4533",
			MusicDir:      t.TempDir(),
			DataDir:       dataDir,
			ScanInterval:  0,
			AuthSecretKey: secretMaterial,
		}

		srv, err := newServer(context.Background(), cfg, nil)
		if err == nil {
			_ = srv.closeResources()
			t.Fatalf("newServer() error = nil, want error for invalid AUTH_SECRET_KEY")
		}
		if srv != nil {
			t.Fatalf("newServer() server = %+v, want nil", srv)
		}
		if strings.Contains(err.Error(), secretMaterial) {
			t.Errorf("newServer() error leaked AUTH_SECRET_KEY contents: %v", err)
		}

		assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 0)
	})

	t.Run("invalid env config", func(t *testing.T) {
		tests := []struct {
			name               string
			port               string
			scanInterval       string
			authSecretKey      string
			uploadsEnabled     string
			uploadMaxFileBytes string
		}{
			{name: "PORT=0", port: "0", scanInterval: "0"},
			{name: "SCAN_INTERVAL=-1s", port: "4533", scanInterval: "-1s"},
			{name: "AUTH_SECRET_KEY=invalid", port: "4533", scanInterval: "0", authSecretKey: "bad-secret-value"},
			{name: "UPLOADS_ENABLED=maybe", port: "4533", scanInterval: "0", uploadsEnabled: "maybe"},
			{name: "UPLOAD_MAX_FILE_BYTES=0", port: "4533", scanInterval: "0", uploadMaxFileBytes: "0"},
			{name: "UPLOAD_MAX_FILE_BYTES=-1", port: "4533", scanInterval: "0", uploadMaxFileBytes: "-1"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PORT", tc.port)
				t.Setenv("MUSIC_DIR", t.TempDir())
				t.Setenv("DATA_DIR", t.TempDir())
				t.Setenv("SCAN_INTERVAL", tc.scanInterval)
				t.Setenv("AUTH_SECRET_KEY", tc.authSecretKey)
				t.Setenv("UPLOADS_ENABLED", tc.uploadsEnabled)
				t.Setenv("UPLOAD_MAX_FILE_BYTES", tc.uploadMaxFileBytes)

				err := run(context.Background(), nil)
				if err == nil {
					t.Fatalf("run() error = nil, want validation error for %s", tc.name)
				}
				if tc.authSecretKey != "" && strings.Contains(err.Error(), tc.authSecretKey) {
					t.Errorf("run() error leaked AUTH_SECRET_KEY: %v", err)
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
		t.Setenv("AUTH_SECRET_KEY", "")
		t.Setenv("UPLOADS_ENABLED", "")
		t.Setenv("UPLOAD_MAX_FILE_BYTES", "")

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

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMultiRootServer_UploadsDisabledStillScansWatchesAndStreams(t *testing.T) {
	t.Run("CreatesUploadsDir0750WhenAbsent", func(t *testing.T) {
		freshMusicDir := t.TempDir()
		freshDataDir := t.TempDir()
		wantUploadsDir := filepath.Join(freshDataDir, uploadsDirname)

		if _, err := os.Lstat(wantUploadsDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("os.Lstat(%q) err = %v, want ErrNotExist before newServer", wantUploadsDir, err)
		}

		srv, err := newServer(context.Background(), config.Config{
			Port:           "4533",
			MusicDir:       freshMusicDir,
			DataDir:        freshDataDir,
			UploadsEnabled: false,
		}, nil)
		if err != nil {
			t.Fatalf("newServer() unexpected error: %v", err)
		}
		if got := srv.cfg.UploadsDir; got != wantUploadsDir {
			t.Errorf("srv.cfg.UploadsDir = %q, want %q", got, wantUploadsDir)
		}

		info, err := os.Lstat(wantUploadsDir)
		if err != nil {
			_ = srv.closeResources()
			t.Fatalf("os.Lstat(%q) unexpected error: %v", wantUploadsDir, err)
		}
		if !info.IsDir() {
			t.Errorf("%q IsDir = false, want true", wantUploadsDir)
		}
		if got, want := info.Mode().Perm(), os.FileMode(0o750); got != want {
			t.Errorf("%q perm = %o, want %o", wantUploadsDir, got, want)
		}

		if err := srv.closeResources(); err != nil {
			t.Fatalf("srv.closeResources() unexpected error: %v", err)
		}
		assertResourcesClosed(t, srv)
	})

	musicDir := t.TempDir()
	dataDir := t.TempDir()
	uploadsDir := filepath.Join(dataDir, uploadsDirname)

	const (
		sharedRel       = "Shared Artist/Shared Album/01 - Twin.flac"
		uploadedOnlyRel = "Inbox/02 - Uploaded Only.mp3"
	)

	musicTwinPath := filepath.Join(musicDir, filepath.FromSlash(sharedRel))
	testutil.WriteMinimalFLAC(t, musicTwinPath)
	testutil.WriteTags(t, musicTwinPath, map[string][]string{
		taglib.Title:       {"Twin From Music"},
		taglib.AlbumArtist: {"Shared Artist"},
		taglib.Artist:      {"Shared Artist"},
		taglib.Album:       {"Shared Album"},
		taglib.TrackNumber: {"1"},
	})
	musicTwinBytes := mustReadFile(t, musicTwinPath)

	uploadsTwinPath := filepath.Join(uploadsDir, filepath.FromSlash(sharedRel))
	testutil.WriteMinimalFLAC(t, uploadsTwinPath)
	testutil.WriteTags(t, uploadsTwinPath, map[string][]string{
		taglib.Title:       {"Twin From Uploads"},
		taglib.AlbumArtist: {"Shared Artist"},
		taglib.Artist:      {"Shared Artist"},
		taglib.Album:       {"Shared Album"},
		taglib.TrackNumber: {"1"},
	})
	uploadsTwinBytes := mustReadFile(t, uploadsTwinPath)
	if bytes.Equal(musicTwinBytes, uploadsTwinBytes) {
		t.Fatalf("music and uploads twin fixtures unexpectedly identical")
	}

	uploadedOnlyPath := filepath.Join(uploadsDir, filepath.FromSlash(uploadedOnlyRel))
	testutil.WriteMinimalMP3(t, uploadedOnlyPath)
	testutil.WriteTags(t, uploadedOnlyPath, map[string][]string{
		taglib.Title:       {"Uploaded Only"},
		taglib.AlbumArtist: {"Upload Artist"},
		taglib.Artist:      {"Upload Artist"},
		taglib.Album:       {"Upload Album"},
		taglib.TrackNumber: {"2"},
	})
	uploadedOnlyBytes := mustReadFile(t, uploadedOnlyPath)

	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("AUTH_SECRET_KEY", "")

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	cfg := config.Config{
		Port:           "4533",
		MusicDir:       musicDir,
		DataDir:        dataDir,
		ScanInterval:   0,
		UploadsEnabled: false,
	}

	srv, err := newServer(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("newServer() unexpected error: %v", err)
	}
	if got := srv.cfg.UploadsDir; got != uploadsDir {
		_ = srv.closeResources()
		t.Fatalf("srv.cfg.UploadsDir = %q, want %q", got, uploadsDir)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = srv.closeResources()
		t.Fatalf("net.Listen() unexpected error: %v", err)
	}
	baseURL := "http://" + ln.Addr().String()

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

	const adminPass = "multiroot-admin-secret-1!"
	var stdout, stderr bytes.Buffer
	if err := runCLI(
		context.Background(),
		[]string{"user", "create", "--username", "alice", "--admin", "--password-stdin"},
		strings.NewReader(adminPass+"\n"),
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("runCLI(user create alice) unexpected error: %v (stderr=%s)", err, stderr.String())
	}

	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	aliceAuth := func(salt string) url.Values {
		return saltedMD5Params("alice", adminPass, salt)
	}

	pollScanStatus(t, client, baseURL, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 3 && s.LastScan != ""
	})

	if logs := logBuf.String(); !strings.Contains(logs, "uploads_dir="+uploadsDir) {
		t.Errorf("startup logs = %q, want substring %q", logs, "uploads_dir="+uploadsDir)
	}

	searchEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500", aliceAuth("mr-search")))
	assertJSONResponseEnvelope(t, searchEnv, "ok")
	if searchEnv.SearchResult3 == nil || len(searchEnv.SearchResult3.Song) != 3 {
		t.Fatalf("search3 result = %+v, want 3 songs", searchEnv.SearchResult3)
	}

	musicTwin, ok := findSongByPath(searchEnv.SearchResult3.Song, sharedRel)
	if !ok {
		t.Fatalf("search3 missing music twin path %q: %+v", sharedRel, searchEnv.SearchResult3.Song)
	}
	if musicTwin.Title != "Twin From Music" {
		t.Errorf("musicTwin.Title = %q, want %q", musicTwin.Title, "Twin From Music")
	}

	uploadsTwin, ok := findSongByPath(searchEnv.SearchResult3.Song, "uploads/"+sharedRel)
	if !ok {
		t.Fatalf("search3 missing uploads twin path %q: %+v", "uploads/"+sharedRel, searchEnv.SearchResult3.Song)
	}
	if uploadsTwin.Title != "Twin From Uploads" {
		t.Errorf("uploadsTwin.Title = %q, want %q", uploadsTwin.Title, "Twin From Uploads")
	}
	if musicTwin.ID == uploadsTwin.ID {
		t.Fatalf("musicTwin.ID == uploadsTwin.ID (%q), want distinct IDs", musicTwin.ID)
	}

	uploadedOnly, ok := findSongByPath(searchEnv.SearchResult3.Song, "uploads/"+uploadedOnlyRel)
	if !ok {
		t.Fatalf("search3 missing uploaded-only path %q: %+v", "uploads/"+uploadedOnlyRel, searchEnv.SearchResult3.Song)
	}

	// Verify getSong emits disambiguated Path in both JSON and XML.
	getSongJSON, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getSong?f=json&id="+url.QueryEscape(uploadsTwin.ID), aliceAuth("mr-song-json")))
	assertJSONResponseEnvelope(t, getSongJSON, "ok")
	if getSongJSON.Song == nil || getSongJSON.Song.Path != "uploads/"+sharedRel {
		t.Errorf("getSong JSON = %+v, want Path=%q", getSongJSON.Song, "uploads/"+sharedRel)
	}

	getSongXMLResp, getSongXMLBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/getSong?id="+url.QueryEscape(uploadsTwin.ID), aliceAuth("mr-song-xml")), nil, nil)
	if got, want := getSongXMLResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /rest/getSong XML status = %d, want %d", got, want)
	}
	getSongXML := decodeXMLResponse(t, getSongXMLBody)
	assertXMLResponseEnvelope(t, getSongXML, "ok")
	if getSongXML.Song == nil || getSongXML.Song.Path != "uploads/"+sharedRel {
		t.Errorf("getSong XML = %+v, want Path=%q", getSongXML.Song, "uploads/"+sharedRel)
	}

	// Verify direct streaming (200/206/416/HEAD) across both roots.
	musicResp, musicBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(musicTwin.ID), aliceAuth("mr-stream-music")), nil, nil)
	if got, want := musicResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("stream music twin status = %d, want %d", got, want)
	}
	if !bytes.Equal(musicBody, musicTwinBytes) {
		t.Errorf("music twin stream body mismatch: got %d bytes, want %d bytes", len(musicBody), len(musicTwinBytes))
	}

	uploadsResp, uploadsBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(uploadsTwin.ID), aliceAuth("mr-stream-uploads")), nil, nil)
	if got, want := uploadsResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("stream uploads twin status = %d, want %d", got, want)
	}
	if got, want := uploadsResp.Header.Get("Content-Type"), "audio/flac"; got != want {
		t.Errorf("uploads twin Content-Type = %q, want %q", got, want)
	}
	if got, want := uploadsResp.Header.Get("Accept-Ranges"), "bytes"; got != want {
		t.Errorf("uploads twin Accept-Ranges = %q, want %q", got, want)
	}
	if got := uploadsResp.Header.Get("ETag"); got == "" || got == musicResp.Header.Get("ETag") {
		t.Errorf("uploads twin ETag = %q (music=%q), want distinct non-empty ETag", got, musicResp.Header.Get("ETag"))
	}
	if !bytes.Equal(uploadsBody, uploadsTwinBytes) {
		t.Errorf("uploads twin stream body mismatch: got %d bytes, want %d bytes", len(uploadsBody), len(uploadsTwinBytes))
	}

	rangeResp, rangeBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(uploadsTwin.ID), aliceAuth("mr-stream-206")), nil, map[string]string{"Range": "bytes=0-3"})
	if got, want := rangeResp.StatusCode, http.StatusPartialContent; got != want {
		t.Fatalf("stream uploads 206 status = %d, want %d", got, want)
	}
	wantRange := fmt.Sprintf("bytes 0-3/%d", len(uploadsTwinBytes))
	if got := rangeResp.Header.Get("Content-Range"); got != wantRange {
		t.Errorf("uploads 206 Content-Range = %q, want %q", got, wantRange)
	}
	if got, want := string(rangeBody), "fLaC"; got != want {
		t.Errorf("uploads 206 body = %q, want %q", got, want)
	}

	oobResp, _ := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(uploadsTwin.ID), aliceAuth("mr-stream-416")), nil, map[string]string{"Range": "bytes=9999999-"})
	if got, want := oobResp.StatusCode, http.StatusRequestedRangeNotSatisfiable; got != want {
		t.Errorf("stream uploads 416 status = %d, want %d", got, want)
	}
	want416 := fmt.Sprintf("bytes */%d", len(uploadsTwinBytes))
	if got := oobResp.Header.Get("Content-Range"); got != want416 {
		t.Errorf("uploads 416 Content-Range = %q, want %q", got, want416)
	}

	headResp, headBody := doHTTPRequest(t, client, http.MethodHead, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(uploadedOnly.ID), aliceAuth("mr-stream-head")), nil, nil)
	if got, want := headResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("HEAD uploads status = %d, want %d", got, want)
	}
	if got, want := headResp.Header.Get("Content-Type"), "audio/mpeg"; got != want {
		t.Errorf("HEAD uploads Content-Type = %q, want %q", got, want)
	}
	if got, want := headResp.Header.Get("Accept-Ranges"), "bytes"; got != want {
		t.Errorf("HEAD uploads Accept-Ranges = %q, want %q", got, want)
	}
	if got, want := headResp.Header.Get("Content-Length"), strconv.Itoa(len(uploadedOnlyBytes)); got != want {
		t.Errorf("HEAD uploads Content-Length = %q, want %q", got, want)
	}
	if got := len(headBody); got != 0 {
		t.Errorf("HEAD uploads body len = %d, want 0", got)
	}

	// Verify live uploads watcher picks up new files and isolates deletions from music root.
	stagedPath := filepath.Join(t.TempDir(), "03 - Live Upload.flac")
	testutil.WriteMinimalFLAC(t, stagedPath)
	testutil.WriteTags(t, stagedPath, map[string][]string{
		taglib.Title:       {"Live Upload"},
		taglib.AlbumArtist: {"Live Uploader"},
		taglib.Artist:      {"Live Uploader"},
		taglib.Album:       {"Live Uploads"},
		taglib.TrackNumber: {"1"},
	})
	liveUploadBytes := mustReadFile(t, stagedPath)

	liveUploadPath := filepath.Join(uploadsDir, "live-drop", "03 - Live Upload.flac")
	testutil.WriteBytes(t, liveUploadPath, liveUploadBytes)

	pollScanStatus(t, client, baseURL, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 4
	})

	liveSearch, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/search3?f=json&query=Live+Upload", aliceAuth("mr-live-search")))
	assertJSONResponseEnvelope(t, liveSearch, "ok")
	if liveSearch.SearchResult3 == nil {
		t.Fatalf("search3 Live Upload missing searchResult3 payload")
	}
	liveSong, ok := findSongByPath(liveSearch.SearchResult3.Song, "uploads/live-drop/03 - Live Upload.flac")
	if !ok {
		t.Fatalf("search3 Live Upload songs = %+v, want uploads/live-drop/03 - Live Upload.flac", liveSearch.SearchResult3.Song)
	}
	liveResp, liveBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(liveSong.ID), aliceAuth("mr-live-stream")), nil, nil)
	if got, want := liveResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("stream live upload status = %d, want %d", got, want)
	}
	if !bytes.Equal(liveBody, liveUploadBytes) {
		t.Errorf("live upload stream body mismatch: got %d bytes, want %d bytes", len(liveBody), len(liveUploadBytes))
	}

	if err := os.Remove(uploadsTwinPath); err != nil {
		t.Fatalf("os.Remove(%q) unexpected error: %v", uploadsTwinPath, err)
	}
	pollScanStatus(t, client, baseURL, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 3
	})

	deletedEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(uploadsTwin.ID)+"&f=json", aliceAuth("mr-del-uploads")))
	assertJSONResponseEnvelope(t, deletedEnv, "failed")
	if deletedEnv.Error == nil || deletedEnv.Error.Code != 70 {
		t.Errorf("stream deleted uploads twin error = %+v, want code 70", deletedEnv.Error)
	}

	// Deleting uploads/<sharedRel> must leave music/<sharedRel> intact and streamable.
	musicStillResp, musicStillBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/stream?id="+url.QueryEscape(musicTwin.ID), aliceAuth("mr-music-intact")), nil, nil)
	if got, want := musicStillResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("stream music twin after uploads delete status = %d, want %d", got, want)
	}
	if !bytes.Equal(musicStillBody, musicTwinBytes) {
		t.Errorf("music twin stream body after uploads delete mismatch: got %d bytes, want %d bytes", len(musicStillBody), len(musicTwinBytes))
	}

	serveErr, timedOut := stopAndWait(10 * time.Second)
	if timedOut {
		t.Fatalf("timed out waiting for graceful shutdown")
	}
	if serveErr != nil {
		t.Fatalf("server.serve() error = %v, want nil", serveErr)
	}

	assertResourcesClosed(t, srv)
	assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 3)
}

type uploadSuccessPayload struct {
	Status string `json:"status"`
	Track  struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		Title  string `json:"title"`
		Artist string `json:"artist"`
		Album  string `json:"album"`
	} `json:"track"`
}

type uploadErrorPayload struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func startTestServer(t *testing.T, cfg config.Config) (*server, string, func(time.Duration) (error, bool)) {
	t.Helper()
	srv, err := newServer(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newServer() unexpected error: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = srv.closeResources()
		t.Fatalf("net.Listen() unexpected error: %v", err)
	}
	baseURL := "http://" + ln.Addr().String()

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
	return srv, baseURL, stopAndWait
}

func TestFullServerUploadAPI(t *testing.T) {
	musicDir := t.TempDir()
	dataDir := t.TempDir()
	uploadsDir := filepath.Join(dataDir, uploadsDirname)

	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("AUTH_SECRET_KEY", "")

	const (
		alicePass = "upload-alice-secret-1!"
		bobPass   = "upload-bob-secret-2@"
	)

	srv1, baseURL, stopSrv1 := startTestServer(t, config.Config{
		Port:           "4533",
		MusicDir:       musicDir,
		DataDir:        dataDir,
		ScanInterval:   0,
		UploadsEnabled: true,
		// UploadMaxFileBytes intentionally left 0 to verify default 256 MiB wiring.
	})

	var stdout, stderr bytes.Buffer
	if err := runCLI(
		context.Background(),
		[]string{"user", "create", "--username", "alice", "--admin", "--password-stdin"},
		strings.NewReader(alicePass+"\n"),
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("runCLI(user create alice) error: %v (stderr=%s)", err, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runCLI(
		context.Background(),
		[]string{"user", "create", "--username", "bob", "--password-stdin"},
		strings.NewReader(bobPass+"\n"),
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("runCLI(user create bob) error: %v (stderr=%s)", err, stderr.String())
	}

	createKey := func(username, name string) string {
		stdout.Reset()
		stderr.Reset()
		if err := runCLI(
			context.Background(),
			[]string{"apikey", "create", "--username", username, "--name", name},
			nil,
			&stdout,
			&stderr,
		); err != nil {
			t.Fatalf("runCLI(apikey create %s) error: %v (stderr=%s)", username, err, stderr.String())
		}
		parsed := parseAPIKeyCreateOutput(t, stdout.String())
		if parsed["api_key"] == "" {
			t.Fatalf("apikey create %s returned empty key", username)
		}
		return parsed["api_key"]
	}
	aliceAPIKey := createKey("alice", "admin-upload")
	bobAPIKey := createKey("bob", "nonadmin-upload")

	aliceAuth := func(salt string) url.Values {
		return saltedMD5Params("alice", alicePass, salt)
	}
	bobAuth := func(salt string) url.Values {
		return saltedMD5Params("bob", bobPass, salt)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	pollScanStatus(t, client, baseURL, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 0 && s.LastScan != ""
	})

	// Verify getUser.uploadRole when UPLOADS_ENABLED=true (XML & JSON).
	assertGetUserUploadRole(t, client, baseURL, apiKeyParams(aliceAPIKey), "alice", true, true)
	assertGetUserUploadRole(t, client, baseURL, apiKeyParams(bobAPIKey), "bob", false, false)

	// Verify GET /api/v1/upload returns 405 JSON + Allow: POST (not 404 from /).
	getResp, getBody := doHTTPRequest(t, client, http.MethodGet, baseURL+"/api/v1/upload", nil, nil)
	if got, want := getResp.StatusCode, http.StatusMethodNotAllowed; got != want {
		t.Fatalf("GET /api/v1/upload status = %d, want %d (body=%s)", got, want, string(getBody))
	}
	if got, want := getResp.Header.Get("Allow"), http.MethodPost; got != want {
		t.Errorf("GET /api/v1/upload Allow = %q, want %q", got, want)
	}
	getErr := decodeUploadError(t, getResp, getBody)
	if got, want := getErr.Error.Code, "method_not_allowed"; got != want {
		t.Errorf("GET /api/v1/upload error.code = %q, want %q", got, want)
	}

	// Build two > 1 MiB valid tagged FLAC fixtures and one > 1 MiB second track.
	const largePayloadSize = (1 << 20) + 16*1024
	v1Bytes := buildLargeFLAC(t, largePayloadSize, 0x00, map[string][]string{
		taglib.Title:       {"Large Upload v1"},
		taglib.AlbumArtist: {"Upload Band"},
		taglib.Artist:      {"Upload Band"},
		taglib.Album:       {"Studio Tape"},
		taglib.TrackNumber: {"1"},
	})
	v2Bytes := buildLargeFLAC(t, largePayloadSize+4096, 0xAA, map[string][]string{
		taglib.Title:       {"Large Upload v2"},
		taglib.AlbumArtist: {"Upload Band"},
		taglib.Artist:      {"Upload Band"},
		taglib.Album:       {"Studio Tape"},
		taglib.TrackNumber: {"1"},
	})
	if len(v1Bytes) <= 1<<20 || len(v2Bytes) <= 1<<20 {
		t.Fatalf("expected > 1 MiB FLAC fixtures, got v1=%d v2=%d", len(v1Bytes), len(v2Bytes))
	}
	if bytes.Equal(v1Bytes[:1024], v2Bytes[:1024]) {
		t.Fatalf("expected v1 and v2 first 1024 bytes to differ for Range overwrite verification")
	}

	track2Bytes := buildLargeFLAC(t, largePayloadSize, 0x55, map[string][]string{
		taglib.Title:       {"Large Upload Two"},
		taglib.AlbumArtist: {"Upload Band"},
		taglib.Artist:      {"Upload Band"},
		taglib.Album:       {"Studio Tape"},
		taglib.TrackNumber: {"2"},
	})

	// Unauthenticated POST -> 401 JSON.
	unauthResp, unauthBody := doUploadRequest(t, client, baseURL+"/api/v1/upload?path="+url.QueryEscape("Studio/Alpha/01 - Large.flac"), "file", "01.flac", v1Bytes)
	if got, want := unauthResp.StatusCode, http.StatusUnauthorized; got != want {
		t.Fatalf("unauthenticated upload status = %d, want %d (body=%s)", got, want, string(unauthBody))
	}
	unauthErr := decodeUploadError(t, unauthResp, unauthBody)
	if got, want := unauthErr.Error.Code, "unauthorized"; got != want {
		t.Errorf("unauthenticated error.code = %q, want %q", got, want)
	}

	// Non-admin POST (apiKey & u+t+s) -> 403 JSON.
	for name, params := range map[string]url.Values{
		"bob-apikey": apiKeyParams(bobAPIKey),
		"bob-token":  bobAuth("bob-upload-403"),
	} {
		params.Set("path", "Studio/Alpha/01 - Large.flac")
		resp, body := doUploadRequest(t, client, withQueryParams(baseURL+"/api/v1/upload", params), "file", "01.flac", v1Bytes)
		if got, want := resp.StatusCode, http.StatusForbidden; got != want {
			t.Fatalf("%s upload status = %d, want %d (body=%s)", name, got, want, string(body))
		}
		errPayload := decodeUploadError(t, resp, body)
		if got, want := errPayload.Error.Code, "forbidden"; got != want {
			t.Errorf("%s error.code = %q, want %q", name, got, want)
		}
	}

	// Corrupt audio upload by admin -> 422 JSON with no staged temp leftovers.
	corruptParams := apiKeyParams(aliceAPIKey)
	corruptParams.Set("path", "Studio/Alpha/corrupt.flac")
	corruptResp, corruptBody := doUploadRequest(t, client, withQueryParams(baseURL+"/api/v1/upload", corruptParams), "file", "corrupt.flac", []byte("not valid audio payload"))
	if got, want := corruptResp.StatusCode, http.StatusUnprocessableEntity; got != want {
		t.Fatalf("corrupt upload status = %d, want %d (body=%s)", got, want, string(corruptBody))
	}
	corruptErr := decodeUploadError(t, corruptResp, corruptBody)
	if got, want := corruptErr.Error.Code, "invalid_audio"; got != want {
		t.Errorf("corrupt upload error.code = %q, want %q", got, want)
	}
	assertZeroUploadTempFiles(t, uploadsDir)

	// Admin > 1 MiB upload via apiKey -> 201 Created + immediate Subsonic visibility + stream 200/206.
	const relTrack1 = "Studio/Alpha/01 - Large.flac"
	up1Params := apiKeyParams(aliceAPIKey)
	up1Params.Set("path", relTrack1)
	up1Resp, up1Body := doUploadRequest(t, client, withQueryParams(baseURL+"/api/v1/upload", up1Params), "file", "01.flac", v1Bytes)
	if got, want := up1Resp.StatusCode, http.StatusCreated; got != want {
		t.Fatalf("admin apiKey upload status = %d, want %d (body=%s)", got, want, string(up1Body))
	}
	up1 := decodeUploadSuccess(t, up1Resp, up1Body)
	if up1.Status != "created" {
		t.Errorf("up1.Status = %q, want %q", up1.Status, "created")
	}
	if up1.Track.ID == "" {
		t.Fatalf("up1.Track.ID is empty")
	}
	if up1.Track.Path != "uploads/"+relTrack1 || up1.Track.Size != int64(len(v1Bytes)) {
		t.Errorf("up1.Track = %+v, want path=%q size=%d", up1.Track, "uploads/"+relTrack1, len(v1Bytes))
	}
	if up1.Track.Title != "Large Upload v1" || up1.Track.Artist != "Upload Band" || up1.Track.Album != "Studio Tape" {
		t.Errorf("up1.Track metadata = %+v, want Large Upload v1 / Upload Band / Studio Tape", up1.Track)
	}

	// Immediate getScanStatus, search3, getAlbum, and stream (200 + Range 206).
	scanEnv1, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getScanStatus?f=json", apiKeyParams(aliceAPIKey)))
	assertJSONResponseEnvelope(t, scanEnv1, "ok")
	if scanEnv1.ScanStatus == nil || scanEnv1.ScanStatus.Scanning || scanEnv1.ScanStatus.Count != 1 {
		t.Fatalf("immediate getScanStatus after upload 1 = %+v, want scanning=false count=1", scanEnv1.ScanStatus)
	}

	search1, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/search3?f=json&query=Large", apiKeyParams(bobAPIKey)))
	assertJSONResponseEnvelope(t, search1, "ok")
	if search1.SearchResult3 == nil {
		t.Fatalf("search3 missing searchResult3")
	}
	song1, ok := findSongByPath(search1.SearchResult3.Song, "uploads/"+relTrack1)
	if !ok || song1.ID != up1.Track.ID {
		t.Fatalf("search3 song1 = %+v (found=%v), want id=%q path=%q", song1, ok, up1.Track.ID, "uploads/"+relTrack1)
	}

	album1, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getAlbum?f=json&id="+url.QueryEscape(song1.AlbumID), apiKeyParams(bobAPIKey)))
	assertJSONResponseEnvelope(t, album1, "ok")
	if album1.Album == nil || album1.Album.SongCount != 1 || len(album1.Album.Song) != 1 || album1.Album.Song[0].ID != up1.Track.ID {
		t.Fatalf("getAlbum after upload 1 = %+v, want 1 song with id=%q", album1.Album, up1.Track.ID)
	}

	assertStreamBytesAndRange206(t, client, baseURL, aliceAuth("stream-v1"), up1.Track.ID, v1Bytes)

	// Admin > 1 MiB upload via u+t+s -> 201 Created + immediate getAlbum/getScanStatus/stream.
	const relTrack2 = "Studio/Alpha/02 - Second Large.flac"
	up2Params := aliceAuth("upload-track-2")
	up2Params.Set("path", relTrack2)
	up2Resp, up2Body := doUploadRequest(t, client, withQueryParams(baseURL+"/api/v1/upload", up2Params), "file", "02.flac", track2Bytes)
	if got, want := up2Resp.StatusCode, http.StatusCreated; got != want {
		t.Fatalf("admin u+t+s upload status = %d, want %d (body=%s)", got, want, string(up2Body))
	}
	up2 := decodeUploadSuccess(t, up2Resp, up2Body)
	if up2.Status != "created" || up2.Track.Path != "uploads/"+relTrack2 {
		t.Errorf("up2 = %+v, want status=created path=%q", up2, "uploads/"+relTrack2)
	}

	scanEnv2, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getScanStatus?f=json", aliceAuth("scan-2")))
	assertJSONResponseEnvelope(t, scanEnv2, "ok")
	if scanEnv2.ScanStatus == nil || scanEnv2.ScanStatus.Count != 2 {
		t.Fatalf("immediate getScanStatus after upload 2 = %+v, want count=2", scanEnv2.ScanStatus)
	}
	album2, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getAlbum?f=json&id="+url.QueryEscape(song1.AlbumID), aliceAuth("album-2")))
	assertJSONResponseEnvelope(t, album2, "ok")
	if album2.Album == nil || album2.Album.SongCount != 2 || len(album2.Album.Song) != 2 {
		t.Fatalf("getAlbum after upload 2 = %+v, want 2 songs", album2.Album)
	}
	assertStreamBytesAndRange206(t, client, baseURL, apiKeyParams(aliceAPIKey), up2.Track.ID, track2Bytes)

	// overwrite=false on existing path -> 409 Conflict; original bytes preserved.
	conflictParams := apiKeyParams(aliceAPIKey)
	conflictParams.Set("path", relTrack1)
	conflictParams.Set("overwrite", "false")
	conflictResp, conflictBody := doUploadRequest(t, client, withQueryParams(baseURL+"/api/v1/upload", conflictParams), "file", "01.flac", v2Bytes)
	if got, want := conflictResp.StatusCode, http.StatusConflict; got != want {
		t.Fatalf("overwrite=false upload status = %d, want %d (body=%s)", got, want, string(conflictBody))
	}
	conflictErr := decodeUploadError(t, conflictResp, conflictBody)
	if got, want := conflictErr.Error.Code, "conflict"; got != want {
		t.Errorf("overwrite=false error.code = %q, want %q", got, want)
	}
	assertZeroUploadTempFiles(t, uploadsDir)
	assertStreamBytesAndRange206(t, client, baseURL, apiKeyParams(bobAPIKey), up1.Track.ID, v1Bytes)

	// overwrite=true on existing path -> 200 OK, preserves track ID, updates bytes & metadata.
	overwriteParams := aliceAuth("upload-overwrite")
	overwriteParams.Set("path", relTrack1)
	overwriteParams.Set("overwrite", "true")
	owResp, owBody := doUploadRequest(t, client, withQueryParams(baseURL+"/api/v1/upload", overwriteParams), "file", "01.flac", v2Bytes)
	if got, want := owResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("overwrite=true upload status = %d, want %d (body=%s)", got, want, string(owBody))
	}
	ow := decodeUploadSuccess(t, owResp, owBody)
	if ow.Status != "updated" {
		t.Errorf("ow.Status = %q, want %q", ow.Status, "updated")
	}
	if ow.Track.ID != up1.Track.ID {
		t.Errorf("ow.Track.ID = %q, want preserved ID %q", ow.Track.ID, up1.Track.ID)
	}
	if ow.Track.Size != int64(len(v2Bytes)) || ow.Track.Title != "Large Upload v2" {
		t.Errorf("ow.Track = %+v, want size=%d title='Large Upload v2'", ow.Track, len(v2Bytes))
	}
	assertZeroUploadTempFiles(t, uploadsDir)

	scanEnvAfterOW, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getScanStatus?f=json", apiKeyParams(aliceAPIKey)))
	assertJSONResponseEnvelope(t, scanEnvAfterOW, "ok")
	if scanEnvAfterOW.ScanStatus == nil || scanEnvAfterOW.ScanStatus.Count != 2 {
		t.Fatalf("getScanStatus after overwrite = %+v, want count=2", scanEnvAfterOW.ScanStatus)
	}
	searchAfterOW, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/search3?f=json&query=v2", apiKeyParams(aliceAPIKey)))
	assertJSONResponseEnvelope(t, searchAfterOW, "ok")
	if searchAfterOW.SearchResult3 == nil {
		t.Fatalf("search3 v2 missing searchResult3")
	}
	song1AfterOW, ok := findSongByPath(searchAfterOW.SearchResult3.Song, "uploads/"+relTrack1)
	if !ok || song1AfterOW.ID != up1.Track.ID || song1AfterOW.Title != "Large Upload v2" {
		t.Fatalf("search3 after overwrite = %+v (found=%v), want id=%q title='Large Upload v2'", song1AfterOW, ok, up1.Track.ID)
	}
	assertStreamBytesAndRange206(t, client, baseURL, aliceAuth("stream-v2"), up1.Track.ID, v2Bytes)

	// Shut down server 1 cleanly and assert uploadHandler + resources closed.
	serveErr1, timedOut1 := stopSrv1(10 * time.Second)
	if timedOut1 || serveErr1 != nil {
		t.Fatalf("server 1 shutdown: timedOut=%v err=%v", timedOut1, serveErr1)
	}
	assertResourcesClosed(t, srv1)
	assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 2)

	// Start server 2 with UPLOADS_ENABLED=false on same dataDir.
	srv2, baseURL2, stopSrv2 := startTestServer(t, config.Config{
		Port:           "4533",
		MusicDir:       musicDir,
		DataDir:        dataDir,
		ScanInterval:   0,
		UploadsEnabled: false,
	})

	pollScanStatus(t, client, baseURL2, aliceAuth, e2ePollTimeout, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 2 && s.LastScan != ""
	})

	// Admin getUser now reports uploadRole=false in XML & JSON.
	assertGetUserUploadRole(t, client, baseURL2, apiKeyParams(aliceAPIKey), "alice", true, false)

	// Unauthenticated POST still returns 401 when UPLOADS_ENABLED=false.
	disUnauthResp, disUnauthBody := doUploadRequest(t, client, baseURL2+"/api/v1/upload?path=Studio/Alpha/03.flac", "file", "03.flac", v1Bytes)
	if got, want := disUnauthResp.StatusCode, http.StatusUnauthorized; got != want {
		t.Fatalf("disabled-server unauthenticated upload status = %d, want %d (body=%s)", got, want, string(disUnauthBody))
	}

	// Admin POST returns 403 Forbidden when UPLOADS_ENABLED=false.
	disParams := apiKeyParams(aliceAPIKey)
	disParams.Set("path", "Studio/Alpha/03.flac")
	disResp, disBody := doUploadRequest(t, client, withQueryParams(baseURL2+"/api/v1/upload", disParams), "file", "03.flac", v1Bytes)
	if got, want := disResp.StatusCode, http.StatusForbidden; got != want {
		t.Fatalf("disabled-server admin upload status = %d, want %d (body=%s)", got, want, string(disBody))
	}
	disErr := decodeUploadError(t, disResp, disBody)
	if got, want := disErr.Error.Code, "forbidden"; got != want {
		t.Errorf("disabled-server error.code = %q, want %q", got, want)
	}

	// Existing uploaded track remains streamable (200 & Range 206).
	assertStreamBytesAndRange206(t, client, baseURL2, apiKeyParams(bobAPIKey), up1.Track.ID, v2Bytes)

	serveErr2, timedOut2 := stopSrv2(10 * time.Second)
	if timedOut2 || serveErr2 != nil {
		t.Fatalf("server 2 shutdown: timedOut=%v err=%v", timedOut2, serveErr2)
	}
	assertResourcesClosed(t, srv2)
	assertDBReopensCleanly(t, filepath.Join(dataDir, dbFilename), 2)
}

func assertGetUserUploadRole(
	t *testing.T,
	client *http.Client,
	baseURL string,
	params url.Values,
	wantUser string,
	wantAdmin, wantUpload bool,
) {
	t.Helper()

	jsonParams := cloneValues(params)
	jsonParams.Set("f", "json")
	jsonEnv, _ := getJSONResponse(t, client, withQueryParams(baseURL+"/rest/getUser", jsonParams))
	assertJSONResponseEnvelope(t, jsonEnv, "ok")
	if jsonEnv.User == nil {
		t.Fatalf("getUser JSON missing user payload")
	}
	if jsonEnv.User.Username != wantUser || jsonEnv.User.AdminRole != wantAdmin || jsonEnv.User.UploadRole != wantUpload {
		t.Errorf("getUser JSON user = %+v, want username=%q adminRole=%v uploadRole=%v", jsonEnv.User, wantUser, wantAdmin, wantUpload)
	}

	xmlResp, xmlBody := doHTTPRequest(t, client, http.MethodGet, withQueryParams(baseURL+"/rest/getUser", params), nil, nil)
	if got, want := xmlResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("getUser XML status = %d, want %d (body=%s)", got, want, string(xmlBody))
	}
	xmlEnv := decodeXMLResponse(t, xmlBody)
	assertXMLResponseEnvelope(t, xmlEnv, "ok")
	if xmlEnv.User == nil {
		t.Fatalf("getUser XML missing user payload")
	}
	if xmlEnv.User.Username != wantUser || xmlEnv.User.AdminRole != wantAdmin || xmlEnv.User.UploadRole != wantUpload {
		t.Errorf("getUser XML user = %+v, want username=%q adminRole=%v uploadRole=%v", xmlEnv.User, wantUser, wantAdmin, wantUpload)
	}
}

func cloneValues(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for k, vals := range in {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func doUploadRequest(
	t *testing.T,
	client *http.Client,
	rawURL string,
	fieldName string,
	filename string,
	fileBytes []byte,
) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(fieldName, filename)
	if err != nil {
		t.Fatalf("CreateFormFile(%q, %q) unexpected error: %v", fieldName, filename, err)
	}
	if _, err := part.Write(fileBytes); err != nil {
		t.Fatalf("part.Write() unexpected error: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close() unexpected error: %v", err)
	}
	return doHTTPRequest(t, client, http.MethodPost, rawURL, &body, map[string]string{
		"Content-Type": mw.FormDataContentType(),
	})
}

func decodeUploadSuccess(t *testing.T, resp *http.Response, body []byte) uploadSuccessPayload {
	t.Helper()
	if got, want := resp.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("upload Content-Type = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("upload X-Content-Type-Options = %q, want %q", got, want)
	}
	var out uploadSuccessPayload
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("json.Unmarshal upload success error = %v; body = %s", err, string(body))
	}
	return out
}

func decodeUploadError(t *testing.T, resp *http.Response, body []byte) uploadErrorPayload {
	t.Helper()
	if got, want := resp.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("upload error Content-Type = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("upload error X-Content-Type-Options = %q, want %q", got, want)
	}
	var out uploadErrorPayload
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("json.Unmarshal upload error = %v; body = %s", err, string(body))
	}
	return out
}

func assertStreamBytesAndRange206(
	t *testing.T,
	client *http.Client,
	baseURL string,
	authParams url.Values,
	trackID string,
	wantBytes []byte,
) {
	t.Helper()
	streamParams := cloneValues(authParams)
	streamParams.Set("id", trackID)
	streamURL := withQueryParams(baseURL+"/rest/stream", streamParams)

	fullResp, fullBody := doHTTPRequest(t, client, http.MethodGet, streamURL, nil, nil)
	if got, want := fullResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /rest/stream status = %d, want %d (body=%s)", got, want, string(fullBody))
	}
	if !bytes.Equal(fullBody, wantBytes) {
		t.Errorf("GET /rest/stream body len = %d, want %d bytes (mismatch)", len(fullBody), len(wantBytes))
	}

	rangeResp, rangeBody := doHTTPRequest(t, client, http.MethodGet, streamURL, nil, map[string]string{
		"Range": "bytes=0-1023",
	})
	if got, want := rangeResp.StatusCode, http.StatusPartialContent; got != want {
		t.Fatalf("GET /rest/stream Range 0-1023 status = %d, want %d", got, want)
	}
	wantContentRange := fmt.Sprintf("bytes 0-1023/%d", len(wantBytes))
	if got := rangeResp.Header.Get("Content-Range"); got != wantContentRange {
		t.Errorf("GET /rest/stream Content-Range = %q, want %q", got, wantContentRange)
	}
	if !bytes.Equal(rangeBody, wantBytes[:1024]) {
		t.Errorf("GET /rest/stream Range 0-1023 body mismatch (got len=%d)", len(rangeBody))
	}
}

func buildLargeFLAC(t *testing.T, payloadSize int, payloadFill byte, tags map[string][]string) []byte {
	t.Helper()
	const (
		sampleRate    uint64 = 44100
		channels      uint64 = 2
		bitsPerSample uint64 = 16
		totalSamples  uint64 = 88200 // 2s at 44100 Hz
	)
	buf := make([]byte, 4+4+34+payloadSize)
	copy(buf[0:4], "fLaC")
	buf[4] = 0x80 // last metadata block, type 0 (STREAMINFO)
	buf[5] = 0x00
	buf[6] = 0x00
	buf[7] = 34
	binary.BigEndian.PutUint16(buf[8:10], 4096)
	binary.BigEndian.PutUint16(buf[10:12], 4096)
	packed := (sampleRate << 44) | ((channels - 1) << 41) | ((bitsPerSample - 1) << 36) | totalSamples
	binary.BigEndian.PutUint64(buf[18:26], packed)
	if payloadFill != 0 {
		for i := 42; i < len(buf); i++ {
			buf[i] = payloadFill
		}
	}

	tmpPath := filepath.Join(t.TempDir(), "fixture.flac")
	testutil.WriteBytes(t, tmpPath, buf)
	if len(tags) > 0 {
		testutil.WriteTags(t, tmpPath, tags)
	}
	return mustReadFile(t, tmpPath)
}

func assertZeroUploadTempFiles(t *testing.T, uploadsDir string) {
	t.Helper()
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error: %v", uploadsDir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") {
			t.Errorf("unexpected leftover staged temp file %q in %q", entry.Name(), uploadsDir)
		}
	}
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

func saltedMD5Params(username, password, salt string) url.Values {
	sum := md5.Sum([]byte(password + salt))
	return url.Values{
		"u": {username},
		"t": {hex.EncodeToString(sum[:])},
		"s": {salt},
	}
}

func apiKeyParams(rawKey string) url.Values {
	return url.Values{"apiKey": {rawKey}}
}

func withQueryParams(rawURL string, params url.Values) string {
	if len(params) == 0 {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for k, vals := range params {
		for _, v := range vals {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func assertAPIKeyExtensionV1(t *testing.T, env subsonic.Response) {
	t.Helper()
	if env.OpenSubsonicExtensions == nil {
		t.Fatalf("openSubsonicExtensions = nil, want apiKeyAuthentication v1")
	}
	exts := *env.OpenSubsonicExtensions
	if len(exts) != 1 || exts[0].Name != "apiKeyAuthentication" || !slices.Equal(exts[0].Versions, []int{1}) {
		t.Errorf("openSubsonicExtensions = %+v, want [{Name:apiKeyAuthentication Versions:[1]}]", exts)
	}
}

func pollScanStatus(
	t *testing.T,
	client *http.Client,
	baseURL string,
	authFn func(salt string) url.Values,
	timeout time.Duration,
	ready func(subsonic.ScanStatus) bool,
) subsonic.ScanStatus {
	t.Helper()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(e2ePollInterval)
	defer ticker.Stop()

	var last subsonic.ScanStatus
	for i := 0; ; i++ {
		rawURL := withQueryParams(baseURL+"/rest/getScanStatus?f=json", authFn(fmt.Sprintf("poll-salt-%d", i)))
		env, _ := getJSONResponse(t, client, rawURL)
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

	for _, src := range []string{store.SourceMusic, store.SourceUploads} {
		req := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
		rec := httptest.NewRecorder()
		err := srv.streamer.Stream(rec, req, &store.Track{ID: "tr_probe", Source: src, RelPath: "probe.wav"}, stream.StreamOptions{})
		if !errors.Is(err, os.ErrClosed) {
			t.Errorf("streamer.Stream(source=%q) after close error = %v, want errors.Is(err, os.ErrClosed)", src, err)
		}
	}

	uploadReq := httptest.NewRequest(http.MethodPost, "/api/v1/upload", nil)
	uploadRec := httptest.NewRecorder()
	srv.uploadHandler.ServeHTTP(uploadRec, uploadReq)
	if got, want := uploadRec.Code, http.StatusInternalServerError; got != want {
		t.Errorf("uploadHandler.ServeHTTP() after close status = %d, want %d (body=%s)", got, want, uploadRec.Body.String())
	}
	errPayload := decodeUploadError(t, uploadRec.Result(), uploadRec.Body.Bytes())
	if got, want := errPayload.Error.Code, "internal_error"; got != want {
		t.Errorf("uploadHandler.ServeHTTP() after close error.code = %q, want %q", got, want)
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
