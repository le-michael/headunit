package subsonic

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"headunit/internal/auth"
)

func (h *handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(healthzBody)))
	w.WriteHeader(http.StatusOK)

	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, healthzBody)
}

func (h *handler) handlePing(w http.ResponseWriter, r *http.Request) {
	h.writeOK(w, r, Response{})
}

func (h *handler) handleGetLicense(w http.ResponseWriter, r *http.Request) {
	h.writeOK(w, r, Response{
		License: &License{Valid: true},
	})
}

func (h *handler) handleGetOpenSubsonicExtensions(w http.ResponseWriter, r *http.Request) {
	exts := []OpenSubsonicExtension{}
	h.writeOK(w, r, Response{
		OpenSubsonicExtensions: &exts,
	})
}

func (h *handler) handleTokenInfo(w http.ResponseWriter, r *http.Request) {
	principal := auth.PrincipalFromContext(r.Context())
	if principal.Method != auth.AuthMethodAPIKey {
		h.writeError(w, r, auth.ErrCodeUnsupportedAuth, msgUnsupportedAuth)
		return
	}
	h.writeOK(w, r, Response{
		TokenInfo: &TokenInfo{
			Username: principal.Username,
		},
	})
}

func (h *handler) handleGetUser(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(formParam(r, "username"))
	if username == "" {
		username = strings.TrimSpace(auth.PrincipalFromContext(r.Context()).Username)
	}
	if username == "" {
		username = defaultUsername
	}

	h.writeOK(w, r, Response{
		User: &User{
			Username:   username,
			StreamRole: true,
			Folder:     []int{1},
		},
	})
}

func (h *handler) handleGetScanStatus(w http.ResponseWriter, r *http.Request) {
	status := h.scanner.Status()
	scanResp := &ScanStatus{
		Scanning: status.Scanning,
		Count:    status.Count,
	}
	if !status.LastScan.IsZero() {
		scanResp.LastScan = status.LastScan.UTC().Format(time.RFC3339)
	}

	h.writeOK(w, r, Response{
		ScanStatus: scanResp,
	})
}

func (h *handler) handleGetGenres(w http.ResponseWriter, r *http.Request) {
	h.writeOK(w, r, Response{
		Genres: &Genres{
			Genre: []struct{}{},
		},
	})
}

func (h *handler) handleGetStarred2(w http.ResponseWriter, r *http.Request) {
	h.writeOK(w, r, Response{
		Starred2: &Starred2{
			Artist: []struct{}{},
			Album:  []struct{}{},
			Song:   []struct{}{},
		},
	})
}

func (h *handler) handleGetPlaylists(w http.ResponseWriter, r *http.Request) {
	h.writeOK(w, r, Response{
		Playlists: &Playlists{
			Playlist: []struct{}{},
		},
	})
}

func (h *handler) handleGetCoverArt(w http.ResponseWriter, r *http.Request) {
	h.writeError(w, r, errCodeNotFound, "Cover art not found")
}

func (h *handler) handleScrobble(w http.ResponseWriter, r *http.Request) {
	h.writeOK(w, r, Response{})
}
