package upload

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.senan.xyz/taglib"

	"headunit/internal/auth"
	"headunit/internal/scanner"
	"headunit/internal/store"
	"headunit/internal/testutil"
)

type uploadEnv struct {
	st         *store.Store
	svc        *scanner.Service
	auth       *auth.SubsonicAuthenticator
	uploadsDir string
	adminUser  string
	adminPass  string
	adminKey   string
	memberUser string
	memberPass string
	memberKey  string
	revokedKey string
	cipher     *auth.PasswordCipher
}

func newUploadEnv(t *testing.T) *uploadEnv {
	t.Helper()

	root := t.TempDir()
	musicDir := filepath.Join(root, "music")
	uploadsDir := filepath.Join(root, "uploads")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(music): %v", err)
	}
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(uploads): %v", err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, "headunit.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("st.Close: %v", err)
		}
	})

	key, err := auth.LoadOrCreateKey(root, "")
	if err != nil {
		t.Fatalf("auth.LoadOrCreateKey: %v", err)
	}
	cipher, err := auth.NewPasswordCipher(key)
	if err != nil {
		t.Fatalf("auth.NewPasswordCipher: %v", err)
	}

	adminPass := "admin-secret-pw"
	adminEnc, err := cipher.Encrypt(adminPass)
	if err != nil {
		t.Fatalf("cipher.Encrypt(admin): %v", err)
	}
	admin, err := st.CreateUser(ctx, store.User{
		Username:           "alice",
		PasswordCiphertext: adminEnc,
		IsAdmin:            true,
	})
	if err != nil {
		t.Fatalf("CreateUser(alice): %v", err)
	}

	memberPass := "member-secret-pw"
	memberEnc, err := cipher.Encrypt(memberPass)
	if err != nil {
		t.Fatalf("cipher.Encrypt(member): %v", err)
	}
	member, err := st.CreateUser(ctx, store.User{
		Username:           "bob",
		PasswordCiphertext: memberEnc,
		IsAdmin:            false,
	})
	if err != nil {
		t.Fatalf("CreateUser(bob): %v", err)
	}

	adminPlain, adminPrefix, adminHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(admin): %v", err)
	}
	if _, err := st.CreateAPIKey(ctx, store.APIKey{
		UserID:    admin.ID,
		Name:      "admin-key",
		KeyPrefix: adminPrefix,
		KeyHash:   adminHash,
	}); err != nil {
		t.Fatalf("CreateAPIKey(admin): %v", err)
	}

	memberPlain, memberPrefix, memberHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(member): %v", err)
	}
	if _, err := st.CreateAPIKey(ctx, store.APIKey{
		UserID:    member.ID,
		Name:      "member-key",
		KeyPrefix: memberPrefix,
		KeyHash:   memberHash,
	}); err != nil {
		t.Fatalf("CreateAPIKey(member): %v", err)
	}

	revokedPlain, revokedPrefix, revokedHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(revoked): %v", err)
	}
	revokedRec, err := st.CreateAPIKey(ctx, store.APIKey{
		UserID:    admin.ID,
		Name:      "revoked-key",
		KeyPrefix: revokedPrefix,
		KeyHash:   revokedHash,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(revoked): %v", err)
	}
	if err := st.RevokeAPIKey(ctx, revokedRec.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	authenticator, err := auth.NewSubsonicAuthenticator(st, cipher)
	if err != nil {
		t.Fatalf("NewSubsonicAuthenticator: %v", err)
	}

	svc, err := scanner.New(musicDir, 0, st, nil, scanner.WithUploadsDir(uploadsDir))
	if err != nil {
		t.Fatalf("scanner.New: %v", err)
	}

	return &uploadEnv{
		st:         st,
		svc:        svc,
		auth:       authenticator,
		uploadsDir: uploadsDir,
		adminUser:  admin.Username,
		adminPass:  adminPass,
		adminKey:   adminPlain,
		memberUser: member.Username,
		memberPass: memberPass,
		memberKey:  memberPlain,
		revokedKey: revokedPlain,
		cipher:     cipher,
	}
}

func (e *uploadEnv) newHandler(t *testing.T, uploadsEnabled bool, maxFileBytes int64) *Handler {
	t.Helper()
	h, err := NewHandler(Dependencies{
		Auth:           e.auth,
		Ingester:       e.svc,
		UploadsDir:     e.uploadsDir,
		UploadsEnabled: uploadsEnabled,
		MaxFileBytes:   maxFileBytes,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Handler.Close: %v", err)
		}
	})
	return h
}

