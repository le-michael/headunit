package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"headunit/internal/metadata"
	"headunit/internal/store"
)

var _ Streamer = (*DirectStreamer)(nil)

type directConfig struct {
	uploadsDir string
}

// DirectOption configures optional settings for NewDirectStreamer.
type DirectOption func(*directConfig)

// WithUploadsDir configures the optional confined os.Root directory for tracks
// with Source == store.SourceUploads. An empty or whitespace-only dir is ignored.
func WithUploadsDir(dir string) DirectOption {
	return func(c *directConfig) {
		c.uploadsDir = dir
	}
}

// DirectStreamer serves audio files directly from confined per-source root
// directories using os.Root and http.ServeContent. Callers should drain active
// HTTP requests before calling Close; Close is nil-safe, idempotent, and safe
// to call concurrently with Stream. All methods are safe for concurrent use.
type DirectStreamer struct {
	roots  map[string]*os.Root // immutable after NewDirectStreamer; synchronized internally by os.Root
	closed atomic.Bool
}

// NewDirectStreamer opens musicDir (and any optional uploads directory) with
// os.OpenRoot to enforce path and symlink confinement for all subsequent
// Stream calls.
func NewDirectStreamer(musicDir string, opts ...DirectOption) (*DirectStreamer, error) {
	trimmed := strings.TrimSpace(musicDir)
	if trimmed == "" {
		return nil, errors.New("stream: music directory must not be empty")
	}

	var cfg directConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	absDir, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("stream: resolve music directory %q: %w", trimmed, err)
	}

	musicRoot, err := os.OpenRoot(absDir)
	if err != nil {
		return nil, fmt.Errorf("stream: open music root %q: %w", absDir, err)
	}

	roots := map[string]*os.Root{
		store.SourceMusic: musicRoot,
	}

	if uploadsTrimmed := strings.TrimSpace(cfg.uploadsDir); uploadsTrimmed != "" {
		absUploads, err := filepath.Abs(uploadsTrimmed)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("stream: resolve uploads directory %q: %w", uploadsTrimmed, err),
				musicRoot.Close(),
			)
		}
		uploadsRoot, err := os.OpenRoot(absUploads)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("stream: open uploads root %q: %w", absUploads, err),
				musicRoot.Close(),
			)
		}
		roots[store.SourceUploads] = uploadsRoot
	}

	return &DirectStreamer{roots: roots}, nil
}

