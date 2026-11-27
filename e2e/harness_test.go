//go:build e2e

package e2e

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

const pollInterval = 50 * time.Millisecond

type config struct {
	baseURL     string
	musicDir    string
	scanTimeout time.Duration
}

type jsonSubsonicEnvelope struct {
	SubsonicResponse subsonic.Response `json:"subsonic-response"`
}

type harness struct {
	cfg          config
	client       *http.Client
	cleanupPaths []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		cfg:    loadConfig(t),
		client: newProxyBypassHTTPClient(t),
	}
	// Register fixture cleanup after newProxyBypassHTTPClient so Go's LIFO
	// t.Cleanup order runs cleanupFixtures before closing idle connections.
	t.Cleanup(func() {
		h.cleanupFixtures(t)
	})
	return h
}

func loadConfig(t *testing.T) config {
	t.Helper()

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("HEADUNIT_E2E_URL")), "/")
	if baseURL == "" {
		t.Fatalf("HEADUNIT_E2E_URL must be set when running -tags=e2e")
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("HEADUNIT_E2E_URL=%q parse error: %v", baseURL, err)
	}
	if (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		t.Fatalf("HEADUNIT_E2E_URL=%q must be a valid HTTP(S) base URL", baseURL)
	}

	musicDir := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_MUSIC_DIR"))
	if musicDir == "" {
		t.Fatalf("HEADUNIT_E2E_MUSIC_DIR must be set when running -tags=e2e")
	}
	info, err := os.Stat(musicDir)
	if err != nil {
		t.Fatalf("HEADUNIT_E2E_MUSIC_DIR %q stat error: %v", musicDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("HEADUNIT_E2E_MUSIC_DIR %q is not a directory", musicDir)
	}

	scanTimeout := 30 * time.Second
	if rawTimeout := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_SCAN_TIMEOUT")); rawTimeout != "" {
		parsed, err := time.ParseDuration(rawTimeout)
		if err != nil {
			t.Fatalf("HEADUNIT_E2E_SCAN_TIMEOUT=%q must be a valid duration: %v", rawTimeout, err)
		}
		if parsed <= 0 {
			t.Fatalf("HEADUNIT_E2E_SCAN_TIMEOUT=%q must be a positive duration", rawTimeout)
		}
		scanTimeout = parsed
	}

	return config{
		baseURL:     baseURL,
		musicDir:    musicDir,
		scanTimeout: scanTimeout,
	}
}

func newProxyBypassHTTPClient(t *testing.T) *http.Client {
	t.Helper()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func (h *harness) waitForCleanLibrary(t *testing.T) subsonic.ScanStatus {
	t.Helper()

	entries, err := os.ReadDir(h.cfg.musicDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q): %v", h.cfg.musicDir, err)
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("HEADUNIT_E2E_MUSIC_DIR %q must be empty before test starts; found %v", h.cfg.musicDir, names)
	}

	return h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.LastScan != "" && s.Count == 0
	})
}

func (h *harness) cleanupFixtures(t *testing.T) {
	t.Helper()

	if len(h.cleanupPaths) == 0 {
		return
	}
	for _, path := range h.cleanupPaths {
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("cleanup os.RemoveAll(%q): %v", path, err)
		}
	}
	h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
		return !s.Scanning && s.Count == 0
	})
}

func (h *harness) publishFile(t *testing.T, filename string, data []byte) string {
	t.Helper()

	dstPath := publishReadableFile(t, h.cfg.musicDir, filename, data)
	h.trackCleanupPath(dstPath)
	return dstPath
}

func (h *harness) publishDirWithFile(t *testing.T, dirName, filename string, data []byte) string {
	t.Helper()

	dstFile := publishReadableDirWithFile(t, h.cfg.musicDir, dirName, filename, data)
	h.trackCleanupPath(filepath.Join(h.cfg.musicDir, dirName))
	return dstFile
}

func (h *harness) trackCleanupPath(path string) {
	if !slices.Contains(h.cleanupPaths, path) {
		h.cleanupPaths = append(h.cleanupPaths, path)
	}
}

