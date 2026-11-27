package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"headunit/internal/auth"
)

type stubAuthenticator struct {
	principal auth.Principal
	err       error
}

func (s stubAuthenticator) Authenticate(*http.Request) (auth.Principal, error) {
	return s.principal, s.err
}

func newFormPostRequest(t *testing.T, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestNoopAuthenticator(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		want auth.Principal
	}{
		{
			name: "GET query parameter",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping?u=alice", nil),
			want: auth.Principal{Username: "alice", Method: auth.AuthMethodNoop},
		},
		{
			name: "GET query parameter trimmed",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping?u=%20alice%20", nil),
			want: auth.Principal{Username: "alice", Method: auth.AuthMethodNoop},
		},
		{
			name: "POST form body",
			req:  newFormPostRequest(t, "/rest/ping", "u=bob"),
			want: auth.Principal{Username: "bob", Method: auth.AuthMethodNoop},
		},
		{
			name: "omitted username defaults to headunit",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping", nil),
			want: auth.Principal{Username: "headunit", Method: auth.AuthMethodNoop},
		},
		{
			name: "empty username defaults to headunit",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping?u=", nil),
			want: auth.Principal{Username: "headunit", Method: auth.AuthMethodNoop},
		},
		{
			name: "whitespace username defaults to headunit",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping?u=%20%20", nil),
			want: auth.Principal{Username: "headunit", Method: auth.AuthMethodNoop},
		},
		{
			name: "nil request defaults to headunit",
			req:  nil,
			want: auth.Principal{Username: "headunit", Method: auth.AuthMethodNoop},
		},
	}

	var a auth.NoopAuthenticator
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.Authenticate(tc.req)
			if err != nil {
				t.Fatalf("Authenticate() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("Authenticate() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPrincipalContext(t *testing.T) {
	t.Run("round-trip", func(t *testing.T) {
		want := auth.Principal{
			UserID:   "usr_0123456789abcdef0123456789abcdef",
			Username: "alice",
			IsAdmin:  true,
			Method:   auth.AuthMethodAPIKey,
		}
		ctx := auth.WithPrincipal(context.Background(), want)
		if got := auth.PrincipalFromContext(ctx); got != want {
			t.Errorf("PrincipalFromContext() = %+v, want %+v", got, want)
		}
	})

	t.Run("missing key returns zero principal", func(t *testing.T) {
		want := auth.Principal{}
		if got := auth.PrincipalFromContext(context.Background()); got != want {
			t.Errorf("PrincipalFromContext() = %+v, want %+v", got, want)
		}
	})

	t.Run("nil context returns zero principal without panic", func(t *testing.T) {
		var nilCtx context.Context
		want := auth.Principal{}
		if got := auth.PrincipalFromContext(nilCtx); got != want {
			t.Errorf("PrincipalFromContext(nil) = %+v, want %+v", got, want)
		}
	})
}

func TestAuthError(t *testing.T) {
	t.Run("nil receiver does not panic", func(t *testing.T) {
		var nilErr *auth.AuthError
		if got := nilErr.Error(); got != "<nil>" {
			t.Errorf("(*AuthError)(nil).Error() = %q, want %q", got, "<nil>")
		}
	})

	tests := []struct {
		name string
		err  *auth.AuthError
		want string
	}{
		{
			name: "custom message",
			err:  &auth.AuthError{Code: auth.ErrCodeInvalidAPIKey, Message: "Custom key error"},
			want: "auth error 44: Custom key error",
		},
		{
			name: "empty message uses canonical fallback",
			err:  &auth.AuthError{Code: auth.ErrCodeUnsupportedAuth},
			want: "auth error 42: Provided authentication mechanism not supported",
		},
		{
			name: "whitespace message uses canonical fallback",
			err:  &auth.AuthError{Code: auth.ErrCodeConflictingAuth, Message: "  \t\n "},
			want: "auth error 43: Multiple conflicting authentication mechanisms provided",
		},
		{
			name: "unknown code without message",
			err:  &auth.AuthError{Code: 50},
			want: "auth error 50",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMiddleware(t *testing.T) {
	const (
		defaultErrCode = 40
		defaultErrMsg  = "Wrong username or password"
		internalHint   = "secret-backend-failure"
	)

	var typedNilAuthErr *auth.AuthError

	tests := []struct {
		name           string
		authenticator  auth.Authenticator
		useNilWriter   bool
		target         string
		wantNextCalls  int
		wantErrCalls   int
		wantPrincipal  auth.Principal
		wantErrCode    int
		wantErrMsg     string
		wantHTTPStatus int
	}{
		{
			name:           "happy path with username",
			authenticator:  auth.NoopAuthenticator{},
			target:         "/rest/ping?u=alice",
			wantNextCalls:  1,
			wantErrCalls:   0,
			wantPrincipal:  auth.Principal{Username: "alice", Method: auth.AuthMethodNoop},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "happy path default username",
			authenticator:  auth.NoopAuthenticator{},
			target:         "/rest/ping",
			wantNextCalls:  1,
			wantErrCalls:   0,
			wantPrincipal:  auth.Principal{Username: "headunit", Method: auth.AuthMethodNoop},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name: "happy path preserves full principal",
			authenticator: stubAuthenticator{
				principal: auth.Principal{
					UserID:   "usr_abc",
					Username: "alice",
					IsAdmin:  true,
					Method:   auth.AuthMethodToken,
				},
			},
			target:         "/rest/ping",
			wantNextCalls:  1,
			wantErrCalls:   0,
			wantPrincipal:  auth.Principal{UserID: "usr_abc", Username: "alice", IsAdmin: true, Method: auth.AuthMethodToken},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "propagates allowed code 40",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeWrongCredentials, Message: "Wrong username or password"}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeWrongCredentials,
			wantErrMsg:     "Wrong username or password",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "propagates allowed code 42",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeUnsupportedAuth, Message: "Provided authentication mechanism not supported"}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeUnsupportedAuth,
			wantErrMsg:     "Provided authentication mechanism not supported",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "propagates allowed code 43",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeConflictingAuth, Message: "Multiple conflicting authentication mechanisms provided"}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeConflictingAuth,
			wantErrMsg:     "Multiple conflicting authentication mechanisms provided",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "propagates allowed code 44",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeInvalidAPIKey, Message: "Invalid API key"}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeInvalidAPIKey,
			wantErrMsg:     "Invalid API key",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "empty AuthError message uses canonical fallback for 40",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeWrongCredentials}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeWrongCredentials,
			wantErrMsg:     "Wrong username or password",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "empty AuthError message uses canonical fallback for 42",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeUnsupportedAuth}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeUnsupportedAuth,
			wantErrMsg:     "Provided authentication mechanism not supported",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "whitespace-only AuthError message uses canonical fallback for 43",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeConflictingAuth, Message: " \t\r\n "}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeConflictingAuth,
			wantErrMsg:     "Multiple conflicting authentication mechanisms provided",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "whitespace-only AuthError message uses canonical fallback for 44",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: auth.ErrCodeInvalidAPIKey, Message: "   "}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeInvalidAPIKey,
			wantErrMsg:     "Invalid API key",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "wrapped AuthError propagates code without leaking outer wrapper text",
			authenticator:  stubAuthenticator{err: fmt.Errorf("%s: %w", internalHint, &auth.AuthError{Code: auth.ErrCodeInvalidAPIKey})},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    auth.ErrCodeInvalidAPIKey,
			wantErrMsg:     "Invalid API key",
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "disallowed AuthError code 0 clamps to 40",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: 0, Message: internalHint}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    defaultErrCode,
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "disallowed AuthError code 50 clamps to 40",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: 50, Message: internalHint}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    defaultErrCode,
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "disallowed AuthError code 70 clamps to 40",
			authenticator:  stubAuthenticator{err: &auth.AuthError{Code: 70, Message: internalHint}},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    defaultErrCode,
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "typed nil AuthError clamps to 40 without panic",
			authenticator:  stubAuthenticator{err: typedNilAuthErr},
			target:         "/rest/ping",
			wantErrCalls:   1,
			wantErrCode:    defaultErrCode,
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "failing authenticator calls error writer without leaking details",
			authenticator:  stubAuthenticator{err: errors.New(internalHint)},
			target:         "/rest/ping?u=alice",
			wantErrCalls:   1,
			wantErrCode:    defaultErrCode,
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "nil authenticator fails closed",
			authenticator:  nil,
			target:         "/rest/ping?u=alice",
			wantErrCalls:   1,
			wantErrCode:    defaultErrCode,
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "nil error writer falls back to HTTP 401 on auth error",
			authenticator:  stubAuthenticator{err: errors.New(internalHint)},
			useNilWriter:   true,
			target:         "/rest/ping?u=alice",
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusUnauthorized,
		},
		{
			name:           "nil error writer falls back to HTTP 401 with allowed AuthError message",
			authenticator:  stubAuthenticator{err: fmt.Errorf("%s: %w", internalHint, &auth.AuthError{Code: auth.ErrCodeInvalidAPIKey})},
			useNilWriter:   true,
			target:         "/rest/ping",
			wantErrMsg:     "Invalid API key",
			wantHTTPStatus: http.StatusUnauthorized,
		},
		{
			name:           "nil error writer falls back to HTTP 401 on nil authenticator",
			authenticator:  nil,
			useNilWriter:   true,
			target:         "/rest/ping?u=alice",
			wantErrMsg:     defaultErrMsg,
			wantHTTPStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				nextCalls    int
				gotPrincipal auth.Principal
				errCalls     int
				gotErrCode   int
				gotErrMsg    string
			)

			var writeErr auth.ErrorWriter
			if !tc.useNilWriter {
				writeErr = func(w http.ResponseWriter, _ *http.Request, code int, msg string) {
					errCalls++
					gotErrCode = code
					gotErrMsg = msg
					w.WriteHeader(http.StatusOK)
				}
			}

			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalls++
				gotPrincipal = auth.PrincipalFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			handler := auth.Middleware(tc.authenticator, writeErr)(next)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			handler.ServeHTTP(rec, req)

			if nextCalls != tc.wantNextCalls {
				t.Errorf("next calls = %d, want %d", nextCalls, tc.wantNextCalls)
			}
			if errCalls != tc.wantErrCalls {
				t.Errorf("writeErr calls = %d, want %d", errCalls, tc.wantErrCalls)
			}
			if rec.Code != tc.wantHTTPStatus {
				t.Errorf("HTTP status = %d, want %d", rec.Code, tc.wantHTTPStatus)
			}
			if tc.wantNextCalls > 0 && gotPrincipal != tc.wantPrincipal {
				t.Errorf("principal in context = %+v, want %+v", gotPrincipal, tc.wantPrincipal)
			}
			if tc.wantErrCalls > 0 {
				if gotErrCode != tc.wantErrCode {
					t.Errorf("error code = %d, want %d", gotErrCode, tc.wantErrCode)
				}
				if gotErrMsg != tc.wantErrMsg {
					t.Errorf("error msg = %q, want %q", gotErrMsg, tc.wantErrMsg)
				}
				if strings.Contains(gotErrMsg, internalHint) {
					t.Errorf("error msg %q leaked internal detail %q", gotErrMsg, internalHint)
				}
			}
			if tc.useNilWriter {
				if gotBody := strings.TrimSpace(rec.Body.String()); gotBody != tc.wantErrMsg {
					t.Errorf("fallback body = %q, want %q", gotBody, tc.wantErrMsg)
				}
			}
			if strings.Contains(rec.Body.String(), internalHint) {
				t.Errorf("response body %q leaked internal error %q", rec.Body.String(), internalHint)
			}
		})
	}
}

func TestMiddleware_Concurrent(t *testing.T) {
	const workers = 32

	handler := auth.Middleware(auth.NoopAuthenticator{}, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := auth.PrincipalFromContext(r.Context())
			_, _ = fmt.Fprint(w, p.Username)
		}),
	)

	results := make([]string, workers)
	statuses := make([]int, workers)
	start := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(id int) {
			defer wg.Done()
			<-start

			user := fmt.Sprintf("user-%d", id)
			req := httptest.NewRequest(http.MethodGet, "/rest/ping?u="+user, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			statuses[id] = rec.Code
			results[id] = rec.Body.String()
		}(i)
	}

	close(start)
	wg.Wait()

	for i := range workers {
		if statuses[i] != http.StatusOK {
			t.Errorf("worker %d status = %d, want %d", i, statuses[i], http.StatusOK)
		}
		want := fmt.Sprintf("user-%d", i)
		if results[i] != want {
			t.Errorf("worker %d username = %q, want %q", i, results[i], want)
		}
	}
}
