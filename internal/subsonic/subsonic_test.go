package subsonic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"headunit/internal/auth"
	"headunit/internal/scanner"
	"headunit/internal/store"
	"headunit/internal/stream"
	"headunit/internal/subsonic"
)

type atomicScanStatusProvider struct {
	scanning atomic.Bool
	count    atomic.Int64
	lastNs   atomic.Int64
	zone     *time.Location
}

func (p *atomicScanStatusProvider) set(scanning bool, count int64, last time.Time) {
	p.scanning.Store(scanning)
	p.count.Store(count)
	if last.IsZero() {
		p.lastNs.Store(0)
		return
	}
	p.lastNs.Store(last.UnixNano())
}

func (p *atomicScanStatusProvider) Status() scanner.ScanStatus {
	var last time.Time
	if ns := p.lastNs.Load(); ns != 0 {
		last = time.Unix(0, ns)
		if p.zone != nil {
			last = last.In(p.zone)
		}
	}
	return scanner.ScanStatus{
		Scanning: p.scanning.Load(),
		Count:    p.count.Load(),
		LastScan: last,
	}
}

type stubAuthenticator struct {
	calls     atomic.Int64
	principal auth.Principal
	err       error
}

func (s *stubAuthenticator) Authenticate(_ *http.Request) (auth.Principal, error) {
	s.calls.Add(1)
	if s.err != nil {
		return auth.Principal{}, s.err
	}
	return s.principal, nil
}

type trackingReadCloser struct {
	reads atomic.Int64
}

func (r *trackingReadCloser) Read(_ []byte) (int, error) {
	r.reads.Add(1)
	return 0, errors.New("unexpected body read")
}

func (*trackingReadCloser) Close() error {
	return nil
}

type noopLibraryStore struct{}

func (noopLibraryStore) ListArtists(context.Context) ([]store.ArtistSummary, error) {
	return []store.ArtistSummary{}, nil
}

func (noopLibraryStore) GetArtist(context.Context, string) (store.ArtistSummary, []store.AlbumSummary, error) {
	return store.ArtistSummary{}, nil, store.ErrNotFound
}

func (noopLibraryStore) GetAlbum(context.Context, string) (store.AlbumSummary, []store.Track, error) {
	return store.AlbumSummary{}, nil, store.ErrNotFound
}

func (noopLibraryStore) ListAlbums(context.Context, store.AlbumListQuery) ([]store.AlbumSummary, error) {
	return []store.AlbumSummary{}, nil
}

func (noopLibraryStore) GetTrack(context.Context, string) (*store.Track, error) {
	return nil, store.ErrNotFound
}

func (noopLibraryStore) Search3(context.Context, store.Search3Query) (store.Search3Result, error) {
	return store.Search3Result{
		Artists: []store.ArtistSummary{},
		Albums:  []store.AlbumSummary{},
		Songs:   []store.Track{},
	}, nil
}

type noopStreamer struct{}

func (noopStreamer) Stream(http.ResponseWriter, *http.Request, *store.Track, stream.StreamOptions) error {
	return nil
}

func (noopStreamer) Close() error {
	return nil
}

type xmlEnvelope struct {
	XMLName                xml.Name                         `xml:"subsonic-response"`
	Xmlns                  string                           `xml:"xmlns,attr"`
	Status                 string                           `xml:"status,attr"`
	Version                string                           `xml:"version,attr"`
	Type                   string                           `xml:"type,attr"`
	ServerVersion          string                           `xml:"serverVersion,attr"`
	OpenSubsonic           bool                             `xml:"openSubsonic,attr"`
	Error                  *subsonic.APIError               `xml:"error"`
	License                *subsonic.License                `xml:"license"`
	OpenSubsonicExtensions []subsonic.OpenSubsonicExtension `xml:"openSubsonicExtensions"`
	TokenInfo              *subsonic.TokenInfo              `xml:"tokenInfo"`
	User                   *subsonic.User                   `xml:"user"`
	ScanStatus             *subsonic.ScanStatus             `xml:"scanStatus"`
	Playlists              *subsonic.Playlists              `xml:"playlists"`
	Genres                 *subsonic.Genres                 `xml:"genres"`
	Starred2               *subsonic.Starred2               `xml:"starred2"`
	MusicFolders           *subsonic.MusicFolders           `xml:"musicFolders"`
	Artists                *subsonic.ArtistsID3             `xml:"artists"`
	Artist                 *subsonic.ArtistWithAlbumsID3    `xml:"artist"`
	Album                  *subsonic.AlbumWithSongsID3      `xml:"album"`
	AlbumList2             *subsonic.AlbumList2             `xml:"albumList2"`
	Song                   *subsonic.Child                  `xml:"song"`
	SearchResult3          *subsonic.SearchResult3          `xml:"searchResult3"`
}

type jsonEnvelope struct {
	SubsonicResponse map[string]any `json:"subsonic-response"`
}

func newTestHandler(t *testing.T, authImpl auth.Authenticator, scanImpl subsonic.ScanStatusProvider) http.Handler {
	t.Helper()
	h, err := subsonic.NewHandler(subsonic.Dependencies{
		Auth:     authImpl,
		Scanner:  scanImpl,
		Store:    noopLibraryStore{},
		Streamer: noopStreamer{},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return h
}

func assertCORSHeaders(t *testing.T, h http.Header) {
	t.Helper()
	want := map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Allow-Methods":  "GET, POST, HEAD, OPTIONS",
		"Access-Control-Allow-Headers":  "Content-Type, Range, If-Range, Authorization",
		"Access-Control-Expose-Headers": "Content-Length, Content-Range, Accept-Ranges, ETag",
	}
	for k, wantVal := range want {
		if gotVal := h.Get(k); gotVal != wantVal {
			t.Errorf("header %q = %q, want %q", k, gotVal, wantVal)
		}
	}
}

func decodeXML(t *testing.T, body []byte) xmlEnvelope {
	t.Helper()
	if !bytes.HasPrefix(body, []byte(xml.Header)) {
		t.Fatalf("XML response missing declaration header; got %q", string(body))
	}
	var env xmlEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		t.Fatalf("xml.Unmarshal() error = %v; body = %s", err, string(body))
	}
	return env
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env jsonEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; body = %s", err, string(body))
	}
	if env.SubsonicResponse == nil {
		t.Fatalf("missing subsonic-response object in %s", string(body))
	}
	return env.SubsonicResponse
}

func assertNoCoverArt(t *testing.T, body []byte) {
	t.Helper()
	s := string(body)
	if strings.Contains(s, "coverArt=") {
		t.Errorf("unexpected XML coverArt= attribute in %s", s)
	}
	if strings.Contains(s, `"coverArt":`) {
		t.Errorf(`unexpected JSON "coverArt" key in %s`, s)
	}
}

func assertEnvelopeFields(t *testing.T, env xmlEnvelope, wantStatus string) {
	t.Helper()
	if env.Xmlns != "http://subsonic.org/restapi" {
		t.Errorf("xmlns = %q, want %q", env.Xmlns, "http://subsonic.org/restapi")
	}
	if env.Status != wantStatus {
		t.Errorf("status = %q, want %q", env.Status, wantStatus)
	}
	if env.Version != "1.16.1" {
		t.Errorf("version = %q, want %q", env.Version, "1.16.1")
	}
	if env.Type != "headunit" {
		t.Errorf("type = %q, want %q", env.Type, "headunit")
	}
	if env.ServerVersion != "0.1.0" {
		t.Errorf("serverVersion = %q, want %q", env.ServerVersion, "0.1.0")
	}
	if !env.OpenSubsonic {
		t.Errorf("openSubsonic = %v, want true", env.OpenSubsonic)
	}
}

func assertJSONEnvelopeFields(t *testing.T, resp map[string]any, wantStatus string) {
	t.Helper()
	if got, _ := resp["status"].(string); got != wantStatus {
		t.Errorf("status = %v, want %q", resp["status"], wantStatus)
	}
	if got, _ := resp["version"].(string); got != "1.16.1" {
		t.Errorf("version = %v, want %q", resp["version"], "1.16.1")
	}
	if got, _ := resp["type"].(string); got != "headunit" {
		t.Errorf("type = %v, want %q", resp["type"], "headunit")
	}
	if got, _ := resp["serverVersion"].(string); got != "0.1.0" {
		t.Errorf("serverVersion = %v, want %q", resp["serverVersion"], "0.1.0")
	}
	if got, ok := resp["openSubsonic"].(bool); !ok || !got {
		t.Errorf("openSubsonic = %v (ok=%v), want true", resp["openSubsonic"], ok)
	}
}

