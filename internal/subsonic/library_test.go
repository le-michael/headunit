package subsonic_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"headunit/internal/auth"
	"headunit/internal/store"
	"headunit/internal/subsonic"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close() error = %v", err)
		}
	})
	return st
}

func seedTracks(t *testing.T, st *store.Store, tracks ...store.Track) {
	t.Helper()
	if err := st.ApplyScanBatch(context.Background(), tracks, nil); err != nil {
		t.Fatalf("ApplyScanBatch() error = %v", err)
	}
}

func newLibraryHandler(t *testing.T, st subsonic.LibraryStore, logger *slog.Logger) http.Handler {
	t.Helper()
	h, err := subsonic.NewHandler(subsonic.Dependencies{
		Auth:     auth.NoopAuthenticator{},
		Scanner:  &atomicScanStatusProvider{},
		Store:    st,
		Users:    &stubUserStore{},
		Streamer: noopStreamer{},
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return h
}

func execSubsonicRequest(t *testing.T, h http.Handler, method, target, formBody string) (*http.Response, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if formBody != "" {
		bodyReader = strings.NewReader(formBody)
	}
	req := httptest.NewRequest(method, target, bodyReader)
	if formBody != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	return res, payload
}

func mustJSONMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	val, exists := parent[key]
	if !exists {
		t.Fatalf("missing JSON key %q in %v", key, parent)
	}
	m, ok := val.(map[string]any)
	if !ok {
		t.Fatalf("JSON key %q has type %T, want map[string]any", key, val)
	}
	return m
}

func mustJSONSlice(t *testing.T, parent map[string]any, key string) []any {
	t.Helper()
	val, exists := parent[key]
	if !exists {
		t.Fatalf("missing JSON key %q in %v", key, parent)
	}
	s, ok := val.([]any)
	if !ok {
		t.Fatalf("JSON key %q has type %T, want []any (non-null slice)", key, val)
	}
	return s
}

func mustJSONItemMap(t *testing.T, items []any, idx int) map[string]any {
	t.Helper()
	if idx >= len(items) {
		t.Fatalf("slice len = %d, want > %d", len(items), idx)
	}
	m, ok := items[idx].(map[string]any)
	if !ok {
		t.Fatalf("slice[%d] has type %T, want map[string]any", idx, items[idx])
	}
	return m
}

func mustJSONString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	val, exists := m[key]
	if !exists {
		t.Fatalf("missing JSON key %q in %v", key, m)
	}
	s, ok := val.(string)
	if !ok {
		t.Fatalf("JSON key %q = %v (%T), want string", key, val, val)
	}
	return s
}

func mustJSONFloat(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	val, exists := m[key]
	if !exists {
		t.Fatalf("missing JSON key %q in %v", key, m)
	}
	n, ok := val.(float64)
	if !ok {
		t.Fatalf("JSON key %q = %v (%T), want float64", key, val, val)
	}
	return n
}

func mustJSONBool(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	val, exists := m[key]
	if !exists {
		t.Fatalf("missing JSON key %q in %v", key, m)
	}
	b, ok := val.(bool)
	if !ok {
		t.Fatalf("JSON key %q = %v (%T), want bool", key, val, val)
	}
	return b
}

type spyLibraryStore struct {
	artists         []store.ArtistSummary
	artist          store.ArtistSummary
	artistAlbums    []store.AlbumSummary
	album           store.AlbumSummary
	albumTracks     []store.Track
	listAlbumsOut   []store.AlbumSummary
	track           *store.Track
	search3Out      store.Search3Result
	err             error
	listAlbumsCalls int
	lastAlbumQuery  store.AlbumListQuery
	lastSearchQuery store.Search3Query
}

func (s *spyLibraryStore) ListArtists(context.Context) ([]store.ArtistSummary, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.artists, nil
}

func (s *spyLibraryStore) GetArtist(context.Context, string) (store.ArtistSummary, []store.AlbumSummary, error) {
	if s.err != nil {
		return store.ArtistSummary{}, nil, s.err
	}
	return s.artist, s.artistAlbums, nil
}

func (s *spyLibraryStore) GetAlbum(context.Context, string) (store.AlbumSummary, []store.Track, error) {
	if s.err != nil {
		return store.AlbumSummary{}, nil, s.err
	}
	return s.album, s.albumTracks, nil
}

func (s *spyLibraryStore) ListAlbums(_ context.Context, q store.AlbumListQuery) ([]store.AlbumSummary, error) {
	s.listAlbumsCalls++
	s.lastAlbumQuery = q
	if s.err != nil {
		return nil, s.err
	}
	return s.listAlbumsOut, nil
}

func (s *spyLibraryStore) GetTrack(context.Context, string) (*store.Track, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.track, nil
}

func (s *spyLibraryStore) Search3(_ context.Context, q store.Search3Query) (store.Search3Result, error) {
	s.lastSearchQuery = q
	if s.err != nil {
		return store.Search3Result{}, s.err
	}
	return s.search3Out, nil
}

