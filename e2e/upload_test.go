//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"go.senan.xyz/taglib"

	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

type uploadResponsePayload struct {
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

func TestUpload(t *testing.T) {
	h := newRawHarness(t)

	adminUser := uniqueTestUsername(t) + "_admin"
	adminPass := "e2e-upload-admin-pass!"
	memberUser := uniqueTestUsername(t) + "_member"
	memberPass := "e2e-upload-member-pass!"

	h.createUser(t, adminUser, adminPass, true)
	t.Cleanup(func() {
		h.deleteUser(t, adminUser)
	})

	h.createUser(t, memberUser, memberPass, false)
	t.Cleanup(func() {
		h.deleteUser(t, memberUser)
	})

	_, adminAPIKey := h.createAPIKey(t, adminUser, "e2e-upload-admin")
	_, memberAPIKey := h.createAPIKey(t, memberUser, "e2e-upload-member")

	h.username = adminUser
	h.password = adminPass
	h.apiKey = adminAPIKey

	// Register /data/uploads child cleanup after user cleanups so Go's LIFO
	// t.Cleanup drains uploaded tracks while admin credentials remain valid,
	// leaving /data/uploads itself intact for TestWatcher.
	t.Cleanup(func() {
		if _, err := h.execComposeRaw(nil, nil, h.cfg.service, "sh", "-c", "find /data/uploads -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +"); err != nil {
			t.Errorf("cleanup /data/uploads children: %v", err)
			return
		}
		h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.Count == 0
		})
	})

	h.waitForCleanLibrary(t)

	flacBytes := buildTaggedFLACBytes(t, "01 - Uploaded Track.flac", map[string][]string{
		taglib.Title:       {"Uploaded Track"},
		taglib.AlbumArtist: {"Upload Artist"},
		taglib.Artist:      {"Upload Artist"},
		taglib.Album:       {"Upload Session"},
		taglib.TrackNumber: {"1"},
		taglib.DiscNumber:  {"1"},
		taglib.Date:        {"2026"},
		taglib.Genre:       {"Electronic"},
	})
	replacementFLACBytes := buildTaggedFLACBytes(t, "01 - Uploaded Track.flac", map[string][]string{
		taglib.Title:       {"Updated Track"},
		taglib.AlbumArtist: {"Upload Artist"},
		taglib.Artist:      {"Upload Artist"},
		taglib.Album:       {"Upload Session"},
		taglib.TrackNumber: {"1"},
		taglib.DiscNumber:  {"1"},
		taglib.Date:        {"2026"},
		taglib.Genre:       {"Electronic"},
	})
	if bytes.Equal(flacBytes, replacementFLACBytes) {
		t.Fatalf("initial and replacement FLAC fixtures unexpectedly identical")
	}
	wavBytes := testutil.MinimalWAVBytes()

	t.Run("GetUserUploadRole", func(t *testing.T) {
		cases := []struct {
			name       string
			path       string
			params     url.Values
			wantUser   string
			wantAdmin  bool
			wantUpload bool
		}{
			{
				name:       "AdminSelfAPIKey",
				path:       "/rest/getUser",
				params:     apiKeyParams(adminAPIKey),
				wantUser:   adminUser,
				wantAdmin:  true,
				wantUpload: true,
			},
			{
				name:       "MemberSelfAPIKey",
				path:       "/rest/getUser",
				params:     apiKeyParams(memberAPIKey),
				wantUser:   memberUser,
				wantAdmin:  false,
				wantUpload: false,
			},
			{
				name:       "AdminQueriesMemberToken",
				path:       "/rest/getUser?username=" + url.QueryEscape(memberUser),
				params:     tokenParams(adminUser, adminPass, h.nextSalt()),
				wantUser:   memberUser,
				wantAdmin:  false,
				wantUpload: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				xmlEnv, _ := h.getXML(t, h.withParams(t, tc.path, tc.params))
				assertXMLResponseEnvelope(t, xmlEnv, "ok")
				assertUploadRoleUser(t, xmlEnv.User, tc.wantUser, tc.wantAdmin, tc.wantUpload)

				jsonPath := h.withParams(t, tc.path, url.Values{"f": {"json"}})
				jsonEnv, _ := h.getJSON(t, h.withParams(t, jsonPath, tc.params))
				assertJSONResponseEnvelope(t, jsonEnv, "ok")
				assertUploadRoleUser(t, jsonEnv.User, tc.wantUser, tc.wantAdmin, tc.wantUpload)
			})
		}
	})

	t.Run("AuthAndRoleRejection", func(t *testing.T) {
		unauthPath := "/api/v1/upload?path=" + url.QueryEscape("Unauth/01.flac")
		unauthResp, unauthBody := doUploadRequest(t, h, unauthPath, "file", "01.flac", flacBytes)
		assertUploadError(t, unauthResp, unauthBody, http.StatusUnauthorized, "unauthorized")

		memberCases := []struct {
			name   string
			params url.Values
		}{
			{name: "APIKey", params: apiKeyParams(memberAPIKey)},
			{name: "SaltedMD5", params: tokenParams(memberUser, memberPass, h.nextSalt())},
		}
		for _, tc := range memberCases {
			t.Run(tc.name, func(t *testing.T) {
				reqPath := h.withParams(t, "/api/v1/upload?path="+url.QueryEscape("Member/01.flac"), tc.params)
				resp, body := doUploadRequest(t, h, reqPath, "file", "01.flac", flacBytes)
				assertUploadError(t, resp, body, http.StatusForbidden, "forbidden")
			})
		}
	})

	t.Run("CorruptAudioRejectedWithoutRemnants", func(t *testing.T) {
		reqPath := h.withParams(t, "/api/v1/upload?path="+url.QueryEscape("bad/corrupt.flac"), apiKeyParams(adminAPIKey))
		resp, body := doUploadRequest(t, h, reqPath, "file", "corrupt.flac", []byte("not-a-valid-flac-stream"))
		assertUploadError(t, resp, body, http.StatusUnprocessableEntity, "invalid_audio")

		out := strings.TrimSpace(h.execCompose(t, nil, nil, h.cfg.service, "ls", "-A", "/data/uploads"))
		if out != "" {
			t.Errorf("/data/uploads = %q, want empty after rejected corrupt upload", out)
		}
	})

	const flacRelPath = "Upload Artist/Upload Session/01 - Uploaded Track.flac"
	const wavFilename = "02 - Uploaded Signal.wav"

	var flacTrackID, wavTrackID string

	if !t.Run("AdminUploadImmediateIndexingAndStream", func(t *testing.T) {
		flacUploadPath := h.withParams(t, "/api/v1/upload?path="+url.QueryEscape(flacRelPath), apiKeyParams(adminAPIKey))
		flacResp, flacRespBody := doUploadRequest(t, h, flacUploadPath, "file", "ignored.flac", flacBytes)
		flacPayload := decodeUploadSuccess(t, flacResp, flacRespBody, http.StatusCreated, "created")

		if got, want := flacPayload.Track.Path, "uploads/"+flacRelPath; got != want {
			t.Errorf("FLAC track.path = %q, want %q", got, want)
		}
		if got, want := flacPayload.Track.Size, int64(len(flacBytes)); got != want {
			t.Errorf("FLAC track.size = %d, want %d", got, want)
		}
		if got, want := flacPayload.Track.Title, "Uploaded Track"; got != want {
			t.Errorf("FLAC track.title = %q, want %q", got, want)
		}
		if got, want := flacPayload.Track.Artist, "Upload Artist"; got != want {
			t.Errorf("FLAC track.artist = %q, want %q", got, want)
		}
		if got, want := flacPayload.Track.Album, "Upload Session"; got != want {
			t.Errorf("FLAC track.album = %q, want %q", got, want)
		}
		if flacPayload.Track.ID == "" {
			t.Fatalf("FLAC track.id = empty, want non-empty")
		}
		flacTrackID = flacPayload.Track.ID

		// Upload WAV via salted MD5 auth without ?path= to exercise filename fallback.
		wavUploadPath := h.withParams(t, "/api/v1/upload", tokenParams(adminUser, adminPass, h.nextSalt()))
		wavResp, wavRespBody := doUploadRequest(t, h, wavUploadPath, "file", wavFilename, wavBytes)
		wavPayload := decodeUploadSuccess(t, wavResp, wavRespBody, http.StatusCreated, "created")

		if got, want := wavPayload.Track.Path, "uploads/"+wavFilename; got != want {
			t.Errorf("WAV track.path = %q, want %q", got, want)
		}
		if got, want := wavPayload.Track.Size, int64(len(wavBytes)); got != want {
			t.Errorf("WAV track.size = %d, want %d", got, want)
		}
		if wavPayload.Track.ID == "" {
			t.Fatalf("WAV track.id = empty, want non-empty")
		}
		wavTrackID = wavPayload.Track.ID

		wantOwner := h.cfg.puid + ":" + h.cfg.pgid
		for _, containerPath := range []string{
			"/data/uploads/" + flacRelPath,
			"/data/uploads/" + wavFilename,
		} {
			gotOwner := strings.TrimSpace(h.execCompose(t, nil, nil, h.cfg.service, "stat", "-c", "%u:%g", containerPath))
			if gotOwner != wantOwner {
				t.Errorf("%s owner = %q, want %q", containerPath, gotOwner, wantOwner)
			}
		}

		remnants := strings.TrimSpace(h.execCompose(t, nil, nil, h.cfg.service, "find", "/data/uploads", "-name", ".upload-*"))
		if remnants != "" {
			t.Errorf("unexpected .upload-* remnants in /data/uploads: %q", remnants)
		}

		if _, err := h.execComposeRaw(nil, nil, h.cfg.service, "sh", "-c", "touch /music/.e2e-upload-ro-probe 2>/dev/null"); err == nil {
			t.Errorf("/music is writable inside container, want read-only mount")
		}
		musicEntries, err := os.ReadDir(h.cfg.musicDir)
		if err != nil {
			t.Fatalf("os.ReadDir(%q): %v", h.cfg.musicDir, err)
		}
		if len(musicEntries) != 0 {
			t.Errorf("%q has %d entries after upload, want 0", h.cfg.musicDir, len(musicEntries))
		}

		// Synchronous ingestion means search3, getAlbum, and /rest/stream work immediately.
		searchEnv, _ := h.getJSON(t, h.withDefaultAuth(t, "/rest/search3?f=json&query=%22%22&artistCount=500&albumCount=500&songCount=500"))
		assertJSONResponseEnvelope(t, searchEnv, "ok")
		if searchEnv.SearchResult3 == nil {
			t.Fatalf("search3 SearchResult3 = nil, want non-nil")
		}

		flacSong, ok := findSongByPath(searchEnv.SearchResult3.Song, "uploads/"+flacRelPath)
		if !ok {
			t.Fatalf("search3 missing song %q in %+v", "uploads/"+flacRelPath, searchEnv.SearchResult3.Song)
		}
		if flacSong.ID != flacTrackID {
			t.Errorf("search3 FLAC song ID = %q, want %q", flacSong.ID, flacTrackID)
		}
		if _, ok := findSongByPath(searchEnv.SearchResult3.Song, "uploads/"+wavFilename); !ok {
			t.Fatalf("search3 missing song %q in %+v", "uploads/"+wavFilename, searchEnv.SearchResult3.Song)
		}

		albumEnv, _ := h.getJSON(t, h.withDefaultAuth(t, "/rest/getAlbum?f=json&id="+url.QueryEscape(flacSong.AlbumID)))
		assertJSONResponseEnvelope(t, albumEnv, "ok")
		if albumEnv.Album == nil {
			t.Fatalf("getAlbum Album = nil, want non-nil")
		}
		if _, ok := findSongByPath(albumEnv.Album.Song, "uploads/"+flacRelPath); !ok {
			t.Fatalf("getAlbum missing song %q in %+v", "uploads/"+flacRelPath, albumEnv.Album.Song)
		}

		streamResp, streamBody := h.doRequest(t, http.MethodGet, h.withDefaultAuth(t, "/rest/stream?id="+url.QueryEscape(flacTrackID)), nil, nil)
		if got, want := streamResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream uploaded FLAC status = %d, want %d (body=%s)", got, want, string(streamBody))
		}
		if got, want := streamResp.Header.Get("Content-Type"), "audio/flac"; got != want {
			t.Errorf("uploaded FLAC Content-Type = %q, want %q", got, want)
		}
		if !bytes.Equal(streamBody, flacBytes) {
			t.Errorf("uploaded FLAC stream len = %d, want %d exact bytes", len(streamBody), len(flacBytes))
		}

		rangeResp, rangeBody := h.doRequest(
			t,
			http.MethodGet,
			h.withDefaultAuth(t, "/rest/stream?id="+url.QueryEscape(wavTrackID)),
			nil,
			map[string]string{"Range": "bytes=0-3"},
		)
		if got, want := rangeResp.StatusCode, http.StatusPartialContent; got != want {
			t.Fatalf("GET /rest/stream uploaded WAV Range status = %d, want %d (body=%s)", got, want, string(rangeBody))
		}
		if got, want := rangeResp.Header.Get("Content-Range"), fmt.Sprintf("bytes 0-3/%d", len(wavBytes)); got != want {
			t.Errorf("uploaded WAV Content-Range = %q, want %q", got, want)
		}
		if got, want := string(rangeBody), "RIFF"; got != want {
			t.Errorf("uploaded WAV Range body = %q, want %q", got, want)
		}
	}) {
		return
	}

	t.Run("ConflictAndOverwritePreservesTrackID", func(t *testing.T) {
		conflictPath := h.withParams(t, "/api/v1/upload?path="+url.QueryEscape(flacRelPath)+"&overwrite=false", apiKeyParams(adminAPIKey))
		conflictResp, conflictBody := doUploadRequest(t, h, conflictPath, "file", "01.flac", replacementFLACBytes)
		assertUploadError(t, conflictResp, conflictBody, http.StatusConflict, "conflict")

		overwritePath := h.withParams(t, "/api/v1/upload?path="+url.QueryEscape(flacRelPath)+"&overwrite=true", tokenParams(adminUser, adminPass, h.nextSalt()))
		overwriteResp, overwriteBody := doUploadRequest(t, h, overwritePath, "file", "01.flac", replacementFLACBytes)
		updated := decodeUploadSuccess(t, overwriteResp, overwriteBody, http.StatusOK, "updated")

		if got, want := updated.Track.ID, flacTrackID; got != want {
			t.Errorf("overwrite track.id = %q, want preserved %q", got, want)
		}
		if got, want := updated.Track.Title, "Updated Track"; got != want {
			t.Errorf("overwrite track.title = %q, want %q", got, want)
		}
		if got, want := updated.Track.Size, int64(len(replacementFLACBytes)); got != want {
			t.Errorf("overwrite track.size = %d, want %d", got, want)
		}

		streamResp, streamBody := h.doRequest(t, http.MethodGet, h.withDefaultAuth(t, "/rest/stream?id="+url.QueryEscape(flacTrackID)), nil, nil)
		if got, want := streamResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/stream overwritten FLAC status = %d, want %d (body=%s)", got, want, string(streamBody))
		}
		if !bytes.Equal(streamBody, replacementFLACBytes) {
			t.Errorf("overwritten FLAC stream len = %d, want %d exact replacement bytes", len(streamBody), len(replacementFLACBytes))
		}

		rangeResp, rangeBody := h.doRequest(
			t,
			http.MethodGet,
			h.withDefaultAuth(t, "/rest/stream?id="+url.QueryEscape(flacTrackID)),
			nil,
			map[string]string{"Range": "bytes=0-3"},
		)
		if got, want := rangeResp.StatusCode, http.StatusPartialContent; got != want {
			t.Fatalf("GET /rest/stream overwritten FLAC Range status = %d, want %d (body=%s)", got, want, string(rangeBody))
		}
		if got, want := string(rangeBody), "fLaC"; got != want {
			t.Errorf("overwritten FLAC Range body = %q, want %q", got, want)
		}

		scanEnv, _ := h.getJSON(t, h.withDefaultAuth(t, "/rest/getScanStatus?f=json"))
		assertJSONResponseEnvelope(t, scanEnv, "ok")
		if scanEnv.ScanStatus == nil || scanEnv.ScanStatus.Count != 2 {
			t.Errorf("getScanStatus after overwrite = %+v, want Count=2", scanEnv.ScanStatus)
		}
	})
}