func TestNewHandler_Validation(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	tests := []struct {
		name    string
		deps    subsonic.Dependencies
		wantErr bool
	}{
		{
			name:    "nil auth",
			deps:    subsonic.Dependencies{Auth: nil, Scanner: scan, Store: noopLibraryStore{}, Streamer: noopStreamer{}},
			wantErr: true,
		},
		{
			name:    "nil scanner",
			deps:    subsonic.Dependencies{Auth: auth.NoopAuthenticator{}, Scanner: nil, Store: noopLibraryStore{}, Streamer: noopStreamer{}},
			wantErr: true,
		},
		{
			name:    "nil store",
			deps:    subsonic.Dependencies{Auth: auth.NoopAuthenticator{}, Scanner: scan, Store: nil, Streamer: noopStreamer{}},
			wantErr: true,
		},
		{
			name:    "nil streamer",
			deps:    subsonic.Dependencies{Auth: auth.NoopAuthenticator{}, Scanner: scan, Store: noopLibraryStore{}, Streamer: nil},
			wantErr: true,
		},
		{
			name:    "nil logger defaults cleanly",
			deps:    subsonic.Dependencies{Auth: auth.NoopAuthenticator{}, Scanner: scan, Store: noopLibraryStore{}, Streamer: noopStreamer{}, Logger: nil},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := subsonic.NewHandler(tc.deps)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NewHandler() error = nil, want non-nil")
				}
				if h != nil {
					t.Fatalf("NewHandler() handler = %v, want nil", h)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewHandler() unexpected error = %v", err)
			}
			if h == nil {
				t.Fatalf("NewHandler() returned nil handler")
			}
		})
	}
}

func TestWriteOKAndWriteError_Direct(t *testing.T) {
	t.Run("WriteOK nil request defaults to XML and clears stray error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		subsonic.WriteOK(rec, nil, subsonic.Response{
			Status: "failed",
			Error:  &subsonic.APIError{Code: 99, Message: "should be cleared"},
		})
		res := rec.Result()
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
		}
		if got := res.Header.Get("Content-Type"); got != "application/xml; charset=utf-8" {
			t.Errorf("Content-Type = %q, want application/xml; charset=utf-8", got)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
		body, _ := io.ReadAll(res.Body)
		if got := res.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
			t.Errorf("Content-Length = %q, want %d", got, len(body))
		}
		env := decodeXML(t, body)
		assertEnvelopeFields(t, env, "ok")
		if env.Error != nil {
			t.Errorf("Error = %+v, want nil", env.Error)
		}
		assertNoCoverArt(t, body)
	})

	formatCases := []struct {
		name     string
		query    string
		wantJSON bool
	}{
		{name: "default empty", query: "", wantJSON: false},
		{name: "xml", query: "?f=xml", wantJSON: false},
		{name: "jsonp falls back to xml", query: "?f=jsonp", wantJSON: false},
		{name: "json lowercase", query: "?f=json", wantJSON: true},
		{name: "json uppercase trimmed", query: "?f=%20JSON%20", wantJSON: true},
	}

	contentTypes := []struct {
		name  string
		value string
	}{
		{name: "multipart", value: "multipart/form-data; boundary=abc"},
		{name: "urlencoded", value: "application/x-www-form-urlencoded"},
	}

	for _, tc := range formatCases {
		for _, ct := range contentTypes {
			t.Run("format_"+tc.name+"_"+ct.name, func(t *testing.T) {
				okBody := &trackingReadCloser{}
				okReq := httptest.NewRequest(http.MethodPost, "/rest/ping"+tc.query, okBody)
				okReq.Header.Set("Content-Type", ct.value)
				okRec := httptest.NewRecorder()

				subsonic.WriteOK(okRec, okReq, subsonic.Response{})
				if got := okBody.reads.Load(); got != 0 {
					t.Errorf("WriteOK body reads = %d, want 0", got)
				}
				okRes := okRec.Result()
				defer okRes.Body.Close()
				okPayload, _ := io.ReadAll(okRes.Body)

				errBody := &trackingReadCloser{}
				errReq := httptest.NewRequest(http.MethodPost, "/rest/ping"+tc.query, errBody)
				errReq.Header.Set("Content-Type", ct.value)
				errRec := httptest.NewRecorder()

				subsonic.WriteError(errRec, errReq, 70, "Not found")
				if got := errBody.reads.Load(); got != 0 {
					t.Errorf("WriteError body reads = %d, want 0", got)
				}
				errRes := errRec.Result()
				defer errRes.Body.Close()
				errPayload, _ := io.ReadAll(errRes.Body)

				if tc.wantJSON {
					if got := okRes.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
						t.Errorf("WriteOK Content-Type = %q, want application/json; charset=utf-8", got)
					}
					if got := errRes.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
						t.Errorf("WriteError Content-Type = %q, want application/json; charset=utf-8", got)
					}
					assertJSONEnvelopeFields(t, decodeJSON(t, okPayload), "ok")
					errMap := decodeJSON(t, errPayload)
					assertJSONEnvelopeFields(t, errMap, "failed")
					errObj, ok := errMap["error"].(map[string]any)
					if !ok {
						t.Fatalf("WriteError JSON error field missing or wrong type: %T", errMap["error"])
					}
					if code, ok := errObj["code"].(float64); !ok || code != 70 {
						t.Errorf("WriteError JSON error.code = %v (ok=%v), want 70", errObj["code"], ok)
					}
					if msg, _ := errObj["message"].(string); msg != "Not found" {
						t.Errorf("WriteError JSON error.message = %q, want %q", msg, "Not found")
					}
				} else {
					if got := okRes.Header.Get("Content-Type"); got != "application/xml; charset=utf-8" {
						t.Errorf("WriteOK Content-Type = %q, want application/xml; charset=utf-8", got)
					}
					if got := errRes.Header.Get("Content-Type"); got != "application/xml; charset=utf-8" {
						t.Errorf("WriteError Content-Type = %q, want application/xml; charset=utf-8", got)
					}
					assertEnvelopeFields(t, decodeXML(t, okPayload), "ok")
					errEnv := decodeXML(t, errPayload)
					assertEnvelopeFields(t, errEnv, "failed")
					if errEnv.Error == nil || errEnv.Error.Code != 70 || errEnv.Error.Message != "Not found" {
						t.Errorf("WriteError XML Error = %+v, want Code=70 Message=%q", errEnv.Error, "Not found")
					}
				}
				assertNoCoverArt(t, okPayload)
				assertNoCoverArt(t, errPayload)
			})
		}
	}

	prePopulatedCases := []struct {
		name     string
		target   string
		form     url.Values
		wantJSON bool
	}{
		{
			name:     "Form f=json overrides empty query",
			target:   "/rest/ping",
			form:     url.Values{"f": []string{"json"}},
			wantJSON: true,
		},
		{
			name:     "Form empty f overrides query f=json",
			target:   "/rest/ping?f=json",
			form:     url.Values{"f": []string{""}},
			wantJSON: false,
		},
	}

	for _, tc := range prePopulatedCases {
		t.Run("prepopulated_form_"+tc.name, func(t *testing.T) {
			okBody := &trackingReadCloser{}
			okReq := httptest.NewRequest(http.MethodPost, tc.target, okBody)
			okReq.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
			okReq.Form = tc.form
			okRec := httptest.NewRecorder()

			subsonic.WriteOK(okRec, okReq, subsonic.Response{})
			if got := okBody.reads.Load(); got != 0 {
				t.Errorf("WriteOK body reads = %d, want 0", got)
			}

			errBody := &trackingReadCloser{}
			errReq := httptest.NewRequest(http.MethodPost, tc.target, errBody)
			errReq.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
			errReq.Form = tc.form
			errRec := httptest.NewRecorder()

			subsonic.WriteError(errRec, errReq, 0, "Generic error")
			if got := errBody.reads.Load(); got != 0 {
				t.Errorf("WriteError body reads = %d, want 0", got)
			}

			wantContentType := "application/xml; charset=utf-8"
			if tc.wantJSON {
				wantContentType = "application/json; charset=utf-8"
			}
			if got := okRec.Header().Get("Content-Type"); got != wantContentType {
				t.Errorf("WriteOK Content-Type = %q, want %q", got, wantContentType)
			}
			if got := errRec.Header().Get("Content-Type"); got != wantContentType {
				t.Errorf("WriteError Content-Type = %q, want %q", got, wantContentType)
			}
			if tc.wantJSON {
				assertJSONEnvelopeFields(t, decodeJSON(t, okRec.Body.Bytes()), "ok")
				assertJSONEnvelopeFields(t, decodeJSON(t, errRec.Body.Bytes()), "failed")
			} else {
				assertEnvelopeFields(t, decodeXML(t, okRec.Body.Bytes()), "ok")
				assertEnvelopeFields(t, decodeXML(t, errRec.Body.Bytes()), "failed")
			}
		})
	}

	t.Run("WriteError code 0 always serializes and HEAD omits body", func(t *testing.T) {
		for _, wantJSON := range []bool{false, true} {
			target := "/rest/ping"
			if wantJSON {
				target += "?f=json"
			}
			getReq := httptest.NewRequest(http.MethodGet, target, nil)
			getRec := httptest.NewRecorder()
			subsonic.WriteError(getRec, getReq, 0, "Generic error")

			headReq := httptest.NewRequest(http.MethodHead, target, nil)
			headRec := httptest.NewRecorder()
			subsonic.WriteError(headRec, headReq, 0, "Generic error")

			getBody := getRec.Body.Bytes()
			if headRec.Body.Len() != 0 {
				t.Errorf("HEAD body length = %d, want 0", headRec.Body.Len())
			}
			if got, want := headRec.Header().Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
				t.Errorf("HEAD Content-Length = %q, want %q", got, want)
			}
			if got, want := headRec.Header().Get("Content-Type"), getRec.Header().Get("Content-Type"); got != want {
				t.Errorf("HEAD Content-Type = %q, want %q", got, want)
			}
			assertNoCoverArt(t, getBody)

			if wantJSON {
				m := decodeJSON(t, getBody)
				assertJSONEnvelopeFields(t, m, "failed")
				errObj, ok := m["error"].(map[string]any)
				if !ok {
					t.Fatalf("error field missing or wrong type: %T", m["error"])
				}
				if code, ok := errObj["code"].(float64); !ok || code != 0 {
					t.Errorf("error.code = %v (ok=%v), want 0", errObj["code"], ok)
				}
				if msg, _ := errObj["message"].(string); msg != "Generic error" {
					t.Errorf("error.message = %q, want %q", msg, "Generic error")
				}
			} else {
				env := decodeXML(t, getBody)
				assertEnvelopeFields(t, env, "failed")
				if env.Error == nil || env.Error.Code != 0 || env.Error.Message != "Generic error" {
					t.Errorf("XML Error = %+v, want Code=0 Message=%q", env.Error, "Generic error")
				}
			}
		}
	})
}

