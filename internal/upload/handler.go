// Package upload implements Headunit's authenticated, admin-only streaming HTTP
// audio upload handler backed by a confined os.Root uploads directory.
package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"headunit/internal/auth"
	"headunit/internal/metadata"
	"headunit/internal/scanner"
	"headunit/internal/store"
)

const (
	multipartOverheadBytes int64 = 1 << 20 // 1 MiB allowance for multipart headers and non-file parts
	maxUploadPathBytes           = 512
	maxUploadPathSegments        = 16
	stagedFileHexBytes           = 16

	errCodeBadRequest       = "bad_request"
	errCodeUnauthorized     = "unauthorized"
	errCodeForbidden        = "forbidden"
	errCodeMethodNotAllowed = "method_not_allowed"
	errCodeConflict         = "conflict"
	errCodePayloadTooLarge  = "payload_too_large"
	errCodeInvalidAudio     = "invalid_audio"
	errCodeInternal         = "internal_error"
)

// UploadIngester synchronously indexes a newly committed file under the uploads root.
type UploadIngester interface {
	IngestUpload(ctx context.Context, relPath string) (*store.Track, error)
}

var _ UploadIngester = (*scanner.Service)(nil)

// Dependencies configures NewHandler.
type Dependencies struct {
	// Auth authenticates incoming upload requests. Required.
	Auth auth.Authenticator

	// Ingester synchronously indexes newly committed audio files. Required.
	Ingester UploadIngester

	// UploadsDir is the target directory for uploaded tracks. Required; must exist
	// and be a directory.
	UploadsDir string

	// UploadsEnabled controls whether authenticated admins may upload files.
	UploadsEnabled bool

	// MaxFileBytes bounds the maximum accepted audio file size in bytes. Required (> 0).
	MaxFileBytes int64

	// Logger records unexpected internal errors. Optional; defaults to
	// slog.New(slog.DiscardHandler).
	Logger *slog.Logger
}

// Handler serves POST /api/v1/upload under an os.Root confined to UploadsDir.
// Call Close during graceful shutdown after HTTP draining completes.
type Handler struct {
	ingester       UploadIngester
	uploadsDir     string
	uploadsEnabled bool
	maxFileBytes   int64
	logger         *slog.Logger
	root           *os.Root
	closed         atomic.Bool
	authHandler    http.Handler
}

type uploadResponse struct {
	Status string        `json:"status"`
	Track  uploadedTrack `json:"track"`
}

type uploadedTrack struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewHandler constructs an upload Handler confined to deps.UploadsDir.
func NewHandler(deps Dependencies) (*Handler, error) {
	if deps.Auth == nil {
		return nil, errors.New("upload: auth authenticator must not be nil")
	}
	if deps.Ingester == nil {
		return nil, errors.New("upload: ingester must not be nil")
	}
	trimmedDir := strings.TrimSpace(deps.UploadsDir)
	if trimmedDir == "" {
		return nil, errors.New("upload: uploads directory must not be empty")
	}
	if deps.MaxFileBytes <= 0 {
		return nil, fmt.Errorf("upload: max file bytes %d must be positive", deps.MaxFileBytes)
	}

	absDir, err := filepath.Abs(trimmedDir)
	if err != nil {
		return nil, fmt.Errorf("upload: resolve uploads directory %q: %w", trimmedDir, err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return nil, fmt.Errorf("upload: stat uploads directory %q: %w", absDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("upload: uploads path %q is not a directory", absDir)
	}

	root, err := os.OpenRoot(absDir)
	if err != nil {
		return nil, fmt.Errorf("upload: open uploads root %q: %w", absDir, err)
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	h := &Handler{
		ingester:       deps.Ingester,
		uploadsDir:     absDir,
		uploadsEnabled: deps.UploadsEnabled,
		maxFileBytes:   deps.MaxFileBytes,
		logger:         logger,
		root:           root,
	}

	authMW := auth.Middleware(deps.Auth, writeAuthError)
	h.authHandler = queryOnlyPreAuthMiddleware(authMW(http.HandlerFunc(h.handleUpload)))
	return h, nil
}

// ServeHTTP handles POST /api/v1/upload requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.root == nil || h.closed.Load() {
		writeError(w, http.StatusInternalServerError, errCodeInternal, "Internal server error")
		return
	}
	if r == nil {
		writeError(w, http.StatusBadRequest, errCodeBadRequest, "Request must not be nil")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, errCodeMethodNotAllowed, "Method not allowed; use POST")
		return
	}
	h.authHandler.ServeHTTP(w, r)
}

