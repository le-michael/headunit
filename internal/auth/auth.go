// Package auth provides the authentication seam, cryptographic primitives, and
// OpenSubsonic authenticators for Headunit HTTP handlers.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type principalContextKey struct{}

// AuthMethod identifies the mechanism that authenticated a Principal.
type AuthMethod string

const (
	// AuthMethodNoop identifies requests accepted by NoopAuthenticator.
	AuthMethodNoop AuthMethod = "noop"

	// AuthMethodToken identifies requests authenticated via salted MD5 tokens (u+t+s).
	AuthMethodToken AuthMethod = "token"

	// AuthMethodAPIKey identifies requests authenticated via OpenSubsonic apiKey.
	AuthMethodAPIKey AuthMethod = "apiKey"
)

const (
	// ErrCodeWrongCredentials is the OpenSubsonic error code for missing or invalid credentials.
	ErrCodeWrongCredentials = 40

	// ErrCodeUnsupportedAuth is the OpenSubsonic error code for deprecated password auth (p=).
	ErrCodeUnsupportedAuth = 42

	// ErrCodeConflictingAuth is the OpenSubsonic error code for conflicting or duplicate auth parameters.
	ErrCodeConflictingAuth = 43

	// ErrCodeInvalidAPIKey is the OpenSubsonic error code for invalid, revoked, or disabled API keys.
	ErrCodeInvalidAPIKey = 44
)

const (
	defaultUsername = "headunit"

	msgWrongCredentials = "Wrong username or password"
	msgUnsupportedAuth  = "Provided authentication mechanism not supported"
	msgConflictingAuth  = "Multiple conflicting authentication mechanisms provided"
	msgInvalidAPIKey    = "Invalid API key"
)

// Principal represents the authenticated caller attached to a request context.
type Principal struct {
	// UserID is the opaque stable user identifier when authenticated against the store.
	UserID string

	// Username is the caller's canonical OpenSubsonic username.
	Username string

	// IsAdmin reports whether the authenticated caller holds administrative privileges.
	IsAdmin bool

	// Method identifies the authentication mechanism that established this Principal.
	Method AuthMethod
}

// AuthError represents a client-safe OpenSubsonic authentication failure with an
// explicit OpenSubsonic error code.
type AuthError struct {
	// Code is the OpenSubsonic error code (40, 42, 43, or 44).
	Code int

	// Message is the client-facing error message; when blank, Middleware substitutes
	// the canonical OpenSubsonic message for Code.
	Message string
}

// Error implements the error interface without panicking on a nil receiver.
func (e *AuthError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if msg := strings.TrimSpace(e.Message); msg != "" {
		return fmt.Sprintf("auth error %d: %s", e.Code, msg)
	}
	if canonical, ok := canonicalAuthErrorMessage(e.Code); ok {
		return fmt.Sprintf("auth error %d: %s", e.Code, canonical)
	}
	return fmt.Sprintf("auth error %d", e.Code)
}

// Authenticator validates an incoming HTTP request and resolves its Principal.
// It is the swap point for enforcing OpenSubsonic token, password, or API-key auth.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// NoopAuthenticator accepts any request without credential verification and
// derives a Principal from the "u" query or form parameter, defaulting to
// "headunit" when absent or blank.
type NoopAuthenticator struct{}

// Authenticate extracts the trimmed "u" parameter or falls back to "headunit".
func (NoopAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	var username string
	if r != nil {
		username = strings.TrimSpace(r.FormValue("u"))
	}
	if username == "" {
		username = defaultUsername
	}
	return Principal{
		Username: username,
		Method:   AuthMethodNoop,
	}, nil
}

// ErrorWriter writes an OpenSubsonic error response for failed authentication.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, code int, msg string)

// Middleware returns HTTP middleware that authenticates each request with a and
// stores the resulting Principal in the request context before calling next.
//
// Middleware fails closed: only *AuthError values carrying an allowlisted code
// (40, 42, 43, or 44) propagate via writeErr (substituting the canonical
// message when Message is blank). When a is nil or returns any other error or
// code, Middleware emits fixed error code 40 ("Wrong username or password") so
// internal details never leak to clients; if writeErr is nil, Middleware falls
// back to HTTP 401 Unauthorized.
func Middleware(a Authenticator, writeErr ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a == nil {
				writeAuthError(w, r, writeErr, ErrCodeWrongCredentials, msgWrongCredentials)
				return
			}
			p, err := a.Authenticate(r)
			if err != nil {
				code, msg := resolveAuthError(err)
				writeAuthError(w, r, writeErr, code, msg)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func resolveAuthError(err error) (int, string) {
	var authErr *AuthError
	if errors.As(err, &authErr) && authErr != nil {
		if canonical, ok := canonicalAuthErrorMessage(authErr.Code); ok {
			if msg := strings.TrimSpace(authErr.Message); msg != "" {
				return authErr.Code, msg
			}
			return authErr.Code, canonical
		}
	}
	return ErrCodeWrongCredentials, msgWrongCredentials
}

func canonicalAuthErrorMessage(code int) (string, bool) {
	switch code {
	case ErrCodeWrongCredentials:
		return msgWrongCredentials, true
	case ErrCodeUnsupportedAuth:
		return msgUnsupportedAuth, true
	case ErrCodeConflictingAuth:
		return msgConflictingAuth, true
	case ErrCodeInvalidAPIKey:
		return msgInvalidAPIKey, true
	default:
		return "", false
	}
}

func writeAuthError(w http.ResponseWriter, r *http.Request, writeErr ErrorWriter, code int, msg string) {
	if writeErr != nil {
		writeErr(w, r, code, msg)
		return
	}
	http.Error(w, msg, http.StatusUnauthorized)
}

// WithPrincipal returns a copy of ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext returns the Principal stored in ctx, or a zero Principal
// if ctx is nil or carries no Principal.
func PrincipalFromContext(ctx context.Context) Principal {
	if ctx == nil {
		return Principal{}
	}
	p, _ := ctx.Value(principalContextKey{}).(Principal)
	return p
}