func TestDualRoutesAndHTTPMethods(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	h := newTestHandler(t, auth.NoopAuthenticator{}, scan)
	srv := httptest.NewServer(h)
	defer srv.Close()

	endpoints := []struct {
		name       string
		wantStatus string
	}{
		{name: "ping", wantStatus: "ok"},
		{name: "getLicense", wantStatus: "ok"},
		{name: "getOpenSubsonicExtensions", wantStatus: "ok"},
		{name: "tokenInfo", wantStatus: "failed"},
		{name: "getUser", wantStatus: "ok"},
		{name: "getScanStatus", wantStatus: "ok"},
		{name: "getGenres", wantStatus: "ok"},
		{name: "getStarred2", wantStatus: "ok"},
		{name: "getPlaylists", wantStatus: "ok"},
		{name: "getCoverArt", wantStatus: "failed"},
		{name: "scrobble", wantStatus: "ok"},
	}

	client := srv.Client()
	for _, ep := range endpoints {
		for _, suffix := range []string{"", ".view"} {
			path := "/rest/" + ep.name + suffix
			for _, format := range []string{"", "json"} {
				query := ""
				if format == "json" {
					query = "?f=json"
				}

				t.Run(ep.name+suffix+"_"+format, func(t *testing.T) {
					// GET
					getResp, err := client.Get(srv.URL + path + query)
					if err != nil {
						t.Fatalf("GET %s error = %v", path, err)
					}
					getBody, _ := io.ReadAll(getResp.Body)
					getResp.Body.Close()

					if getResp.StatusCode != http.StatusOK {
						t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
					}
					assertCORSHeaders(t, getResp.Header)
					if format == "json" {
						m := decodeJSON(t, getBody)
						assertJSONEnvelopeFields(t, m, ep.wantStatus)
					} else {
						env := decodeXML(t, getBody)
						assertEnvelopeFields(t, env, ep.wantStatus)
					}

					// HEAD
					headResp, err := client.Head(srv.URL + path + query)
					if err != nil {
						t.Fatalf("HEAD %s error = %v", path, err)
					}
					headBody, _ := io.ReadAll(headResp.Body)
					headResp.Body.Close()

					if headResp.StatusCode != http.StatusOK {
						t.Fatalf("HEAD status = %d, want 200", headResp.StatusCode)
					}
					assertCORSHeaders(t, headResp.Header)
					if len(headBody) != 0 {
						t.Errorf("HEAD body len = %d, want 0", len(headBody))
					}
					if got, want := headResp.Header.Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
						t.Errorf("HEAD Content-Length = %q, want %q", got, want)
					}
					if got, want := headResp.Header.Get("Content-Type"), getResp.Header.Get("Content-Type"); got != want {
						t.Errorf("HEAD Content-Type = %q, want %q", got, want)
					}

					// POST form body
					form := url.Values{}
					if format != "" {
						form.Set("f", format)
					}
					postFormResp, err := client.PostForm(srv.URL+path, form)
					if err != nil {
						t.Fatalf("POST form %s error = %v", path, err)
					}
					postFormBody, _ := io.ReadAll(postFormResp.Body)
					postFormResp.Body.Close()
					if postFormResp.StatusCode != http.StatusOK {
						t.Fatalf("POST form status = %d, want 200", postFormResp.StatusCode)
					}
					assertCORSHeaders(t, postFormResp.Header)
					if format == "json" {
						assertJSONEnvelopeFields(t, decodeJSON(t, postFormBody), ep.wantStatus)
					} else {
						assertEnvelopeFields(t, decodeXML(t, postFormBody), ep.wantStatus)
					}

					// POST query-only (empty body)
					postQueryResp, err := client.Post(srv.URL+path+query, "application/x-www-form-urlencoded", nil)
					if err != nil {
						t.Fatalf("POST query %s error = %v", path, err)
					}
					postQueryBody, _ := io.ReadAll(postQueryResp.Body)
					postQueryResp.Body.Close()
					if postQueryResp.StatusCode != http.StatusOK {
						t.Fatalf("POST query status = %d, want 200", postQueryResp.StatusCode)
					}
					assertCORSHeaders(t, postQueryResp.Header)
					if format == "json" {
						assertJSONEnvelopeFields(t, decodeJSON(t, postQueryBody), ep.wantStatus)
					} else {
						assertEnvelopeFields(t, decodeXML(t, postQueryBody), ep.wantStatus)
					}
				})
			}
		}
	}

	t.Run("OPTIONS preflight skips auth and form parsing", func(t *testing.T) {
		failingAuth := &stubAuthenticator{err: errors.New("should not run")}
		authHandler := newTestHandler(t, failingAuth, scan)
		authSrv := httptest.NewServer(authHandler)
		defer authSrv.Close()

		for _, path := range []string{"/rest/ping", "/rest/getOpenSubsonicExtensions", "/rest/tokenInfo", "/rest/unknownPath"} {
			req, err := http.NewRequest(http.MethodOptions, authSrv.URL+path, nil)
			if err != nil {
				t.Fatalf("NewRequest error = %v", err)
			}
			resp, err := authSrv.Client().Do(req)
			if err != nil {
				t.Fatalf("OPTIONS %s error = %v", path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("OPTIONS %s status = %d, want 204", path, resp.StatusCode)
			}
			if len(body) != 0 {
				t.Errorf("OPTIONS %s body len = %d, want 0", path, len(body))
			}
			assertCORSHeaders(t, resp.Header)
		}
		if calls := failingAuth.calls.Load(); calls != 0 {
			t.Errorf("Authenticator calls during OPTIONS = %d, want 0", calls)
		}
	})

	t.Run("unknown path returns 404 with CORS", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/rest/doesNotExist")
		if err != nil {
			t.Fatalf("GET unknown path error = %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		assertCORSHeaders(t, resp.Header)
	})

	t.Run("disallowed method returns 405 with Allow and CORS", func(t *testing.T) {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			req, err := http.NewRequest(method, srv.URL+"/rest/ping", nil)
			if err != nil {
				t.Fatalf("NewRequest error = %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s /rest/ping error = %v", method, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
				t.Errorf("%s Allow = %q, want GET and POST", method, allow)
			}
			assertCORSHeaders(t, resp.Header)
		}
	})
}

func TestPostOverQueryPrecedence(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	h := newTestHandler(t, auth.NoopAuthenticator{}, scan)

	tests := []struct {
		name         string
		target       string
		body         string
		wantJSON     bool
		wantUsername string
	}{
		{
			name:     "POST empty f overrides query f=json to XML",
			target:   "/rest/ping?f=json",
			body:     "f=",
			wantJSON: false,
		},
		{
			name:     "POST f=xml overrides query f=json to XML",
			target:   "/rest/ping?f=json",
			body:     "f=xml",
			wantJSON: false,
		},
		{
			name:     "POST f=json overrides query f=xml to JSON",
			target:   "/rest/ping?f=xml",
			body:     "f=json",
			wantJSON: true,
		},
		{
			name:         "POST empty username overrides query username and falls back to principal",
			target:       "/rest/getUser?username=alice",
			body:         "username=&u=bob",
			wantJSON:     false,
			wantUsername: "bob",
		},
		{
			name:         "POST empty username with JSON query overrides query username and falls back to principal",
			target:       "/rest/getUser?username=alice&f=json",
			body:         "username=&u=bob",
			wantJSON:     true,
			wantUsername: "bob",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			if tc.wantJSON {
				if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
					t.Errorf("Content-Type = %q, want %q", got, want)
				}
				m := decodeJSON(t, rec.Body.Bytes())
				assertJSONEnvelopeFields(t, m, "ok")
				if tc.wantUsername != "" {
					u, ok := m["user"].(map[string]any)
					if !ok {
						t.Fatalf("JSON user missing or wrong type: %T", m["user"])
					}
					if got, _ := u["username"].(string); got != tc.wantUsername {
						t.Errorf("user.username = %q, want %q", got, tc.wantUsername)
					}
				}
			} else {
				if got, want := rec.Header().Get("Content-Type"), "application/xml; charset=utf-8"; got != want {
					t.Errorf("Content-Type = %q, want %q", got, want)
				}
				env := decodeXML(t, rec.Body.Bytes())
				assertEnvelopeFields(t, env, "ok")
				if tc.wantUsername != "" {
					if env.User == nil {
						t.Fatalf("XML user = nil, want username %q", tc.wantUsername)
					}
					if env.User.Username != tc.wantUsername {
						t.Errorf("XML user.username = %q, want %q", env.User.Username, tc.wantUsername)
					}
				}
			}
		})
	}
}