type spyReadCloser struct {
	r         io.Reader
	bytesRead int
}

func (s *spyReadCloser) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.bytesRead += n
	return n, err
}

func (s *spyReadCloser) Close() error {
	return nil
}

type stubIngester struct {
	track *store.Track
	err   error
	calls int
}

func (s *stubIngester) IngestUpload(_ context.Context, _ string) (*store.Track, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.track, nil
}

func buildMultipartBody(t *testing.T, fieldName, filename string, payload []byte, extraFields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range extraFields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("WriteField(%q): %v", k, err)
		}
	}
	if fieldName != "" {
		fw, err := mw.CreateFormFile(fieldName, filename)
		if err != nil {
			t.Fatalf("CreateFormFile(%q, %q): %v", fieldName, filename, err)
		}
		if _, err := fw.Write(payload); err != nil {
			t.Fatalf("fw.Write: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func makeTaggedFLACBytes(t *testing.T, title, artist, album string) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.flac")
	testutil.WriteMinimalFLAC(t, p)
	testutil.WriteTags(t, p, map[string][]string{
		taglib.Title:       {title},
		taglib.Artist:      {artist},
		taglib.AlbumArtist: {artist},
		taglib.Album:       {album},
	})
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", p, err)
	}
	return data
}

func makeMP3Bytes(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.mp3")
	testutil.WriteMinimalMP3(t, p)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", p, err)
	}
	return data
}

func tokenParams(username, password, salt string) url.Values {
	sum := md5.Sum([]byte(password + salt))
	return url.Values{
		"u": {username},
		"t": {hex.EncodeToString(sum[:])},
		"s": {salt},
	}
}

func decodeErrorJSON(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	assertJSONHeaders(t, rec)
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal error response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func decodeSuccessJSON(t *testing.T, rec *httptest.ResponseRecorder) uploadResponse {
	t.Helper()
	assertJSONHeaders(t, rec)
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal success response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func assertJSONHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
}

func assertNoStagedTempFiles(t *testing.T, uploadsDir string) {
	t.Helper()
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q): %v", uploadsDir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") {
			t.Errorf("leftover staged temp file %q in %q", entry.Name(), uploadsDir)
		}
	}
}

func TestNewHandlerValidation(t *testing.T) {
	env := newUploadEnv(t)
	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cases := []struct {
		name string
		deps Dependencies
	}{
		{
			name: "nil auth",
			deps: Dependencies{
				Ingester:     env.svc,
				UploadsDir:   env.uploadsDir,
				MaxFileBytes: 1024,
			},
		},
		{
			name: "nil ingester",
			deps: Dependencies{
				Auth:         env.auth,
				UploadsDir:   env.uploadsDir,
				MaxFileBytes: 1024,
			},
		},
		{
			name: "empty uploads dir",
			deps: Dependencies{
				Auth:         env.auth,
				Ingester:     env.svc,
				UploadsDir:   "   ",
				MaxFileBytes: 1024,
			},
		},
		{
			name: "missing uploads dir",
			deps: Dependencies{
				Auth:         env.auth,
				Ingester:     env.svc,
				UploadsDir:   filepath.Join(t.TempDir(), "missing"),
				MaxFileBytes: 1024,
			},
		},
		{
			name: "file instead of uploads dir",
			deps: Dependencies{
				Auth:         env.auth,
				Ingester:     env.svc,
				UploadsDir:   filePath,
				MaxFileBytes: 1024,
			},
		},
		{
			name: "zero max file bytes",
			deps: Dependencies{
				Auth:         env.auth,
				Ingester:     env.svc,
				UploadsDir:   env.uploadsDir,
				MaxFileBytes: 0,
			},
		},
		{
			name: "negative max file bytes",
			deps: Dependencies{
				Auth:         env.auth,
				Ingester:     env.svc,
				UploadsDir:   env.uploadsDir,
				MaxFileBytes: -10,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewHandler(tc.deps)
			if err == nil {
				_ = h.Close()
				t.Fatalf("NewHandler() error = nil, want non-nil")
			}
		})
	}
}

