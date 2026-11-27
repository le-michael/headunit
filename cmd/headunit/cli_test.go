package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"headunit/internal/auth"
	"headunit/internal/store"
)

func TestParseServeArgsAndCLIDispatch(t *testing.T) {
	t.Run("parseServeArgs", func(t *testing.T) {
		tests := []struct {
			name       string
			args       []string
			wantErr    error
			wantErrSub string
			wantStderr string
			exactErrIs bool
		}{
			{
				name: "empty args",
				args: nil,
			},
			{
				name:       "help flag",
				args:       []string{"--help"},
				wantErr:    flag.ErrHelp,
				exactErrIs: true,
				wantStderr: "Usage: headunit serve",
			},
			{
				name:       "short help flag",
				args:       []string{"-h"},
				wantErr:    flag.ErrHelp,
				exactErrIs: true,
				wantStderr: "Usage: headunit serve",
			},
			{
				name:       "unknown flag",
				args:       []string{"--unknown"},
				wantErrSub: "flag provided but not defined",
			},
			{
				name:       "extra positional arg",
				args:       []string{"extra"},
				wantErrSub: "unexpected positional arguments (count=1)",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var stderr bytes.Buffer
				err := parseServeArgs(tc.args, &stderr)
				if tc.exactErrIs {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("parseServeArgs(%v) error = %v, want %v", tc.args, err, tc.wantErr)
					}
				} else if tc.wantErrSub != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
						t.Fatalf("parseServeArgs(%v) error = %v, want substring %q", tc.args, err, tc.wantErrSub)
					}
				} else if err != nil {
					t.Fatalf("parseServeArgs(%v) unexpected error: %v", tc.args, err)
				}

				if tc.wantStderr != "" {
					if got := stderr.String(); !strings.Contains(got, tc.wantStderr) {
						t.Errorf("stderr = %q, want substring %q", got, tc.wantStderr)
					}
				} else if got := stderr.String(); got != "" {
					t.Errorf("stderr = %q, want empty", got)
				}
			})
		}
	})

	t.Run("runCLI dispatch", func(t *testing.T) {
		tests := []struct {
			name       string
			args       []string
			wantErrSub string
			wantStderr string
		}{
			{
				name:       "empty args",
				args:       nil,
				wantErrSub: "missing command",
				wantStderr: "Usage: headunit [command]",
			},
			{
				name:       "help command",
				args:       []string{"help"},
				wantStderr: "Usage: headunit [command]",
			},
			{
				name:       "short -h",
				args:       []string{"-h"},
				wantStderr: "Usage: headunit [command]",
			},
			{
				name:       "long --help",
				args:       []string{"--help"},
				wantStderr: "Usage: headunit [command]",
			},
			{
				name:       "unknown top-level command",
				args:       []string{"bogus"},
				wantErrSub: `unknown command "bogus"`,
			},
			{
				name:       "missing user subcommand",
				args:       []string{"user"},
				wantErrSub: "missing user subcommand",
				wantStderr: "Usage: headunit user <subcommand>",
			},
			{
				name:       "missing apikey subcommand",
				args:       []string{"apikey"},
				wantErrSub: "missing apikey subcommand",
				wantStderr: "Usage: headunit apikey <subcommand>",
			},
			{
				name:       "unknown user subcommand",
				args:       []string{"user", "bogus"},
				wantErrSub: `unknown user subcommand "bogus"`,
			},
			{
				name:       "unknown apikey subcommand",
				args:       []string{"apikey", "bogus"},
				wantErrSub: `unknown apikey subcommand "bogus"`,
			},
			{
				name:       "user help",
				args:       []string{"user", "--help"},
				wantStderr: "Usage: headunit user <subcommand>",
			},
			{
				name:       "apikey help",
				args:       []string{"apikey", "--help"},
				wantStderr: "Usage: headunit apikey <subcommand>",
			},
			{
				name:       "user create --help",
				args:       []string{"user", "create", "--help"},
				wantStderr: "Usage: headunit user create",
			},
			{
				name:       "user passwd --help",
				args:       []string{"user", "passwd", "--help"},
				wantStderr: "Usage: headunit user passwd",
			},
			{
				name:       "user list --help",
				args:       []string{"user", "list", "--help"},
				wantStderr: "Usage: headunit user list",
			},
			{
				name:       "user set-admin --help",
				args:       []string{"user", "set-admin", "--help"},
				wantStderr: "Usage: headunit user set-admin",
			},
			{
				name:       "user disable --help",
				args:       []string{"user", "disable", "--help"},
				wantStderr: "Usage: headunit user disable",
			},
			{
				name:       "user enable --help",
				args:       []string{"user", "enable", "--help"},
				wantStderr: "Usage: headunit user enable",
			},
			{
				name:       "user delete --help",
				args:       []string{"user", "delete", "--help"},
				wantStderr: "Usage: headunit user delete",
			},
			{
				name:       "apikey create --help",
				args:       []string{"apikey", "create", "--help"},
				wantStderr: "Usage: headunit apikey create",
			},
			{
				name:       "apikey list --help",
				args:       []string{"apikey", "list", "--help"},
				wantStderr: "Usage: headunit apikey list",
			},
			{
				name:       "apikey revoke --help",
				args:       []string{"apikey", "revoke", "--help"},
				wantStderr: "Usage: headunit apikey revoke",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				stdout, stderr, err := runCLICapture(t, tc.args, nil)
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
				if tc.wantErrSub != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
						t.Fatalf("runCLI(%v) error = %v, want substring %q", tc.args, err, tc.wantErrSub)
					}
				} else if err != nil {
					t.Fatalf("runCLI(%v) unexpected error: %v", tc.args, err)
				}
				if tc.wantStderr != "" && !strings.Contains(stderr, tc.wantStderr) {
					t.Errorf("stderr = %q, want substring %q", stderr, tc.wantStderr)
				}
			})
		}
	})
}

func TestCLIUserLifecycleAndAuthRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("AUTH_SECRET_KEY", "")

	const (
		aliceInitialPW = "alice-initial-pw!"
		aliceUpdatedPW = "alice-updated-pw!"
		bobPW          = "bob-admin-pw!"
	)

	// 1. Create standard user via --password-stdin and check 0600 auth.key creation.
	out := mustRunCLISuccess(t, []string{"user", "create", "--username", "alice", "--password-stdin"}, strings.NewReader(aliceInitialPW+"\n"))
	if !strings.HasPrefix(out, "created user alice (id=") || !strings.Contains(out, ", admin=false)\n") {
		t.Fatalf("unexpected user create stdout: %q", out)
	}

	keyPath := filepath.Join(dataDir, "auth.key")
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat %s: %v", keyPath, err)
	}
	if got, want := keyInfo.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("auth.key mode = %o, want %o", got, want)
	}

	// 2. Create admin user via --password-env.
	t.Setenv("BOB_PW", bobPW)
	out = mustRunCLISuccess(t, []string{"user", "create", "--username", "bob", "--admin", "--password-env", "BOB_PW"}, nil)
	if !strings.HasPrefix(out, "created user bob (id=") || !strings.Contains(out, ", admin=true)\n") {
		t.Fatalf("unexpected admin user create stdout: %q", out)
	}

	// 3. List users and verify table structure, ordering, timestamps, and no secrets.
	listOut := mustRunCLISuccess(t, []string{"user", "list"}, nil)
	rows := parseTabwriterTable(t, listOut, []string{"ID", "USERNAME", "ADMIN", "DISABLED", "CREATED", "UPDATED"})
	if len(rows) != 2 {
		t.Fatalf("user list row count = %d, want 2; output:\n%s", len(rows), listOut)
	}
	if rows[0]["USERNAME"] != "alice" || rows[0]["ADMIN"] != "false" || rows[0]["DISABLED"] != "false" {
		t.Fatalf("unexpected alice row: %+v", rows[0])
	}
	if rows[1]["USERNAME"] != "bob" || rows[1]["ADMIN"] != "true" || rows[1]["DISABLED"] != "false" {
		t.Fatalf("unexpected bob row: %+v", rows[1])
	}
	for _, r := range rows {
		if _, err := time.Parse(time.RFC3339, r["CREATED"]); err != nil {
			t.Errorf("invalid CREATED timestamp %q: %v", r["CREATED"], err)
		}
		if _, err := time.Parse(time.RFC3339, r["UPDATED"]); err != nil {
			t.Errorf("invalid UPDATED timestamp %q: %v", r["UPDATED"], err)
		}
	}
	assertNoSecretLeak(t, aliceInitialPW, listOut, "", nil)
	assertNoSecretLeak(t, bobPW, listOut, "", nil)

	// 4. Verify real authentication via SubsonicAuthenticator before password/status changes.
	assertTokenAuth(t, dataDir, "", "alice", aliceInitialPW, false)
	assertTokenAuth(t, dataDir, "", "bob", bobPW, true)

	// 5. Promote and demote alice via user set-admin.
	out = mustRunCLISuccess(t, []string{"user", "set-admin", "--username", "alice", "--admin=true"}, nil)
	if got, want := out, "updated user alice (admin=true)\n"; got != want {
		t.Fatalf("set-admin stdout = %q, want %q", got, want)
	}
	assertTokenAuth(t, dataDir, "", "alice", aliceInitialPW, true)

	out = mustRunCLISuccess(t, []string{"user", "set-admin", "--username", "alice", "--admin=false"}, nil)
	if got, want := out, "updated user alice (admin=false)\n"; got != want {
		t.Fatalf("set-admin stdout = %q, want %q", got, want)
	}
	assertTokenAuth(t, dataDir, "", "alice", aliceInitialPW, false)

	// 6. Disable and re-enable alice.
	out = mustRunCLISuccess(t, []string{"user", "disable", "--username", "alice"}, nil)
	if got, want := out, "disabled user alice\n"; got != want {
		t.Fatalf("disable stdout = %q, want %q", got, want)
	}
	assertTokenAuthCode(t, dataDir, "", "alice", aliceInitialPW, auth.ErrCodeWrongCredentials)

	out = mustRunCLISuccess(t, []string{"user", "enable", "--username", "alice"}, nil)
	if got, want := out, "enabled user alice\n"; got != want {
		t.Fatalf("enable stdout = %q, want %q", got, want)
	}
	assertTokenAuth(t, dataDir, "", "alice", aliceInitialPW, false)

	// 7. Update alice password and verify old fails / new succeeds.
	out = mustRunCLISuccess(t, []string{"user", "passwd", "--username", "alice", "--password-stdin"}, strings.NewReader(aliceUpdatedPW+"\n"))
	if got, want := out, "updated password for user alice\n"; got != want {
		t.Fatalf("passwd stdout = %q, want %q", got, want)
	}
	assertTokenAuthCode(t, dataDir, "", "alice", aliceInitialPW, auth.ErrCodeWrongCredentials)
	assertTokenAuth(t, dataDir, "", "alice", aliceUpdatedPW, false)

	// 8. Create an API key for alice, then delete alice and verify API key cascade.
	keyCreateOut := mustRunCLISuccess(t, []string{"apikey", "create", "--username", "alice", "--name", "phone"}, nil)
	createdKey := parseAPIKeyCreateOutput(t, keyCreateOut)
	assertAPIKeyAuth(t, dataDir, "", createdKey["api_key"], false, "alice")

	out = mustRunCLISuccess(t, []string{"user", "delete", "--username", "alice"}, nil)
	if got, want := out, "deleted user alice\n"; got != want {
		t.Fatalf("delete stdout = %q, want %q", got, want)
	}

	assertTokenAuthCode(t, dataDir, "", "alice", aliceUpdatedPW, auth.ErrCodeWrongCredentials)
	assertAPIKeyAuthCode(t, dataDir, "", createdKey["api_key"], auth.ErrCodeInvalidAPIKey)

	afterDeleteUsers := parseTabwriterTable(t, mustRunCLISuccess(t, []string{"user", "list"}, nil), []string{"ID", "USERNAME", "ADMIN", "DISABLED", "CREATED", "UPDATED"})
	if len(afterDeleteUsers) != 1 || afterDeleteUsers[0]["USERNAME"] != "bob" {
		t.Fatalf("user list after delete = %+v, want only bob", afterDeleteUsers)
	}

	afterDeleteKeys := parseTabwriterTable(t, mustRunCLISuccess(t, []string{"apikey", "list"}, nil), []string{"ID", "USERNAME", "NAME", "PREFIX", "CREATED", "REVOKED"})
	if len(afterDeleteKeys) != 0 {
		t.Fatalf("apikey list after user delete = %+v, want empty due to cascade", afterDeleteKeys)
	}
}