// Close releases the underlying os.Root handle held by Handler.
// Close is nil-safe, idempotent, and safe to call concurrently with ServeHTTP.
func (h *Handler) Close() error {
	if h == nil {
		return nil
	}
	h.closed.Store(true)
	if h.root == nil {
		return nil
	}
	return h.root.Close()
}

// queryOnlyPreAuthMiddleware populates r2.Form strictly from r.URL.RawQuery so
// downstream authenticators never invoke r.ParseForm() or consume r.Body.
func queryOnlyPreAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rawQuery string
		if r.URL != nil {
			rawQuery = r.URL.RawQuery
		}
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeBadRequest, "Invalid query string")
			return
		}
		if query == nil {
			query = url.Values{}
		}

		r2 := r.Clone(r.Context())
		r2.Form = query
		r2.PostForm = nil
		next.ServeHTTP(w, r2)
	})
}

func writeAuthError(w http.ResponseWriter, _ *http.Request, _ int, msg string) {
	writeError(w, http.StatusUnauthorized, errCodeUnauthorized, msg)
}

func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request) {
	principal := auth.PrincipalFromContext(r.Context())
	if !h.uploadsEnabled {
		writeError(w, http.StatusForbidden, errCodeForbidden, "Uploads are disabled")
		return
	}
	if !principal.IsAdmin {
		writeError(w, http.StatusForbidden, errCodeForbidden, "Administrator privileges required for uploads")
		return
	}

	overwrite, err := parseOverwriteParam(r.Form)
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadRequest, err.Error())
		return
	}

	var (
		relPath string
		ext     string
	)
	pathVals, hasPath := r.Form["path"]
	if hasPath {
		if len(pathVals) > 1 {
			writeError(w, http.StatusBadRequest, errCodeBadRequest, "Duplicate path query parameter")
			return
		}
		relPath, ext, err = sanitizeUploadPath(pathVals[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeBadRequest, err.Error())
			return
		}
	}

	stagedRelPath, resolvedRelPath, status, errMsg, code := h.stageMultipartUpload(w, r, hasPath, relPath, ext)
	if status != 0 {
		writeError(w, status, code, errMsg)
		return
	}
	defer h.cleanupStagedFile(&stagedRelPath)

	if err := metadata.ValidateAudio(filepath.Join(h.uploadsDir, stagedRelPath)); err != nil {
		if errors.Is(err, metadata.ErrInvalidAudio) {
			writeError(w, http.StatusUnprocessableEntity, errCodeInvalidAudio, "Uploaded file is not a valid audio file")
			return
		}
		h.logger.Error("upload: validate staged audio failed", "rel_path", resolvedRelPath, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "Internal server error")
		return
	}

	httpStatus, statusText, errCode, errMsg := h.commitStagedFile(&stagedRelPath, resolvedRelPath, overwrite)
	if errCode != "" {
		writeError(w, httpStatus, errCode, errMsg)
		return
	}

	track, err := h.ingester.IngestUpload(r.Context(), resolvedRelPath)
	if err != nil {
		// Never delete an already-committed file after IngestUpload failure because
		// overwrite=true may have replaced an earlier file at the same path.
		h.logger.Error("upload: ingest committed track failed", "rel_path", resolvedRelPath, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "Internal server error")
		return
	}
	if track == nil {
		h.logger.Error("upload: ingest committed track returned nil track", "rel_path", resolvedRelPath)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "Internal server error")
		return
	}

	displayRelPath := track.RelPath
	if displayRelPath == "" {
		displayRelPath = resolvedRelPath
	}
	writeJSON(w, httpStatus, uploadResponse{
		Status: statusText,
		Track: uploadedTrack{
			ID:     track.ID,
			Path:   "uploads/" + displayRelPath,
			Size:   track.FileSize,
			Title:  track.Title,
			Artist: track.Artist,
			Album:  track.Album,
		},
	})
}