func TestLibraryDualRoutesAndMethods(t *testing.T) {
	st := openTestStore(t)
	created := time.Date(2026, 2, 10, 8, 30, 0, 0, time.UTC)
	seedTracks(t, st, store.Track{
		ID:          "tr_route_01",
		RelPath:     "miles/kind_of_blue/01.mp3",
		FileSize:    4096,
		MtimeNs:     12345,
		Title:       "So What",
		Artist:      "Miles Davis",
		AlbumArtist: "Miles Davis",
		Album:       "Kind of Blue",
		TrackNum:    1,
		DiscNum:     1,
		Year:        1959,
		Genre:       "Jazz",
		DurationSec: 540,
		BitRate:     320,
		CreatedAt:   created,
	})
	artistID := store.ArtistID("Miles Davis")
	albumID := store.AlbumID("Miles Davis", "Kind of Blue")
	h := newLibraryHandler(t, st, nil)

	endpoints := []struct {
		name  string
		query string
	}{
		{name: "getMusicFolders", query: ""},
		{name: "getArtists", query: ""},
		{name: "getArtist", query: "id=" + artistID},
		{name: "getAlbum", query: "id=" + albumID},
		{name: "getAlbumList2", query: "type=alphabeticalByName"},
		{name: "getSong", query: "id=tr_route_01"},
		{name: "search3", query: "query=Miles"},
	}

	for _, ep := range endpoints {
		for _, suffix := range []string{"", ".view"} {
			for _, format := range []string{"xml", "json"} {
				t.Run(ep.name+suffix+"_"+format, func(t *testing.T) {
					params := url.Values{}
					if ep.query != "" {
						qVals, err := url.ParseQuery(ep.query)
						if err != nil {
							t.Fatalf("ParseQuery(%q) error = %v", ep.query, err)
						}
						for k, vs := range qVals {
							params[k] = vs
						}
					}
					if format == "json" {
						params.Set("f", "json")
					}
					encoded := params.Encode()
					basePath := "/rest/" + ep.name + suffix
					getTarget := basePath
					if encoded != "" {
						getTarget += "?" + encoded
					}

					getRes, getBody := execSubsonicRequest(t, h, http.MethodGet, getTarget, "")
					if getRes.StatusCode != http.StatusOK {
						t.Fatalf("GET status = %d, want 200", getRes.StatusCode)
					}
					assertCORSHeaders(t, getRes.Header)
					assertNoCoverArt(t, getBody)

					if format == "json" {
						respMap := decodeJSON(t, getBody)
						assertJSONEnvelopeFields(t, respMap, "ok")
					} else {
						env := decodeXML(t, getBody)
						assertEnvelopeFields(t, env, "ok")
					}

					headRes, headBody := execSubsonicRequest(t, h, http.MethodHead, getTarget, "")
					if headRes.StatusCode != http.StatusOK {
						t.Fatalf("HEAD status = %d, want 200", headRes.StatusCode)
					}
					if len(headBody) != 0 {
						t.Errorf("HEAD body len = %d, want 0", len(headBody))
					}
					if got, want := headRes.Header.Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
						t.Errorf("HEAD Content-Length = %q, want %q", got, want)
					}

					postFormRes, postFormBody := execSubsonicRequest(t, h, http.MethodPost, basePath, encoded)
					if postFormRes.StatusCode != http.StatusOK {
						t.Fatalf("POST form status = %d, want 200", postFormRes.StatusCode)
					}
					if !bytes.Equal(postFormBody, getBody) {
						t.Errorf("POST form body != GET body\ngot:  %s\nwant: %s", postFormBody, getBody)
					}

					postQueryRes, postQueryBody := execSubsonicRequest(t, h, http.MethodPost, getTarget, "")
					if postQueryRes.StatusCode != http.StatusOK {
						t.Fatalf("POST query status = %d, want 200", postQueryRes.StatusCode)
					}
					if !bytes.Equal(postQueryBody, getBody) {
						t.Errorf("POST query body != GET body\ngot:  %s\nwant: %s", postQueryBody, getBody)
					}
				})
			}
		}
	}
}

