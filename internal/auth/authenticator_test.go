package auth_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"headunit/internal/auth"
	"headunit/internal/store"
)

type mockAuthStore struct {
	users      map[string]store.User
	usersErr   error
	apiKeys    map[string]store.APIKeyLookup
	apiKeysErr error
}

func (m *mockAuthStore) GetUserByUsername(_ context.Context, username string) (*store.User, error) {
	if m.usersErr != nil {
		return nil, m.usersErr
	}
	for _, u := range m.users {
		if strings.EqualFold(u.Username, username) {
			copyUser := u
			return &copyUser, nil
		}
	}
	return nil, fmt.Errorf("mock: user %q: %w", username, store.ErrNotFound)
}

func (m *mockAuthStore) LookupAPIKeyByHash(_ context.Context, keyHash string) (*store.APIKeyLookup, error) {
	if m.apiKeysErr != nil {
		return nil, m.apiKeysErr
	}
	lookup, ok := m.apiKeys[keyHash]
	if !ok {
		return nil, fmt.Errorf("mock: api key: %w", store.ErrNotFound)
	}
	copyLookup := lookup
	return &copyLookup, nil
}

func subsonicToken(password, salt string) string {
	sum := md5.Sum([]byte(password + salt))
	return hex.EncodeToString(sum[:])
}

func newTestCipher(t *testing.T) *auth.PasswordCipher {
	t.Helper()
	c, err := auth.NewPasswordCipher(mustDecodeHex(t, testKeyHex))
	if err != nil {
		t.Fatalf("NewPasswordCipher() error = %v", err)
	}
	return c
}

func mustEncrypt(t *testing.T, c *auth.PasswordCipher, plaintext string) []byte {
	t.Helper()
	blob, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	return blob
}

func TestNewSubsonicAuthenticator_Validation(t *testing.T) {
	st := &mockAuthStore{}
	cipher := newTestCipher(t)

	if _, err := auth.NewSubsonicAuthenticator(nil, cipher); err == nil {
		t.Error("NewSubsonicAuthenticator(nil store) = nil error, want error")
	}
	if _, err := auth.NewSubsonicAuthenticator(st, nil); err == nil {
		t.Error("NewSubsonicAuthenticator(nil cipher) = nil error, want error")
	}
	if _, err := auth.NewSubsonicAuthenticator(st, &auth.PasswordCipher{}); err == nil {
		t.Error("NewSubsonicAuthenticator(uninitialized cipher) = nil error, want error")
	}
	if got, err := auth.NewSubsonicAuthenticator(st, cipher); err != nil || got == nil {
		t.Fatalf("NewSubsonicAuthenticator(valid) = (%v, %v), want non-nil authenticator and nil error", got, err)
	}
}