func TestCLIPasswordReadingRules(t *testing.T) {
	maxPW := strings.Repeat("p", maxPasswordBytes)
	oversizedPW := strings.Repeat("p", maxPasswordBytes+1)

	t.Run("password-stdin", func(t *testing.T) {
		tests := []struct {
			name       string
			input      string
			wantPW     string
			wantErrSub string
		}{
			{name: "lf trimmed", input: "pw\n", wantPW: "pw"},
			{name: "crlf trimmed", input: "pw\r\n", wantPW: "pw"},
			{name: "spaces preserved", input: "  pw  \n", wantPW: "  pw  "},
			{name: "at most one lf trimmed", input: "pw\n\n", wantPW: "pw\n"},
			{name: "at most one crlf trimmed", input: "pw\r\n\r\n", wantPW: "pw\r\n"},
			{name: "bare cr preserved", input: "pw\r", wantPW: "pw\r"},
			{name: "no trailing newline", input: "pw", wantPW: "pw"},
			{name: "spaces only accepted", input: "   \n", wantPW: "   "},
			{name: "exact 64 KiB plus lf", input: maxPW + "\n", wantPW: maxPW},
			{name: "exact 64 KiB plus crlf", input: maxPW + "\r\n", wantPW: maxPW},
			{name: "empty input", input: "", wantErrSub: "must not be empty"},
			{name: "only lf", input: "\n", wantErrSub: "must not be empty"},
			{name: "only crlf", input: "\r\n", wantErrSub: "must not be empty"},
			{name: "64 KiB + 1", input: oversizedPW, wantErrSub: "exceeds maximum length"},
			{name: "64 KiB + 1 plus lf", input: oversizedPW + "\n", wantErrSub: "exceeds maximum length"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				dataDir := t.TempDir()
				t.Setenv("DATA_DIR", dataDir)
				t.Setenv("AUTH_SECRET_KEY", "")

				stdout, stderr, err := runCLICapture(
					t,
					[]string{"user", "create", "--username", "alice", "--password-stdin"},
					strings.NewReader(tc.input),
				)
				if tc.wantErrSub != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
						t.Fatalf("error = %v, want substring %q", err, tc.wantErrSub)
					}
					if stdout != "" {
						t.Errorf("stdout = %q, want empty on failure", stdout)
					}
					if _, statErr := os.Stat(filepath.Join(dataDir, "auth.key")); !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("auth.key should not be created on rejected password, stat err = %v", statErr)
					}
					if _, statErr := os.Stat(filepath.Join(dataDir, dbFilename)); !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("db should not be created on rejected password, stat err = %v", statErr)
					}
					return
				}

				if err != nil {
					t.Fatalf("unexpected error: %v (stderr=%q)", err, stderr)
				}
				if stderr != "" {
					t.Errorf("stderr = %q, want empty", stderr)
				}
				gotPW := decryptStoredPassword(t, dataDir, "", "alice")
				if gotPW != tc.wantPW {
					t.Errorf("stored password = %q (len=%d), want %q (len=%d)", gotPW, len(gotPW), tc.wantPW, len(tc.wantPW))
				}
			})
		}
	})

	t.Run("password-env", func(t *testing.T) {
		tests := []struct {
			name       string
			envFlag    string
			setEnv     bool
			envVal     string
			wantPW     string
			wantErrSub string
		}{
			{
				name:    "verbatim spaces and newlines",
				envFlag: "TEST_CLI_PW",
				setEnv:  true,
				envVal:  "  pw\nwith\r\nspaces\t ",
				wantPW:  "  pw\nwith\r\nspaces\t ",
			},
			{
				name:    "whitespace only accepted",
				envFlag: "TEST_CLI_PW",
				setEnv:  true,
				envVal:  "   ",
				wantPW:  "   ",
			},
			{
				name:    "exact 64 KiB",
				envFlag: "TEST_CLI_PW",
				setEnv:  true,
				envVal:  maxPW,
				wantPW:  maxPW,
			},
			{
				name:       "empty value",
				envFlag:    "TEST_CLI_PW",
				setEnv:     true,
				envVal:     "",
				wantErrSub: "must not be empty",
			},
			{
				name:       "unset variable",
				envFlag:    "TEST_CLI_PW_UNSET",
				setEnv:     false,
				wantErrSub: "is not set",
			},
			{
				name:       "empty variable name",
				envFlag:    "",
				setEnv:     false,
				wantErrSub: "variable name must not be empty",
			},
			{
				name:       "oversized value",
				envFlag:    "TEST_CLI_PW",
				setEnv:     true,
				envVal:     oversizedPW,
				wantErrSub: "exceeds maximum length",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				dataDir := t.TempDir()
				t.Setenv("DATA_DIR", dataDir)
				t.Setenv("AUTH_SECRET_KEY", "")

				if tc.setEnv {
					t.Setenv(tc.envFlag, tc.envVal)
				} else if tc.envFlag != "" {
					if err := os.Unsetenv(tc.envFlag); err != nil {
						t.Fatalf("unsetenv %s: %v", tc.envFlag, err)
					}
				}

				stdout, stderr, err := runCLICapture(
					t,
					[]string{"user", "create", "--username", "alice", "--password-env=" + tc.envFlag},
					nil,
				)
				if tc.wantErrSub != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
						t.Fatalf("error = %v, want substring %q", err, tc.wantErrSub)
					}
					if stdout != "" {
						t.Errorf("stdout = %q, want empty on failure", stdout)
					}
					if tc.envVal != "" {
						assertNoSecretLeak(t, tc.envVal, stdout, stderr, err)
					}
					return
				}

				if err != nil {
					t.Fatalf("unexpected error: %v (stderr=%q)", err, stderr)
				}
				if stderr != "" {
					t.Errorf("stderr = %q, want empty", stderr)
				}
				gotPW := decryptStoredPassword(t, dataDir, "", "alice")
				if gotPW != tc.wantPW {
					t.Errorf("stored password = %q (len=%d), want %q (len=%d)", gotPW, len(gotPW), tc.wantPW, len(tc.wantPW))
				}
			})
		}
	})
}