func TestGetMusicFolders(t *testing.T) {
	h := newLibraryHandler(t, noopLibraryStore{}, nil)

	t.Run("XML", func(t *testing.T) {
		res, body := execSubsonicRequest(t, h, http.MethodGet, "/rest/getMusicFolders", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		assertNoCoverArt(t, body)
		env := decodeXML(t, body)
		assertEnvelopeFields(t, env, "ok")
		if env.MusicFolders == nil {
			t.Fatalf("MusicFolders = nil, want non-nil")
		}
		want := []subsonic.MusicFolder{{ID: 1, Name: "Music"}}
		if !reflect.DeepEqual(env.MusicFolders.MusicFolder, want) {
			t.Errorf("MusicFolder = %+v, want %+v", env.MusicFolders.MusicFolder, want)
		}
	})

	t.Run("JSON", func(t *testing.T) {
		res, body := execSubsonicRequest(t, h, http.MethodGet, "/rest/getMusicFolders?f=json", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		assertNoCoverArt(t, body)
		resp := decodeJSON(t, body)
		assertJSONEnvelopeFields(t, resp, "ok")
		foldersObj := mustJSONMap(t, resp, "musicFolders")
		folders := mustJSONSlice(t, foldersObj, "musicFolder")
		if len(folders) != 1 {
			t.Fatalf("len(musicFolder) = %d, want 1", len(folders))
		}
		folder := mustJSONItemMap(t, folders, 0)
		if got := mustJSONFloat(t, folder, "id"); got != 1 {
			t.Errorf("id = %v, want 1", got)
		}
		if got := mustJSONString(t, folder, "name"); got != "Music" {
			t.Errorf("name = %q, want %q", got, "Music")
		}
	})
}

func TestGetArtists(t *testing.T) {
	t.Run("empty library emits non-nil empty index", func(t *testing.T) {
		st := openTestStore(t)
		h := newLibraryHandler(t, st, nil)

		_, xmlBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getArtists", "")
		assertNoCoverArt(t, xmlBody)
		env := decodeXML(t, xmlBody)
		if env.Artists == nil {
			t.Fatalf("Artists = nil, want non-nil")
		}
		if env.Artists.IgnoredArticles != "The El La Los Las Le Les" {
			t.Errorf("IgnoredArticles = %q, want %q", env.Artists.IgnoredArticles, "The El La Los Las Le Les")
		}
		if len(env.Artists.Index) != 0 {
			t.Errorf("len(Index) = %d, want 0", len(env.Artists.Index))
		}

		_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getArtists?f=json", "")
		assertNoCoverArt(t, jsonBody)
		resp := decodeJSON(t, jsonBody)
		artistsObj := mustJSONMap(t, resp, "artists")
		if got := mustJSONString(t, artistsObj, "ignoredArticles"); got != "The El La Los Las Le Les" {
			t.Errorf("ignoredArticles = %q, want %q", got, "The El La Los Las Le Les")
		}
		idx := mustJSONSlice(t, artistsObj, "index")
		if len(idx) != 0 {
			t.Errorf("len(index) = %d, want 0", len(idx))
		}
	})

	t.Run("article stripping, intra-bucket sorting, # bucket, and musicFolderId tolerance", func(t *testing.T) {
		st := openTestStore(t)
		artistNames := []string{
			"Bach",
			"The Beatles",
			"Beatles",
			"The   Beatles",
			"Björk",
			"Los Lobos",
			"La Roux",
			"Las Ketchup",
			"Le Tigre",
			"Les Claypool",
			"El Guincho",
			"Elton John",
			"the doors",
			"The",
			"The The",
			"Theme Park",
			"100 gecs",
			"!!!",
			"Édith Piaf",
			"坂本龍一",
		}
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		tracks := make([]store.Track, len(artistNames))
		for i, name := range artistNames {
			tracks[i] = store.Track{
				ID:          "tr_art_" + strconv.Itoa(i),
				RelPath:     "artists/" + strconv.Itoa(i) + ".mp3",
				FileSize:    1000,
				MtimeNs:     1000,
				Title:       "Track " + strconv.Itoa(i),
				Artist:      name,
				AlbumArtist: name,
				Album:       "Album " + strconv.Itoa(i),
				CreatedAt:   now,
			}
		}
		seedTracks(t, st, tracks...)
		h := newLibraryHandler(t, st, nil)

		wantBuckets := []struct {
			bucket  string
			artists []string
		}{
			{bucket: "B", artists: []string{"Bach", "Beatles", "The   Beatles", "The Beatles", "Björk"}},
			{bucket: "C", artists: []string{"Les Claypool"}},
			{bucket: "D", artists: []string{"the doors"}},
			{bucket: "E", artists: []string{"Elton John"}},
			{bucket: "G", artists: []string{"El Guincho"}},
			{bucket: "K", artists: []string{"Las Ketchup"}},
			{bucket: "L", artists: []string{"Los Lobos"}},
			{bucket: "R", artists: []string{"La Roux"}},
			{bucket: "T", artists: []string{"The", "The The", "Theme Park", "Le Tigre"}},
			{bucket: "#", artists: []string{"!!!", "100 gecs", "Édith Piaf", "坂本龍一"}},
		}

		for _, query := range []string{"", "?musicFolderId=1", "?musicFolderId=-1", "?musicFolderId=0"} {
			_, xmlBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getArtists"+query, "")
			assertNoCoverArt(t, xmlBody)
			env := decodeXML(t, xmlBody)
			if env.Artists == nil {
				t.Fatalf("query %q: Artists = nil", query)
			}
			if len(env.Artists.Index) != len(wantBuckets) {
				t.Fatalf("query %q: len(Index) = %d, want %d", query, len(env.Artists.Index), len(wantBuckets))
			}
			for i, wb := range wantBuckets {
				gotBucket := env.Artists.Index[i]
				if gotBucket.Name != wb.bucket {
					t.Errorf("query %q: Index[%d].Name = %q, want %q", query, i, gotBucket.Name, wb.bucket)
				}
				var gotNames []string
				for _, a := range gotBucket.Artist {
					gotNames = append(gotNames, a.Name)
					if a.ID == "" {
						t.Errorf("artist %q has empty ID", a.Name)
					}
					if a.AlbumCount != 1 {
						t.Errorf("artist %q AlbumCount = %d, want 1", a.Name, a.AlbumCount)
					}
				}
				if !slices.Equal(gotNames, wb.artists) {
					t.Errorf("query %q: bucket %q artists = %v, want %v", query, wb.bucket, gotNames, wb.artists)
				}
			}
		}

		// Verify JSON parity including intra-bucket order and numeric albumCount.
		_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getArtists?f=json&musicFolderId=1", "")
		assertNoCoverArt(t, jsonBody)
		resp := decodeJSON(t, jsonBody)
		artistsObj := mustJSONMap(t, resp, "artists")
		indexes := mustJSONSlice(t, artistsObj, "index")
		if len(indexes) != len(wantBuckets) {
			t.Fatalf("JSON len(index) = %d, want %d", len(indexes), len(wantBuckets))
		}
		lastBucket := mustJSONItemMap(t, indexes, len(indexes)-1)
		if got := mustJSONString(t, lastBucket, "name"); got != "#" {
			t.Errorf("last bucket name = %q, want \"#\"", got)
		}
		bBucket := mustJSONItemMap(t, indexes, 0)
		bArtists := mustJSONSlice(t, bBucket, "artist")
		firstB := mustJSONItemMap(t, bArtists, 0)
		if got := mustJSONFloat(t, firstB, "albumCount"); got != 1 {
			t.Errorf("albumCount = %v, want 1", got)
		}
	})
}

func TestGetArtist(t *testing.T) {
	st := openTestStore(t)
	created := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	seedTracks(t, st,
		store.Track{
			ID:          "tr_a1",
			RelPath:     "artist/a/01.mp3",
			FileSize:    100,
			MtimeNs:     1,
			Title:       "Song A",
			Artist:      "Artist One",
			AlbumArtist: "Artist One",
			Album:       "Beta Album",
			Year:        2020,
			Genre:       "Indie",
			DurationSec: 120,
			CreatedAt:   created,
		},
		store.Track{
			ID:          "tr_a2",
			RelPath:     "artist/b/01.mp3",
			FileSize:    200,
			MtimeNs:     2,
			Title:       "Song B",
			Artist:      "Artist One",
			AlbumArtist: "Artist One",
			Album:       "Alpha Album",
			Year:        0,
			Genre:       "",
			DurationSec: 0,
			CreatedAt:   created,
		},
	)
	artistID := store.ArtistID("Artist One")
	h := newLibraryHandler(t, st, nil)

	t.Run("happy path XML and JSON", func(t *testing.T) {
		_, xmlBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getArtist?id="+artistID, "")
		assertNoCoverArt(t, xmlBody)
		env := decodeXML(t, xmlBody)
		if env.Artist == nil {
			t.Fatalf("Artist = nil")
		}
		if env.Artist.ID != artistID || env.Artist.Name != "Artist One" || env.Artist.AlbumCount != 2 {
			t.Errorf("Artist = %+v, unexpected fields", env.Artist)
		}
		if len(env.Artist.Album) != 2 {
			t.Fatalf("len(Album) = %d, want 2", len(env.Artist.Album))
		}
		// Ordered alphabetically by album name: Alpha Album then Beta Album.
		if env.Artist.Album[0].Name != "Alpha Album" || env.Artist.Album[1].Name != "Beta Album" {
			t.Errorf("albums order = [%q, %q], want [Alpha Album, Beta Album]",
				env.Artist.Album[0].Name, env.Artist.Album[1].Name)
		}
		if env.Artist.Album[1].Created != "2026-03-15T12:00:00Z" {
			t.Errorf("Created = %q, want 2026-03-15T12:00:00Z", env.Artist.Album[1].Created)
		}

		_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getArtist?f=json&id="+artistID, "")
		assertNoCoverArt(t, jsonBody)
		resp := decodeJSON(t, jsonBody)
		artistObj := mustJSONMap(t, resp, "artist")
		albums := mustJSONSlice(t, artistObj, "album")
		alpha := mustJSONItemMap(t, albums, 0)
		// Alpha Album has duration 0 (must serialize) and year/genre 0/"" (must omit).
		if got := mustJSONFloat(t, alpha, "duration"); got != 0 {
			t.Errorf("Alpha duration = %v, want 0", got)
		}
		if got := mustJSONFloat(t, alpha, "songCount"); got != 1 {
			t.Errorf("Alpha songCount = %v, want 1", got)
		}
		if _, exists := alpha["year"]; exists {
			t.Errorf("Alpha unexpected year field when 0: %v", alpha["year"])
		}
		if _, exists := alpha["genre"]; exists {
			t.Errorf("Alpha unexpected genre field when empty: %v", alpha["genre"])
		}
		beta := mustJSONItemMap(t, albums, 1)
		if got := mustJSONFloat(t, beta, "year"); got != 2020 {
			t.Errorf("Beta year = %v, want 2020", got)
		}
		if got := mustJSONString(t, beta, "genre"); got != "Indie" {
			t.Errorf("Beta genre = %q, want Indie", got)
		}
	})

	errorCases := []struct {
		name     string
		query    string
		wantCode int
	}{
		{name: "missing id", query: "", wantCode: 10},
		{name: "blank id", query: "id=%20%20", wantCode: 10},
		{name: "unknown id", query: "id=ar_doesnotexist", wantCode: 70},
	}
	for _, tc := range errorCases {
		for _, format := range []string{"xml", "json"} {
			t.Run(tc.name+"_"+format, func(t *testing.T) {
				q := tc.query
				if format == "json" {
					if q != "" {
						q += "&"
					}
					q += "f=json"
				}
				target := "/rest/getArtist"
				if q != "" {
					target += "?" + q
				}
				_, body := execSubsonicRequest(t, h, http.MethodGet, target, "")
				assertNoCoverArt(t, body)
				if format == "json" {
					resp := decodeJSON(t, body)
					assertJSONEnvelopeFields(t, resp, "failed")
					errObj := mustJSONMap(t, resp, "error")
					if got := int(mustJSONFloat(t, errObj, "code")); got != tc.wantCode {
						t.Errorf("error code = %d, want %d", got, tc.wantCode)
					}
				} else {
					env := decodeXML(t, body)
					assertEnvelopeFields(t, env, "failed")
					if env.Error == nil || env.Error.Code != tc.wantCode {
						t.Errorf("error = %+v, want code %d", env.Error, tc.wantCode)
					}
				}
			})
		}
	}
}

func TestGetAlbum(t *testing.T) {
	st := openTestStore(t)
	created := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	seedTracks(t, st,
		store.Track{
			ID:          "tr_comp_d2t1",
			RelPath:     "comp/d2_01.opus",
			FileSize:    2048,
			MtimeNs:     20,
			Title:       "Disc 2 Opener",
			Artist:      "Guest Artist B",
			AlbumArtist: "Various Artists",
			Album:       "Compilation Vol 1",
			TrackNum:    1,
			DiscNum:     2,
			Year:        2021,
			Genre:       "Electronic",
			DurationSec: 185,
			BitRate:     160,
			CreatedAt:   created,
		},
		store.Track{
			ID:          "tr_comp_d1t2",
			RelPath:     "comp/d1_02.flac",
			FileSize:    4096,
			MtimeNs:     10,
			Title:       "Zero Meta Track",
			Artist:      "Guest Artist A",
			AlbumArtist: "Various Artists",
			Album:       "Compilation Vol 1",
			TrackNum:    0,
			DiscNum:     1,
			Year:        0,
			Genre:       "",
			DurationSec: 0,
			BitRate:     0,
			CreatedAt:   created,
		},
		store.Track{
			ID:          "tr_comp_d1t1",
			RelPath:     "comp/d1_01.mp3",
			FileSize:    1024,
			MtimeNs:     5,
			Title:       "Disc 1 Opener",
			Artist:      "Guest Artist C",
			AlbumArtist: "Various Artists",
			Album:       "Compilation Vol 1",
			TrackNum:    1,
			DiscNum:     1,
			Year:        2021,
			Genre:       "Ambient",
			DurationSec: 100,
			BitRate:     320,
			CreatedAt:   created,
		},
	)
	albumID := store.AlbumID("Various Artists", "Compilation Vol 1")
	artistID := store.ArtistID("Various Artists")
	h := newLibraryHandler(t, st, nil)

	t.Run("multi-disc compilation order, opus mapping, and zero vs omitted fields", func(t *testing.T) {
		_, xmlBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbum?id="+albumID, "")
		assertNoCoverArt(t, xmlBody)
		env := decodeXML(t, xmlBody)
		if env.Album == nil {
			t.Fatalf("Album = nil")
		}
		if env.Album.Artist != "Various Artists" || env.Album.ArtistID != artistID {
			t.Errorf("Album artist = (%q, %q), want (Various Artists, %q)", env.Album.Artist, env.Album.ArtistID, artistID)
		}
		if env.Album.SongCount != 3 || env.Album.Duration != 285 {
			t.Errorf("Album counts = (%d, %d), want (3, 285)", env.Album.SongCount, env.Album.Duration)
		}
		if len(env.Album.Song) != 3 {
			t.Fatalf("len(Song) = %d, want 3", len(env.Album.Song))
		}
		// Order: disc 1 track 0 (Zero Meta Track), disc 1 track 1, disc 2 track 1.
		wantIDs := []string{"tr_comp_d1t2", "tr_comp_d1t1", "tr_comp_d2t1"}
		for i, wantID := range wantIDs {
			if env.Album.Song[i].ID != wantID {
				t.Errorf("Song[%d].ID = %q, want %q", i, env.Album.Song[i].ID, wantID)
			}
			if env.Album.Song[i].Parent != albumID {
				t.Errorf("Song[%d].Parent = %q, want %q", i, env.Album.Song[i].Parent, albumID)
			}
			if env.Album.Song[i].ArtistID != artistID {
				t.Errorf("Song[%d].ArtistID = %q, want %q", i, env.Album.Song[i].ArtistID, artistID)
			}
		}
		if env.Album.Song[0].Artist != "Guest Artist A" {
			t.Errorf("Song[0].Artist = %q, want Guest Artist A", env.Album.Song[0].Artist)
		}
		if env.Album.Song[2].Suffix != "opus" || env.Album.Song[2].ContentType != "audio/ogg" {
			t.Errorf("Opus format = (%q, %q), want (opus, audio/ogg)", env.Album.Song[2].Suffix, env.Album.Song[2].ContentType)
		}

		_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbum?f=json&id="+albumID, "")
		assertNoCoverArt(t, jsonBody)
		resp := decodeJSON(t, jsonBody)
		albumObj := mustJSONMap(t, resp, "album")
		songs := mustJSONSlice(t, albumObj, "song")
		s0 := mustJSONItemMap(t, songs, 0)
		if got := mustJSONBool(t, s0, "isDir"); got {
			t.Errorf("isDir = true, want false")
		}
		if got := mustJSONBool(t, s0, "isVideo"); got {
			t.Errorf("isVideo = true, want false")
		}
		if got := mustJSONFloat(t, s0, "duration"); got != 0 {
			t.Errorf("duration = %v, want 0", got)
		}
		for _, omittedKey := range []string{"track", "year", "genre", "bitRate"} {
			if _, exists := s0[omittedKey]; exists {
				t.Errorf("expected %q to be omitted when zero/empty, got %v", omittedKey, s0[omittedKey])
			}
		}
	})

	for _, tc := range []struct {
		name  string
		query string
		code  int
	}{
		{name: "missing id", query: "", code: 10},
		{name: "blank id", query: "?id=%20", code: 10},
		{name: "unknown id", query: "?id=al_missing", code: 70},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, body := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbum"+tc.query, "")
			env := decodeXML(t, body)
			assertEnvelopeFields(t, env, "failed")
			if env.Error == nil || env.Error.Code != tc.code {
				t.Errorf("error = %+v, want code %d", env.Error, tc.code)
			}
		})
	}
}