func TestSubsonicAuthenticator_TokenAuth(t *testing.T) {
	cipher := newTestCipher(t)

	const (
		alicePass = "  päss\r\nwörd  "
		bobPass   = "simple-password"
	)

	st := &mockAuthStore{
		users: map[string]store.User{
			"alice": {
				ID:                 "usr_alice",
				Username:           "alice",
				PasswordCiphertext: mustEncrypt(t, cipher, alicePass),
				IsAdmin:            true,
			},
			"bob": {
				ID:                 "usr_bob",
				Username:           "BobUser",
				PasswordCiphertext: mustEncrypt(t, cipher, bobPass),
				IsAdmin:            false,
			},
		},
	}

	authenticator, err := auth.NewSubsonicAuthenticator(st, cipher)
	if err != nil {
		t.Fatalf("NewSubsonicAuthenticator() error = %v", err)
	}

	const asciiSalt = "123456"
	const utf8Salt = "éééééé" // 6 runes

	tests := []struct {
		name string
		req  *http.Request
		want auth.Principal
	}{
		{
			name: "GET query admin with verbatim whitespace/UTF-8 password",
			req: httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
				"u": {"alice"},
				"t": {subsonicToken(alicePass, asciiSalt)},
				"s": {asciiSalt},
			}.Encode(), nil),
			want: auth.Principal{
				UserID:   "usr_alice",
				Username: "alice",
				IsAdmin:  true,
				Method:   auth.AuthMethodToken,
			},
		},
		{
			name: "POST form body non-admin with mixed-case username and uppercase hex token",
			req: newFormPostRequest(t, "/rest/ping", url.Values{
				"u": {"bobuser"},
				"t": {strings.ToUpper(subsonicToken(bobPass, asciiSalt))},
				"s": {asciiSalt},
			}.Encode()),
			want: auth.Principal{
				UserID:   "usr_bob",
				Username: "BobUser",
				IsAdmin:  false,
				Method:   auth.AuthMethodToken,
			},
		},
		{
			name: "6-rune UTF-8 salt succeeds",
			req: httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
				"u": {"alice"},
				"t": {subsonicToken(alicePass, utf8Salt)},
				"s": {utf8Salt},
			}.Encode(), nil),
			want: auth.Principal{
				UserID:   "usr_alice",
				Username: "alice",
				IsAdmin:  true,
				Method:   auth.AuthMethodToken,
			},
		},
		{
			name: "non-auth duplicate query parameters allowed",
			req: httptest.NewRequest(http.MethodGet, "/rest/getAlbum?"+url.Values{
				"u":  {"alice"},
				"t":  {subsonicToken(alicePass, asciiSalt)},
				"s":  {asciiSalt},
				"id": {"al_1", "al_2"},
			}.Encode(), nil),
			want: auth.Principal{
				UserID:   "usr_alice",
				Username: "alice",
				IsAdmin:  true,
				Method:   auth.AuthMethodToken,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authenticator.Authenticate(tc.req)
			if err != nil {
				t.Fatalf("Authenticate() unexpected error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Authenticate() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSubsonicAuthenticator_APIKeyAuth(t *testing.T) {
	cipher := newTestCipher(t)

	adminKey, _, adminHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(admin) error = %v", err)
	}
	userKey, _, userHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(user) error = %v", err)
	}

	st := &mockAuthStore{
		apiKeys: map[string]store.APIKeyLookup{
			adminHash: {
				Key: store.APIKey{
					ID:       "key_admin",
					UserID:   "usr_admin",
					Username: "admin",
					KeyHash:  adminHash,
				},
				UserIsAdmin: true,
			},
			userHash: {
				Key: store.APIKey{
					ID:       "key_user",
					UserID:   "usr_listener",
					Username: "listener",
					KeyHash:  userHash,
				},
				UserIsAdmin: false,
			},
		},
	}

	authenticator, err := auth.NewSubsonicAuthenticator(st, cipher)
	if err != nil {
		t.Fatalf("NewSubsonicAuthenticator() error = %v", err)
	}

	tests := []struct {
		name string
		req  *http.Request
		want auth.Principal
	}{
		{
			name: "GET query API key regular user",
			req: httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
				"apiKey": {userKey},
				"f":      {"json"},
				"v":      {"1.16.1"},
				"c":      {"test-client"},
			}.Encode(), nil),
			want: auth.Principal{
				UserID:   "usr_listener",
				Username: "listener",
				IsAdmin:  false,
				Method:   auth.AuthMethodAPIKey,
			},
		},
		{
			name: "POST form body API key admin user",
			req: newFormPostRequest(t, "/rest/ping", url.Values{
				"apiKey": {adminKey},
				"f":      {"json"},
			}.Encode()),
			want: auth.Principal{
				UserID:   "usr_admin",
				Username: "admin",
				IsAdmin:  true,
				Method:   auth.AuthMethodAPIKey,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authenticator.Authenticate(tc.req)
			if err != nil {
				t.Fatalf("Authenticate() unexpected error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Authenticate() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSubsonicAuthenticator_ErrorCodes(t *testing.T) {
	const (
		msg40 = "Wrong username or password"
		msg42 = "Provided authentication mechanism not supported"
		msg43 = "Multiple conflicting authentication mechanisms provided"
		msg44 = "Invalid API key"

		alicePass     = "super-secret-alice-password"
		salt          = "somesalt"
		backendErrMsg = "sqlite-secret-backend-path"
	)

	cipher := newTestCipher(t)
	validToken := subsonicToken(alicePass, salt)

	activeKey, _, activeHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(active) error = %v", err)
	}
	revokedKey, _, revokedHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(revoked) error = %v", err)
	}
	disabledUserKey, _, disabledUserHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(disabled) error = %v", err)
	}
	mismatchKey, _, mismatchHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey(mismatch) error = %v", err)
	}

	baseStore := func() *mockAuthStore {
		return &mockAuthStore{
			users: map[string]store.User{
				"alice": {
					ID:                 "usr_alice",
					Username:           "alice",
					PasswordCiphertext: mustEncrypt(t, cipher, alicePass),
				},
				"disabled": {
					ID:                 "usr_disabled",
					Username:           "disabled",
					PasswordCiphertext: mustEncrypt(t, cipher, alicePass),
					Disabled:           true,
				},
				"corrupt": {
					ID:                 "usr_corrupt",
					Username:           "corrupt",
					PasswordCiphertext: []byte("corrupt-ciphertext-bytes"),
				},
			},
			apiKeys: map[string]store.APIKeyLookup{
				activeHash: {
					Key: store.APIKey{
						ID:       "key_active",
						UserID:   "usr_alice",
						Username: "alice",
						KeyHash:  activeHash,
					},
				},
				revokedHash: {
					Key: store.APIKey{
						ID:        "key_revoked",
						UserID:    "usr_alice",
						Username:  "alice",
						KeyHash:   revokedHash,
						RevokedAt: time.Now().UTC(),
					},
				},
				disabledUserHash: {
					Key: store.APIKey{
						ID:       "key_disabled_user",
						UserID:   "usr_disabled",
						Username: "disabled",
						KeyHash:  disabledUserHash,
					},
					UserDisabled: true,
				},
				mismatchHash: {
					Key: store.APIKey{
						ID:       "key_mismatch",
						UserID:   "usr_alice",
						Username: "alice",
						KeyHash:  strings.Repeat("0", 64),
					},
				},
			},
		}
	}

	tests := []struct {
		name      string
		store     *mockAuthStore
		useNilRec bool
		req       *http.Request
		wantCode  int
		wantMsg   string
	}{
		// --- Code 40: Wrong / missing / malformed credentials or backend failure ---
		{
			name:      "nil receiver returns 40",
			store:     baseStore(),
			useNilRec: true,
			req:       httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey, nil),
			wantCode:  auth.ErrCodeWrongCredentials,
			wantMsg:   msg40,
		},
		{
			name:     "nil request returns 40",
			store:    baseStore(),
			req:      nil,
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "request with nil URL returns 40",
			store:    baseStore(),
			req:      &http.Request{Method: http.MethodGet},
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "missing all credentials",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping", nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "missing u parameter",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "missing t parameter",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "missing s parameter",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "empty username",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "whitespace-only username",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=%20%20&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "padded username rejected",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=%20alice%20&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "empty salt",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken+"&s=", nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "short ASCII salt (5 runes)",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+subsonicToken(alicePass, "12345")+"&s=12345", nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "short multi-byte UTF-8 salt (3 runes / 6 bytes)",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{"u": {"alice"}, "t": {subsonicToken(alicePass, "ééé")}, "s": {"ééé"}}.Encode(), nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "short multi-byte UTF-8 salt (5 runes)",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{"u": {"alice"}, "t": {subsonicToken(alicePass, "ééééé")}, "s": {"ééééé"}}.Encode(), nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "empty token",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t=&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "short token (31 hex chars)",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken[:31]+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "long token (33 hex chars)",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken+"0&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "non-hex token (32 chars with g)",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken[:31]+"g&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "wrong password",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+subsonicToken("wrong-password", salt)+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "unknown user",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=nobody&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "disabled user token auth",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=disabled&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "corrupt password ciphertext fails closed without leaking bytes",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=corrupt&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "malformed URL query escape",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=%ZZ&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name:     "malformed POST form body escape",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping", "u=alice&t=%ZZ&s="+salt),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name: "store error during GetUserByUsername returns 40 without leaking details",
			store: func() *mockAuthStore {
				s := baseStore()
				s.usersErr = errors.New(backendErrMsg)
				return s
			}(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},
		{
			name: "store error during LookupAPIKeyByHash returns 40 (not 44) without leaking details",
			store: func() *mockAuthStore {
				s := baseStore()
				s.apiKeysErr = errors.New(backendErrMsg)
				return s
			}(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey, nil),
			wantCode: auth.ErrCodeWrongCredentials,
			wantMsg:  msg40,
		},

		// --- Code 42: Deprecated p / enc: password auth ---
		{
			name:     "deprecated plaintext password with u",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&p=secret", nil),
			wantCode: auth.ErrCodeUnsupportedAuth,
			wantMsg:  msg42,
		},
		{
			name:     "deprecated enc password with u",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&p=enc:736563726574", nil),
			wantCode: auth.ErrCodeUnsupportedAuth,
			wantMsg:  msg42,
		},
		{
			name:     "deprecated password without u",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?p=secret", nil),
			wantCode: auth.ErrCodeUnsupportedAuth,
			wantMsg:  msg42,
		},
		{
			name:     "deprecated empty p parameter",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?p=", nil),
			wantCode: auth.ErrCodeUnsupportedAuth,
			wantMsg:  msg42,
		},
		{
			name:     "deprecated POST body p parameter",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping", "u=alice&p=secret"),
			wantCode: auth.ErrCodeUnsupportedAuth,
			wantMsg:  msg42,
		},

		// --- Code 43: Conflicting mechanisms or duplicate auth parameter keys ---
		{
			name:     "conflict apiKey and u",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey+"&u=alice", nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict empty apiKey and u",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey=&u=alice", nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict apiKey and p",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey+"&p=secret", nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict apiKey and t",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey+"&t="+validToken, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict apiKey and s",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey+"&s="+salt, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict apiKey and full token params",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey+"&u=alice&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict apiKey in query and u in POST body",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping?apiKey="+activeKey, "u=alice"),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict u in query and apiKey in POST body",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping?u=alice", "apiKey="+activeKey),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict p and t takes precedence over 42",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&p=secret&t="+validToken, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict p and s takes precedence over 42",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&p=secret&s="+salt, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "conflict p in query and t in POST body",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping?p=secret", "t="+validToken),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "duplicate u in query",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&u=alice&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "duplicate apiKey in query",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKey+"&apiKey="+activeKey, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "duplicate p in query",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&p=a&p=b", nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "duplicate t in query",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken+"&t="+validToken+"&s="+salt, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "duplicate s in query",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice&t="+validToken+"&s="+salt+"&s="+salt, nil),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "duplicate u split across query and POST body",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping?u=alice", "u=bob&t="+validToken+"&s="+salt),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},
		{
			name:     "duplicate apiKey split across query and POST body",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping?apiKey="+activeKey, "apiKey="+activeKey),
			wantCode: auth.ErrCodeConflictingAuth,
			wantMsg:  msg43,
		},

		// --- Code 44: Invalid / revoked / disabled API key ---
		{
			name:     "empty apiKey value",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey=", nil),
			wantCode: auth.ErrCodeInvalidAPIKey,
			wantMsg:  msg44,
		},
		{
			name:     "whitespace-only apiKey value",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey=%20%20", nil),
			wantCode: auth.ErrCodeInvalidAPIKey,
			wantMsg:  msg44,
		},
		{
			name:     "unknown apiKey",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey=hu_unknown", nil),
			wantCode: auth.ErrCodeInvalidAPIKey,
			wantMsg:  msg44,
		},
		{
			name:     "revoked apiKey",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+revokedKey, nil),
			wantCode: auth.ErrCodeInvalidAPIKey,
			wantMsg:  msg44,
		},
		{
			name:     "disabled user apiKey",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+disabledUserKey, nil),
			wantCode: auth.ErrCodeInvalidAPIKey,
			wantMsg:  msg44,
		},
		{
			name:     "mismatched hash in store record fails constant-time check",
			store:    baseStore(),
			req:      httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+mismatchKey, nil),
			wantCode: auth.ErrCodeInvalidAPIKey,
			wantMsg:  msg44,
		},
		{
			name:     "POST form body unknown apiKey",
			store:    baseStore(),
			req:      newFormPostRequest(t, "/rest/ping", "apiKey=hu_does_not_exist"),
			wantCode: auth.ErrCodeInvalidAPIKey,
			wantMsg:  msg44,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var authenticator *auth.SubsonicAuthenticator
			if !tc.useNilRec {
				var err error
				authenticator, err = auth.NewSubsonicAuthenticator(tc.store, cipher)
				if err != nil {
					t.Fatalf("NewSubsonicAuthenticator() error = %v", err)
				}
			}

			_, err := authenticator.Authenticate(tc.req)
			if err == nil {
				t.Fatalf("Authenticate() = nil error, want *AuthError(%d)", tc.wantCode)
			}

			var authErr *auth.AuthError
			if !errors.As(err, &authErr) || authErr == nil {
				t.Fatalf("Authenticate() error = %T (%v), want *auth.AuthError", err, err)
			}
			if authErr.Code != tc.wantCode {
				t.Errorf("AuthError.Code = %d, want %d", authErr.Code, tc.wantCode)
			}
			if authErr.Message != tc.wantMsg {
				t.Errorf("AuthError.Message = %q, want %q", authErr.Message, tc.wantMsg)
			}

			errText := err.Error()
			for _, sensitive := range []string{alicePass, activeKey, revokedKey, disabledUserKey, backendErrMsg, "corrupt-ciphertext-bytes"} {
				if strings.Contains(errText, sensitive) {
					t.Errorf("error %q leaked sensitive detail %q", errText, sensitive)
				}
			}
		})
	}
}