func TestCLIUserValidationAndSecretHygiene(t *testing.T) {
	t.Run("user list on fresh DATA_DIR does not create auth.key", func(t *testing.T) {
		dataDir := t.TempDir()
		t.Setenv("DATA_DIR", dataDir)
		t.Setenv("AUTH_SECRET_KEY", "")

		out := mustRunCLISuccess(t, []string{"user", "list"}, nil)
		rows := parseTabwriterTable(t, out, []string{"ID", "USERNAME", "ADMIN", "DISABLED", "CREATED", "UPDATED"})
		if len(rows) != 0 {
			t.Fatalf("expected 0 users, got %d", len(rows))
		}
		if _, err := os.Stat(filepath.Join(dataDir, "auth.key")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("auth.key created on user list; stat err = %v", err)
		}
	})

	t.Run("AUTH_SECRET_KEY precedence without creating auth.key", func(t *testing.T) {
		dataDir := t.TempDir()
		secretKeyHex := strings.Repeat("a1", 32)
		t.Setenv("DATA_DIR", dataDir)
		t.Setenv("AUTH_SECRET_KEY", strings.ToUpper(secretKeyHex))

		const pw = "env-key-password!"
		mustRunCLISuccess(t, []string{"user", "create", "--username", "alice", "--password-stdin"}, strings.NewReader(pw+"\n"))

		if _, err := os.Stat(filepath.Join(dataDir, "auth.key")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("auth.key must not be created when AUTH_SECRET_KEY is set; stat err = %v", err)
		}
		if got := decryptStoredPassword(t, dataDir, secretKeyHex, "alice"); got != pw {
			t.Fatalf("decrypted password = %q, want %q", got, pw)
		}

		const pw2 = "env-key-password-2!"
		mustRunCLISuccess(t, []string{"user", "passwd", "--username", "alice", "--password-stdin"}, strings.NewReader(pw2+"\n"))
		if _, err := os.Stat(filepath.Join(dataDir, "auth.key")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("auth.key must not be created on passwd when AUTH_SECRET_KEY is set; stat err = %v", err)
		}
		if got := decryptStoredPassword(t, dataDir, secretKeyHex, "alice"); got != pw2 {
			t.Fatalf("decrypted updated password = %q, want %q", got, pw2)
		}
	})

	t.Run("invalid AUTH_SECRET_KEY never leaks raw value", func(t *testing.T) {
		dataDir := t.TempDir()
		badSecret := "super-secret-invalid-key-material-xyz123"
		t.Setenv("DATA_DIR", dataDir)
		t.Setenv("AUTH_SECRET_KEY", badSecret)

		stdout, stderr, err := runCLICapture(
			t,
			[]string{"user", "create", "--username", "alice", "--password-stdin"},
			strings.NewReader("alice-pw\n"),
		)
		if err == nil || !strings.Contains(err.Error(), "invalid AUTH_SECRET_KEY") {
			t.Fatalf("error = %v, want invalid AUTH_SECRET_KEY", err)
		}
		assertNoSecretLeak(t, badSecret, stdout, stderr, err)
		assertNoSecretLeak(t, "alice-pw", stdout, stderr, err)
	})

	t.Run("validation errors and Go flag bool pitfall", func(t *testing.T) {
		dataDir := t.TempDir()
		t.Setenv("DATA_DIR", dataDir)
		t.Setenv("AUTH_SECRET_KEY", "")

		mustRunCLISuccess(t, []string{"user", "create", "--username", "Alice", "--password-stdin"}, strings.NewReader("pw1\n"))

		t.Setenv("MY_SECRET_PW", "top-secret-password-999")

		tests := []struct {
			name       string
			args       []string
			stdin      string
			wantErrSub string
			secret     string
		}{
			{
				name:       "create missing username",
				args:       []string{"user", "create", "--password-stdin"},
				stdin:      "pw\n",
				wantErrSub: "--username must not be empty",
			},
			{
				name:       "create leading space username",
				args:       []string{"user", "create", "--username", " alice", "--password-stdin"},
				stdin:      "pw\n",
				wantErrSub: "leading or trailing whitespace",
			},
			{
				name:       "create control char username",
				args:       []string{"user", "create", "--username", "alice\t", "--password-stdin"},
				stdin:      "pw\n",
				wantErrSub: "leading or trailing whitespace",
			},
			{
				name:       "create control char middle username",
				args:       []string{"user", "create", "--username", "al\x00ice", "--password-stdin"},
				stdin:      "pw\n",
				wantErrSub: "control characters",
			},
			{
				name:       "duplicate username case-insensitive",
				args:       []string{"user", "create", "--username", "alice", "--password-stdin"},
				stdin:      "pw2\n",
				wantErrSub: `user "alice" already exists`,
			},
			{
				name:       "both password flags",
				args:       []string{"user", "create", "--username", "bob", "--password-stdin", "--password-env", "MY_SECRET_PW"},
				stdin:      "top-secret-password-999\n",
				wantErrSub: "cannot specify both --password-stdin and --password-env",
				secret:     "top-secret-password-999",
			},
			{
				name:       "neither password flag",
				args:       []string{"user", "create", "--username", "bob"},
				wantErrSub: "must specify either --password-stdin or --password-env",
			},
			{
				name:       "unsupported --password flag",
				args:       []string{"user", "create", "--username", "bob", "--password=secret-inline-pw"},
				wantErrSub: "flag provided but not defined",
				secret:     "secret-inline-pw",
			},
			{
				name:       "passwd missing username",
				args:       []string{"user", "passwd", "--password-stdin"},
				stdin:      "pw\n",
				wantErrSub: "--username must not be empty",
			},
			{
				name:       "passwd unknown user",
				args:       []string{"user", "passwd", "--username", "nobody", "--password-stdin"},
				stdin:      "pw\n",
				wantErrSub: `user "nobody" not found`,
			},
			{
				name:       "set-admin missing username",
				args:       []string{"user", "set-admin", "--admin=true"},
				wantErrSub: "--username must not be empty",
			},
			{
				name:       "set-admin missing --admin",
				args:       []string{"user", "set-admin", "--username", "Alice"},
				wantErrSub: "--admin must be specified",
			},
			{
				name:       "set-admin positional false pitfall",
				args:       []string{"user", "set-admin", "--username", "Alice", "--admin", "false"},
				wantErrSub: "unexpected positional arguments (count=1)",
			},
			{
				name:       "set-admin unknown user",
				args:       []string{"user", "set-admin", "--username", "nobody", "--admin=true"},
				wantErrSub: `user "nobody" not found`,
			},
			{
				name:       "disable missing username",
				args:       []string{"user", "disable"},
				wantErrSub: "--username must not be empty",
			},
			{
				name:       "disable unknown user",
				args:       []string{"user", "disable", "--username", "nobody"},
				wantErrSub: `user "nobody" not found`,
			},
			{
				name:       "enable missing username",
				args:       []string{"user", "enable"},
				wantErrSub: "--username must not be empty",
			},
			{
				name:       "enable unknown user",
				args:       []string{"user", "enable", "--username", "nobody"},
				wantErrSub: `user "nobody" not found`,
			},
			{
				name:       "delete missing username",
				args:       []string{"user", "delete"},
				wantErrSub: "--username must not be empty",
			},
			{
				name:       "delete unknown user",
				args:       []string{"user", "delete", "--username", "nobody"},
				wantErrSub: `user "nobody" not found`,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var stdin io.Reader
				if tc.stdin != "" {
					stdin = strings.NewReader(tc.stdin)
				}
				stdout, stderr, err := runCLICapture(t, tc.args, stdin)
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("runCLI(%v) error = %v, want substring %q", tc.args, err, tc.wantErrSub)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
				if tc.secret != "" {
					assertNoSecretLeak(t, tc.secret, stdout, stderr, err)
				}
			})
		}

		// Verify the "--admin false" positional pitfall did NOT promote Alice.
		rows := parseTabwriterTable(t, mustRunCLISuccess(t, []string{"user", "list"}, nil), []string{"ID", "USERNAME", "ADMIN", "DISABLED", "CREATED", "UPDATED"})
		if len(rows) != 1 || rows[0]["ADMIN"] != "false" {
			t.Fatalf("Alice should remain non-admin after rejected '--admin false' call, got rows: %+v", rows)
		}
	})
}

