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
			want: auth.Principal{Username: "alice"},
		},
		{
			name: "GET query parameter trimmed",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping?u=%20alice%20", nil),
			want: auth.Principal{Username: "alice"},
		},
		{
			name: "POST form body",
			req:  newFormPostRequest(t, "/rest/ping", "u=bob"),
			want: auth.Principal{Username: "bob"},
		},
		{
			name: "omitted username defaults to headunit",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping", nil),
			want: auth.Principal{Username: "headunit"},
		},
		{
			name: "empty username defaults to headunit",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping?u=", nil),
			want: auth.Principal{Username: "headunit"},
		},
		{
			name: "whitespace username defaults to headunit",
			req:  httptest.NewRequest(http.MethodGet, "/rest/ping?u=%20%20", nil),
			want: auth.Principal{Username: "headunit"},
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
		want := auth.Principal{Username: "alice"}
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

func TestMiddleware(t *testing.T) {
	const (
		wantErrCode  = 40
		wantErrMsg   = "Wrong username or password"
		internalHint = "secret-backend-failure"
	)

	tests := []struct {
		name           string
		authenticator  auth.Authenticator
		useNilWriter   bool
		target         string
		wantNextCalls  int
		wantErrCalls   int
		wantPrincipal  auth.Principal
		wantHTTPStatus int
	}{
		{
			name:           "happy path with username",
			authenticator:  auth.NoopAuthenticator{},
			target:         "/rest/ping?u=alice",
			wantNextCalls:  1,
			wantErrCalls:   0,
			wantPrincipal:  auth.Principal{Username: "alice"},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "happy path default username",
			authenticator:  auth.NoopAuthenticator{},
			target:         "/rest/ping",
			wantNextCalls:  1,
			wantErrCalls:   0,
			wantPrincipal:  auth.Principal{Username: "headunit"},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "failing authenticator calls error writer without leaking details",
			authenticator:  stubAuthenticator{err: errors.New(internalHint)},
			target:         "/rest/ping?u=alice",
			wantNextCalls:  0,
			wantErrCalls:   1,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "nil authenticator fails closed",
			authenticator:  nil,
			target:         "/rest/ping?u=alice",
			wantNextCalls:  0,
			wantErrCalls:   1,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "nil error writer falls back to HTTP 401 on auth error",
			authenticator:  stubAuthenticator{err: errors.New(internalHint)},
			useNilWriter:   true,
			target:         "/rest/ping?u=alice",
			wantNextCalls:  0,
			wantErrCalls:   0,
			wantHTTPStatus: http.StatusUnauthorized,
		},
		{
			name:           "nil error writer falls back to HTTP 401 on nil authenticator",
			authenticator:  nil,
			useNilWriter:   true,
			target:         "/rest/ping?u=alice",
			wantNextCalls:  0,
			wantErrCalls:   0,
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
				if gotErrCode != wantErrCode {
					t.Errorf("error code = %d, want %d", gotErrCode, wantErrCode)
				}
				if gotErrMsg != wantErrMsg {
					t.Errorf("error msg = %q, want %q", gotErrMsg, wantErrMsg)
				}
			}
			if tc.useNilWriter {
				if gotBody := strings.TrimSpace(rec.Body.String()); gotBody != wantErrMsg {
					t.Errorf("fallback body = %q, want %q", gotBody, wantErrMsg)
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