func TestGetAlbumList2(t *testing.T) {
	t.Run("supported types against seeded SQLite", func(t *testing.T) {
		st := openTestStore(t)
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		seedTracks(t, st,
			store.Track{
				ID: "tr_1", RelPath: "1.mp3", FileSize: 100, MtimeNs: 1,
				Title: "T1", Artist: "Zed", AlbumArtist: "Zed", Album: "Alpha", CreatedAt: base,
			},
			store.Track{
				ID: "tr_2", RelPath: "2.mp3", FileSize: 100, MtimeNs: 2,
				Title: "T2", Artist: "Amy", AlbumArtist: "Amy", Album: "Zebra", CreatedAt: base.Add(2 * time.Hour),
			},
			store.Track{
				ID: "tr_3", RelPath: "3.mp3", FileSize: 100, MtimeNs: 3,
				Title: "T3", Artist: "Mid", AlbumArtist: "Mid", Album: "Middle", CreatedAt: base.Add(time.Hour),
			},
		)
		h := newLibraryHandler(t, st, nil)

		cases := []struct {
			listType  string
			wantOrder []string
		}{
			{listType: "alphabeticalByName", wantOrder: []string{"Alpha", "Middle", "Zebra"}},
			{listType: "alphabeticalByArtist", wantOrder: []string{"Zebra", "Middle", "Alpha"}},
			{listType: "newest", wantOrder: []string{"Zebra", "Middle", "Alpha"}},
		}
		for _, tc := range cases {
			_, body := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbumList2?type="+tc.listType+"&musicFolderId=1", "")
			assertNoCoverArt(t, body)
			env := decodeXML(t, body)
			if env.AlbumList2 == nil {
				t.Fatalf("%s: AlbumList2 = nil", tc.listType)
			}
			var got []string
			for _, a := range env.AlbumList2.Album {
				got = append(got, a.Name)
			}
			if !slices.Equal(got, tc.wantOrder) {
				t.Errorf("%s order = %v, want %v", tc.listType, got, tc.wantOrder)
			}
		}

		// random returns all 3 albums.
		_, randJSON := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbumList2?f=json&type=random", "")
		assertNoCoverArt(t, randJSON)
		resp := decodeJSON(t, randJSON)
		listObj := mustJSONMap(t, resp, "albumList2")
		albums := mustJSONSlice(t, listObj, "album")
		if len(albums) != 3 {
			t.Errorf("random len = %d, want 3", len(albums))
		}

		// Pagination slice against real SQLite.
		_, pageBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbumList2?type=alphabeticalByName&size=1&offset=1", "")
		pageEnv := decodeXML(t, pageBody)
		if len(pageEnv.AlbumList2.Album) != 1 || pageEnv.AlbumList2.Album[0].Name != "Middle" {
			t.Errorf("paginated album = %+v, want [Middle]", pageEnv.AlbumList2.Album)
		}
	})

	t.Run("empty database emits empty album slice", func(t *testing.T) {
		st := openTestStore(t)
		h := newLibraryHandler(t, st, nil)
		_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbumList2?f=json&type=newest", "")
		resp := decodeJSON(t, jsonBody)
		listObj := mustJSONMap(t, resp, "albumList2")
		albums := mustJSONSlice(t, listObj, "album")
		if len(albums) != 0 {
			t.Errorf("len(album) = %d, want 0", len(albums))
		}
	})

	t.Run("deferred valid types return empty list without calling store", func(t *testing.T) {
		spy := &spyLibraryStore{}
		h := newLibraryHandler(t, spy, nil)
		for _, dt := range []string{"recent", "frequent", "highest", "starred", "byYear", "byGenre"} {
			_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbumList2?f=json&type="+dt, "")
			assertNoCoverArt(t, jsonBody)
			resp := decodeJSON(t, jsonBody)
			assertJSONEnvelopeFields(t, resp, "ok")
			listObj := mustJSONMap(t, resp, "albumList2")
			albums := mustJSONSlice(t, listObj, "album")
			if len(albums) != 0 {
				t.Errorf("type=%s: len(album) = %d, want 0", dt, len(albums))
			}
		}
		if spy.listAlbumsCalls != 0 {
			t.Errorf("ListAlbums called %d times for deferred types, want 0", spy.listAlbumsCalls)
		}
	})

	t.Run("invalid or missing type returns error 10", func(t *testing.T) {
		h := newLibraryHandler(t, noopLibraryStore{}, nil)
		for _, q := range []string{"", "?type=", "?type=bogus", "?type=Newest"} {
			_, body := execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbumList2"+q, "")
			env := decodeXML(t, body)
			assertEnvelopeFields(t, env, "failed")
			if env.Error == nil || env.Error.Code != 10 {
				t.Errorf("query %q: error = %+v, want code 10", q, env.Error)
			}
		}
	})

	t.Run("size and offset normalization and clamping", func(t *testing.T) {
		cases := []struct {
			query      string
			wantLimit  int
			wantOffset int
		}{
			{query: "type=newest", wantLimit: 10, wantOffset: 0},
			{query: "type=newest&size=0&offset=5", wantLimit: 0, wantOffset: 5},
			{query: "type=newest&size=-3&offset=-7", wantLimit: 0, wantOffset: 0},
			{query: "type=newest&size=999&offset=12", wantLimit: 500, wantOffset: 12},
			{query: "type=newest&size=abc&offset=xyz", wantLimit: 10, wantOffset: 0},
		}
		for _, tc := range cases {
			spy := &spyLibraryStore{}
			h := newLibraryHandler(t, spy, nil)
			_, _ = execSubsonicRequest(t, h, http.MethodGet, "/rest/getAlbumList2?"+tc.query, "")
			if spy.lastAlbumQuery.Limit != tc.wantLimit || spy.lastAlbumQuery.Offset != tc.wantOffset {
				t.Errorf("%q -> (limit=%d, offset=%d), want (%d, %d)",
					tc.query, spy.lastAlbumQuery.Limit, spy.lastAlbumQuery.Offset, tc.wantLimit, tc.wantOffset)
			}
		}
	})
}

