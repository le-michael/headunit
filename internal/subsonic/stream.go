package subsonic

import (
	"errors"
	"net/http"
	"strings"

	"headunit/internal/store"
	"headunit/internal/stream"
)

func (h *handler) handleStream(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireIDParam(w, r)
	if !ok {
		return
	}

	opts := stream.StreamOptions{
		MaxBitRate: parseMinInt(formParam(r, "maxBitRate"), 0, 0),
		Format:     strings.TrimSpace(formParam(r, "format")),
	}

	track, err := h.store.GetTrack(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, r, errCodeNotFound, "Song not found")
			return
		}
		h.writeInternalError(w, r, "get stream track", err)
		return
	}
	if track == nil {
		h.writeError(w, r, errCodeNotFound, "Song not found")
		return
	}

	if err := h.streamer.Stream(w, r, track, opts); err != nil {
		// Streamer guarantees zero headers and zero bytes have been written when
		// returning an error, so writing an HTTP 200 Subsonic error envelope is safe.
		if errors.Is(err, stream.ErrFileNotFound) {
			h.writeError(w, r, errCodeNotFound, "Audio file not found")
			return
		}
		h.writeInternalError(w, r, "stream audio", err)
	}
}