func TestCLIAPIKeyLifecycleAndAuthRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("AUTH_SECRET_KEY", "")

	mustRunCLISuccess(t, []string{"user", "create", "--username", "Alice", "--admin", "--password-stdin"}, strings.NewReader("alice-pw\n"))
	mustRunCLISuccess(t, []string{"user", "create", "--username", "Bob", "--password-stdin"}, strings.NewReader("bob-pw\n"))

	// 1. Create API key with --name using case-insensitive username lookup.
	createOut := mustRunCLISuccess(t, []string{"apikey", "create", "--username", "alice", "--name", "laptop"}, nil)
	k1 := parseAPIKeyCreateOutput(t, createOut)
	if k1["username"] != "Alice" {
		t.Errorf("apikey username = %q, want %q", k1["username"], "Alice")
	}
	if k1["name"] != "laptop" {
		t.Errorf("apikey name = %q, want %q", k1["name"], "laptop")
	}
	if !strings.HasPrefix(k1["api_key"], "hu_") || len(k1["api_key"]) != 67 {
		t.Errorf("api_key = %q (len=%d), want hu_ prefix and len 67", k1["api_key"], len(k1["api_key"]))
	}
	keyHash := auth.HashAPIKey(k1["api_key"])
	assertNoSecretLeak(t, keyHash, createOut, "", nil)

	// 2. Create API key without --name for Bob.
	createOut2 := mustRunCLISuccess(t, []string{"apikey", "create", "--username", "bob"}, nil)
	k2 := parseAPIKeyCreateOutput(t, createOut2)
	if k2["username"] != "Bob" || k2["name"] != "" {
		t.Errorf("unexpected unnamed apikey metadata: %+v", k2)
	}

	// 3. Authenticate with newly created API key via SubsonicAuthenticator.
	assertAPIKeyAuth(t, dataDir, "", k1["api_key"], true, "Alice")

	// 4. List all API keys and filtered by --username (case-insensitive).
	allOut := mustRunCLISuccess(t, []string{"apikey", "list"}, nil)
	allRows := parseTabwriterTable(t, allOut, []string{"ID", "USERNAME", "NAME", "PREFIX", "CREATED", "REVOKED"})
	if len(allRows) != 2 {
		t.Fatalf("apikey list all rows = %d, want 2; output:\n%s", len(allRows), allOut)
	}
	if allRows[0]["ID"] != k1["id"] || allRows[0]["NAME"] != "laptop" || allRows[0]["PREFIX"] != k1["api_key"][:11] || allRows[0]["REVOKED"] != "-" {
		t.Errorf("unexpected row 0: %+v", allRows[0])
	}
	if allRows[1]["ID"] != k2["id"] || allRows[1]["NAME"] != "" || allRows[1]["PREFIX"] != k2["api_key"][:11] || allRows[1]["REVOKED"] != "-" {
		t.Errorf("unexpected row 1: %+v", allRows[1])
	}
	assertNoSecretLeak(t, k1["api_key"], allOut, "", nil)
	assertNoSecretLeak(t, keyHash, allOut, "", nil)

	aliceOut := mustRunCLISuccess(t, []string{"apikey", "list", "--username", "alice"}, nil)
	aliceRows := parseTabwriterTable(t, aliceOut, []string{"ID", "USERNAME", "NAME", "PREFIX", "CREATED", "REVOKED"})
	if len(aliceRows) != 1 || aliceRows[0]["ID"] != k1["id"] {
		t.Fatalf("filtered apikey list = %+v, want only %s", aliceRows, k1["id"])
	}

	// 5. Revoke key and verify timestamp, idempotent repeat preserving timestamp, and code 44 rejection.
	revokeOut := mustRunCLISuccess(t, []string{"apikey", "revoke", "--id", k1["id"]}, nil)
	if got, want := revokeOut, fmt.Sprintf("revoked api key %s\n", k1["id"]); got != want {
		t.Fatalf("revoke stdout = %q, want %q", got, want)
	}

	firstRevokedAt := lookupAPIKeyRevokedAt(t, dataDir, k1["id"])
	if firstRevokedAt.IsZero() {
		t.Fatalf("expected non-zero revoked_at after revoke")
	}

	revokedRows := parseTabwriterTable(t, mustRunCLISuccess(t, []string{"apikey", "list", "--username", "alice"}, nil), []string{"ID", "USERNAME", "NAME", "PREFIX", "CREATED", "REVOKED"})
	if len(revokedRows) != 1 || revokedRows[0]["REVOKED"] == "-" {
		t.Fatalf("expected RFC3339 REVOKED timestamp in list, got %+v", revokedRows)
	}
	if _, err := time.Parse(time.RFC3339, revokedRows[0]["REVOKED"]); err != nil {
		t.Errorf("invalid REVOKED timestamp %q: %v", revokedRows[0]["REVOKED"], err)
	}

	assertAPIKeyAuthCode(t, dataDir, "", k1["api_key"], auth.ErrCodeInvalidAPIKey)

	// Repeat revoke succeeds and preserves the original revoked_at timestamp.
	mustRunCLISuccess(t, []string{"apikey", "revoke", "--id", k1["id"]}, nil)
	secondRevokedAt := lookupAPIKeyRevokedAt(t, dataDir, k1["id"])
	if !secondRevokedAt.Equal(firstRevokedAt) {
		t.Fatalf("idempotent revoke changed revoked_at from %v to %v", firstRevokedAt, secondRevokedAt)
	}

	// Bob's key remains valid.
	assertAPIKeyAuth(t, dataDir, "", k2["api_key"], false, "Bob")

	// 6. Error cases.
	errorTests := []struct {
		name       string
		args       []string
		wantErrSub string
	}{
		{
			name:       "create unknown user",
			args:       []string{"apikey", "create", "--username", "nobody"},
			wantErrSub: `user "nobody" not found`,
		},
		{
			name:       "create missing username",
			args:       []string{"apikey", "create"},
			wantErrSub: "--username must not be empty",
		},
		{
			name:       "create control char tab in name",
			args:       []string{"apikey", "create", "--username", "alice", "--name", "foo\tbar"},
			wantErrSub: "control characters",
		},
		{
			name:       "create control char newline in name",
			args:       []string{"apikey", "create", "--username", "alice", "--name", "foo\nbar"},
			wantErrSub: "control characters",
		},
		{
			name:       "create control char DEL in name",
			args:       []string{"apikey", "create", "--username", "alice", "--name", "foo\x7fbar"},
			wantErrSub: "control characters",
		},
		{
			name:       "list empty username flag",
			args:       []string{"apikey", "list", "--username="},
			wantErrSub: "--username must not be empty when specified",
		},
		{
			name:       "revoke missing id",
			args:       []string{"apikey", "revoke"},
			wantErrSub: "--id must not be empty",
		},
		{
			name:       "revoke unknown id",
			args:       []string{"apikey", "revoke", "--id", "key_does_not_exist"},
			wantErrSub: `api key "key_does_not_exist" not found`,
		},
	}

	for _, tc := range errorTests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := runCLICapture(t, tc.args, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("runCLI(%v) error = %v, want substring %q", tc.args, err, tc.wantErrSub)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestHeadunitCtlWrapper(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "scripts", "headunit-ctl.sh")

	type invocation struct {
		args  []string
		stdin string
		env   string
	}

	runWrapper := func(t *testing.T, args []string, stdin string, extraEnv map[string]string) (stdout, stderr string, exitCode int, invoked bool, got invocation) {
		t.Helper()

		fakeBinDir := t.TempDir()
		captureDir := t.TempDir()
		markerFile := filepath.Join(captureDir, "invoked")
		argsFile := filepath.Join(captureDir, "args.txt")
		stdinFile := filepath.Join(captureDir, "stdin.txt")
		envFile := filepath.Join(captureDir, "env.txt")

		fakeDocker := filepath.Join(fakeBinDir, "docker")
		fakeDockerScript := `#!/usr/bin/env bash
set -euo pipefail
: > "$FAKE_DOCKER_CAPTURE_DIR/invoked"
printf '%s\n' "$@" > "$FAKE_DOCKER_CAPTURE_DIR/args.txt"
cat > "$FAKE_DOCKER_CAPTURE_DIR/stdin.txt"
printf '%s' "${MY_PW-__UNSET__}" > "$FAKE_DOCKER_CAPTURE_DIR/env.txt"
`
		if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0o755); err != nil {
			t.Fatalf("write fake docker: %v", err)
		}

		cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}

		env := make([]string, 0, len(os.Environ())+len(extraEnv)+2)
		for _, entry := range os.Environ() {
			k, _, _ := strings.Cut(entry, "=")
			if k == "PATH" || k == "HEADUNIT_SERVICE" || k == "MY_PW" || k == "FAKE_DOCKER_CAPTURE_DIR" {
				continue
			}
			env = append(env, entry)
		}
		env = append(env, "PATH="+fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		env = append(env, "FAKE_DOCKER_CAPTURE_DIR="+captureDir)
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
		cmd.Env = env

		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf

		err := cmd.Run()
		if err == nil {
			exitCode = 0
		} else {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("exec wrapper failed unexpectedly: %v", err)
			}
			exitCode = exitErr.ExitCode()
		}

		if _, statErr := os.Stat(markerFile); statErr == nil {
			invoked = true
			rawArgs, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read captured args: %v", err)
			}
			trimmedArgs := strings.TrimSuffix(string(rawArgs), "\n")
			if trimmedArgs != "" {
				got.args = strings.Split(trimmedArgs, "\n")
			}
			rawStdin, err := os.ReadFile(stdinFile)
			if err != nil {
				t.Fatalf("read captured stdin: %v", err)
			}
			got.stdin = string(rawStdin)
			rawEnv, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatalf("read captured env: %v", err)
			}
			got.env = string(rawEnv)
		}

		return outBuf.String(), errBuf.String(), exitCode, invoked, got
	}

	t.Run("0 args exits 1 without invoking docker", func(t *testing.T) {
		stdout, stderr, code, invoked, _ := runWrapper(t, nil, "", nil)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if invoked {
			t.Fatal("docker should not be invoked when 0 args are passed")
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "Usage: headunit-ctl.sh") {
			t.Errorf("stderr = %q, want usage message", stderr)
		}
	})

	t.Run("user list forwards compose exec", func(t *testing.T) {
		stdout, stderr, code, invoked, got := runWrapper(t, []string{"user", "list"}, "", nil)
		if code != 0 || stderr != "" || stdout != "" {
			t.Fatalf("unexpected result: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !invoked {
			t.Fatal("expected docker to be invoked")
		}
		wantArgs := []string{"compose", "exec", "-T", "headunit", "/usr/local/bin/entrypoint.sh", "user", "list"}
		if !slices.Equal(got.args, wantArgs) {
			t.Fatalf("docker args = %v, want %v", got.args, wantArgs)
		}
	})

	t.Run("--password-env VAR passes -e VAR and forwards env", func(t *testing.T) {
		_, stderr, code, invoked, got := runWrapper(
			t,
			[]string{"user", "create", "--username", "alice", "--password-env", "MY_PW"},
			"",
			map[string]string{"MY_PW": "secret-val"},
		)
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stderr=%q, want 0 and empty", code, stderr)
		}
		if !invoked {
			t.Fatal("expected docker to be invoked")
		}
		wantArgs := []string{"compose", "exec", "-T", "-e", "MY_PW", "headunit", "/usr/local/bin/entrypoint.sh", "user", "create", "--username", "alice", "--password-env", "MY_PW"}
		if !slices.Equal(got.args, wantArgs) {
			t.Fatalf("docker args = %v, want %v", got.args, wantArgs)
		}
		if got.env != "secret-val" {
			t.Fatalf("MY_PW forwarded to docker = %q, want %q", got.env, "secret-val")
		}
	})

	t.Run("--password-env=VAR passes -e VAR and forwards env", func(t *testing.T) {
		_, stderr, code, invoked, got := runWrapper(
			t,
			[]string{"user", "passwd", "--username", "alice", "--password-env=MY_PW"},
			"",
			map[string]string{"MY_PW": "secret-val-2"},
		)
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stderr=%q, want 0 and empty", code, stderr)
		}
		if !invoked {
			t.Fatal("expected docker to be invoked")
		}
		wantArgs := []string{"compose", "exec", "-T", "-e", "MY_PW", "headunit", "/usr/local/bin/entrypoint.sh", "user", "passwd", "--username", "alice", "--password-env=MY_PW"}
		if !slices.Equal(got.args, wantArgs) {
			t.Fatalf("docker args = %v, want %v", got.args, wantArgs)
		}
		if got.env != "secret-val-2" {
			t.Fatalf("MY_PW forwarded to docker = %q, want %q", got.env, "secret-val-2")
		}
	})

	t.Run("--password-env BAD=LEAK exits 1 without invoking docker or leaking secret", func(t *testing.T) {
		stdout, stderr, code, invoked, _ := runWrapper(
			t,
			[]string{"user", "create", "--username", "alice", "--password-env", "BAD=LEAK"},
			"",
			nil,
		)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if invoked {
			t.Fatal("docker should not be invoked for invalid --password-env var name")
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "invalid environment variable name") {
			t.Errorf("stderr = %q, want invalid environment variable name message", stderr)
		}
		assertNoSecretLeak(t, "LEAK", stdout, stderr, nil)
	})

	t.Run("trailing --password-env with no value exits 1 without invoking docker", func(t *testing.T) {
		for _, flagName := range []string{"--password-env", "-password-env"} {
			stdout, stderr, code, invoked, _ := runWrapper(
				t,
				[]string{"user", "create", "--username", "alice", flagName},
				"",
				nil,
			)
			if code != 1 {
				t.Fatalf("%s: exit code = %d, want 1", flagName, code)
			}
			if invoked {
				t.Fatalf("%s: docker should not be invoked for trailing --password-env with no value", flagName)
			}
			if stdout != "" {
				t.Errorf("%s: stdout = %q, want empty", flagName, stdout)
			}
			if !strings.Contains(stderr, "requires an environment variable name") {
				t.Errorf("%s: stderr = %q, want missing value error", flagName, stderr)
			}
		}
	})

	t.Run("--password-stdin forwards stdin verbatim", func(t *testing.T) {
		const stdinPW = "  my-secret-pw\n"
		_, stderr, code, invoked, got := runWrapper(
			t,
			[]string{"user", "create", "--username", "alice", "--password-stdin"},
			stdinPW,
			nil,
		)
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stderr=%q, want 0 and empty", code, stderr)
		}
		if !invoked {
			t.Fatal("expected docker to be invoked")
		}
		wantArgs := []string{"compose", "exec", "-T", "headunit", "/usr/local/bin/entrypoint.sh", "user", "create", "--username", "alice", "--password-stdin"}
		if !slices.Equal(got.args, wantArgs) {
			t.Fatalf("docker args = %v, want %v", got.args, wantArgs)
		}
		if got.stdin != stdinPW {
			t.Fatalf("forwarded stdin = %q, want %q", got.stdin, stdinPW)
		}
	})

	t.Run("HEADUNIT_SERVICE override", func(t *testing.T) {
		_, stderr, code, invoked, got := runWrapper(
			t,
			[]string{"apikey", "list"},
			"",
			map[string]string{"HEADUNIT_SERVICE": "custom-music"},
		)
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stderr=%q, want 0 and empty", code, stderr)
		}
		if !invoked {
			t.Fatal("expected docker to be invoked")
		}
		wantArgs := []string{"compose", "exec", "-T", "custom-music", "/usr/local/bin/entrypoint.sh", "apikey", "list"}
		if !slices.Equal(got.args, wantArgs) {
			t.Fatalf("docker args = %v, want %v", got.args, wantArgs)
		}
	})
}