// Host-staged temp files default to 0600/0700 owned by a host UID that may differ from
// the container PUID, and internal/scanner/watcher.go drops Chmod-only events. Staging in
// dot-prefixed paths (metadata.IsIgnoredName), chmodding past umask, and renaming guarantees a single Create event on readable data.
func publishReadableFile(t *testing.T, dir, filename string, data []byte) string {
	t.Helper()

	tmpFile, err := os.CreateTemp(dir, ".e2e-stage-*.tmp")
	if err != nil {
		t.Fatalf("os.CreateTemp(%q): %v", dir, err)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		t.Fatalf("write temp file %q: %v", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file %q: %v", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		t.Fatalf("os.Chmod(%q, 0644): %v", tmpPath, err)
	}

	dstPath := filepath.Join(dir, filename)
	if err := os.Rename(tmpPath, dstPath); err != nil {
		t.Fatalf("os.Rename(%q, %q): %v", tmpPath, dstPath, err)
	}
	cleanupTmp = false
	return dstPath
}

func publishReadableDirWithFile(t *testing.T, musicDir, dirName, filename string, data []byte) string {
	t.Helper()

	stageDir, err := os.MkdirTemp(musicDir, ".e2e-dir-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp(%q): %v", musicDir, err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	if err := os.Chmod(stageDir, 0o755); err != nil {
		t.Fatalf("os.Chmod(%q, 0755): %v", stageDir, err)
	}

	stagedFile := filepath.Join(stageDir, filename)
	if err := os.WriteFile(stagedFile, data, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", stagedFile, err)
	}
	if err := os.Chmod(stagedFile, 0o644); err != nil {
		t.Fatalf("os.Chmod(%q, 0644): %v", stagedFile, err)
	}

	dstDir := filepath.Join(musicDir, dirName)
	if err := os.Rename(stageDir, dstDir); err != nil {
		t.Fatalf("os.Rename(%q, %q): %v", stageDir, dstDir, err)
	}
	cleanupStage = false
	return filepath.Join(dstDir, filename)
}

func buildTaggedFLACBytes(t *testing.T, filename string, tags map[string][]string) []byte {
	t.Helper()

	stagedPath := filepath.Join(t.TempDir(), filename)
	testutil.WriteMinimalFLAC(t, stagedPath)
	testutil.WriteTags(t, stagedPath, tags)
	return mustReadFile(t, stagedPath)
}

func (h *harness) doRequest(
	t *testing.T,
	method string,
	path string,
	body io.Reader,
	headers map[string]string,
) (*http.Response, []byte) {
	t.Helper()

	rawURL := h.cfg.baseURL + path
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatalf("http.NewRequest(%s, %q) unexpected error: %v", method, rawURL, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s unexpected error: %v", method, rawURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s read body error: %v", method, rawURL, err)
	}
	return resp, respBody
}

func (h *harness) getJSON(t *testing.T, path string) (subsonic.Response, []byte) {
	t.Helper()

	resp, body := h.doRequest(t, http.MethodGet, path, nil, nil)
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET %s status = %d, want %d (body=%s)", path, got, want, string(body))
	}
	if got, want := resp.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("GET %s Content-Type = %q, want %q", path, got, want)
	}
	return decodeJSONResponse(t, body), body
}

func (h *harness) pollScanStatus(t *testing.T, ready func(subsonic.ScanStatus) bool) subsonic.ScanStatus {
	t.Helper()

	deadline := time.Now().Add(h.cfg.scanTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var last subsonic.ScanStatus
	for {
		env, _ := h.getJSON(t, "/rest/getScanStatus?f=json")
		assertJSONResponseEnvelope(t, env, "ok")
		if env.ScanStatus != nil {
			last = *env.ScanStatus
			if ready(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for scanStatus condition; last = %+v", h.cfg.scanTimeout, last)
		}
		<-ticker.C
	}
}

func (h *harness) assertStreamNotFound(t *testing.T, trackID string) {
	t.Helper()

	path := "/rest/stream?id=" + url.QueryEscape(trackID) + "&f=json"
	errResp, errBody := h.doRequest(t, http.MethodGet, path, nil, nil)
	if got, want := errResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET %s status = %d, want %d", path, got, want)
	}
	if got, want := errResp.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("stream error Content-Type = %q, want %q", got, want)
	}
	for _, hdr := range []string{"ETag", "Accept-Ranges", "Content-Range"} {
		if got := errResp.Header.Get(hdr); got != "" {
			t.Errorf("stream error header %s = %q, want empty", hdr, got)
		}
	}
	errEnv := decodeJSONResponse(t, errBody)
	assertJSONResponseEnvelope(t, errEnv, "failed")
	if errEnv.Error == nil || errEnv.Error.Code != 70 {
		t.Errorf("stream error payload = %+v, want code 70", errEnv.Error)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) unexpected error: %v", path, err)
	}
	return data
}

func decodeJSONResponse(t *testing.T, body []byte) subsonic.Response {
	t.Helper()

	var env jsonSubsonicEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; body = %s", err, string(body))
	}
	return env.SubsonicResponse
}

func decodeXMLResponse(t *testing.T, body []byte) subsonic.Response {
	t.Helper()

	var env subsonic.Response
	if err := xml.Unmarshal(body, &env); err != nil {
		t.Fatalf("xml.Unmarshal() error = %v; body = %s", err, string(body))
	}
	return env
}

func assertXMLResponseEnvelope(t *testing.T, env subsonic.Response, wantStatus string) {
	t.Helper()

	if got, want := env.Xmlns, "http://subsonic.org/restapi"; got != want {
		t.Errorf("xml xmlns = %q, want %q", got, want)
	}
	assertCommonEnvelope(t, env, wantStatus)
}

func assertJSONResponseEnvelope(t *testing.T, env subsonic.Response, wantStatus string) {
	t.Helper()

	assertCommonEnvelope(t, env, wantStatus)
}

func assertCommonEnvelope(t *testing.T, env subsonic.Response, wantStatus string) {
	t.Helper()

	if got := env.Status; got != wantStatus {
		t.Errorf("status = %q, want %q", got, wantStatus)
	}
	if got, want := env.Version, "1.16.1"; got != want {
		t.Errorf("version = %q, want %q", got, want)
	}
	if got, want := env.Type, "headunit"; got != want {
		t.Errorf("type = %q, want %q", got, want)
	}
	if got, want := env.ServerVersion, "0.1.0"; got != want {
		t.Errorf("serverVersion = %q, want %q", got, want)
	}
	if !env.OpenSubsonic {
		t.Errorf("openSubsonic = false, want true")
	}
}

func findSongByPath(songs []subsonic.Child, relPath string) (subsonic.Child, bool) {
	for _, s := range songs {
		if s.Path == relPath {
			return s, true
		}
	}
	return subsonic.Child{}, false
}