// Stream serves track's backing audio file directly from the confined root
// matching track.Source (defaulting empty Source to store.SourceMusic) with
// full HTTP Range, ETag, and conditional request support.
//
// DirectStreamer ignores opts. Before modifying any response header or writing
// any body bytes, Stream validates inputs, checks that the target path is a
// regular file via os.Root, opens the file, and re-verifies regularity on the
// opened descriptor. Any error returned before http.ServeContent leaves w
// completely untouched; once headers are set and http.ServeContent is invoked,
// Stream always returns nil.
func (d *DirectStreamer) Stream(w http.ResponseWriter, r *http.Request, track *store.Track, _ StreamOptions) error {
	if d == nil || len(d.roots) == 0 || d.closed.Load() {
		return fmt.Errorf("stream: streamer is closed: %w", os.ErrClosed)
	}
	if w == nil {
		return errors.New("stream: response writer must not be nil")
	}
	if r == nil {
		return errors.New("stream: request must not be nil")
	}
	if track == nil {
		return errors.New("stream: track must not be nil")
	}

	source := track.Source
	if source == "" {
		source = store.SourceMusic
	}
	root := d.roots[source]
	if root == nil {
		return fmt.Errorf("stream: track source %q not configured: %w", source, ErrFileNotFound)
	}

	relPath := track.RelPath
	if err := validateRelPath(relPath); err != nil {
		return err
	}

	// Pre-open Stat rejects non-regular files (such as FIFOs) before Open can
	// block waiting for a peer writer.
	preInfo, err := root.Stat(relPath)
	if err != nil {
		return classifyRootErr("stat", relPath, err)
	}
	if !preInfo.Mode().IsRegular() {
		return fmt.Errorf("stream: %q is not a regular file: %w", relPath, ErrFileNotFound)
	}

	f, err := root.Open(relPath)
	if err != nil {
		return classifyRootErr("open", relPath, err)
	}
	defer f.Close()

	// Re-stat the opened file descriptor to prevent TOCTOU swaps and obtain
	// authoritative size and modification time for ETag and ServeContent.
	info, err := f.Stat()
	if err != nil {
		// Unwrap *os.PathError so f.Stat's underlying file path (which includes
		// the host root dir) is never leaked in Stream error messages.
		var pe *os.PathError
		if errors.As(err, &pe) && pe.Err != nil {
			return fmt.Errorf("stream: fstat %q: %w", relPath, pe.Err)
		}
		return fmt.Errorf("stream: fstat %q: %w", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("stream: %q is not a regular file: %w", relPath, ErrFileNotFound)
	}

	_, contentType, ok := metadata.LookupFormat(relPath)
	if !ok {
		// Force an explicit binary Content-Type so http.ServeContent never falls
		// back to OS MIME tables or content sniffing.
		contentType = "application/octet-stream"
	}

	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("ETag", formatETag(source, track.ID, relPath, info.Size(), info.ModTime().UnixNano()))

	http.ServeContent(w, r, path.Base(relPath), info.ModTime(), f)
	return nil
}

// Close releases all underlying os.Root handles held by the DirectStreamer.
// Close is nil-safe, idempotent, and safe to call concurrently with Stream.
func (d *DirectStreamer) Close() error {
	if d == nil {
		return nil
	}
	d.closed.Store(true)
	if len(d.roots) == 0 {
		return nil
	}
	var errs []error
	for _, root := range d.roots {
		if root != nil {
			errs = append(errs, root.Close())
		}
	}
	return errors.Join(errs...)
}

func validateRelPath(relPath string) error {
	if !fs.ValidPath(relPath) || relPath == "." || strings.ContainsRune(relPath, '\x00') {
		return fmt.Errorf("stream: invalid relative path %q: %w", relPath, ErrFileNotFound)
	}
	return nil
}

func classifyRootErr(op, relPath string, err error) error {
	if isNotFoundRootErr(err) {
		return fmt.Errorf("stream: %s %q: %w (%w)", op, relPath, ErrFileNotFound, err)
	}
	return fmt.Errorf("stream: %s %q: %w", op, relPath, err)
}

func isNotFoundRootErr(err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return true
	}
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Err != nil {
		// os.Root reports escaping and absolute symlinks via an unexported
		// *os.PathError sentinel ("path escapes from parent"). Exclude real
		// errno, permission, and closed-root errors so they remain internal.
		var errno syscall.Errno
		if !errors.As(pe.Err, &errno) &&
			!errors.Is(pe.Err, os.ErrClosed) &&
			!errors.Is(pe.Err, os.ErrPermission) &&
			strings.Contains(pe.Err.Error(), "escapes") {
			return true
		}
	}
	return false
}

func formatETag(source, trackID, relPath string, size, modTimeNs int64) string {
	tag := trackID
	if !isValidETagToken(trackID) {
		key := source + ":" + relPath
		if trackID != "" {
			key = source + ":" + trackID + ":" + relPath
		}
		sum := sha256.Sum256([]byte(key))
		tag = hex.EncodeToString(sum[:8])
	}
	return fmt.Sprintf("\"%s-%d-%d\"", tag, size, modTimeNs)
}

func isValidETagToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		// Restrict to ASCII RFC 9110 etagc bytes (0x21 '!' and 0x23-0x7E).
		if b != 0x21 && (b < 0x23 || b > 0x7E) {
			return false
		}
	}
	return true
}