func assertUploadRoleUser(t *testing.T, u *subsonic.User, wantName string, wantAdmin, wantUpload bool) {
	t.Helper()

	if u == nil {
		t.Fatalf("user payload = nil, want non-nil")
	}
	if got := u.Username; got != wantName {
		t.Errorf("user.Username = %q, want %q", got, wantName)
	}
	if got := u.AdminRole; got != wantAdmin {
		t.Errorf("user.AdminRole = %t, want %t", got, wantAdmin)
	}
	if got := u.UploadRole; got != wantUpload {
		t.Errorf("user.UploadRole = %t, want %t", got, wantUpload)
	}
}

func doUploadRequest(t *testing.T, h *harness, rawPath, fieldName, fileName string, data []byte) (*http.Response, []byte) {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(fieldName, fileName)
	if err != nil {
		t.Fatalf("multipart.CreateFormFile(%q, %q): %v", fieldName, fileName, err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("part.Write(): %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart.Close(): %v", err)
	}

	return h.doRequest(t, http.MethodPost, rawPath, &body, map[string]string{
		"Content-Type": mw.FormDataContentType(),
	})
}

func decodeUploadSuccess(t *testing.T, resp *http.Response, body []byte, wantStatus int, wantStatusText string) uploadResponsePayload {
	t.Helper()

	if got := resp.StatusCode; got != wantStatus {
		t.Fatalf("upload status = %d, want %d (body=%s)", got, wantStatus, string(body))
	}
	assertUploadJSONHeaders(t, resp)

	var payload uploadResponsePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal(uploadResponse) error = %v; body = %s", err, string(body))
	}
	if got := payload.Status; got != wantStatusText {
		t.Errorf("upload status field = %q, want %q", got, wantStatusText)
	}
	return payload
}

func assertUploadError(t *testing.T, resp *http.Response, body []byte, wantStatus int, wantCode string) {
	t.Helper()

	if got := resp.StatusCode; got != wantStatus {
		t.Fatalf("upload error status = %d, want %d (body=%s)", got, wantStatus, string(body))
	}
	assertUploadJSONHeaders(t, resp)

	var payload uploadErrorPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal(uploadError) error = %v; body = %s", err, string(body))
	}
	if got := payload.Error.Code; got != wantCode {
		t.Errorf("upload error.code = %q, want %q (body=%s)", got, wantCode, string(body))
	}
	if payload.Error.Message == "" {
		t.Errorf("upload error.message = empty, want non-empty")
	}
}

func assertUploadJSONHeaders(t *testing.T, resp *http.Response) {
	t.Helper()

	if got, want := resp.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
}