func TestFormMiddlewareErrors(t *testing.T) {
	const limit = 1 << 20 // 1 MiB
	scan := &atomicScanStatusProvider{}

	t.Run("exact 1 MiB body succeeds", func(t *testing.T) {
		stubAuth := &stubAuthenticator{principal: auth.Principal{Username: "alice"}}
		h := newTestHandler(t, stubAuth, scan)

		prefix := []byte("u=")
		body := make([]byte, limit)
		copy(body, prefix)
		for i := len(prefix); i < len(body); i++ {
			body[i] = 'a'
		}

		req := httptest.NewRequest(http.MethodPost, "/rest/ping", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		env := decodeXML(t, rec.Body.Bytes())
		assertEnvelopeFields(t, env, "ok")
		if got := stubAuth.calls.Load(); got != 1 {
			t.Errorf("Authenticator calls = %d, want 1", got)
		}
	})

	oversizedBody := make([]byte, limit+1)
	copy(oversizedBody, "u=")
	for i := 2; i < len(oversizedBody); i++ {
		oversizedBody[i] = 'a'
	}

	errorCases := []struct {
		name        string
		target      string
		contentType string
		body        []byte
		nilBody     bool
		wantJSON    bool
		wantMsg     string
	}{
		{
			name:        "oversized XML",
			target:      "/rest/ping",
			contentType: "application/x-www-form-urlencoded",
			body:        oversizedBody,
			wantJSON:    false,
			wantMsg:     "Request body exceeds maximum size",
		},
		{
			name:        "oversized JSON",
			target:      "/rest/ping?f=json",
			contentType: "application/x-www-form-urlencoded",
			body:        oversizedBody,
			wantJSON:    true,
			wantMsg:     "Request body exceeds maximum size",
		},
		{
			name:        "malformed form body",
			target:      "/rest/ping",
			contentType: "application/x-www-form-urlencoded",
			body:        []byte("u=%ZZ"),
			wantJSON:    false,
			wantMsg:     "Malformed request parameters",
		},
		{
			name:        "malformed form body with json",
			target:      "/rest/ping?f=json",
			contentType: "application/x-www-form-urlencoded",
			body:        []byte("u=%ZZ"),
			wantJSON:    true,
			wantMsg:     "Malformed request parameters",
		},
		{
			name:     "malformed query string with json",
			target:   "/rest/ping?f=json&u=%ZZ",
			wantJSON: true,
			wantMsg:  "Malformed request parameters",
		},
		{
			name:        "malformed Content-Type",
			target:      "/rest/ping",
			contentType: "invalid/type; =",
			body:        []byte("u=alice"),
			wantJSON:    false,
			wantMsg:     "Malformed request parameters",
		},
		{
			name:        "POST with nil Body",
			target:      "/rest/ping",
			contentType: "application/x-www-form-urlencoded",
			nilBody:     true,
			wantJSON:    false,
			wantMsg:     "Malformed request parameters",
		},
	}

	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			stubAuth := &stubAuthenticator{principal: auth.Principal{Username: "alice"}}
			h := newTestHandler(t, stubAuth, scan)

			method := http.MethodGet
			var bodyReader io.Reader
			if tc.body != nil || tc.nilBody {
				method = http.MethodPost
			}
			if tc.body != nil {
				bodyReader = bytes.NewReader(tc.body)
			}
			req := httptest.NewRequest(method, tc.target, bodyReader)
			if tc.nilBody {
				req.Body = nil
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			assertCORSHeaders(t, rec.Header())
			if got := stubAuth.calls.Load(); got != 0 {
				t.Errorf("Authenticator calls = %d, want 0", got)
			}

			if tc.wantJSON {
				m := decodeJSON(t, rec.Body.Bytes())
				assertJSONEnvelopeFields(t, m, "failed")
				errObj := m["error"].(map[string]any)
				if code, _ := errObj["code"].(float64); code != 0 {
					t.Errorf("error.code = %v, want 0", errObj["code"])
				}
				if msg, _ := errObj["message"].(string); msg != tc.wantMsg {
					t.Errorf("error.message = %q, want %q", msg, tc.wantMsg)
				}
			} else {
				env := decodeXML(t, rec.Body.Bytes())
				assertEnvelopeFields(t, env, "failed")
				if env.Error == nil || env.Error.Code != 0 || env.Error.Message != tc.wantMsg {
					t.Errorf("XML error = %+v, want Code=0 Message=%q", env.Error, tc.wantMsg)
				}
			}
		})
	}
}

