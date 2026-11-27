//go:build e2e

package e2e

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"headunit/internal/subsonic"
)

func TestSystem(t *testing.T) {
	h := newHarness(t)

	t.Run("PingXML", func(t *testing.T) {
		resp, body := h.doRequest(t, http.MethodGet, "/rest/ping", nil, nil)
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("GET /rest/ping status = %d, want %d (body=%s)", got, want, string(body))
		}
		if got, want := resp.Header.Get("Content-Type"), "application/xml; charset=utf-8"; got != want {
			t.Errorf("GET /rest/ping Content-Type = %q, want %q", got, want)
		}
		if got, want := resp.Header.Get("X-Content-Type-Options"), "nosniff"; got != want {
			t.Errorf("GET /rest/ping X-Content-Type-Options = %q, want %q", got, want)
		}
		if !bytes.HasPrefix(body, []byte(xml.Header)) {
			t.Errorf("GET /rest/ping body = %q, want xml.Header prefix %q", string(body), xml.Header)
		}
		xmlEnv := decodeXMLResponse(t, body)
		assertXMLResponseEnvelope(t, xmlEnv, "ok")
		if xmlEnv.Error != nil {
			t.Errorf("GET /rest/ping error = %+v, want nil", xmlEnv.Error)
		}
	})

	t.Run("PingJSON", func(t *testing.T) {
		jsonEnv, _ := h.getJSON(t, "/rest/ping.view?f=json")
		assertJSONResponseEnvelope(t, jsonEnv, "ok")
	})

	t.Run("OpenSubsonicExtensions", func(t *testing.T) {
		extEnv, extBody := h.getJSON(t, "/rest/getOpenSubsonicExtensions.view?f=json")
		assertJSONResponseEnvelope(t, extEnv, "ok")
		if !strings.Contains(string(extBody), `"openSubsonicExtensions":[]`) {
			t.Errorf("getOpenSubsonicExtensions body = %s, want empty openSubsonicExtensions array", string(extBody))
		}
	})

	t.Run("MusicFolders", func(t *testing.T) {
		foldersEnv, _ := h.getJSON(t, "/rest/getMusicFolders?f=json")
		assertJSONResponseEnvelope(t, foldersEnv, "ok")
		if foldersEnv.MusicFolders == nil {
			t.Fatalf("getMusicFolders MusicFolders = nil, want non-nil")
		}
		want := []subsonic.MusicFolder{{ID: 1, Name: "Music"}}
		if got := foldersEnv.MusicFolders.MusicFolder; !slices.Equal(got, want) {
			t.Errorf("getMusicFolders = %+v, want %+v", got, want)
		}
	})

	t.Run("ScanStatusBaseline", func(t *testing.T) {
		status := h.pollScanStatus(t, func(s subsonic.ScanStatus) bool {
			return !s.Scanning && s.LastScan != ""
		})
		if got, want := status.Count, int64(0); got != want {
			t.Errorf("scanStatus.Count = %d, want %d", got, want)
		}
		if _, err := time.Parse(time.RFC3339, status.LastScan); err != nil {
			t.Errorf("scanStatus.LastScan = %q, want valid RFC3339 timestamp: %v", status.LastScan, err)
		}
	})
}