func TestHandlerCloseAndPostShutdown(t *testing.T) {
	var nilHandler *Handler
	if err := nilHandler.Close(); err != nil {
		t.Fatalf("(*Handler)(nil).Close() = %v, want nil", err)
	}
	rec := httptest.NewRecorder()
	nilHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/upload", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil handler status = %d, want 500", rec.Code)
	}
	errResp := decodeErrorJSON(t, rec)
	if errResp.Error.Code != errCodeInternal {
		t.Errorf("error.code = %q, want %q", errResp.Error.Code, errCodeInternal)
	}

	env := newUploadEnv(t)
	h, err := NewHandler(Dependencies{
		Auth:           env.auth,
		Ingester:       env.svc,
		UploadsDir:     env.uploadsDir,
		UploadsEnabled: false, // closed check must execute before auth/uploadsEnabled
		MaxFileBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("first Close() = %v, want nil", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}

	probes := []struct {
		name   string
		method string
		target string
	}{
		{name: "unauthenticated post", method: http.MethodPost, target: "/api/v1/upload"},
		{name: "authenticated post", method: http.MethodPost, target: "/api/v1/upload?apiKey=" + url.QueryEscape(env.adminKey)},
		{name: "get probe", method: http.MethodGet, target: "/api/v1/upload"},
	}
	for _, tc := range probes {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.target, nil)
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			got := decodeErrorJSON(t, rec)
			if got.Error.Code != errCodeInternal {
				t.Errorf("error.code = %q, want %q", got.Error.Code, errCodeInternal)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	env := newUploadEnv(t)
	h := env.newHandler(t, true, 1<<20)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), nil)
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rec.Code)
			}
			if got, want := rec.Header().Get("Allow"), http.MethodPost; got != want {
				t.Errorf("Allow = %q, want %q", got, want)
			}
			got := decodeErrorJSON(t, rec)
			if got.Error.Code != errCodeMethodNotAllowed {
				t.Errorf("error.code = %q, want %q", got.Error.Code, errCodeMethodNotAllowed)
			}
		})
	}
}

func TestPreAuthQueryAndAuthErrorsDoNotConsumeBody(t *testing.T) {
	env := newUploadEnv(t)

	cases := []struct {
		name           string
		uploadsEnabled bool
		rawQuery       string
		wantStatus     int
		wantCode       string
	}{
		{
			name:           "malformed percent escape",
			uploadsEnabled: true,
			rawQuery:       "apiKey=%ZZ",
			wantStatus:     http.StatusBadRequest,
			wantCode:       errCodeBadRequest,
		},
		{
			name:           "semicolon in query",
			uploadsEnabled: true,
			rawQuery:       "a=1;b=2",
			wantStatus:     http.StatusBadRequest,
			wantCode:       errCodeBadRequest,
		},
		{
			name:           "missing auth",
			uploadsEnabled: true,
			rawQuery:       "",
			wantStatus:     http.StatusUnauthorized,
			wantCode:       errCodeUnauthorized,
		},
		{
			name:           "missing auth even when uploads disabled",
			uploadsEnabled: false,
			rawQuery:       "",
			wantStatus:     http.StatusUnauthorized,
			wantCode:       errCodeUnauthorized,
		},
		{
			name:           "invalid api key",
			uploadsEnabled: true,
			rawQuery:       "apiKey=hu_invalid",
			wantStatus:     http.StatusUnauthorized,
			wantCode:       errCodeUnauthorized,
		},
		{
			name:           "revoked api key",
			uploadsEnabled: true,
			rawQuery:       "apiKey=" + url.QueryEscape(env.revokedKey),
			wantStatus:     http.StatusUnauthorized,
			wantCode:       errCodeUnauthorized,
		},
		{
			name:           "bad salted token",
			uploadsEnabled: true,
			rawQuery:       "u=alice&t=00000000000000000000000000000000&s=abcdef",
			wantStatus:     http.StatusUnauthorized,
			wantCode:       errCodeUnauthorized,
		},
		{
			name:           "deprecated plaintext password p=",
			uploadsEnabled: true,
			rawQuery:       "u=alice&p=admin-secret-pw",
			wantStatus:     http.StatusUnauthorized,
			wantCode:       errCodeUnauthorized,
		},
		{
			name:           "conflicting auth mechanisms",
			uploadsEnabled: true,
			rawQuery:       "apiKey=" + url.QueryEscape(env.adminKey) + "&u=alice",
			wantStatus:     http.StatusUnauthorized,
			wantCode:       errCodeUnauthorized,
		},
		{
			name:           "uploads disabled for admin",
			uploadsEnabled: false,
			rawQuery:       "apiKey=" + url.QueryEscape(env.adminKey),
			wantStatus:     http.StatusForbidden,
			wantCode:       errCodeForbidden,
		},
		{
			name:           "non-admin member",
			uploadsEnabled: true,
			rawQuery:       "apiKey=" + url.QueryEscape(env.memberKey),
			wantStatus:     http.StatusForbidden,
			wantCode:       errCodeForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := env.newHandler(t, tc.uploadsEnabled, 1<<20)
			body := &spyReadCloser{r: strings.NewReader("this-body-must-not-be-read")}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?"+tc.rawQuery, body)
			req.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if body.bytesRead != 0 {
				t.Errorf("body.bytesRead = %d, want 0", body.bytesRead)
			}
			got := decodeErrorJSON(t, rec)
			if got.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got.Error.Code, tc.wantCode)
			}
			if got.Error.Message == "" {
				t.Error("error.message must not be empty")
			}
		})
	}
}