func runCLICapture(t *testing.T, args []string, stdin io.Reader) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runCLI(context.Background(), args, stdin, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func mustRunCLISuccess(t *testing.T, args []string, stdin io.Reader) string {
	t.Helper()
	stdout, stderr, err := runCLICapture(t, args, stdin)
	if err != nil {
		t.Fatalf("runCLI(%v) unexpected error: %v (stderr=%q)", args, err, stderr)
	}
	if stderr != "" {
		t.Fatalf("runCLI(%v) unexpected stderr = %q", args, stderr)
	}
	return stdout
}

func assertNoSecretLeak(t *testing.T, secret, stdout, stderr string, err error) {
	t.Helper()
	if secret == "" {
		return
	}
	if strings.Contains(stdout, secret) {
		t.Errorf("stdout leaked secret %q: %q", secret, stdout)
	}
	if strings.Contains(stderr, secret) {
		t.Errorf("stderr leaked secret %q: %q", secret, stderr)
	}
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked secret %q: %v", secret, err)
	}
}

func decryptStoredPassword(t *testing.T, dataDir, authSecretKey, username string) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dataDir, dbFilename))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("store.Close: %v", closeErr)
		}
	}()

	user, err := st.GetUserByUsername(ctx, username)
	if err != nil {
		t.Fatalf("GetUserByUsername(%q): %v", username, err)
	}

	key, err := auth.LoadOrCreateKey(dataDir, authSecretKey)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	cipher, err := auth.NewPasswordCipher(key)
	if err != nil {
		t.Fatalf("NewPasswordCipher: %v", err)
	}
	pw, err := cipher.Decrypt(user.PasswordCiphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	return pw
}