func TestGetSong(t *testing.T) {
	t.Run("full 21-field Child verification in XML and JSON", func(t *testing.T) {
		st := openTestStore(t)
		created := time.Date(2026, 5, 20, 14, 15, 16, 0, time.UTC)
		seedTracks(t, st, store.Track{
			ID:          "tr_full_01",
			RelPath:     "rock/band/03_song.opus",
			FileSize:    98765,
			MtimeNs:     999,
			Title:       "Full Song",
			Artist:      "Track Artist",
			AlbumArtist: "Album Artist",
			Album:       "The Record",
			TrackNum:    3,
			DiscNum:     2,
			Year:        2024,
			Genre:       "Rock",
			DurationSec: 215,
			BitRate:     192,
			CreatedAt:   created,
		})
		albumID := store.AlbumID("Album Artist", "The Record")
		artistID := store.ArtistID("Album Artist")
		h := newLibraryHandler(t, st, nil)

		_, xmlBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getSong?id=tr_full_01", "")
		assertNoCoverArt(t, xmlBody)
		env := decodeXML(t, xmlBody)
		if env.Song == nil {
			t.Fatalf("Song = nil")
		}
		wantSong := subsonic.Child{
			ID:          "tr_full_01",
			Parent:      albumID,
			IsDir:       false,
			Title:       "Full Song",
			Album:       "The Record",
			Artist:      "Track Artist",
			Track:       3,
			Year:        2024,
			Genre:       "Rock",
			Size:        98765,
			ContentType: "audio/ogg",
			Suffix:      "opus",
			Duration:    215,
			BitRate:     192,
			Path:        "rock/band/03_song.opus",
			IsVideo:     false,
			DiscNumber:  2,
			Created:     "2026-05-20T14:15:16Z",
			AlbumID:     albumID,
			ArtistID:    artistID,
			Type:        "music",
		}
		if !reflect.DeepEqual(*env.Song, wantSong) {
			t.Errorf("XML Song = %+v, want %+v", *env.Song, wantSong)
		}

		_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getSong?f=json&id=tr_full_01", "")
		assertNoCoverArt(t, jsonBody)
		resp := decodeJSON(t, jsonBody)
		songMap := mustJSONMap(t, resp, "song")
		if len(songMap) != 21 {
			t.Errorf("JSON song field count = %d, want 21 (%v)", len(songMap), songMap)
		}
		if got := mustJSONString(t, songMap, "type"); got != "music" {
			t.Errorf("type = %q, want music", got)
		}
		if got := mustJSONString(t, songMap, "contentType"); got != "audio/ogg" {
			t.Errorf("contentType = %q, want audio/ogg", got)
		}
		if got := mustJSONString(t, songMap, "suffix"); got != "opus" {
			t.Errorf("suffix = %q, want opus", got)
		}
	})

	t.Run("zero-field presence, optional-field omission, and unknown extension fallback", func(t *testing.T) {
		spy := &spyLibraryStore{
			track: &store.Track{
				ID:          "tr_sparse",
				RelPath:     "misc/noext",
				FileSize:    0,
				Title:       "Sparse",
				Artist:      "Anon",
				AlbumArtist: "Anon",
				ArtistID:    "ar_anon",
				Album:       "Misc",
				AlbumID:     "al_misc",
			},
		}
		h := newLibraryHandler(t, spy, nil)

		_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getSong?f=json&id=tr_sparse", "")
		assertNoCoverArt(t, jsonBody)
		resp := decodeJSON(t, jsonBody)
		songMap := mustJSONMap(t, resp, "song")
		if got := mustJSONBool(t, songMap, "isDir"); got {
			t.Errorf("isDir = true, want false")
		}
		if got := mustJSONBool(t, songMap, "isVideo"); got {
			t.Errorf("isVideo = true, want false")
		}
		if got := mustJSONFloat(t, songMap, "duration"); got != 0 {
			t.Errorf("duration = %v, want 0", got)
		}
		if got := mustJSONFloat(t, songMap, "size"); got != 0 {
			t.Errorf("size = %v, want 0", got)
		}
		if got := mustJSONString(t, songMap, "contentType"); got != "application/octet-stream" {
			t.Errorf("contentType = %q, want application/octet-stream", got)
		}
		for _, omitted := range []string{"track", "discNumber", "year", "genre", "bitRate", "created", "suffix"} {
			if _, exists := songMap[omitted]; exists {
				t.Errorf("expected %q to be omitted, got %v", omitted, songMap[omitted])
			}
		}

		// Extension with unknown MIME preserves suffix and uses octet-stream.
		spy.track.RelPath = "misc/sample.XYZ"
		_, extBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/getSong?f=json&id=tr_sparse", "")
		extSong := mustJSONMap(t, decodeJSON(t, extBody), "song")
		if got := mustJSONString(t, extSong, "suffix"); got != "xyz" {
			t.Errorf("suffix = %q, want xyz", got)
		}
		if got := mustJSONString(t, extSong, "contentType"); got != "application/octet-stream" {
			t.Errorf("contentType = %q, want application/octet-stream", got)
		}
	})

	t.Run("errors and nil-track guard", func(t *testing.T) {
		st := openTestStore(t)
		h := newLibraryHandler(t, st, nil)

		for _, tc := range []struct {
			query string
			code  int
		}{
			{query: "", code: 10},
			{query: "?id=%20", code: 10},
			{query: "?id=tr_missing", code: 70},
		} {
			_, body := execSubsonicRequest(t, h, http.MethodGet, "/rest/getSong"+tc.query, "")
			env := decodeXML(t, body)
			if env.Error == nil || env.Error.Code != tc.code {
				t.Errorf("%q: error = %+v, want code %d", tc.query, env.Error, tc.code)
			}
		}

		// Nil track with nil error must return 70 rather than panicking.
		nilTrackHandler := newLibraryHandler(t, &spyLibraryStore{track: nil}, nil)
		_, body := execSubsonicRequest(t, nilTrackHandler, http.MethodGet, "/rest/getSong?id=tr_any", "")
		env := decodeXML(t, body)
		if env.Error == nil || env.Error.Code != 70 {
			t.Errorf("nil track error = %+v, want code 70", env.Error)
		}
	})
}

