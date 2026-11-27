package auth

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"headunit/internal/store"
)

const (
	minSubsonicSaltRunes = 6
	subsonicTokenHexLen  = md5.Size * 2
)

var authParamKeys = [...]string{"apiKey", "u", "p", "t", "s"}

// AuthStore provides read-only account and API key lookups required by
// SubsonicAuthenticator. Implementations must be safe for concurrent use.
type AuthStore interface {
	GetUserByUsername(ctx context.Context, username string) (*store.User, error)
	LookupAPIKeyByHash(ctx context.Context, keyHash string) (*store.APIKeyLookup, error)
}

var _ AuthStore = (*store.Store)(nil)

// SubsonicAuthenticator authenticates OpenSubsonic HTTP requests using either
// salted MD5 tokens (u+t+s) or OpenSubsonic API keys (apiKeyAuthentication).
type SubsonicAuthenticator struct {
	store  AuthStore
	cipher *PasswordCipher
}

// NewSubsonicAuthenticator constructs a SubsonicAuthenticator backed by st and cipher.
func NewSubsonicAuthenticator(st AuthStore, cipher *PasswordCipher) (*SubsonicAuthenticator, error) {
	if st == nil {
		return nil, errors.New("auth: store must not be nil")
	}
	if cipher == nil || cipher.aead == nil {
		return nil, errors.New("auth: password cipher must not be nil")
	}
	return &SubsonicAuthenticator{
		store:  st,
		cipher: cipher,
	}, nil
}

// Authenticate validates OpenSubsonic credentials on r and returns the
// resolved Principal or a client-safe *AuthError.
func (a *SubsonicAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	if a == nil || a.store == nil || a.cipher == nil || a.cipher.aead == nil || r == nil {
		return Principal{}, errWrongCredentials()
	}
	if r.Form == nil {
		if r.URL == nil {
			return Principal{}, errWrongCredentials()
		}
		if err := r.ParseForm(); err != nil {
			return Principal{}, errWrongCredentials()
		}
	}

	for _, key := range authParamKeys {
		if len(r.Form[key]) > 1 {
			return Principal{}, errConflictingAuth()
		}
	}

	_, hasAPIKey := r.Form["apiKey"]
	_, hasU := r.Form["u"]
	_, hasP := r.Form["p"]
	_, hasT := r.Form["t"]
	_, hasS := r.Form["s"]

	if (hasAPIKey && (hasU || hasP || hasT || hasS)) || (hasP && (hasT || hasS)) {
		return Principal{}, errConflictingAuth()
	}
	if hasP {
		return Principal{}, errUnsupportedAuth()
	}
	if hasAPIKey {
		return a.authenticateAPIKey(r.Context(), r.Form.Get("apiKey"))
	}
	return a.authenticateToken(r.Context(), r.Form.Get("u"), r.Form.Get("t"), r.Form.Get("s"))
}

func (a *SubsonicAuthenticator) authenticateAPIKey(ctx context.Context, rawKey string) (Principal, error) {
	if strings.TrimSpace(rawKey) == "" {
		return Principal{}, errInvalidAPIKey()
	}

	keyHash := HashAPIKey(rawKey)
	lookup, err := a.store.LookupAPIKeyByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Principal{}, errInvalidAPIKey()
		}
		return Principal{}, errWrongCredentials()
	}
	if lookup == nil {
		return Principal{}, errWrongCredentials()
	}
	if subtle.ConstantTimeCompare([]byte(lookup.Key.KeyHash), []byte(keyHash)) != 1 {
		return Principal{}, errInvalidAPIKey()
	}
	if !lookup.Key.RevokedAt.IsZero() || lookup.UserDisabled {
		return Principal{}, errInvalidAPIKey()
	}

	return Principal{
		UserID:   lookup.Key.UserID,
		Username: lookup.Key.Username,
		IsAdmin:  lookup.UserIsAdmin,
		Method:   AuthMethodAPIKey,
	}, nil
}

func (a *SubsonicAuthenticator) authenticateToken(ctx context.Context, username, tokenHex, salt string) (Principal, error) {
	if strings.TrimSpace(username) == "" {
		return Principal{}, errWrongCredentials()
	}
	if utf8.RuneCountInString(salt) < minSubsonicSaltRunes {
		return Principal{}, errWrongCredentials()
	}
	if len(tokenHex) != subsonicTokenHexLen {
		return Principal{}, errWrongCredentials()
	}
	providedToken, err := hex.DecodeString(tokenHex)
	if err != nil || len(providedToken) != md5.Size {
		return Principal{}, errWrongCredentials()
	}

	user, err := a.store.GetUserByUsername(ctx, username)
	if err != nil || user == nil || user.Disabled {
		return Principal{}, errWrongCredentials()
	}

	password, err := a.cipher.Decrypt(user.PasswordCiphertext)
	if err != nil {
		return Principal{}, errWrongCredentials()
	}

	expected := md5.Sum([]byte(password + salt))
	if subtle.ConstantTimeCompare(providedToken, expected[:]) != 1 {
		return Principal{}, errWrongCredentials()
	}

	return Principal{
		UserID:   user.ID,
		Username: user.Username,
		IsAdmin:  user.IsAdmin,
		Method:   AuthMethodToken,
	}, nil
}

func errWrongCredentials() error {
	return &AuthError{Code: ErrCodeWrongCredentials, Message: msgWrongCredentials}
}

func errUnsupportedAuth() error {
	return &AuthError{Code: ErrCodeUnsupportedAuth, Message: msgUnsupportedAuth}
}

func errConflictingAuth() error {
	return &AuthError{Code: ErrCodeConflictingAuth, Message: msgConflictingAuth}
}

func errInvalidAPIKey() error {
	return &AuthError{Code: ErrCodeInvalidAPIKey, Message: msgInvalidAPIKey}
}
