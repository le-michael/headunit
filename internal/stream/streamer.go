// Package stream provides the audio streaming seam and direct byte-range
// streamer for Headunit.
package stream

import (
	"errors"
	"net/http"

	"headunit/internal/store"
)

// ErrFileNotFound is returned (typically wrapped) when a track's backing audio
// file is missing, has an invalid relative path, escapes the confined music
// root, or is not a regular file. Callers should match it with errors.Is.
var ErrFileNotFound = errors.New("stream: audio file not found")

// StreamOptions carries optional transcoding hints from OpenSubsonic
// /rest/stream requests. DirectStreamer ignores these fields; future
// transcoding Streamer implementations (such as an FFmpeg-backed wrapper)
// will interpret them.
type StreamOptions struct {
	// MaxBitRate is the requested maximum bitrate in kbps (0 means no limit).
	MaxBitRate int

	// Format is the requested output format suffix (for example, "mp3" or "raw").
	Format string
}

// Streamer serves audio playback for an indexed track over HTTP.
// It is the swap point for future on-the-fly transcoding implementations.
type Streamer interface {
	// Stream writes the audio stream for track to w in response to r.
	//
	// Implementations must validate and open the backing media source before
	// modifying response headers or writing any bytes to w. Whenever Stream
	// returns a non-nil error, zero headers and zero bytes have been written to
	// w so callers (such as OpenSubsonic /rest/stream) can emit an
	// application-level error envelope instead.
	Stream(w http.ResponseWriter, r *http.Request, track *store.Track, opts StreamOptions) error

	// Close releases underlying OS resources held by the Streamer.
	// Implementations must make Close safe to call multiple times.
	Close() error
}