func (h *Handler) stageMultipartUpload(
	w http.ResponseWriter,
	r *http.Request,
	hasPath bool,
	relPath string,
	ext string,
) (stagedName, finalRelPath string, errStatus int, errMsg, errCode string) {
	bodyLimit := h.maxFileBytes + multipartOverheadBytes
	if h.maxFileBytes > math.MaxInt64-multipartOverheadBytes {
		bodyLimit = math.MaxInt64
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)

	mr, err := r.MultipartReader()
	if err != nil {
		return "", "", http.StatusBadRequest, "Request must be multipart/form-data", errCodeBadRequest
	}

	var seenFile bool
	defer func() {
		if errStatus != 0 {
			h.cleanupStagedFile(&stagedName)
		}
	}()

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if isMaxBytesErr(err) {
				return stagedName, "", http.StatusRequestEntityTooLarge, "Uploaded payload exceeds maximum size", errCodePayloadTooLarge
			}
			return stagedName, "", http.StatusBadRequest, "Malformed multipart request body", errCodeBadRequest
		}

		if part.FormName() != "file" {
			_, copyErr := io.Copy(io.Discard, part)
			_ = part.Close()
			if copyErr != nil {
				if isMaxBytesErr(copyErr) {
					return stagedName, "", http.StatusRequestEntityTooLarge, "Uploaded payload exceeds maximum size", errCodePayloadTooLarge
				}
				return stagedName, "", http.StatusBadRequest, "Malformed multipart request body", errCodeBadRequest
			}
			continue
		}

		if seenFile {
			_ = part.Close()
			return stagedName, "", http.StatusBadRequest, "Duplicate file part", errCodeBadRequest
		}
		seenFile = true

		if !hasPath {
			relPath, ext, err = sanitizeUploadPath(part.FileName())
			if err != nil {
				_ = part.Close()
				return "", "", http.StatusBadRequest, err.Error(), errCodeBadRequest
			}
		}

		tmpName, err := generateStagedFilename(ext)
		if err != nil {
			_ = part.Close()
			h.logger.Error("upload: generate staged filename failed", "error", err)
			return "", "", http.StatusInternalServerError, "Internal server error", errCodeInternal
		}

		tmpFile, err := h.root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			_ = part.Close()
			h.logger.Error("upload: create staged file failed", "rel_path", relPath, "error", err)
			return "", "", http.StatusInternalServerError, "Internal server error", errCodeInternal
		}
		stagedName = tmpName

		written, srcErr, dstErr := copyBoundedPart(tmpFile, part, h.maxFileBytes)
		closeErr := tmpFile.Close()
		_ = part.Close()

		if dstErr != nil {
			h.logger.Error("upload: write staged file failed", "rel_path", relPath, "error", dstErr)
			return stagedName, "", http.StatusInternalServerError, "Internal server error", errCodeInternal
		}
		if srcErr != nil {
			if isMaxBytesErr(srcErr) {
				return stagedName, "", http.StatusRequestEntityTooLarge, "Uploaded file exceeds maximum size", errCodePayloadTooLarge
			}
			return stagedName, "", http.StatusBadRequest, "Malformed multipart file stream", errCodeBadRequest
		}
		if closeErr != nil {
			h.logger.Error("upload: close staged file failed", "rel_path", relPath, "error", closeErr)
			return stagedName, "", http.StatusInternalServerError, "Internal server error", errCodeInternal
		}
		if written > h.maxFileBytes {
			return stagedName, "", http.StatusRequestEntityTooLarge, "Uploaded file exceeds maximum size", errCodePayloadTooLarge
		}
		if written == 0 {
			return stagedName, "", http.StatusBadRequest, "Uploaded file must not be empty", errCodeBadRequest
		}
	}

	if !seenFile {
		return "", "", http.StatusBadRequest, "Missing file part", errCodeBadRequest
	}
	return stagedName, relPath, 0, "", ""
}

func (h *Handler) commitStagedFile(stagedName *string, relPath string, overwrite bool) (httpStatus int, statusText, errCode, errMsg string) {
	if parent := path.Dir(relPath); parent != "." {
		if err := h.root.MkdirAll(filepath.FromSlash(parent), 0o750); err != nil {
			if isConflictErr(err) {
				return http.StatusConflict, "", errCodeConflict, "Target path conflicts with an existing file or directory"
			}
			h.logger.Error("upload: create parent directories failed", "rel_path", relPath, "error", err)
			return http.StatusInternalServerError, "", errCodeInternal, "Internal server error"
		}
	}

	osRelPath := filepath.FromSlash(relPath)
	err := h.root.Link(*stagedName, osRelPath)
	if err == nil {
		if rmErr := h.root.Remove(*stagedName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			h.logger.Warn("upload: remove staged link after commit failed", "rel_path", relPath, "error", rmErr)
		}
		*stagedName = ""
		return http.StatusCreated, "created", "", ""
	}

	if !overwrite {
		if isConflictErr(err) {
			return http.StatusConflict, "", errCodeConflict, "Track already exists at target path"
		}
		h.logger.Error("upload: link staged file failed", "rel_path", relPath, "error", err)
		return http.StatusInternalServerError, "", errCodeInternal, "Internal server error"
	}

	if !errors.Is(err, os.ErrExist) {
		if isConflictErr(err) {
			return http.StatusConflict, "", errCodeConflict, "Target path conflicts with an existing directory"
		}
		h.logger.Error("upload: link staged file failed", "rel_path", relPath, "error", err)
		return http.StatusInternalServerError, "", errCodeInternal, "Internal server error"
	}

	info, statErr := h.root.Lstat(osRelPath)
	if statErr != nil {
		if isConflictErr(statErr) {
			return http.StatusConflict, "", errCodeConflict, "Target path conflicts with an existing directory"
		}
		h.logger.Error("upload: stat existing target failed", "rel_path", relPath, "error", statErr)
		return http.StatusInternalServerError, "", errCodeInternal, "Internal server error"
	}
	if info.IsDir() {
		return http.StatusConflict, "", errCodeConflict, "Target path conflicts with an existing directory"
	}

	if renameErr := h.root.Rename(*stagedName, osRelPath); renameErr != nil {
		if isConflictErr(renameErr) {
			return http.StatusConflict, "", errCodeConflict, "Target path conflicts with an existing directory"
		}
		h.logger.Error("upload: replace existing target failed", "rel_path", relPath, "error", renameErr)
		return http.StatusInternalServerError, "", errCodeInternal, "Internal server error"
	}

	*stagedName = ""
	return http.StatusOK, "updated", "", ""
}

