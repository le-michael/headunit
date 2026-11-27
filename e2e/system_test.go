//go:build e2e

package e2e

import (
	"slices"
	"testing"
	"time"

	"headunit/internal/subsonic"
)

func TestSystem(t *testing.T) {
	h := newHarness(t)

	t.Run("Healthz", func(t *testing.T) {
		h.assertHealthz(t)
	})

	t.Run("PingXML", func(t *testing.T) {
		xmlEnv, _ := h.getXML(t, h.withTokenAuth(t, "/rest/ping"))
		assertXMLResponseEnvelope(t, xmlEnv, "ok")
		if xmlEnv.Error != nil {
			t.Errorf("GET /rest/ping error = %+v, want nil", xmlEnv.Error)
		}
	})

	t.Run("PingJSON", func(t *testing.T) {
		jsonEnv, _ := h.getJSON(t, h.withAPIKeyAuth(t, "/rest/ping.view?f=json"))
		assertJSONResponseEnvelope(t, jsonEnv, "ok")
	})

	t.Run("OpenSubsonicExtensions", func(t *testing.T) {
		extEnv, _ := h.getJSON(t, "/rest/getOpenSubsonicExtensions.view?f=json")
		assertJSONResponseEnvelope(t, extEnv, "ok")
		assertAPIKeyExtension(t, extEnv)
	})

	t.Run("TokenInfo", func(t *testing.T) {
		tokEnv, _ := h.getJSON(t, h.withAPIKeyAuth(t, "/rest/tokenInfo?f=json"))
		assertJSONResponseEnvelope(t, tokEnv, "ok")
		if tokEnv.TokenInfo == nil || tokEnv.TokenInfo.Username != h.username {
			t.Errorf("getTokenInfo = %+v, want username %q", tokEnv.TokenInfo, h.username)
		}
	})

	t.Run("MusicFolders", func(t *testing.T) {
		foldersEnv, _ := h.getJSON(t, h.withDefaultAuth(t, "/rest/getMusicFolders?f=json"))
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