func TestInvalidQueryParamsBeforeReadingBody(t *testing.T) {
	env := newUploadEnv(t)
	h := env.newHandler(t, true, 1<<20)

	tooLongPath := strings.Repeat("a", 509) + ".mp3"       // 513 bytes
	tooManySegments := strings.Repeat("d/", 16) + "01.mp3" // 17 segments

	invalidQueries := []struct {
		name     string
		rawQuery string
	}{
		{name: "empty path", rawQuery: "path="},
		{name: "whitespace path", rawQuery: "path=%20%20"},
		{name: "parent traversal", rawQuery: "path=../01.mp3"},
		{name: "mid traversal", rawQuery: "path=a/../01.mp3"},
		{name: "leading slash", rawQuery: "path=/abs.mp3"},
		{name: "leading backslash", rawQuery: "path=" + url.QueryEscape(`\abs.mp3`)},
		{name: "windows drive", rawQuery: "path=" + url.QueryEscape(`C:\a.mp3`)},
		{name: "windows drive no slash", rawQuery: "path=D:a.mp3"},
		{name: "trailing slash", rawQuery: "path=artist/"},
		{name: "nul byte", rawQuery: "path=a%00b.mp3"},
		{name: "control char", rawQuery: "path=a%0Ab.mp3"},
		{name: "del char", rawQuery: "path=a%7Fb.mp3"},
		{name: "hidden dir", rawQuery: "path=.hidden/a.mp3"},
		{name: "hidden file", rawQuery: "path=a/.b.mp3"},
		{name: "ignored tmp extension", rawQuery: "path=a/b.mp3.tmp"},
		{name: "ignored part dir", rawQuery: "path=a/dir.part/01.mp3"},
		{name: "double slash", rawQuery: "path=a//b.mp3"},
		{name: "over 512 bytes", rawQuery: "path=" + tooLongPath},
		{name: "over 16 segments", rawQuery: "path=" + tooManySegments},
		{name: "unsupported txt extension", rawQuery: "path=notes.txt"},
		{name: "duplicate path", rawQuery: "path=a.mp3&path=b.mp3"},
		{name: "invalid overwrite 1", rawQuery: "path=a.mp3&overwrite=1"},
		{name: "invalid overwrite yes", rawQuery: "path=a.mp3&overwrite=yes"},
		{name: "invalid overwrite TRUE", rawQuery: "path=a.mp3&overwrite=TRUE"},
		{name: "duplicate overwrite", rawQuery: "path=a.mp3&overwrite=true&overwrite=false"},
	}

	for _, tc := range invalidQueries {
		t.Run(tc.name, func(t *testing.T) {
			body := &spyReadCloser{r: strings.NewReader("unread-body")}
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey)+"&"+tc.rawQuery,
				body,
			)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if body.bytesRead != 0 {
				t.Errorf("body.bytesRead = %d, want 0", body.bytesRead)
			}
			got := decodeErrorJSON(t, rec)
			if got.Error.Code != errCodeBadRequest {
				t.Errorf("error.code = %q, want %q", got.Error.Code, errCodeBadRequest)
			}
		})
	}
}

