//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"headunit/internal/subsonic"
)

func TestAuth(t *testing.T) {
	h := newRawHarness(t)

	t.Run("Healthz", func(t *testing.T) {
		h.assertHealthz(t)
	})

	t.Run("ZeroUsersFailClosed", func(t *testing.T) {
		assertSubsonicErrorXMLAndJSON(t, h, "/rest/ping", 40)
	})

	t.Run("UnauthenticatedOpenSubsonicExtensions", func(t *testing.T) {
		xmlEnv, _ := h.getXML(t, "/rest/getOpenSubsonicExtensions")
		assertXMLResponseEnvelope(t, xmlEnv, "ok")
		assertAPIKeyExtension(t, xmlEnv)

		jsonEnv, _ := h.getJSON(t, "/rest/getOpenSubsonicExtensions.view?f=json")
		assertJSONResponseEnvelope(t, jsonEnv, "ok")
		assertAPIKeyExtension(t, jsonEnv)

		form := url.Values{"f": {"json"}}.Encode()
		postResp, postBody := h.doRequest(
			t,
			http.MethodPost,
			"/rest/getOpenSubsonicExtensions",
			strings.NewReader(form),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		)
		if got, want := postResp.StatusCode, http.StatusOK; got != want {
			t.Fatalf("POST /rest/getOpenSubsonicExtensions status = %d, want %d (body=%s)", got, want, string(postBody))
		}
		postEnv := decodeJSONResponse(t, postBody)
		assertJSONResponseEnvelope(t, postEnv, "ok")
		assertAPIKeyExtension(t, postEnv)
	})

	const (
		alicePass = "alice-e2e-pass!"
		bobPass   = "bob-e2e-pass!"
	)

	h.createUser(t, "alice", alicePass, true)
	t.Cleanup(func() {
		h.deleteUser(t, "alice")
	})

	h.createUserWithEnv(t, "bob", "HEADUNIT_E2E_BOB_PASSWORD", bobPass, false)
	t.Cleanup(func() {
		h.deleteUser(t, "bob")
	})

	bobKeyID, bobAPIKey := h.createAPIKey(t, "bob", "e2e-bob-key")

	t.Run("AuthKeyOwnershipAndPermissions", func(t *testing.T) {
		got := strings.TrimSpace(h.execCompose(t, nil, nil, h.cfg.service, "stat", "-c", "%u:%g %a", "/data/auth.key"))
		want := h.cfg.puid + ":" + h.cfg.pgid + " 600"
		if got != want {
			t.Errorf("/data/auth.key stat = %q, want %q", got, want)
		}
	})

	t.Run("AuthenticatedPing", func(t *testing.T) {
		bobToken := tokenParams("bob", bobPass, h.nextSalt())
		bobKey := apiKeyParams(bobAPIKey)

		cases := []struct {
			name   string
			params url.Values
		}{
			{name: "SaltedMD5", params: bobToken},
			{name: "APIKey", params: bobKey},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				xmlEnv, _ := h.getXML(t, h.withParams(t, "/rest/ping", tc.params))
				assertXMLResponseEnvelope(t, xmlEnv, "ok")

				jsonEnv, _ := h.getJSON(t, h.withParams(t, "/rest/ping?f=json", tc.params))
				assertJSONResponseEnvelope(t, jsonEnv, "ok")
			})
		}
	})

	t.Run("TokenInfo", func(t *testing.T) {
		keyParams := apiKeyParams(bobAPIKey)

		xmlEnv, _ := h.getXML(t, h.withParams(t, "/rest/tokenInfo", keyParams))
		assertXMLResponseEnvelope(t, xmlEnv, "ok")
		if xmlEnv.TokenInfo == nil || xmlEnv.TokenInfo.Username != "bob" {
			t.Errorf("XML tokenInfo = %+v, want username=bob", xmlEnv.TokenInfo)
		}

		jsonEnv, _ := h.getJSON(t, h.withParams(t, "/rest/tokenInfo?f=json", keyParams))
		assertJSONResponseEnvelope(t, jsonEnv, "ok")
		if jsonEnv.TokenInfo == nil || jsonEnv.TokenInfo.Username != "bob" {
			t.Errorf("JSON tokenInfo = %+v, want username=bob", jsonEnv.TokenInfo)
		}

		tokenAuth := tokenParams("bob", bobPass, h.nextSalt())
		assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/tokenInfo", tokenAuth), 42)
	})

	t.Run("AuthErrors", func(t *testing.T) {
		wrongToken := tokenParams("bob", "wrong-password", h.nextSalt())
		cases := []struct {
			name     string
			params   url.Values
			wantCode int
		}{
			{
				name:     "DeprecatedPlaintextPassword",
				params:   url.Values{"u": {"bob"}, "p": {bobPass}},
				wantCode: 42,
			},
			{
				name:     "DeprecatedHexPassword",
				params:   url.Values{"u": {"bob"}, "p": {"enc:736563726574"}},
				wantCode: 42,
			},
			{
				name:     "ConflictingAPIKeyAndUser",
				params:   url.Values{"apiKey": {bobAPIKey}, "u": {"bob"}},
				wantCode: 43,
			},
			{
				name: "ConflictingPasswordAndToken",
				params: func() url.Values {
					v := tokenParams("bob", bobPass, h.nextSalt())
					v.Set("p", bobPass)
					return v
				}(),
				wantCode: 43,
			},
			{
				name:     "UnknownAPIKey",
				params:   apiKeyParams("hu_invalid_unknown_key"),
				wantCode: 44,
			},
			{
				name:     "WrongPassword",
				params:   wrongToken,
				wantCode: 40,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/ping", tc.params), tc.wantCode)
			})
		}
	})

	t.Run("GetUser", func(t *testing.T) {
		bobKey := apiKeyParams(bobAPIKey)
		aliceToken := tokenParams("alice", alicePass, h.nextSalt())

		assertExpectedUser := func(t *testing.T, u *subsonic.User, wantName string, wantAdmin bool) {
			t.Helper()
			if u == nil {
				t.Fatalf("user payload = nil, want non-nil")
			}
			if got := u.Username; got != wantName {
				t.Errorf("user.Username = %q, want %q", got, wantName)
			}
			if got := u.AdminRole; got != wantAdmin {
				t.Errorf("user.AdminRole = %t, want %t", got, wantAdmin)
			}
			if got := u.UploadRole; got != wantAdmin {
				t.Errorf("user.UploadRole = %t, want %t", got, wantAdmin)
			}
			if !u.StreamRole {
				t.Errorf("user.StreamRole = false, want true")
			}
			if !slices.Equal(u.Folder, []int{1}) {
				t.Errorf("user.Folder = %v, want [1]", u.Folder)
			}
			if u.ScrobblingEnabled || u.SettingsRole || u.DownloadRole ||
				u.PlaylistRole || u.CoverArtRole || u.CommentRole || u.PodcastRole ||
				u.JukeboxRole || u.ShareRole || u.VideoConversionRole {
				t.Errorf("unexpected extra role/scrobbling true in %+v", *u)
			}
		}

		t.Run("SelfOmittedAndExplicit", func(t *testing.T) {
			for _, path := range []string{"/rest/getUser", "/rest/getUser?username=bob"} {
				authedPath := h.withParams(t, path, bobKey)
				xmlEnv, _ := h.getXML(t, authedPath)
				assertXMLResponseEnvelope(t, xmlEnv, "ok")
				assertExpectedUser(t, xmlEnv.User, "bob", false)

				jsonEnv, jsonBody := h.getJSON(t, h.withParams(t, authedPath, url.Values{"f": {"json"}}))
				assertJSONResponseEnvelope(t, jsonEnv, "ok")
				assertExpectedUser(t, jsonEnv.User, "bob", false)
				if strings.Contains(string(jsonBody), `"coverArt"`) {
					t.Errorf("getUser body unexpectedly contains coverArt: %s", string(jsonBody))
				}
			}
		})

		t.Run("NonAdminAntiEnumeration", func(t *testing.T) {
			assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/getUser?username=alice", bobKey), 50)
			assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/getUser?username=nobody", bobKey), 50)
		})

		t.Run("AdminSelfAndOther", func(t *testing.T) {
			xmlSelf, _ := h.getXML(t, h.withParams(t, "/rest/getUser", aliceToken))
			assertXMLResponseEnvelope(t, xmlSelf, "ok")
			assertExpectedUser(t, xmlSelf.User, "alice", true)

			jsonOther, _ := h.getJSON(t, h.withParams(t, "/rest/getUser?username=bob&f=json", aliceToken))
			assertJSONResponseEnvelope(t, jsonOther, "ok")
			assertExpectedUser(t, jsonOther.User, "bob", false)
		})

		t.Run("AdminUnknownUser", func(t *testing.T) {
			assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/getUser?username=nobody", aliceToken), 70)
		})
	})

	t.Run("LiveRevokeAndDisableWithoutRestart", func(t *testing.T) {
		h.revokeAPIKey(t, bobKeyID)

		revokedKey := apiKeyParams(bobAPIKey)
		assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/ping", revokedKey), 44)
		assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/tokenInfo", revokedKey), 44)

		// Bob's salted MD5 token remains valid until the user is disabled.
		preDisableEnv, _ := h.getJSON(t, h.withParams(t, "/rest/ping?f=json", tokenParams("bob", bobPass, h.nextSalt())))
		assertJSONResponseEnvelope(t, preDisableEnv, "ok")

		h.disableUser(t, "bob")

		disabledToken := tokenParams("bob", bobPass, h.nextSalt())
		assertSubsonicErrorXMLAndJSON(t, h, h.withParams(t, "/rest/ping", disabledToken), 40)

		// Admin Alice remains active after Bob is disabled.
		aliceEnv, _ := h.getJSON(t, h.withParams(t, "/rest/ping?f=json", tokenParams("alice", alicePass, h.nextSalt())))
		assertJSONResponseEnvelope(t, aliceEnv, "ok")
	})
}

func assertSubsonicErrorXMLAndJSON(t *testing.T, h *harness, rawPath string, wantCode int) {
	t.Helper()

	xmlEnv, _ := h.getXML(t, rawPath)
	assertXMLResponseEnvelope(t, xmlEnv, "failed")
	if xmlEnv.Error == nil || xmlEnv.Error.Code != wantCode {
		t.Errorf("GET %s XML error = %+v, want code %d", rawPath, xmlEnv.Error, wantCode)
	}

	jsonPath := h.withParams(t, rawPath, url.Values{"f": {"json"}})
	jsonEnv, _ := h.getJSON(t, jsonPath)
	assertJSONResponseEnvelope(t, jsonEnv, "failed")
	if jsonEnv.Error == nil || jsonEnv.Error.Code != wantCode {
		t.Errorf("GET %s JSON error = %+v, want code %d", jsonPath, jsonEnv.Error, wantCode)
	}
}