func TestSearch3(t *testing.T) {
	t.Run("bulk sync, substring, wildcards, and zero counts against SQLite", func(t *testing.T) {
		st := openTestStore(t)
		now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		seedTracks(t, st,
			store.Track{
				ID: "tr_s1", RelPath: "s/1.mp3", FileSize: 100, MtimeNs: 1,
				Title: "100% Pure", Artist: "Wild_Card", AlbumArtist: "Wild_Card",
				Album: `Rock \ Roll`, CreatedAt: now,
			},
			store.Track{
				ID: "tr_s2", RelPath: "s/2.mp3", FileSize: 100, MtimeNs: 2,
				Title: `"quoted" track`, Artist: "Normal Band", AlbumArtist: "Normal Band",
				Album: "Plain Album", CreatedAt: now,
			},
			store.Track{
				ID: "tr_s3", RelPath: "s/3.mp3", FileSize: 100, MtimeNs: 3,
				Title: "Jazz Waltz", Artist: "Jazz Trio", AlbumArtist: "Jazz Trio",
				Album: "Midnight", CreatedAt: now,
			},
		)
		h := newLibraryHandler(t, st, nil)

		// All empty-query variants must return all 3 artists, 3 albums, and 3 songs.
		for _, q := range []string{"", "?query=", "?query=%20%20", "?query=%22%22", "?query=%20%22%22%20"} {
			sep := "?"
			if q != "" {
				sep = "&"
			}
			_, jsonBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/search3"+q+sep+"f=json&musicFolderId=-1", "")
			assertNoCoverArt(t, jsonBody)
			resp := decodeJSON(t, jsonBody)
			sr := mustJSONMap(t, resp, "searchResult3")
			if got := len(mustJSONSlice(t, sr, "artist")); got != 3 {
				t.Errorf("%q artist len = %d, want 3", q, got)
			}
			if got := len(mustJSONSlice(t, sr, "album")); got != 3 {
				t.Errorf("%q album len = %d, want 3", q, got)
			}
			if got := len(mustJSONSlice(t, sr, "song")); got != 3 {
				t.Errorf("%q song len = %d, want 3", q, got)
			}
		}

		// Non-empty quoted query preserves quotes.
		_, quoteBody := execSubsonicRequest(t, h, http.MethodGet, "/rest/search3?query=%22quoted%22", "")
		quoteEnv := decodeXML(t, quoteBody)
		if len(quoteEnv.SearchResult3.Song) != 1 || quoteEnv.SearchResult3.Song[0].ID != "tr_s2" {
			t.Errorf("quoted search songs = %+v, want [tr_s2]", quoteEnv.SearchResult3.Song)
		}

		// SQLite LIKE wildcard characters %, _, \ must match literally.
		for _, tc := range []struct {
			rawQuery string
			wantSong string
		}{
			{rawQuery: "%", wantSong: "tr_s1"},
			{rawQuery: "_", wantSong: "tr_s1"},
			{rawQuery: `\`, wantSong: "tr_s1"},
		} {
			target := "/rest/search3?query=" + url.QueryEscape(tc.rawQuery)
			_, body := execSubsonicRequest(t, h, http.MethodGet, target, "")
			env := decodeXML(t, body)
			if len(env.SearchResult3.Song) != 1 || env.SearchResult3.Song[0].ID != tc.wantSong {
				t.Errorf("wildcard %q songs = %+v, want [%s]", tc.rawQuery, env.SearchResult3.Song, tc.wantSong)
			}
		}

		// Explicit 0 counts return empty JSON slices (never null).
		_, zeroBody := execSubsonicRequest(t, h, http.MethodGet,
			"/rest/search3?f=json&query=&artistCount=0&albumCount=0&songCount=0", "")
		assertNoCoverArt(t, zeroBody)
		zeroSR := mustJSONMap(t, decodeJSON(t, zeroBody), "searchResult3")
		if got := len(mustJSONSlice(t, zeroSR, "artist")); got != 0 {
			t.Errorf("artist len = %d, want 0", got)
		}
		if got := len(mustJSONSlice(t, zeroSR, "album")); got != 0 {
			t.Errorf("album len = %d, want 0", got)
		}
		if got := len(mustJSONSlice(t, zeroSR, "song")); got != 0 {
			t.Errorf("song len = %d, want 0", got)
		}

		// Deterministic offset pagination.
		_, offBody := execSubsonicRequest(t, h, http.MethodGet,
			"/rest/search3?query=&songCount=1&songOffset=1", "")
		offEnv := decodeXML(t, offBody)
		// Alphabetical song titles: "quoted" track ("), 100% Pure (1), Jazz Waltz (J).
		if len(offEnv.SearchResult3.Song) != 1 || offEnv.SearchResult3.Song[0].ID != "tr_s1" {
			t.Errorf("song offset=1 got %+v, want tr_s1", offEnv.SearchResult3.Song)
		}
	})

	t.Run("parameter defaults, clamping, and invalid fallback via spy", func(t *testing.T) {
		spy := &spyLibraryStore{
			search3Out: store.Search3Result{
				Artists: []store.ArtistSummary{},
				Albums:  []store.AlbumSummary{},
				Songs:   []store.Track{},
			},
		}
		h := newLibraryHandler(t, spy, nil)

		cases := []struct {
			query string
			want  store.Search3Query
		}{
			{
				query: "",
				want: store.Search3Query{
					Query: "", ArtistCount: 20, AlbumCount: 20, SongCount: 20,
				},
			},
			{
				query: "query=%20%22%22%20&artistCount=0&albumCount=-5&songCount=999&artistOffset=-1&albumOffset=2&songOffset=bad",
				want: store.Search3Query{
					Query: "", ArtistCount: 0, AlbumCount: 0, SongCount: 500,
					ArtistOffset: 0, AlbumOffset: 2, SongOffset: 0,
				},
			},
			{
				query: "query=miles&artistCount=bad&albumCount=%20&songCount=7",
				want: store.Search3Query{
					Query: "miles", ArtistCount: 20, AlbumCount: 20, SongCount: 7,
				},
			},
		}
		for _, tc := range cases {
			_, _ = execSubsonicRequest(t, h, http.MethodGet, "/rest/search3?"+tc.query, "")
			if !reflect.DeepEqual(spy.lastSearchQuery, tc.want) {
				t.Errorf("%q -> %+v, want %+v", tc.query, spy.lastSearchQuery, tc.want)
			}
		}
	})
}

func TestLibraryStoreErrorsSanitized(t *testing.T) {
	const secretErr = "sqlite internal secret path /data/headunit.db corrupted"
	spy := &spyLibraryStore{err: errors.New(secretErr)}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	h := newLibraryHandler(t, spy, logger)

	endpoints := []string{
		"/rest/getArtists",
		"/rest/getArtist?id=ar_1",
		"/rest/getAlbum?id=al_1",
		"/rest/getAlbumList2?type=newest",
		"/rest/getSong?id=tr_1",
		"/rest/search3?query=jazz",
	}

	for _, ep := range endpoints {
		for _, format := range []string{"xml", "json"} {
			t.Run(ep+"_"+format, func(t *testing.T) {
				logBuf.Reset()
				target := ep
				if format == "json" {
					sep := "?"
					if strings.Contains(ep, "?") {
						sep = "&"
					}
					target += sep + "f=json"
				}
				res, body := execSubsonicRequest(t, h, http.MethodGet, target, "")
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", res.StatusCode)
				}
				if strings.Contains(string(body), secretErr) || strings.Contains(string(body), "/data/headunit.db") {
					t.Fatalf("internal error leaked into response body: %s", body)
				}
				if format == "json" {
					resp := decodeJSON(t, body)
					assertJSONEnvelopeFields(t, resp, "failed")
					errObj := mustJSONMap(t, resp, "error")
					if got := int(mustJSONFloat(t, errObj, "code")); got != 0 {
						t.Errorf("error code = %d, want 0", got)
					}
					if got := mustJSONString(t, errObj, "message"); got != "Internal server error" {
						t.Errorf("error message = %q, want Internal server error", got)
					}
				} else {
					env := decodeXML(t, body)
					assertEnvelopeFields(t, env, "failed")
					if env.Error == nil || env.Error.Code != 0 || env.Error.Message != "Internal server error" {
						t.Errorf("error = %+v, want {Code:0 Message:Internal server error}", env.Error)
					}
				}
				if !strings.Contains(logBuf.String(), secretErr) {
					t.Errorf("expected slog output to record internal error %q, got %q", secretErr, logBuf.String())
				}
			})
		}
	}
}