func TestAuthMiddlewareIntegration(t *testing.T) {
	scan := &atomicScanStatusProvider{}

	t.Run("failing authenticator emits error 40 without leaking internal error", func(t *testing.T) {
		stubAuth := &stubAuthenticator{err: errors.New("secret-db-connection-failed")}
		h := newTestHandler(t, stubAuth, scan)

		for _, tc := range []struct {
			name   string
			method string
			target string
			body   string
			isJSON bool
		}{
			{name: "GET XML", method: http.MethodGet, target: "/rest/ping"},
			{name: "GET JSON", method: http.MethodGet, target: "/rest/ping?f=json", isJSON: true},
			{name: "POST XML", method: http.MethodPost, target: "/rest/getUser", body: "u=alice"},
			{name: "POST JSON", method: http.MethodPost, target: "/rest/getUser", body: "f=json&u=alice", isJSON: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var bodyReader io.Reader
				if tc.method == http.MethodPost {
					bodyReader = strings.NewReader(tc.body)
				}
				req := httptest.NewRequest(tc.method, tc.target, bodyReader)
				if tc.method == http.MethodPost {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				raw := rec.Body.String()
				if strings.Contains(raw, "secret-db-connection-failed") {
					t.Fatalf("response leaked internal error: %s", raw)
				}

				if tc.isJSON {
					m := decodeJSON(t, rec.Body.Bytes())
					assertJSONEnvelopeFields(t, m, "failed")
					errObj := m["error"].(map[string]any)
					if code, _ := errObj["code"].(float64); code != 40 {
						t.Errorf("error.code = %v, want 40", errObj["code"])
					}
					if msg, _ := errObj["message"].(string); msg != "Wrong username or password" {
						t.Errorf("error.message = %q, want %q", msg, "Wrong username or password")
					}
				} else {
					env := decodeXML(t, rec.Body.Bytes())
					assertEnvelopeFields(t, env, "failed")
					if env.Error == nil || env.Error.Code != 40 || env.Error.Message != "Wrong username or password" {
						t.Errorf("XML error = %+v, want Code=40 Message=%q", env.Error, "Wrong username or password")
					}
				}
			})
		}
	})

	t.Run("NoopAuthenticator forwards u from query and form", func(t *testing.T) {
		h := newTestHandler(t, auth.NoopAuthenticator{}, scan)

		// Query u
		req := httptest.NewRequest(http.MethodGet, "/rest/getUser?u=alice&f=json", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		m := decodeJSON(t, rec.Body.Bytes())
		user := m["user"].(map[string]any)
		if user["username"] != "alice" {
			t.Errorf("username = %v, want alice", user["username"])
		}

		// Form u
		req = httptest.NewRequest(http.MethodPost, "/rest/getUser", strings.NewReader("u=bob&f=json"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		m = decodeJSON(t, rec.Body.Bytes())
		user = m["user"].(map[string]any)
		if user["username"] != "bob" {
			t.Errorf("username = %v, want bob", user["username"])
		}
	})
}

func TestSystemEndpoints(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	h := newTestHandler(t, auth.NoopAuthenticator{}, scan)

	t.Run("ping bare envelope", func(t *testing.T) {
		// XML
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/ping", nil))
		env := decodeXML(t, rec.Body.Bytes())
		assertEnvelopeFields(t, env, "ok")
		if env.Error != nil || env.License != nil || env.User != nil || env.ScanStatus != nil {
			t.Errorf("ping XML contained unexpected sub-elements: %+v", env)
		}

		// JSON
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/ping?f=json", nil))
		m := decodeJSON(t, rec.Body.Bytes())
		assertJSONEnvelopeFields(t, m, "ok")
		if len(m) != 5 {
			t.Errorf("ping JSON key count = %d (%v), want 5", len(m), m)
		}
	})

	t.Run("getLicense", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getLicense", nil))
		env := decodeXML(t, rec.Body.Bytes())
		if env.License == nil || !env.License.Valid {
			t.Errorf("XML License = %+v, want Valid=true", env.License)
		}

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getLicense?f=json", nil))
		m := decodeJSON(t, rec.Body.Bytes())
		lic, ok := m["license"].(map[string]any)
		if !ok || lic["valid"] != true {
			t.Errorf("JSON license = %v, want {valid: true}", m["license"])
		}
	})

	t.Run("getOpenSubsonicExtensions", func(t *testing.T) {
		// XML: zero <openSubsonicExtensions> elements
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getOpenSubsonicExtensions", nil))
		rawXML := rec.Body.String()
		if strings.Contains(rawXML, "openSubsonicExtensions") {
			t.Errorf("XML unexpectedly contains openSubsonicExtensions: %s", rawXML)
		}
		env := decodeXML(t, rec.Body.Bytes())
		if len(env.OpenSubsonicExtensions) != 0 {
			t.Errorf("XML OpenSubsonicExtensions len = %d, want 0", len(env.OpenSubsonicExtensions))
		}

		// JSON: "openSubsonicExtensions":[]
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getOpenSubsonicExtensions?f=json", nil))
		rawJSON := rec.Body.String()
		if !strings.Contains(rawJSON, `"openSubsonicExtensions":[]`) {
			t.Errorf(`JSON missing "openSubsonicExtensions":[]: %s`, rawJSON)
		}
		m := decodeJSON(t, rec.Body.Bytes())
		exts, ok := m["openSubsonicExtensions"].([]any)
		if !ok || len(exts) != 0 {
			t.Errorf("JSON openSubsonicExtensions = %#v, want empty slice", m["openSubsonicExtensions"])
		}
	})

	t.Run("getUser precedence and full schema", func(t *testing.T) {
		precedenceCases := []struct {
			name          string
			principalUser string
			query         string
			wantUser      string
		}{
			{name: "username overrides principal", principalUser: "alice", query: "?username=%20charlie%20&f=json", wantUser: "charlie"},
			{name: "blank username falls back to principal", principalUser: "alice", query: "?username=%20%20&f=json", wantUser: "alice"},
			{name: "blank principal falls back to headunit", principalUser: "  ", query: "?f=json", wantUser: "headunit"},
		}
		for _, tc := range precedenceCases {
			t.Run(tc.name, func(t *testing.T) {
				uh := newTestHandler(t, &stubAuthenticator{principal: auth.Principal{Username: tc.principalUser}}, scan)
				rec := httptest.NewRecorder()
				uh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getUser"+tc.query, nil))
				m := decodeJSON(t, rec.Body.Bytes())
				u := m["user"].(map[string]any)
				if u["username"] != tc.wantUser {
					t.Errorf("username = %v, want %q", u["username"], tc.wantUser)
				}
			})
		}

		// Verify all 13 booleans + folder: [1] in JSON and XML, and no coverArt key.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getUser?f=json", nil))
		assertNoCoverArt(t, rec.Body.Bytes())
		m := decodeJSON(t, rec.Body.Bytes())
		u := m["user"].(map[string]any)

		wantBools := map[string]bool{
			"scrobblingEnabled":   false,
			"adminRole":           false,
			"settingsRole":        false,
			"downloadRole":        false,
			"uploadRole":          false,
			"playlistRole":        false,
			"coverArtRole":        false,
			"commentRole":         false,
			"podcastRole":         false,
			"streamRole":          true,
			"jukeboxRole":         false,
			"shareRole":           false,
			"videoConversionRole": false,
		}
		for k, wantVal := range wantBools {
			gotVal, ok := u[k].(bool)
			if !ok || gotVal != wantVal {
				t.Errorf("JSON user[%q] = %v (present=%v), want %v", k, u[k], ok, wantVal)
			}
		}
		folders, ok := u["folder"].([]any)
		if !ok || len(folders) != 1 || folders[0] != float64(1) {
			t.Errorf("JSON user.folder = %#v, want [1]", u["folder"])
		}

		// XML check
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getUser", nil))
		assertNoCoverArt(t, rec.Body.Bytes())
		rawXML := rec.Body.String()
		for k, wantVal := range wantBools {
			needle := k + `="` + strconv.FormatBool(wantVal) + `"`
			if !strings.Contains(rawXML, needle) {
				t.Errorf("XML missing %s in %s", needle, rawXML)
			}
		}
		env := decodeXML(t, rec.Body.Bytes())
		if env.User == nil || !slices.Equal(env.User.Folder, []int{1}) || !env.User.StreamRole || env.User.DownloadRole {
			t.Errorf("XML User = %+v", env.User)
		}
	})

	t.Run("getScanStatus zero and non-UTC", func(t *testing.T) {
		est := time.FixedZone("EST", -5*3600)
		localScan := &atomicScanStatusProvider{zone: est}
		sh := newTestHandler(t, auth.NoopAuthenticator{}, localScan)

		// Zero state: scanning=false, count=0 present, lastScan omitted
		rec := httptest.NewRecorder()
		sh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getScanStatus?f=json", nil))
		m := decodeJSON(t, rec.Body.Bytes())
		ss := m["scanStatus"].(map[string]any)
		if v, ok := ss["scanning"].(bool); !ok || v {
			t.Errorf("JSON scanning = %v (ok=%v), want false", ss["scanning"], ok)
		}
		if v, ok := ss["count"].(float64); !ok || v != 0 {
			t.Errorf("JSON count = %v (ok=%v), want 0", ss["count"], ok)
		}
		if _, exists := ss["lastScan"]; exists {
			t.Errorf("JSON lastScan unexpectedly present: %v", ss["lastScan"])
		}

		rec = httptest.NewRecorder()
		sh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getScanStatus", nil))
		if strings.Contains(rec.Body.String(), "lastScan=") {
			t.Errorf("XML lastScan unexpectedly present: %s", rec.Body.String())
		}
		env := decodeXML(t, rec.Body.Bytes())
		if env.ScanStatus == nil || env.ScanStatus.Scanning || env.ScanStatus.Count != 0 || env.ScanStatus.LastScan != "" {
			t.Errorf("XML ScanStatus = %+v", env.ScanStatus)
		}

		// Non-zero non-UTC time -> converted to UTC RFC3339 (Z)
		ts := time.Date(2026, 11, 26, 13, 15, 30, 0, est)
		localScan.set(true, 42, ts)

		rec = httptest.NewRecorder()
		sh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getScanStatus?f=json", nil))
		m = decodeJSON(t, rec.Body.Bytes())
		ss = m["scanStatus"].(map[string]any)
		if ss["scanning"] != true || ss["count"] != float64(42) || ss["lastScan"] != "2026-11-26T18:15:30Z" {
			t.Errorf("JSON scanStatus = %+v", ss)
		}

		rec = httptest.NewRecorder()
		sh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getScanStatus", nil))
		env = decodeXML(t, rec.Body.Bytes())
		if env.ScanStatus == nil || !env.ScanStatus.Scanning || env.ScanStatus.Count != 42 || env.ScanStatus.LastScan != "2026-11-26T18:15:30Z" {
			t.Errorf("XML ScanStatus = %+v", env.ScanStatus)
		}
	})
}

func TestCompatibilityStubs(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	h := newTestHandler(t, auth.NoopAuthenticator{}, scan)

	t.Run("getGenres", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getGenres?f=json", nil))
		if !strings.Contains(rec.Body.String(), `"genres":{"genre":[]}`) {
			t.Errorf("unexpected JSON getGenres: %s", rec.Body.String())
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getGenres", nil))
		if !strings.Contains(rec.Body.String(), "<genres></genres>") {
			t.Errorf("unexpected XML getGenres: %s", rec.Body.String())
		}
	})

	t.Run("getStarred2", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getStarred2?f=json", nil))
		if !strings.Contains(rec.Body.String(), `"starred2":{"artist":[],"album":[],"song":[]}`) {
			t.Errorf("unexpected JSON getStarred2: %s", rec.Body.String())
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getStarred2", nil))
		if !strings.Contains(rec.Body.String(), "<starred2></starred2>") {
			t.Errorf("unexpected XML getStarred2: %s", rec.Body.String())
		}
	})

	t.Run("getPlaylists", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getPlaylists?f=json", nil))
		if !strings.Contains(rec.Body.String(), `"playlists":{"playlist":[]}`) {
			t.Errorf("unexpected JSON getPlaylists: %s", rec.Body.String())
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getPlaylists", nil))
		if !strings.Contains(rec.Body.String(), "<playlists></playlists>") {
			t.Errorf("unexpected XML getPlaylists: %s", rec.Body.String())
		}
	})

	t.Run("scrobble", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/scrobble?id=tr_1&f=json", nil))
		m := decodeJSON(t, rec.Body.Bytes())
		assertJSONEnvelopeFields(t, m, "ok")
		if len(m) != 5 {
			t.Errorf("scrobble JSON keys = %v, want bare envelope", m)
		}

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/scrobble?id=tr_1", nil))
		env := decodeXML(t, rec.Body.Bytes())
		assertEnvelopeFields(t, env, "ok")
	})

	t.Run("getCoverArt returns HTTP 200 and Subsonic error 70", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getCoverArt?id=al_1&f=json", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		m := decodeJSON(t, rec.Body.Bytes())
		assertJSONEnvelopeFields(t, m, "failed")
		errObj := m["error"].(map[string]any)
		if code, _ := errObj["code"].(float64); code != 70 {
			t.Errorf("error.code = %v, want 70", errObj["code"])
		}

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getCoverArt?id=al_1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		env := decodeXML(t, rec.Body.Bytes())
		assertEnvelopeFields(t, env, "failed")
		if env.Error == nil || env.Error.Code != 70 {
			t.Errorf("XML error = %+v, want Code=70", env.Error)
		}
	})
}

