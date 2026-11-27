//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"headunit/internal/subsonic"
	"headunit/internal/testutil"
)

const (
	pollInterval = 50 * time.Millisecond
	execTimeout  = 30 * time.Second
)

var testUserSeq atomic.Uint64

type config struct {
	baseURL        string
	musicDir       string
	scanTimeout    time.Duration
	composeFile    string
	composeProject string
	service        string
	puid           string
	pgid           string
}

type jsonSubsonicEnvelope struct {
	SubsonicResponse subsonic.Response `json:"subsonic-response"`
}

type harness struct {
	cfg          config
	client       *http.Client
	cleanupPaths []string
	username     string
	password     string
	apiKeyID     string
	apiKey       string
	saltSeq      atomic.Uint64
}

func newRawHarness(t *testing.T) *harness {
	t.Helper()

	return &harness{
		cfg:    loadConfig(t),
		client: newProxyBypassHTTPClient(t),
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := newRawHarness(t)
	username := uniqueTestUsername(t)
	password := "e2e-pass-" + username

	h.createUser(t, username, password, false)
	// Register user deletion before cleanupFixtures so Go's LIFO t.Cleanup
	// drains library fixtures while auth credentials remain valid, and both run
	// before newProxyBypassHTTPClient closes idle connections.
	t.Cleanup(func() {
		h.deleteUser(t, username)
	})

	keyID, apiKey := h.createAPIKey(t, username, "e2e-harness")
	h.username = username
	h.password = password
	h.apiKeyID = keyID
	h.apiKey = apiKey

	t.Cleanup(func() {
		h.cleanupFixtures(t)
	})
	return h
}

func uniqueTestUsername(t *testing.T) string {
	t.Helper()

	var b strings.Builder
	for i := range len(t.Name()) {
		c := t.Name()[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	return fmt.Sprintf("e2e_%s_%d_%d", b.String(), os.Getpid(), testUserSeq.Add(1))
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

	composeFile := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_COMPOSE_FILE"))
	if composeFile == "" {
		t.Fatalf("HEADUNIT_E2E_COMPOSE_FILE must be set when running -tags=e2e")
	}
	if _, err := os.Stat(composeFile); err != nil {
		t.Fatalf("HEADUNIT_E2E_COMPOSE_FILE %q stat error: %v", composeFile, err)
	}

	composeProject := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_COMPOSE_PROJECT"))
	if composeProject == "" {
		t.Fatalf("HEADUNIT_E2E_COMPOSE_PROJECT must be set when running -tags=e2e")
	}

	service := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_SERVICE"))
	if service == "" {
		t.Fatalf("HEADUNIT_E2E_SERVICE must be set when running -tags=e2e")
	}

	puid := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_PUID"))
	if !isNonNegativeIntString(puid) {
		t.Fatalf("HEADUNIT_E2E_PUID=%q must be a non-negative integer when running -tags=e2e", puid)
	}

	pgid := strings.TrimSpace(os.Getenv("HEADUNIT_E2E_PGID"))
	if !isNonNegativeIntString(pgid) {
		t.Fatalf("HEADUNIT_E2E_PGID=%q must be a non-negative integer when running -tags=e2e", pgid)
	}

	return config{
		baseURL:        baseURL,
		musicDir:       musicDir,
		scanTimeout:    scanTimeout,
		composeFile:    composeFile,
		composeProject: composeProject,
		service:        service,
		puid:           puid,
		pgid:           pgid,
	}
}

func isNonNegativeIntString(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (h *harness) execCompose(t *testing.T, stdin io.Reader, extraEnv []string, execArgs ...string) string {
	t.Helper()

	out, err := h.execComposeRaw(stdin, extraEnv, execArgs...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

func (h *harness) execComposeRaw(stdin io.Reader, extraEnv []string, execArgs ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	fullArgs := make([]string, 0, 6+len(execArgs))
	fullArgs = append(fullArgs, "compose", "-f", h.cfg.composeFile, "-p", h.cfg.composeProject, "exec", "-T")
	fullArgs = append(fullArgs, execArgs...)

	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	cmd.Stdin = stdin
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w (stderr: %s)", strings.Join(fullArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (h *harness) execHeadunit(t *testing.T, stdin io.Reader, extraEnv []string, args ...string) string {
	t.Helper()

	out, err := h.execHeadunitRaw(stdin, extraEnv, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

func (h *harness) execHeadunitRaw(stdin io.Reader, extraEnv []string, args ...string) (string, error) {
	execArgs := make([]string, 0, len(extraEnv)*2+2+len(args))
	for _, entry := range extraEnv {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return "", fmt.Errorf("invalid extraEnv entry %q: want KEY=VALUE", entry)
		}
		// Forward only the variable name on argv (-e VAR); secret values stay in cmd.Env.
		execArgs = append(execArgs, "-e", name)
	}
	execArgs = append(execArgs, h.cfg.service, "/usr/local/bin/entrypoint.sh")
	execArgs = append(execArgs, args...)
	return h.execComposeRaw(stdin, extraEnv, execArgs...)
}

func (h *harness) createUser(t *testing.T, username, password string, isAdmin bool) {
	t.Helper()

	args := []string{"user", "create", "--username", username, "--password-stdin"}
	if isAdmin {
		args = append(args, "--admin")
	}
	h.execHeadunit(t, strings.NewReader(password+"\n"), nil, args...)
}

func (h *harness) createUserWithEnv(t *testing.T, username, envVar, password string, isAdmin bool) {
	t.Helper()

	args := []string{"user", "create", "--username", username, "--password-env", envVar}
	if isAdmin {
		args = append(args, "--admin")
	}
	h.execHeadunit(t, nil, []string{envVar + "=" + password}, args...)
}

func (h *harness) createAPIKey(t *testing.T, username, keyName string) (keyID, apiKey string) {
	t.Helper()

	args := []string{"apikey", "create", "--username", username}
	if keyName != "" {
		args = append(args, "--name", keyName)
	}
	out := h.execHeadunit(t, nil, nil, args...)
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "id":
			keyID = strings.TrimSpace(v)
		case "api_key":
			apiKey = strings.TrimSpace(v)
		}
	}
	if keyID == "" || apiKey == "" {
		t.Fatalf("apikey create output missing id or api_key: %q", out)
	}
	return keyID, apiKey
}

func (h *harness) revokeAPIKey(t *testing.T, keyID string) {
	t.Helper()

	h.execHeadunit(t, nil, nil, "apikey", "revoke", "--id", keyID)
}

func (h *harness) disableUser(t *testing.T, username string) {
	t.Helper()

	h.execHeadunit(t, nil, nil, "user", "disable", "--username", username)
}

func (h *harness) deleteUser(t *testing.T, username string) {
	t.Helper()

	if _, err := h.execHeadunitRaw(nil, nil, "user", "delete", "--username", username); err != nil {
		t.Errorf("delete user %q: %v", username, err)
	}
}

func tokenParams(username, password, salt string) url.Values {
	sum := md5.Sum([]byte(password + salt))
	v := url.Values{}
	v.Set("u", username)
	v.Set("t", hex.EncodeToString(sum[:]))
	v.Set("s", salt)
	return v
}

func apiKeyParams(apiKey string) url.Values {
	v := url.Values{}
	v.Set("apiKey", apiKey)
	return v
}

func (h *harness) nextSalt() string {
	return fmt.Sprintf("e2esalt%04d", h.saltSeq.Add(1))
}

func (h *harness) withParams(t *testing.T, rawPath string, params url.Values) string {
	t.Helper()

	if len(params) == 0 {
		return rawPath
	}
	u, err := url.Parse(rawPath)
	if err != nil {
		t.Fatalf("url.Parse(%q) unexpected error: %v", rawPath, err)
	}
	q := u.Query()
	for k, vals := range params {
		q[k] = slices.Clone(vals)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (h *harness) withTokenAuth(t *testing.T, rawPath string) string {
	t.Helper()

	if h.username == "" || h.password == "" {
		t.Fatalf("withTokenAuth called on harness without provisioned user")
	}
	return h.withParams(t, rawPath, tokenParams(h.username, h.password, h.nextSalt()))
}

func (h *harness) withAPIKeyAuth(t *testing.T, rawPath string) string {
	t.Helper()

	if h.apiKey == "" {
		t.Fatalf("withAPIKeyAuth called on harness without provisioned API key")
	}
	return h.withParams(t, rawPath, apiKeyParams(h.apiKey))
}

func (h *harness) withDefaultAuth(t *testing.T, rawPath string) string {
	t.Helper()

	if h.apiKey != "" {
		return h.withAPIKeyAuth(t, rawPath)
	}
	if h.username != "" && h.password != "" {
		return h.withTokenAuth(t, rawPath)
	}
	return rawPath
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

func (h *harness) getXML(t *testing.T, path string) (subsonic.Response, []byte) {
	t.Helper()

	resp, body := h.doRequest(t, http.MethodGet, path, nil, nil)
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET %s status = %d, want %d (body=%s)", path, got, want, string(body))
	}
	if got, want := resp.Header.Get("Content-Type"), "application/xml; charset=utf-8"; got != want {
		t.Errorf("GET %s Content-Type = %q, want %q", path, got, want)
	}
	if got, want := resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("GET %s X-Content-Type-Options = %q, want %q", path, got, want)
	}
	if !bytes.HasPrefix(body, []byte(xml.Header)) {
		t.Errorf("GET %s body = %q, want xml.Header prefix %q", path, string(body), xml.Header)
	}
	return decodeXMLResponse(t, body), body
}

func (h *harness) pollScanStatus(t *testing.T, ready func(subsonic.ScanStatus) bool) subsonic.ScanStatus {
	t.Helper()

	deadline := time.Now().Add(h.cfg.scanTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var last subsonic.ScanStatus
	for {
		env, _ := h.getJSON(t, h.withDefaultAuth(t, "/rest/getScanStatus?f=json"))
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

	path := h.withDefaultAuth(t, "/rest/stream?id="+url.QueryEscape(trackID)+"&f=json")
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

func (h *harness) assertHealthz(t *testing.T) {
	t.Helper()

	getResp, getBody := h.doRequest(t, http.MethodGet, "/healthz", nil, nil)
	if got, want := getResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /healthz status = %d, want %d (body=%s)", got, want, string(getBody))
	}
	if got, want := getResp.Header.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("GET /healthz Content-Type = %q, want %q", got, want)
	}
	if got, want := getResp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("GET /healthz X-Content-Type-Options = %q, want %q", got, want)
	}
	if got, want := string(getBody), "ok\n"; got != want {
		t.Errorf("GET /healthz body = %q, want %q", got, want)
	}

	headResp, headBody := h.doRequest(t, http.MethodHead, "/healthz", nil, nil)
	if got, want := headResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("HEAD /healthz status = %d, want %d", got, want)
	}
	if got, want := headResp.Header.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("HEAD /healthz Content-Type = %q, want %q", got, want)
	}
	if got, want := headResp.Header.Get("Content-Length"), "3"; got != want {
		t.Errorf("HEAD /healthz Content-Length = %q, want %q", got, want)
	}
	if got := len(headBody); got != 0 {
		t.Errorf("HEAD /healthz body len = %d, want 0", got)
	}
}

func assertAPIKeyExtension(t *testing.T, env subsonic.Response) {
	t.Helper()

	if env.OpenSubsonicExtensions == nil {
		t.Fatalf("openSubsonicExtensions = nil, want non-nil")
	}
	exts := *env.OpenSubsonicExtensions
	if got, want := len(exts), 1; got != want {
		t.Fatalf("len(openSubsonicExtensions) = %d, want %d (%+v)", got, want, exts)
	}
	if got, want := exts[0].Name, "apiKeyAuthentication"; got != want {
		t.Errorf("openSubsonicExtensions[0].Name = %q, want %q", got, want)
	}
	if got, want := exts[0].Versions, []int{1}; !slices.Equal(got, want) {
		t.Errorf("openSubsonicExtensions[0].Versions = %v, want %v", got, want)
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
