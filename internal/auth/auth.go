// Package auth provides the authentication seam for Headunit HTTP handlers.
package auth

import (
	"context"
	"net/http"
	"strings"
)

type principalContextKey struct{}

const (
	defaultUsername         = "headunit"
	errCodeWrongCredentials = 40
	msgWrongCredentials     = "Wrong username or password"
)

// Principal represents the authenticated caller attached to a request context.
type Principal struct {
	// Username is the caller's OpenSubsonic username.
	Username string
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
	username := strings.TrimSpace(r.FormValue("u"))
	if username == "" {
		username = defaultUsername
	}
	return Principal{Username: username}, nil
}

// ErrorWriter writes an OpenSubsonic error response for failed authentication.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, code int, msg string)

// Middleware returns HTTP middleware that authenticates each request with a and
// stores the resulting Principal in the request context before calling next.
//
// Middleware fails closed when a is nil or returns an error. Failed requests
// emit fixed Subsonic error code 40 ("Wrong username or password") via writeErr
// so internal authentication errors are never leaked to clients; if writeErr is
// nil, Middleware falls back to HTTP 401 Unauthorized.
func Middleware(a Authenticator, writeErr ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a == nil {
				writeAuthError(w, r, writeErr)
				return
			}
			p, err := a.Authenticate(r)
			if err != nil {
				writeAuthError(w, r, writeErr)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func writeAuthError(w http.ResponseWriter, r *http.Request, writeErr ErrorWriter) {
	if writeErr != nil {
		writeErr(w, r, errCodeWrongCredentials, msgWrongCredentials)
		return
	}
	http.Error(w, msgWrongCredentials, http.StatusUnauthorized)
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