func TestHealthz(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	failingAuth := &stubAuthenticator{err: errors.New("auth must not run")}
	h := newTestHandler(t, failingAuth, scan)
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := srv.Client()

	assertHealthzHeaders := func(t *testing.T, hdr http.Header) {
		t.Helper()
		if got := hdr.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type = %q, want %q", got, "text/plain; charset=utf-8")
		}
		if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
		}
		if got := hdr.Get("Content-Length"); got != "3" {
			t.Errorf("Content-Length = %q, want %q", got, "3")
		}
		if got := hdr.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	}

	t.Run("GET /healthz", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz error = %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		assertHealthzHeaders(t, resp.Header)
		if string(body) != "ok\n" {
			t.Errorf("body = %q, want %q", string(body), "ok\n")
		}
	})

	t.Run("HEAD /healthz", func(t *testing.T) {
		resp, err := client.Head(srv.URL + "/healthz")
		if err != nil {
			t.Fatalf("HEAD /healthz error = %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		assertHealthzHeaders(t, resp.Header)
		if len(body) != 0 {
			t.Errorf("HEAD body len = %d, want 0", len(body))
		}

		req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("recorder status = %d, want 200", rec.Code)
		}
		assertHealthzHeaders(t, rec.Header())
		if rec.Body.Len() != 0 {
			t.Errorf("recorder HEAD body = %q (len=%d), want empty", rec.Body.String(), rec.Body.Len())
		}
	})

	t.Run("GET /healthz with malformed query string bypasses form parsing", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/healthz?bad=%ZZ")
		if err != nil {
			t.Fatalf("GET /healthz?bad=%%ZZ error = %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if string(body) != "ok\n" {
			t.Errorf("body = %q, want %q", string(body), "ok\n")
		}
	})

	t.Run("disallowed methods return 405 without CORS", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			req, err := http.NewRequest(method, srv.URL+"/healthz", nil)
			if err != nil {
				t.Fatalf("NewRequest error = %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s /healthz error = %v", method, err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "HEAD") {
				t.Errorf("%s Allow = %q, want GET and HEAD", method, allow)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("%s Access-Control-Allow-Origin = %q, want empty", method, got)
			}
		}
	})

	t.Run("unknown non-rest path returns 404", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/doesNotExist")
		if err != nil {
			t.Fatalf("GET /doesNotExist error = %v", err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})

	if calls := failingAuth.calls.Load(); calls != 0 {
		t.Errorf("Authenticator calls during /healthz tests = %d, want 0", calls)
	}
}