func TestAdminUploadSuccessPaths(t *testing.T) {
	flacPayload := makeTaggedFLACBytes(t, "Opening", "Artist", "Album")
	mp3Payload := makeMP3Bytes(t)
	wavPayload := testutil.MinimalWAVBytes()

	cases := []struct {
		name         string
		useTokenAuth bool
		queryPath    string
		overwrite    string
		partFilename string
		payload      []byte
		wantRelPath  string
		wantTitle    string
		wantArtist   string
		wantAlbum    string
	}{
		{
			name:         "apiKey with explicit nested path",
			queryPath:    "Artist/Album/01.flac",
			partFilename: "ignored.mp3",
			payload:      flacPayload,
			wantRelPath:  "Artist/Album/01.flac",
			wantTitle:    "Opening",
			wantArtist:   "Artist",
			wantAlbum:    "Album",
		},
		{
			name:         "salted md5 token with omitted path fallback",
			useTokenAuth: true,
			partFilename: "02 - Fallback.mp3",
			payload:      mp3Payload,
			wantRelPath:  "02 - Fallback.mp3",
			wantTitle:    "Fallback",
		},
		{
			name:         "windows backslash path converted and uppercase ext",
			queryPath:    `Band\Record\03.WAV`,
			overwrite:    "false",
			partFilename: "track.wav",
			payload:      wavPayload,
			wantRelPath:  "Band/Record/03.WAV",
			wantTitle:    "03",
		},
		{
			name:         "overwrite=true on new path returns 201 created",
			queryPath:    "Fresh/04.flac",
			overwrite:    "true",
			partFilename: "04.flac",
			payload:      flacPayload,
			wantRelPath:  "Fresh/04.flac",
			wantTitle:    "Opening",
			wantArtist:   "Artist",
			wantAlbum:    "Album",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newUploadEnv(t)
			h := env.newHandler(t, true, int64(len(tc.payload))+1024)

			q := url.Values{}
			if tc.useTokenAuth {
				for k, vs := range tokenParams(env.adminUser, env.adminPass, "saltsalt") {
					q[k] = vs
				}
			} else {
				q.Set("apiKey", env.adminKey)
			}
			if tc.queryPath != "" {
				q.Set("path", tc.queryPath)
			}
			if tc.overwrite != "" {
				q.Set("overwrite", tc.overwrite)
			}

			body, ct := buildMultipartBody(t, "file", tc.partFilename, tc.payload, map[string]string{"note": "ignored"})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?"+q.Encode(), body)
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
			}
			resp := decodeSuccessJSON(t, rec)
			if resp.Status != "created" {
				t.Errorf("status = %q, want %q", resp.Status, "created")
			}
			if !strings.HasPrefix(resp.Track.ID, "tr_") {
				t.Errorf("track.id = %q, want tr_ prefix", resp.Track.ID)
			}
			if got, want := resp.Track.Path, "uploads/"+tc.wantRelPath; got != want {
				t.Errorf("track.path = %q, want %q", got, want)
			}
			if got, want := resp.Track.Size, int64(len(tc.payload)); got != want {
				t.Errorf("track.size = %d, want %d", got, want)
			}
			if resp.Track.Title != tc.wantTitle {
				t.Errorf("track.title = %q, want %q", resp.Track.Title, tc.wantTitle)
			}
			if tc.wantArtist != "" && resp.Track.Artist != tc.wantArtist {
				t.Errorf("track.artist = %q, want %q", resp.Track.Artist, tc.wantArtist)
			}
			if tc.wantAlbum != "" && resp.Track.Album != tc.wantAlbum {
				t.Errorf("track.album = %q, want %q", resp.Track.Album, tc.wantAlbum)
			}

			diskBytes, err := os.ReadFile(filepath.Join(env.uploadsDir, filepath.FromSlash(tc.wantRelPath)))
			if err != nil {
				t.Fatalf("os.ReadFile committed file: %v", err)
			}
			if !bytes.Equal(diskBytes, tc.payload) {
				t.Errorf("committed file bytes differ from uploaded payload")
			}

			persisted, err := env.st.GetTrack(context.Background(), resp.Track.ID)
			if err != nil {
				t.Fatalf("store.GetTrack(%q): %v", resp.Track.ID, err)
			}
			if persisted.Source != store.SourceUploads || persisted.RelPath != tc.wantRelPath {
				t.Errorf("persisted track = (%q, %q), want (%q, %q)", persisted.Source, persisted.RelPath, store.SourceUploads, tc.wantRelPath)
			}
			assertNoStagedTempFiles(t, env.uploadsDir)
		})
	}
}