func newTestAuthenticator(t *testing.T, dataDir, authSecretKey string) (*auth.SubsonicAuthenticator, func()) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dataDir, dbFilename))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	key, err := auth.LoadOrCreateKey(dataDir, authSecretKey)
	if err != nil {
		_ = st.Close()
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	cipher, err := auth.NewPasswordCipher(key)
	if err != nil {
		_ = st.Close()
		t.Fatalf("NewPasswordCipher: %v", err)
	}
	authenticator, err := auth.NewSubsonicAuthenticator(st, cipher)
	if err != nil {
		_ = st.Close()
		t.Fatalf("NewSubsonicAuthenticator: %v", err)
	}
	return authenticator, func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("store.Close: %v", closeErr)
		}
	}
}

func subsonicToken(password, salt string) string {
	sum := md5.Sum([]byte(password + salt))
	return hex.EncodeToString(sum[:])
}

func assertTokenAuth(t *testing.T, dataDir, authSecretKey, username, password string, wantAdmin bool) {
	t.Helper()
	authenticator, cleanup := newTestAuthenticator(t, dataDir, authSecretKey)
	defer cleanup()

	const salt = "test-salt-123"
	q := url.Values{}
	q.Set("u", username)
	q.Set("t", subsonicToken(password, salt))
	q.Set("s", salt)
	req := httptest.NewRequest(http.MethodGet, "/rest/ping?"+q.Encode(), nil)

	p, err := authenticator.Authenticate(req)
	if err != nil {
		t.Fatalf("token Authenticate(%q) unexpected error: %v", username, err)
	}
	if !strings.EqualFold(p.Username, username) {
		t.Errorf("Principal.Username = %q, want %q", p.Username, username)
	}
	if p.Method != auth.AuthMethodToken {
		t.Errorf("Principal.Method = %q, want %q", p.Method, auth.AuthMethodToken)
	}
	if p.IsAdmin != wantAdmin {
		t.Errorf("Principal.IsAdmin = %t, want %t", p.IsAdmin, wantAdmin)
	}
}