func TestGetOpenSubsonicExtensionsPublicBypass(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	failingAuth := &stubAuthenticator{err: errors.New("auth must not run")}
	h := newTestHandler(t, failingAuth, scan)
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := srv.Client()

	assertEmptyExtensions := func(t *testing.T, body []byte, isJSON bool) {
		t.Helper()
		if isJSON {
			if !strings.Contains(string(body), `"openSubsonicExtensions":[]`) {
				t.Errorf(`JSON missing "openSubsonicExtensions":[]: %s`, string(body))
			}
			m := decodeJSON(t, body)
			assertJSONEnvelopeFields(t, m, "ok")
			exts, ok := m["openSubsonicExtensions"].([]any)
			if !ok || len(exts) != 0 {
				t.Errorf("JSON openSubsonicExtensions = %#v, want empty slice", m["openSubsonicExtensions"])
			}
			return
		}
		if strings.Contains(string(body), "openSubsonicExtensions") {
			t.Errorf("XML unexpectedly contains openSubsonicExtensions: %s", string(body))
		}
		env := decodeXML(t, body)
		assertEnvelopeFields(t, env, "ok")
		if len(env.OpenSubsonicExtensions) != 0 {
			t.Errorf("XML OpenSubsonicExtensions len = %d, want 0", len(env.OpenSubsonicExtensions))
		}
	}

	for _, suffix := range []string{"", ".view"} {
		path := "/rest/getOpenSubsonicExtensions" + suffix
		for _, format := range []string{"", "json"} {
			isJSON := format == "json"
			query := ""
			if isJSON {
				query = "?f=json"
			}

			t.Run("routes"+suffix+"_"+format, func(t *testing.T) {
				// GET
				getResp, err := client.Get(srv.URL + path + query)
				if err != nil {
					t.Fatalf("GET %s error = %v", path, err)
				}
				getBody, _ := io.ReadAll(getResp.Body)
				getResp.Body.Close()
				if getResp.StatusCode != http.StatusOK {
					t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
				}
				assertCORSHeaders(t, getResp.Header)
				assertEmptyExtensions(t, getBody, isJSON)

				// HEAD
				headResp, err := client.Head(srv.URL + path + query)
				if err != nil {
					t.Fatalf("HEAD %s error = %v", path, err)
				}
				headBody, _ := io.ReadAll(headResp.Body)
				headResp.Body.Close()
				if headResp.StatusCode != http.StatusOK {
					t.Fatalf("HEAD status = %d, want 200", headResp.StatusCode)
				}
				assertCORSHeaders(t, headResp.Header)
				if len(headBody) != 0 {
					t.Errorf("HEAD body len = %d, want 0", len(headBody))
				}
				if got, want := headResp.Header.Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
					t.Errorf("HEAD Content-Length = %q, want %q", got, want)
				}

				// POST form
				form := url.Values{}
				if isJSON {
					form.Set("f", "json")
				}
				postFormResp, err := client.PostForm(srv.URL+path, form)
				if err != nil {
					t.Fatalf("POST form %s error = %v", path, err)
				}
				postFormBody, _ := io.ReadAll(postFormResp.Body)
				postFormResp.Body.Close()
				if postFormResp.StatusCode != http.StatusOK {
					t.Fatalf("POST form status = %d, want 200", postFormResp.StatusCode)
				}
				assertCORSHeaders(t, postFormResp.Header)
				assertEmptyExtensions(t, postFormBody, isJSON)

				// POST query-only
				postQueryResp, err := client.Post(srv.URL+path+query, "application/x-www-form-urlencoded", nil)
				if err != nil {
					t.Fatalf("POST query %s error = %v", path, err)
				}
				postQueryBody, _ := io.ReadAll(postQueryResp.Body)
				postQueryResp.Body.Close()
				if postQueryResp.StatusCode != http.StatusOK {
					t.Fatalf("POST query status = %d, want 200", postQueryResp.StatusCode)
				}
				assertCORSHeaders(t, postQueryResp.Header)
				assertEmptyExtensions(t, postQueryBody, isJSON)
			})
		}
	}

	t.Run("disallowed methods return 405 with Allow and CORS without auth", func(t *testing.T) {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			req, err := http.NewRequest(method, srv.URL+"/rest/getOpenSubsonicExtensions", nil)
			if err != nil {
				t.Fatalf("NewRequest error = %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s /rest/getOpenSubsonicExtensions error = %v", method, err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
				t.Errorf("%s Allow = %q, want GET and POST", method, allow)
			}
			assertCORSHeaders(t, resp.Header)
		}
	})

	t.Run("malformed POST body returns error 0 with CORS without auth", func(t *testing.T) {
		resp, err := client.Post(srv.URL+"/rest/getOpenSubsonicExtensions?f=json", "application/x-www-form-urlencoded", strings.NewReader("bad=%ZZ"))
		if err != nil {
			t.Fatalf("POST malformed body error = %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		assertCORSHeaders(t, resp.Header)
		m := decodeJSON(t, body)
		assertJSONEnvelopeFields(t, m, "failed")
		errObj, ok := m["error"].(map[string]any)
		if !ok {
			t.Fatalf("JSON missing error object: %s", string(body))
		}
		if code, ok := errObj["code"].(float64); !ok || code != 0 {
			t.Errorf("error.code = %v (ok=%v), want 0", errObj["code"], ok)
		}
	})

	if calls := failingAuth.calls.Load(); calls != 0 {
		t.Fatalf("Authenticator calls before /rest/ping = %d, want 0", calls)
	}

	t.Run("authenticated endpoint on same handler still enforces auth", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/rest/ping")
		if err != nil {
			t.Fatalf("GET /rest/ping error = %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		env := decodeXML(t, body)
		assertEnvelopeFields(t, env, "failed")
		if env.Error == nil || env.Error.Code != 40 {
			t.Errorf("XML error = %+v, want Code=40", env.Error)
		}
		if calls := failingAuth.calls.Load(); calls != 1 {
			t.Errorf("Authenticator calls after /rest/ping = %d, want 1", calls)
		}
	})
}

func TestTokenInfo(t *testing.T) {
	scan := &atomicScanStatusProvider{}

	assertTokenInfoOK := func(t *testing.T, body []byte, isJSON bool, wantUsername string) {
		t.Helper()
		assertNoCoverArt(t, body)
		if isJSON {
			m := decodeJSON(t, body)
			assertJSONEnvelopeFields(t, m, "ok")
			ti, ok := m["tokenInfo"].(map[string]any)
			if !ok {
				t.Fatalf("JSON missing tokenInfo object: %s", string(body))
			}
			if ti["username"] != wantUsername {
				t.Errorf("JSON tokenInfo.username = %v, want %q", ti["username"], wantUsername)
			}
			return
		}
		env := decodeXML(t, body)
		assertEnvelopeFields(t, env, "ok")
		if env.TokenInfo == nil || env.TokenInfo.Username != wantUsername {
			t.Errorf("XML TokenInfo = %+v, want Username=%q", env.TokenInfo, wantUsername)
		}
		wantTag := `<tokenInfo username="` + wantUsername + `"></tokenInfo>`
		if !strings.Contains(string(body), wantTag) {
			t.Errorf("XML = %s, want substring %s", string(body), wantTag)
		}
	}

	assertErrorEnvelope := func(t *testing.T, body []byte, isJSON bool, wantCode int, wantMsg string) {
		t.Helper()
		if isJSON {
			m := decodeJSON(t, body)
			assertJSONEnvelopeFields(t, m, "failed")
			if _, exists := m["tokenInfo"]; exists {
				t.Errorf("JSON unexpectedly included tokenInfo on error: %s", string(body))
			}
			errObj, ok := m["error"].(map[string]any)
			if !ok {
				t.Fatalf("JSON missing error object: %s", string(body))
			}
			if code, ok := errObj["code"].(float64); !ok || int(code) != wantCode {
				t.Errorf("error.code = %v (ok=%v), want %d", errObj["code"], ok, wantCode)
			}
			if msg, _ := errObj["message"].(string); msg != wantMsg {
				t.Errorf("error.message = %q, want %q", msg, wantMsg)
			}
			return
		}
		env := decodeXML(t, body)
		assertEnvelopeFields(t, env, "failed")
		if env.TokenInfo != nil {
			t.Errorf("XML unexpectedly included TokenInfo on error: %+v", env.TokenInfo)
		}
		if env.Error == nil || env.Error.Code != wantCode || env.Error.Message != wantMsg {
			t.Errorf("XML error = %+v, want Code=%d Message=%q", env.Error, wantCode, wantMsg)
		}
	}

	t.Run("API key principal succeeds across routes, methods, and formats", func(t *testing.T) {
		h := newTestHandler(t, &stubAuthenticator{
			principal: auth.Principal{Username: "alice", Method: auth.AuthMethodAPIKey},
		}, scan)
		srv := httptest.NewServer(h)
		defer srv.Close()
		client := srv.Client()

		for _, suffix := range []string{"", ".view"} {
			path := "/rest/tokenInfo" + suffix
			for _, format := range []string{"", "json"} {
				isJSON := format == "json"
				query := ""
				if isJSON {
					query = "?f=json"
				}

				t.Run("tokenInfo"+suffix+"_"+format, func(t *testing.T) {
					// GET
					getResp, err := client.Get(srv.URL + path + query)
					if err != nil {
						t.Fatalf("GET %s error = %v", path, err)
					}
					getBody, _ := io.ReadAll(getResp.Body)
					getResp.Body.Close()
					if getResp.StatusCode != http.StatusOK {
						t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
					}
					assertCORSHeaders(t, getResp.Header)
					assertTokenInfoOK(t, getBody, isJSON, "alice")

					// HEAD
					headResp, err := client.Head(srv.URL + path + query)
					if err != nil {
						t.Fatalf("HEAD %s error = %v", path, err)
					}
					headBody, _ := io.ReadAll(headResp.Body)
					headResp.Body.Close()
					if headResp.StatusCode != http.StatusOK {
						t.Fatalf("HEAD status = %d, want 200", headResp.StatusCode)
					}
					assertCORSHeaders(t, headResp.Header)
					if len(headBody) != 0 {
						t.Errorf("HEAD body len = %d, want 0", len(headBody))
					}
					if got, want := headResp.Header.Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
						t.Errorf("HEAD Content-Length = %q, want %q", got, want)
					}

					// POST form
					form := url.Values{}
					if isJSON {
						form.Set("f", "json")
					}
					postFormResp, err := client.PostForm(srv.URL+path, form)
					if err != nil {
						t.Fatalf("POST form %s error = %v", path, err)
					}
					postFormBody, _ := io.ReadAll(postFormResp.Body)
					postFormResp.Body.Close()
					if postFormResp.StatusCode != http.StatusOK {
						t.Fatalf("POST form status = %d, want 200", postFormResp.StatusCode)
					}
					assertCORSHeaders(t, postFormResp.Header)
					assertTokenInfoOK(t, postFormBody, isJSON, "alice")

					// POST query-only
					postQueryResp, err := client.Post(srv.URL+path+query, "application/x-www-form-urlencoded", nil)
					if err != nil {
						t.Fatalf("POST query %s error = %v", path, err)
					}
					postQueryBody, _ := io.ReadAll(postQueryResp.Body)
					postQueryResp.Body.Close()
					if postQueryResp.StatusCode != http.StatusOK {
						t.Fatalf("POST query status = %d, want 200", postQueryResp.StatusCode)
					}
					assertCORSHeaders(t, postQueryResp.Header)
					assertTokenInfoOK(t, postQueryBody, isJSON, "alice")
				})
			}
		}
	})

	t.Run("non-API-key auth methods return error 42", func(t *testing.T) {
		methods := []struct {
			name   string
			method auth.AuthMethod
		}{
			{name: "token", method: auth.AuthMethodToken},
			{name: "noop", method: auth.AuthMethodNoop},
			{name: "empty", method: ""},
		}
		for _, tc := range methods {
			t.Run(tc.name, func(t *testing.T) {
				h := newTestHandler(t, &stubAuthenticator{
					principal: auth.Principal{Username: "alice", Method: tc.method},
				}, scan)

				for _, format := range []string{"", "json"} {
					isJSON := format == "json"
					target := "/rest/tokenInfo"
					if isJSON {
						target += "?f=json"
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

					if rec.Code != http.StatusOK {
						t.Fatalf("status = %d, want 200", rec.Code)
					}
					assertCORSHeaders(t, rec.Header())
					assertErrorEnvelope(t, rec.Body.Bytes(), isJSON, 42, "Provided authentication mechanism not supported")
				}
			})
		}
	})

	t.Run("auth middleware errors propagate safely", func(t *testing.T) {
		cases := []struct {
			name     string
			err      error
			wantCode int
			wantMsg  string
			secret   string
		}{
			{
				name:     "invalid API key returns 44",
				err:      &auth.AuthError{Code: auth.ErrCodeInvalidAPIKey},
				wantCode: 44,
				wantMsg:  "Invalid API key",
			},
			{
				name:     "wrong credentials returns 40",
				err:      &auth.AuthError{Code: auth.ErrCodeWrongCredentials},
				wantCode: 40,
				wantMsg:  "Wrong username or password",
			},
			{
				name:     "untyped internal error returns 40 without leaking details",
				err:      errors.New("secret-db-connection-failed"),
				wantCode: 40,
				wantMsg:  "Wrong username or password",
				secret:   "secret-db-connection-failed",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := newTestHandler(t, &stubAuthenticator{err: tc.err}, scan)

				for _, format := range []string{"", "json"} {
					isJSON := format == "json"
					target := "/rest/tokenInfo"
					if isJSON {
						target += "?f=json"
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

					if rec.Code != http.StatusOK {
						t.Fatalf("status = %d, want 200", rec.Code)
					}
					assertCORSHeaders(t, rec.Header())
					if tc.secret != "" && strings.Contains(rec.Body.String(), tc.secret) {
						t.Fatalf("response leaked internal error: %s", rec.Body.String())
					}
					assertErrorEnvelope(t, rec.Body.Bytes(), isJSON, tc.wantCode, tc.wantMsg)
				}
			})
		}
	})

	t.Run("disallowed methods return 405 with Allow and CORS", func(t *testing.T) {
		h := newTestHandler(t, &stubAuthenticator{
			principal: auth.Principal{Username: "alice", Method: auth.AuthMethodAPIKey},
		}, scan)
		srv := httptest.NewServer(h)
		defer srv.Close()
		client := srv.Client()

		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			req, err := http.NewRequest(method, srv.URL+"/rest/tokenInfo", nil)
			if err != nil {
				t.Fatalf("NewRequest error = %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s /rest/tokenInfo error = %v", method, err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
				t.Errorf("%s Allow = %q, want GET and POST", method, allow)
			}
			assertCORSHeaders(t, resp.Header)
		}
	})
}

func TestConcurrentRequests(t *testing.T) {
	scan := &atomicScanStatusProvider{}
	h := newTestHandler(t, auth.NoopAuthenticator{}, scan)
	srv := httptest.NewServer(h)
	defer srv.Close()

	const workers = 32
	const iterations = 12
	paths := []string{
		"/rest/ping",
		"/rest/ping.view?f=json",
		"/rest/getLicense",
		"/rest/getOpenSubsonicExtensions?f=json",
		"/rest/tokenInfo?f=json",
		"/rest/getUser?u=listener",
		"/rest/getScanStatus?f=json",
		"/rest/getGenres?f=json",
		"/rest/getStarred2",
		"/rest/getPlaylists?f=json",
		"/rest/getCoverArt",
		"/rest/scrobble.view",
	}

	var wg sync.WaitGroup
	errCh := make(chan error, workers*iterations)

	// Concurrently mutate scanner state while serving requests.
	stopUpdates := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		var n int64
		for {
			select {
			case <-stopUpdates:
				return
			default:
				n++
				scan.set(n%2 == 0, n, time.Unix(1795716930+n, 0).UTC())
			}
		}
	}()

	var clientWG sync.WaitGroup
	client := srv.Client()
	for w := range workers {
		clientWG.Add(1)
		go func(workerID int) {
			defer clientWG.Done()
			for i := range iterations {
				idx := (workerID + i) % len(paths)
				target := srv.URL + paths[idx]

				var (
					resp *http.Response
					err  error
				)
				switch workerID % 6 {
				case 0:
					resp, err = client.Get(target)
				case 1:
					resp, err = client.Head(target)
				case 2:
					resp, err = client.PostForm(srv.URL+"/rest/getUser", url.Values{"f": {"json"}, "username": {"user"}})
				case 3:
					var req *http.Request
					req, err = http.NewRequest(http.MethodOptions, target, nil)
					if err == nil {
						resp, err = client.Do(req)
					}
				case 4:
					resp, err = client.Get(srv.URL + "/healthz")
				case 5:
					resp, err = client.Head(srv.URL + "/healthz")
				}
				if err != nil {
					errCh <- err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
					errCh <- errors.New("unexpected status: " + strconv.Itoa(resp.StatusCode))
					return
				}
			}
		}(w)
	}

	clientWG.Wait()
	close(stopUpdates)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent request failed: %v", err)
	}
}