func TestSubsonicAuthenticator_SQLiteIntegration(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(tmpDir, "headunit.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("st.Close() error = %v", closeErr)
		}
	})

	key, err := auth.LoadOrCreateKey(tmpDir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateKey() error = %v", err)
	}
	cipher, err := auth.NewPasswordCipher(key)
	if err != nil {
		t.Fatalf("NewPasswordCipher() error = %v", err)
	}

	const (
		adminPass    = "  admin\r\npass-🎵  "
		listenerPass = "listener-pass"
		salt         = "abcdef"
	)

	adminUser, err := st.CreateUser(ctx, store.User{
		Username:           "AdminUser",
		PasswordCiphertext: mustEncrypt(t, cipher, adminPass),
		IsAdmin:            true,
	})
	if err != nil {
		t.Fatalf("CreateUser(admin) error = %v", err)
	}

	listenerUser, err := st.CreateUser(ctx, store.User{
		Username:           "listener",
		PasswordCiphertext: mustEncrypt(t, cipher, listenerPass),
		IsAdmin:            false,
	})
	if err != nil {
		t.Fatalf("CreateUser(listener) error = %v", err)
	}

	listenerKeyPlain, listenerPrefix, listenerHash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}
	listenerKeyRecord, err := st.CreateAPIKey(ctx, store.APIKey{
		UserID:    listenerUser.ID,
		Name:      "phone",
		KeyPrefix: listenerPrefix,
		KeyHash:   listenerHash,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}

	authenticator, err := auth.NewSubsonicAuthenticator(st, cipher)
	if err != nil {
		t.Fatalf("NewSubsonicAuthenticator() error = %v", err)
	}

	type middlewareResult struct {
		status    int
		errCode   int
		errMsg    string
		principal auth.Principal
	}

	runRequest := func(req *http.Request) middlewareResult {
		var res middlewareResult
		writeErr := func(w http.ResponseWriter, _ *http.Request, code int, msg string) {
			res.errCode = code
			res.errMsg = msg
			w.WriteHeader(http.StatusUnauthorized)
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			res.principal = auth.PrincipalFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		auth.Middleware(authenticator, writeErr)(next).ServeHTTP(rec, req)
		res.status = rec.Code
		return res
	}

	t.Run("mixed-case username token auth returns canonical Principal", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
			"u": {"adminuser"},
			"t": {subsonicToken(adminPass, salt)},
			"s": {salt},
		}.Encode(), nil)
		got := runRequest(req)
		wantPrincipal := auth.Principal{
			UserID:   adminUser.ID,
			Username: "AdminUser",
			IsAdmin:  true,
			Method:   auth.AuthMethodToken,
		}
		if got.status != http.StatusOK || got.principal != wantPrincipal {
			t.Errorf("got = %+v, want status 200 and principal %+v", got, wantPrincipal)
		}
	})

	t.Run("API key auth returns Principal", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
			"apiKey": {listenerKeyPlain},
		}.Encode(), nil)
		got := runRequest(req)
		wantPrincipal := auth.Principal{
			UserID:   listenerUser.ID,
			Username: "listener",
			IsAdmin:  false,
			Method:   auth.AuthMethodAPIKey,
		}
		if got.status != http.StatusOK || got.principal != wantPrincipal {
			t.Errorf("got = %+v, want status 200 and principal %+v", got, wantPrincipal)
		}
	})

	t.Run("disabling user rejects both token (40) and API key (44)", func(t *testing.T) {
		if err := st.SetUserDisabled(ctx, listenerUser.Username, true); err != nil {
			t.Fatalf("SetUserDisabled(true) error = %v", err)
		}

		tokRes := runRequest(httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
			"u": {"listener"},
			"t": {subsonicToken(listenerPass, salt)},
			"s": {salt},
		}.Encode(), nil))
		if tokRes.status != http.StatusUnauthorized || tokRes.errCode != auth.ErrCodeWrongCredentials {
			t.Errorf("disabled token auth = %+v, want 401/code 40", tokRes)
		}

		keyRes := runRequest(httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
			"apiKey": {listenerKeyPlain},
		}.Encode(), nil))
		if keyRes.status != http.StatusUnauthorized || keyRes.errCode != auth.ErrCodeInvalidAPIKey {
			t.Errorf("disabled API key auth = %+v, want 401/code 44", keyRes)
		}

		if err := st.SetUserDisabled(ctx, listenerUser.Username, false); err != nil {
			t.Fatalf("SetUserDisabled(false) error = %v", err)
		}
	})

	t.Run("revoking API key rejects key (44) while token auth still succeeds", func(t *testing.T) {
		if err := st.RevokeAPIKey(ctx, listenerKeyRecord.ID); err != nil {
			t.Fatalf("RevokeAPIKey() error = %v", err)
		}

		keyRes := runRequest(httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
			"apiKey": {listenerKeyPlain},
		}.Encode(), nil))
		if keyRes.status != http.StatusUnauthorized || keyRes.errCode != auth.ErrCodeInvalidAPIKey {
			t.Errorf("revoked API key auth = %+v, want 401/code 44", keyRes)
		}

		tokRes := runRequest(httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
			"u": {"listener"},
			"t": {subsonicToken(listenerPass, salt)},
			"s": {salt},
		}.Encode(), nil))
		if tokRes.status != http.StatusOK || tokRes.principal.Username != "listener" {
			t.Errorf("token auth after key revoke = %+v, want 200/listener", tokRes)
		}
	})

	t.Run("concurrent middleware requests under -race", func(t *testing.T) {
		activeKeyPlain, activePrefix, activeHash, err := auth.GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() error = %v", err)
		}
		if _, err := st.CreateAPIKey(ctx, store.APIKey{
			UserID:    adminUser.ID,
			Name:      "concurrent",
			KeyPrefix: activePrefix,
			KeyHash:   activeHash,
		}); err != nil {
			t.Fatalf("CreateAPIKey() error = %v", err)
		}

		const workers = 32
		results := make([]middlewareResult, workers)
		start := make(chan struct{})

		var wg sync.WaitGroup
		wg.Add(workers)
		for i := range workers {
			go func(id int) {
				defer wg.Done()
				<-start

				var req *http.Request
				switch id % 6 {
				case 0:
					req = httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
						"u": {"AdminUser"},
						"t": {subsonicToken(adminPass, salt)},
						"s": {salt},
					}.Encode(), nil)
				case 1:
					req = httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
						"apiKey": {activeKeyPlain},
					}.Encode(), nil)
				case 2:
					req = httptest.NewRequest(http.MethodGet, "/rest/ping?"+url.Values{
						"u": {"AdminUser"},
						"t": {subsonicToken("wrong", salt)},
						"s": {salt},
					}.Encode(), nil)
				case 3:
					req = httptest.NewRequest(http.MethodGet, "/rest/ping?u=AdminUser&p=legacy", nil)
				case 4:
					req = httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+activeKeyPlain+"&u=AdminUser", nil)
				case 5:
					req = httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+listenerKeyPlain, nil)
				}
				results[id] = runRequest(req)
			}(i)
		}

		close(start)
		wg.Wait()

		for i := range workers {
			got := results[i]
			switch i % 6 {
			case 0:
				if got.status != http.StatusOK || got.principal.Method != auth.AuthMethodToken || got.principal.Username != "AdminUser" {
					t.Errorf("worker %d token auth = %+v, want 200 AdminUser token", i, got)
				}
			case 1:
				if got.status != http.StatusOK || got.principal.Method != auth.AuthMethodAPIKey || got.principal.Username != "AdminUser" {
					t.Errorf("worker %d API key auth = %+v, want 200 AdminUser apiKey", i, got)
				}
			case 2:
				if got.status != http.StatusUnauthorized || got.errCode != auth.ErrCodeWrongCredentials {
					t.Errorf("worker %d bad token = %+v, want 401/40", i, got)
				}
			case 3:
				if got.status != http.StatusUnauthorized || got.errCode != auth.ErrCodeUnsupportedAuth {
					t.Errorf("worker %d legacy p = %+v, want 401/42", i, got)
				}
			case 4:
				if got.status != http.StatusUnauthorized || got.errCode != auth.ErrCodeConflictingAuth {
					t.Errorf("worker %d conflicting = %+v, want 401/43", i, got)
				}
			case 5:
				if got.status != http.StatusUnauthorized || got.errCode != auth.ErrCodeInvalidAPIKey {
					t.Errorf("worker %d revoked key = %+v, want 401/44", i, got)
				}
			}
		}
	})
}