func assertTokenAuthCode(t *testing.T, dataDir, authSecretKey, username, password string, wantCode int) {
	t.Helper()
	authenticator, cleanup := newTestAuthenticator(t, dataDir, authSecretKey)
	defer cleanup()

	const salt = "test-salt-123"
	q := url.Values{}
	q.Set("u", username)
	q.Set("t", subsonicToken(password, salt))
	q.Set("s", salt)
	req := httptest.NewRequest(http.MethodGet, "/rest/ping?"+q.Encode(), nil)

	_, err := authenticator.Authenticate(req)
	var authErr *auth.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("token Authenticate(%q) error = %v, want *auth.AuthError", username, err)
	}
	if authErr.Code != wantCode {
		t.Fatalf("AuthError.Code = %d, want %d", authErr.Code, wantCode)
	}
}

func assertAPIKeyAuth(t *testing.T, dataDir, authSecretKey, apiKey string, wantAdmin bool, wantUsername string) {
	t.Helper()
	authenticator, cleanup := newTestAuthenticator(t, dataDir, authSecretKey)
	defer cleanup()

	q := url.Values{}
	q.Set("apiKey", apiKey)
	req := httptest.NewRequest(http.MethodGet, "/rest/ping?"+q.Encode(), nil)

	p, err := authenticator.Authenticate(req)
	if err != nil {
		t.Fatalf("apiKey Authenticate unexpected error: %v", err)
	}
	if p.Username != wantUsername {
		t.Errorf("Principal.Username = %q, want %q", p.Username, wantUsername)
	}
	if p.IsAdmin != wantAdmin {
		t.Errorf("Principal.IsAdmin = %t, want %t", p.IsAdmin, wantAdmin)
	}
	if p.Method != auth.AuthMethodAPIKey {
		t.Errorf("Principal.Method = %q, want %q", p.Method, auth.AuthMethodAPIKey)
	}
}

func assertAPIKeyAuthCode(t *testing.T, dataDir, authSecretKey, apiKey string, wantCode int) {
	t.Helper()
	authenticator, cleanup := newTestAuthenticator(t, dataDir, authSecretKey)
	defer cleanup()

	q := url.Values{}
	q.Set("apiKey", apiKey)
	req := httptest.NewRequest(http.MethodGet, "/rest/ping?"+q.Encode(), nil)

	_, err := authenticator.Authenticate(req)
	var authErr *auth.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("apiKey Authenticate error = %v, want *auth.AuthError", err)
	}
	if authErr.Code != wantCode {
		t.Fatalf("AuthError.Code = %d, want %d", authErr.Code, wantCode)
	}
}

func lookupAPIKeyRevokedAt(t *testing.T, dataDir, keyID string) time.Time {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dataDir, dbFilename))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("store.Close: %v", closeErr)
		}
	}()

	keys, err := st.ListAPIKeys(ctx, "")
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	for _, k := range keys {
		if k.ID == keyID {
			return k.RevokedAt
		}
	}
	t.Fatalf("api key %q not found in store", keyID)
	return time.Time{}
}

func parseAPIKeyCreateOutput(t *testing.T, out string) map[string]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("apikey create output lines = %d, want 4; output:\n%s", len(lines), out)
	}
	wantKeys := []string{"id", "username", "name", "api_key"}
	parsed := make(map[string]string, len(wantKeys))
	for i, wantKey := range wantKeys {
		k, v, ok := strings.Cut(lines[i], ": ")
		if !ok || k != wantKey {
			t.Fatalf("line %d = %q, want %q prefix", i, lines[i], wantKey+": ")
		}
		parsed[k] = v
	}
	return parsed
}

func parseTabwriterTable(t *testing.T, out string, wantHeaders []string) []map[string]string {
	t.Helper()
	trimmed := strings.TrimSuffix(out, "\n")
	if trimmed == "" {
		t.Fatalf("table output is empty, want header %v", wantHeaders)
	}
	lines := strings.Split(trimmed, "\n")
	headerLine := lines[0]

	gotHeaders := strings.Fields(headerLine)
	if !slices.Equal(gotHeaders, wantHeaders) {
		t.Fatalf("table headers = %v, want %v (header line: %q)", gotHeaders, wantHeaders, headerLine)
	}

	starts := make([]int, len(wantHeaders))
	searchFrom := 0
	for i, h := range wantHeaders {
		idx := strings.Index(headerLine[searchFrom:], h)
		if idx < 0 {
			t.Fatalf("header %q not found in %q", h, headerLine)
		}
		starts[i] = searchFrom + idx
		searchFrom = starts[i] + len(h)
	}

	rows := make([]map[string]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		row := make(map[string]string, len(wantHeaders))
		for i, h := range wantHeaders {
			start := starts[i]
			if start >= len(line) {
				row[h] = ""
				continue
			}
			end := len(line)
			if i+1 < len(starts) && starts[i+1] < end {
				end = starts[i+1]
			}
			row[h] = strings.TrimSpace(line[start:end])
		}
		rows = append(rows, row)
	}
	return rows
}