func (h *Handler) cleanupStagedFile(stagedName *string) {
	if stagedName == nil || *stagedName == "" {
		return
	}
	if err := h.root.Remove(*stagedName); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.logger.Warn("upload: remove staged file failed", "staged_name", *stagedName, "error", err)
	}
	*stagedName = ""
}

func parseOverwriteParam(form url.Values) (bool, error) {
	vals := form["overwrite"]
	if len(vals) > 1 {
		return false, errors.New("duplicate overwrite query parameter")
	}
	if len(vals) == 0 {
		return false, nil
	}
	switch vals[0] {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("invalid overwrite query parameter")
	}
}

// sanitizeUploadPath validates a caller-supplied relative upload path and returns
// its forward-slash form along with the canonical lowercase extension (".mp3", etc.).
func sanitizeUploadPath(raw string) (relPath, ext string, err error) {
	if strings.TrimSpace(raw) == "" {
		return "", "", errors.New("upload path must not be empty")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] == 0x7f {
			return "", "", errors.New("upload path contains invalid control characters")
		}
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\`) {
		return "", "", errors.New("upload path must be relative")
	}
	if hasWindowsVolumePrefix(raw) || filepath.VolumeName(raw) != "" {
		return "", "", errors.New("upload path must not contain a volume prefix")
	}
	if strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, `\`) {
		return "", "", errors.New("upload path must not end with a directory separator")
	}

	slashed := strings.ReplaceAll(raw, `\`, "/")
	if len(slashed) > maxUploadPathBytes {
		return "", "", fmt.Errorf("upload path exceeds %d bytes", maxUploadPathBytes)
	}

	segments := strings.Split(slashed, "/")
	if len(segments) > maxUploadPathSegments {
		return "", "", fmt.Errorf("upload path exceeds %d segments", maxUploadPathSegments)
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." || strings.TrimSpace(seg) == "" {
			return "", "", errors.New("upload path contains an invalid segment")
		}
		if metadata.IsIgnoredName(seg) {
			return "", "", errors.New("upload path contains an ignored file or directory name")
		}
	}

	if !fs.ValidPath(slashed) || slashed == "." {
		return "", "", errors.New("upload path is not a valid relative path")
	}

	suffix, _, ok := metadata.LookupFormat(slashed)
	if !ok {
		return "", "", errors.New("unsupported audio format")
	}
	return slashed, "." + suffix, nil
}

func hasWindowsVolumePrefix(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	return (p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')
}

func generateStagedFilename(ext string) (string, error) {
	var raw [stagedFileHexBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".upload-" + hex.EncodeToString(raw[:]) + ext, nil
}

// copyBoundedPart streams up to maxBytes+1 bytes from part to dst while keeping
// source read errors distinct from destination write errors.
func copyBoundedPart(dst io.Writer, part *multipart.Part, maxBytes int64) (written int64, srcErr, dstErr error) {
	limit := maxBytes + 1
	if maxBytes == math.MaxInt64 {
		limit = math.MaxInt64
	}
	lr := io.LimitReader(part, limit)
	buf := make([]byte, 32*1024)
	for {
		n, rErr := lr.Read(buf)
		if n > 0 {
			nw, wErr := dst.Write(buf[:n])
			written += int64(nw)
			if wErr != nil {
				return written, nil, wErr
			}
			if nw != n {
				return written, nil, io.ErrShortWrite
			}
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				return written, nil, nil
			}
			return written, rErr, nil
		}
	}
}

func isMaxBytesErr(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func isConflictErr(err error) bool {
	return errors.Is(err, os.ErrExist) ||
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOTDIR) ||
		errors.Is(err, syscall.ENOTEMPTY)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{
		Error: errorDetail{
			Code:    code,
			Message: message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"Internal server error"}}` + "\n"))
		return
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