func TestMultipartErrorsSizeLimitsAndAudioValidation(t *testing.T) {
	validWAV := testutil.MinimalWAVBytes()

	t.Run("non-multipart content type", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey)+"&path=a.wav", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if got := decodeErrorJSON(t, rec).Error.Code; got != errCodeBadRequest {
			t.Errorf("error.code = %q, want %q", got, errCodeBadRequest)
		}
	})

	t.Run("missing file part", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		body, ct := buildMultipartBody(t, "", "", nil, map[string]string{"other": "value"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey)+"&path=a.wav", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("duplicate file part", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for range 2 {
			fw, err := mw.CreateFormFile("file", "a.wav")
			if err != nil {
				t.Fatalf("CreateFormFile: %v", err)
			}
			if _, err := fw.Write(validWAV); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("truncated multipart stream", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		body, ct := buildMultipartBody(t, "file", "a.wav", validWAV, nil)
		raw := body.Bytes()
		// Cut off closing multipart boundary.
		truncated := bytes.NewReader(raw[:len(raw)-16])
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), truncated)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("empty 0-byte file part", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		body, ct := buildMultipartBody(t, "file", "empty.wav", nil, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("invalid fallback filename when path omitted", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		body, ct := buildMultipartBody(t, "file", "notes.txt", []byte("hello"), nil)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("exact MaxFileBytes succeeds", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, int64(len(validWAV)))

		body, ct := buildMultipartBody(t, "file", "exact.wav", validWAV, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("MaxFileBytes+1 returns 413 and cleans temp file", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, int64(len(validWAV)-1))

		body, ct := buildMultipartBody(t, "file", "toobig.wav", validWAV, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
		}
		if got := decodeErrorJSON(t, rec).Error.Code; got != errCodePayloadTooLarge {
			t.Errorf("error.code = %q, want %q", got, errCodePayloadTooLarge)
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("oversized trailing non-file part returns 413 and cleans temp file", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, int64(len(validWAV)))

		oversizedExtra := strings.Repeat("x", int(multipartOverheadBytes)+len(validWAV)+64)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", "valid.wav")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := fw.Write(validWAV); err != nil {
			t.Fatalf("fw.Write: %v", err)
		}
		if err := mw.WriteField("extra", oversizedExtra); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("mw.Close: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
		}
		if got := decodeErrorJSON(t, rec).Error.Code; got != errCodePayloadTooLarge {
			t.Errorf("error.code = %q, want %q", got, errCodePayloadTooLarge)
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("corrupt or plaintext audio returns 422 and cleans temp file", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			filename string
			payload  []byte
		}{
			{name: "plaintext as mp3", filename: "fake.mp3", payload: []byte("not really an mp3")},
			{name: "plaintext as flac", filename: "fake.flac", payload: []byte("plain text pretending to be flac")},
			{name: "corrupt wav", filename: "bad.wav", payload: []byte("RIFF\x00\x00\x00\x00WAVEjunk")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env := newUploadEnv(t)
				h := env.newHandler(t, true, 1<<20)

				body, ct := buildMultipartBody(t, "file", tc.filename, tc.payload, nil)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey), body)
				req.Header.Set("Content-Type", ct)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
				}
				if got := decodeErrorJSON(t, rec).Error.Code; got != errCodeInvalidAudio {
					t.Errorf("error.code = %q, want %q", got, errCodeInvalidAudio)
				}
				assertNoStagedTempFiles(t, env.uploadsDir)
			})
		}
	})
}

func TestOverwriteAndConflictBehavior(t *testing.T) {
	flacV1 := makeTaggedFLACBytes(t, "Version One", "Artist", "Album")
	flacV2 := makeTaggedFLACBytes(t, "Version Two", "Artist", "Album")

	t.Run("overwrite=false conflict preserves existing file and returns 409", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		uploadOnce := func(overwrite string, payload []byte) *httptest.ResponseRecorder {
			q := url.Values{"apiKey": {env.adminKey}, "path": {"Artist/01.flac"}}
			if overwrite != "" {
				q.Set("overwrite", overwrite)
			}
			body, ct := buildMultipartBody(t, "file", "01.flac", payload, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?"+q.Encode(), body)
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}

		first := uploadOnce("", flacV1)
		if first.Code != http.StatusCreated {
			t.Fatalf("first upload status = %d, want 201; body=%s", first.Code, first.Body.String())
		}
		firstResp := decodeSuccessJSON(t, first)

		second := uploadOnce("false", flacV2)
		if second.Code != http.StatusConflict {
			t.Fatalf("second upload status = %d, want 409; body=%s", second.Code, second.Body.String())
		}
		if got := decodeErrorJSON(t, second).Error.Code; got != errCodeConflict {
			t.Errorf("error.code = %q, want %q", got, errCodeConflict)
		}
		assertNoStagedTempFiles(t, env.uploadsDir)

		third := uploadOnce("true", flacV2)
		if third.Code != http.StatusOK {
			t.Fatalf("overwrite upload status = %d, want 200; body=%s", third.Code, third.Body.String())
		}
		thirdResp := decodeSuccessJSON(t, third)
		if thirdResp.Status != "updated" {
			t.Errorf("status = %q, want %q", thirdResp.Status, "updated")
		}
		if thirdResp.Track.ID != firstResp.Track.ID {
			t.Errorf("track.id changed across overwrite: got %q, want %q", thirdResp.Track.ID, firstResp.Track.ID)
		}
		if thirdResp.Track.Title != "Version Two" {
			t.Errorf("track.title = %q, want %q", thirdResp.Track.Title, "Version Two")
		}

		diskBytes, err := os.ReadFile(filepath.Join(env.uploadsDir, "Artist", "01.flac"))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(diskBytes, flacV2) {
			t.Errorf("overwritten file bytes do not match v2")
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("directory at target path returns 409 for both overwrite=false and overwrite=true", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		if err := os.MkdirAll(filepath.Join(env.uploadsDir, "Artist", "01.flac"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		for _, ow := range []string{"false", "true"} {
			body, ct := buildMultipartBody(t, "file", "01.flac", flacV1, nil)
			req := httptest.NewRequest(
				http.MethodPost,
				fmt.Sprintf("/api/v1/upload?apiKey=%s&path=Artist/01.flac&overwrite=%s", url.QueryEscape(env.adminKey), ow),
				body,
			)
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("overwrite=%s status = %d, want 409; body=%s", ow, rec.Code, rec.Body.String())
			}
			if got := decodeErrorJSON(t, rec).Error.Code; got != errCodeConflict {
				t.Errorf("overwrite=%s error.code = %q, want %q", ow, got, errCodeConflict)
			}
			assertNoStagedTempFiles(t, env.uploadsDir)
		}
	})

	t.Run("parent path component is a regular file returns 409", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		if err := os.WriteFile(filepath.Join(env.uploadsDir, "Artist"), []byte("regular-file"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		for _, ow := range []string{"false", "true"} {
			body, ct := buildMultipartBody(t, "file", "01.flac", flacV1, nil)
			req := httptest.NewRequest(
				http.MethodPost,
				fmt.Sprintf("/api/v1/upload?apiKey=%s&path=Artist/01.flac&overwrite=%s", url.QueryEscape(env.adminKey), ow),
				body,
			)
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("overwrite=%s status = %d, want 409; body=%s", ow, rec.Code, rec.Body.String())
			}
			if got := decodeErrorJSON(t, rec).Error.Code; got != errCodeConflict {
				t.Errorf("overwrite=%s error.code = %q, want %q", ow, got, errCodeConflict)
			}
			assertNoStagedTempFiles(t, env.uploadsDir)
		}
	})
}

func TestIngestUploadFailureReturns500WithoutLeakingAndPreservesCommittedFile(t *testing.T) {
	env := newUploadEnv(t)
	stub := &stubIngester{
		err: errors.New("sqlite error at /secret/internal/path/headunit.db"),
	}
	h, err := NewHandler(Dependencies{
		Auth:           env.auth,
		Ingester:       stub,
		UploadsDir:     env.uploadsDir,
		UploadsEnabled: true,
		MaxFileBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	defer func() { _ = h.Close() }()

	wav := testutil.MinimalWAVBytes()
	body, ct := buildMultipartBody(t, "file", "01.wav", wav, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey)+"&path=Album/01.wav", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeErrorJSON(t, rec)
	if resp.Error.Code != errCodeInternal {
		t.Errorf("error.code = %q, want %q", resp.Error.Code, errCodeInternal)
	}
	if strings.Contains(rec.Body.String(), "/secret/internal/path") || strings.Contains(rec.Body.String(), "sqlite") {
		t.Errorf("500 response leaked internal error details: %s", rec.Body.String())
	}
	if stub.calls != 1 {
		t.Errorf("IngestUpload calls = %d, want 1", stub.calls)
	}
	committed, err := os.ReadFile(filepath.Join(env.uploadsDir, "Album", "01.wav"))
	if err != nil {
		t.Fatalf("expected committed file to remain on disk: %v", err)
	}
	if !bytes.Equal(committed, wav) {
		t.Errorf("committed file bytes mismatch")
	}
	assertNoStagedTempFiles(t, env.uploadsDir)
}

func TestConcurrentUploadsRace(t *testing.T) {
	flacV1 := makeTaggedFLACBytes(t, "Race Track", "Race Artist", "Race Album")
	flacV2 := makeTaggedFLACBytes(t, "Race Track Alt", "Race Artist", "Race Album")

	t.Run("concurrent overwrite=false yields exactly one 201 and N-1 409", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		const workers = 8
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, workers)

		for i := range workers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				var buf bytes.Buffer
				mw := multipart.NewWriter(&buf)
				fw, _ := mw.CreateFormFile("file", "race.flac")
				_, _ = fw.Write(flacV1)
				_ = mw.Close()

				req := httptest.NewRequest(
					http.MethodPost,
					"/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey)+"&path=Race/01.flac&overwrite=false",
					&buf,
				)
				req.Header.Set("Content-Type", mw.FormDataContentType())
				rec := httptest.NewRecorder()

				<-start
				h.ServeHTTP(rec, req)
				codes[idx] = rec.Code
			}(i)
		}

		close(start)
		wg.Wait()

		var created, conflicts int
		for _, code := range codes {
			switch code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflicts++
			default:
				t.Errorf("unexpected status code %d", code)
			}
		}
		if created != 1 || conflicts != workers-1 {
			t.Errorf("counts = (created=%d, conflicts=%d), want (1, %d)", created, conflicts, workers-1)
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("concurrent overwrite=true succeeds for all requests", func(t *testing.T) {
		env := newUploadEnv(t)
		h := env.newHandler(t, true, 1<<20)

		const workers = 8
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, workers)

		for i := range workers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				payload := flacV1
				if idx%2 == 1 {
					payload = flacV2
				}
				var buf bytes.Buffer
				mw := multipart.NewWriter(&buf)
				fw, _ := mw.CreateFormFile("file", "race.flac")
				_, _ = fw.Write(payload)
				_ = mw.Close()

				req := httptest.NewRequest(
					http.MethodPost,
					"/api/v1/upload?apiKey="+url.QueryEscape(env.adminKey)+"&path=RaceOverwrite/01.flac&overwrite=true",
					&buf,
				)
				req.Header.Set("Content-Type", mw.FormDataContentType())
				rec := httptest.NewRecorder()

				<-start
				h.ServeHTTP(rec, req)
				codes[idx] = rec.Code
			}(i)
		}

		close(start)
		wg.Wait()

		for i, code := range codes {
			if code != http.StatusCreated && code != http.StatusOK {
				t.Errorf("worker %d status = %d, want 201 or 200", i, code)
			}
		}

		finalBytes, err := os.ReadFile(filepath.Join(env.uploadsDir, "RaceOverwrite", "01.flac"))
		if err != nil {
			t.Fatalf("ReadFile final file: %v", err)
		}
		if !bytes.Equal(finalBytes, flacV1) && !bytes.Equal(finalBytes, flacV2) {
			t.Errorf("final file contents corrupted across concurrent overwrites")
		}
		assertNoStagedTempFiles(t, env.uploadsDir)
	})

	t.Run("concurrent ServeHTTP and Close under race detector", func(t *testing.T) {
		env := newUploadEnv(t)
		h, err := NewHandler(Dependencies{
			Auth:           env.auth,
			Ingester:       env.svc,
			UploadsDir:     env.uploadsDir,
			UploadsEnabled: true,
			MaxFileBytes:   1 << 20,
		})
		if err != nil {
			t.Fatalf("NewHandler: %v", err)
		}

		const workers = 6
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				var buf bytes.Buffer
				mw := multipart.NewWriter(&buf)
				fw, _ := mw.CreateFormFile("file", "c.flac")
				_, _ = fw.Write(flacV1)
				_ = mw.Close()

				req := httptest.NewRequest(
					http.MethodPost,
					fmt.Sprintf("/api/v1/upload?apiKey=%s&path=CloseRace/%d.flac", url.QueryEscape(env.adminKey), idx),
					&buf,
				)
				req.Header.Set("Content-Type", mw.FormDataContentType())
				rec := httptest.NewRecorder()
				<-start
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusCreated && rec.Code != http.StatusInternalServerError {
					t.Errorf("unexpected status during concurrent Close: %d", rec.Code)
				}
			}(i)
		}

		close(start)
		if err := h.Close(); err != nil {
			t.Errorf("h.Close() = %v", err)
		}
		wg.Wait()
	})
}
