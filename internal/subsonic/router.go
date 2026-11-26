package subsonic

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"headunit/internal/auth"
	"headunit/internal/scanner"
	"headunit/internal/store"
	"headunit/internal/stream"
)

// ScanStatusProvider supplies point-in-time library scan status for getScanStatus.
type ScanStatusProvider interface {
	// Status returns a point-in-time snapshot of library scan progress.
	Status() scanner.ScanStatus
}

// LibraryStore provides read-only library queries for OpenSubsonic ID3
// browsing, search3, and stream track lookup. Implementations must be safe
// for concurrent use across HTTP handler goroutines.
type LibraryStore interface {
	ListArtists(ctx context.Context) ([]store.ArtistSummary, error)
	GetArtist(ctx context.Context, artistID string) (store.ArtistSummary, []store.AlbumSummary, error)
	GetAlbum(ctx context.Context, albumID string) (store.AlbumSummary, []store.Track, error)
	ListAlbums(ctx context.Context, q store.AlbumListQuery) ([]store.AlbumSummary, error)
	GetTrack(ctx context.Context, trackID string) (*store.Track, error)
	Search3(ctx context.Context, q store.Search3Query) (store.Search3Result, error)
}

// Compile-time interface assertions.
var (
	_ ScanStatusProvider = (*scanner.Service)(nil)
	_ LibraryStore       = (*store.Store)(nil)
	_ auth.ErrorWriter   = WriteError
)

// Dependencies groups the collaborators wired into the OpenSubsonic HTTP handler.
type Dependencies struct {
	// Auth authenticates incoming /rest/* requests. Required.
	Auth auth.Authenticator

	// Scanner supplies atomic library scan status for getScanStatus. Required.
	Scanner ScanStatusProvider

	// Store supplies read-only library metadata and track lookups. Required.
	Store LibraryStore

	// Streamer serves audio playback for /rest/stream. Required.
	Streamer stream.Streamer

	// Logger records unexpected internal serialization and backend errors. Optional;
	// nil defaults to slog.New(slog.DiscardHandler).
	Logger *slog.Logger
}

type handler struct {
	scanner  ScanStatusProvider
	store    LibraryStore
	streamer stream.Streamer
	logger   *slog.Logger
}

// NewHandler constructs the Headunit OpenSubsonic v1.16.1 HTTP handler.
// Returns an error if deps.Auth, deps.Scanner, deps.Store, or deps.Streamer is nil.
func NewHandler(deps Dependencies) (http.Handler, error) {
	if deps.Auth == nil {
		return nil, errors.New("subsonic: auth authenticator must not be nil")
	}
	if deps.Scanner == nil {
		return nil, errors.New("subsonic: scanner status provider must not be nil")
	}
	if deps.Store == nil {
		return nil, errors.New("subsonic: library store must not be nil")
	}
	if deps.Streamer == nil {
		return nil, errors.New("subsonic: streamer must not be nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	h := &handler{
		scanner:  deps.Scanner,
		store:    deps.Store,
		streamer: deps.Streamer,
		logger:   logger,
	}

	mux := http.NewServeMux()
	routes := []struct {
		method string
		fn     http.HandlerFunc
	}{
		{method: "ping", fn: h.handlePing},
		{method: "getLicense", fn: h.handleGetLicense},
		{method: "getOpenSubsonicExtensions", fn: h.handleGetOpenSubsonicExtensions},
		{method: "getUser", fn: h.handleGetUser},
		{method: "getScanStatus", fn: h.handleGetScanStatus},
		{method: "getMusicFolders", fn: h.handleGetMusicFolders},
		{method: "getArtists", fn: h.handleGetArtists},
		{method: "getArtist", fn: h.handleGetArtist},
		{method: "getAlbum", fn: h.handleGetAlbum},
		{method: "getAlbumList2", fn: h.handleGetAlbumList2},
		{method: "getSong", fn: h.handleGetSong},
		{method: "search3", fn: h.handleSearch3},
		{method: "stream", fn: h.handleStream},
		{method: "getGenres", fn: h.handleGetGenres},
		{method: "getStarred2", fn: h.handleGetStarred2},
		{method: "getPlaylists", fn: h.handleGetPlaylists},
		{method: "getCoverArt", fn: h.handleGetCoverArt},
		{method: "scrobble", fn: h.handleScrobble},
	}

	for _, rt := range routes {
		registerEndpoint(mux, rt.method, rt.fn)
	}

	return h.corsMiddleware(h.parseFormMiddleware(auth.Middleware(deps.Auth, h.writeError)(mux))), nil
}

func registerEndpoint(mux *http.ServeMux, method string, fn http.HandlerFunc) {
	for _, path := range []string{"/rest/" + method, "/rest/" + method + ".view"} {
		mux.HandleFunc("GET "+path, fn)
		mux.HandleFunc("POST "+path, fn)
	}
}

func (h *handler) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range, If-Range, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, ETag")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (h *handler) parseFormMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
		}
		if err := r.ParseForm(); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				h.writeError(w, r, errCodeGeneric, "Request body exceeds maximum size")
				return
			}
			h.writeError(w, r, errCodeGeneric, "Malformed request parameters")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *handler) writeOK(w http.ResponseWriter, r *http.Request, resp Response) {
	writeOKWithLogger(w, r, h.logger, resp)
}

func (h *handler) writeError(w http.ResponseWriter, r *http.Request, code int, message string) {
	writeErrorWithLogger(w, r, h.logger, code, message)
}
