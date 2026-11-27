package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"headunit/internal/auth"
	"headunit/internal/config"
	"headunit/internal/store"
)

const maxPasswordBytes = 64 * 1024

// parseServeArgs validates flags/args passed after "headunit serve".
// Returns flag.ErrHelp after writing usage to stderr when -h/--help is requested.
func parseServeArgs(args []string, stderr io.Writer) error {
	if stderr == nil {
		stderr = io.Discard
	}
	fs := newFlagSet("headunit serve")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printServeUsage(stderr)
			return flag.ErrHelp
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional arguments (count=%d)", fs.NArg())
	}
	return nil
}

// runCLI executes non-server CLI commands ("user", "apikey", "help", "-h", "--help").
// Help invocations write usage to stderr and return nil; failures return a secret-free error.
func runCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) == 0 {
		printTopLevelUsage(stderr)
		return errors.New("missing command")
	}

	switch args[0] {
	case "help", "-h", "-help", "--help":
		printTopLevelUsage(stderr)
		return nil
	case "user":
		return runUserCLI(ctx, args[1:], stdin, stdout, stderr)
	case "apikey":
		return runAPIKeyCLI(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runUserCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUserUsage(stderr)
		return errors.New("missing user subcommand")
	}

	switch args[0] {
	case "help", "-h", "-help", "--help":
		printUserUsage(stderr)
		return nil
	case "create":
		return runUserCreate(ctx, args[1:], stdin, stdout, stderr)
	case "passwd":
		return runUserPasswd(ctx, args[1:], stdin, stdout, stderr)
	case "list":
		return runUserList(ctx, args[1:], stdout, stderr)
	case "set-admin":
		return runUserSetAdmin(ctx, args[1:], stdout, stderr)
	case "disable":
		return runUserSetDisabled(ctx, args[1:], stdout, stderr, true)
	case "enable":
		return runUserSetDisabled(ctx, args[1:], stdout, stderr, false)
	case "delete":
		return runUserDelete(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

func runAPIKeyCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printAPIKeyUsage(stderr)
		return errors.New("missing apikey subcommand")
	}

	switch args[0] {
	case "help", "-h", "-help", "--help":
		printAPIKeyUsage(stderr)
		return nil
	case "create":
		return runAPIKeyCreate(ctx, args[1:], stdout, stderr)
	case "list":
		return runAPIKeyList(ctx, args[1:], stdout, stderr)
	case "revoke":
		return runAPIKeyRevoke(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown apikey subcommand %q", args[0])
	}
}

func runUserCreate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit user create")
	username := fs.String("username", "", "Account username (required)")
	isAdmin := fs.Bool("admin", false, "Grant administrator privileges")
	passwordStdin := fs.Bool("password-stdin", false, "Read password from standard input")
	passwordEnv := fs.String("password-env", "", "Read password verbatim from environment variable")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit user create --username <name> [--admin] (--password-stdin | --password-env <VAR>)")
	})
	if err != nil || helped {
		return err
	}
	if err := validateCLIUsername(*username); err != nil {
		return err
	}
	password, err := readPassword(fs, *passwordStdin, *passwordEnv, stdin)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	key, err := auth.LoadOrCreateKey(cfg.DataDir, cfg.AuthSecretKey)
	if err != nil {
		return err
	}
	cipher, err := auth.NewPasswordCipher(key)
	if err != nil {
		return err
	}
	ciphertext, err := cipher.Encrypt(password)
	if err != nil {
		return err
	}

	var created *store.User
	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		var createErr error
		created, createErr = st.CreateUser(ctx, store.User{
			Username:           *username,
			PasswordCiphertext: ciphertext,
			IsAdmin:            *isAdmin,
		})
		return mapUserStoreError(createErr, *username)
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "created user %s (id=%s, admin=%t)\n", created.Username, created.ID, created.IsAdmin)
	return err
}

func runUserPasswd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit user passwd")
	username := fs.String("username", "", "Account username (required)")
	passwordStdin := fs.Bool("password-stdin", false, "Read password from standard input")
	passwordEnv := fs.String("password-env", "", "Read password verbatim from environment variable")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit user passwd --username <name> (--password-stdin | --password-env <VAR>)")
	})
	if err != nil || helped {
		return err
	}
	if *username == "" {
		return errors.New("--username must not be empty")
	}
	password, err := readPassword(fs, *passwordStdin, *passwordEnv, stdin)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	key, err := auth.LoadOrCreateKey(cfg.DataDir, cfg.AuthSecretKey)
	if err != nil {
		return err
	}
	cipher, err := auth.NewPasswordCipher(key)
	if err != nil {
		return err
	}
	ciphertext, err := cipher.Encrypt(password)
	if err != nil {
		return err
	}

	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		return mapUserStoreError(st.UpdateUserPassword(ctx, *username, ciphertext), *username)
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "updated password for user %s\n", *username)
	return err
}

func runUserList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit user list")
	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit user list")
	})
	if err != nil || helped {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var users []store.User
	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		var listErr error
		users, listErr = st.ListUsers(ctx)
		return listErr
	})
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tUSERNAME\tADMIN\tDISABLED\tCREATED\tUPDATED"); err != nil {
		return err
	}
	for _, u := range users {
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%s\t%t\t%t\t%s\t%s\n",
			u.ID,
			u.Username,
			u.IsAdmin,
			u.Disabled,
			u.CreatedAt.UTC().Format(time.RFC3339),
			u.UpdatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func runUserSetAdmin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit user set-admin")
	username := fs.String("username", "", "Account username (required)")
	isAdmin := fs.Bool("admin", false, "Set administrator status (required: --admin=true or --admin=false)")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit user set-admin --username <name> --admin=<true|false>")
	})
	if err != nil || helped {
		return err
	}
	if *username == "" {
		return errors.New("--username must not be empty")
	}
	if !flagWasSet(fs, "admin") {
		return errors.New("--admin must be specified (--admin=true or --admin=false)")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		return mapUserStoreError(st.SetUserAdmin(ctx, *username, *isAdmin), *username)
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "updated user %s (admin=%t)\n", *username, *isAdmin)
	return err
}

func runUserSetDisabled(ctx context.Context, args []string, stdout, stderr io.Writer, disabled bool) error {
	action := "enable"
	verb := "enabled"
	if disabled {
		action = "disable"
		verb = "disabled"
	}

	fs := newFlagSet("headunit user " + action)
	username := fs.String("username", "", "Account username (required)")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, fmt.Sprintf("headunit user %s --username <name>", action))
	})
	if err != nil || helped {
		return err
	}
	if *username == "" {
		return errors.New("--username must not be empty")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		return mapUserStoreError(st.SetUserDisabled(ctx, *username, disabled), *username)
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "%s user %s\n", verb, *username)
	return err
}

func runUserDelete(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit user delete")
	username := fs.String("username", "", "Account username (required)")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit user delete --username <name>")
	})
	if err != nil || helped {
		return err
	}
	if *username == "" {
		return errors.New("--username must not be empty")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		return mapUserStoreError(st.DeleteUser(ctx, *username), *username)
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "deleted user %s\n", *username)
	return err
}

func runAPIKeyCreate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit apikey create")
	username := fs.String("username", "", "Owning account username (required)")
	name := fs.String("name", "", "Optional API key label")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit apikey create --username <name> [--name <label>]")
	})
	if err != nil || helped {
		return err
	}
	if *username == "" {
		return errors.New("--username must not be empty")
	}
	if err := validateControlFree("api key name", *name); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var (
		user      *store.User
		created   *store.APIKey
		plaintext string
	)
	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		var lookupErr error
		user, lookupErr = st.GetUserByUsername(ctx, *username)
		if lookupErr != nil {
			return mapUserStoreError(lookupErr, *username)
		}

		var (
			prefix, hash string
			genErr       error
		)
		plaintext, prefix, hash, genErr = auth.GenerateAPIKey()
		if genErr != nil {
			return genErr
		}

		var createErr error
		created, createErr = st.CreateAPIKey(ctx, store.APIKey{
			UserID:    user.ID,
			Name:      *name,
			KeyPrefix: prefix,
			KeyHash:   hash,
		})
		if createErr != nil {
			if errors.Is(createErr, store.ErrNotFound) {
				return fmt.Errorf("user %q not found", *username)
			}
			if errors.Is(createErr, store.ErrAlreadyExists) {
				return errors.New("api key already exists")
			}
			return createErr
		}
		return nil
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(
		stdout,
		"id: %s\nusername: %s\nname: %s\napi_key: %s\n",
		created.ID,
		user.Username,
		created.Name,
		plaintext,
	)
	return err
}

func runAPIKeyList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit apikey list")
	username := fs.String("username", "", "Filter keys by owning username")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit apikey list [--username <name>]")
	})
	if err != nil || helped {
		return err
	}
	if flagWasSet(fs, "username") && *username == "" {
		return errors.New("--username must not be empty when specified")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var keys []store.APIKey
	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		var listErr error
		keys, listErr = st.ListAPIKeys(ctx, *username)
		return listErr
	})
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tUSERNAME\tNAME\tPREFIX\tCREATED\tREVOKED"); err != nil {
		return err
	}
	for _, k := range keys {
		revoked := "-"
		if !k.RevokedAt.IsZero() {
			revoked = k.RevokedAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID,
			k.Username,
			k.Name,
			k.KeyPrefix,
			k.CreatedAt.UTC().Format(time.RFC3339),
			revoked,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func runAPIKeyRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("headunit apikey revoke")
	keyID := fs.String("id", "", "API key ID (required)")

	helped, err := parseSubcommandFlags(fs, args, stderr, func(w io.Writer, fs *flag.FlagSet) {
		printSubcommandUsage(w, fs, "headunit apikey revoke --id <key_id>")
	})
	if err != nil || helped {
		return err
	}
	if *keyID == "" {
		return errors.New("--id must not be empty")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	err = withStore(ctx, cfg.DataDir, func(st *store.Store) error {
		revokeErr := st.RevokeAPIKey(ctx, *keyID)
		if errors.Is(revokeErr, store.ErrNotFound) {
			return fmt.Errorf("api key %q not found", *keyID)
		}
		return revokeErr
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "revoked api key %s\n", *keyID)
	return err
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func parseSubcommandFlags(fs *flag.FlagSet, args []string, stderr io.Writer, usageFn func(io.Writer, *flag.FlagSet)) (bool, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageFn(stderr, fs)
			return true, nil
		}
		return false, err
	}
	if fs.NArg() > 0 {
		return false, fmt.Errorf("unexpected positional arguments (count=%d)", fs.NArg())
	}
	return false, nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func readPassword(fs *flag.FlagSet, passwordStdin bool, passwordEnv string, stdin io.Reader) (string, error) {
	passwordEnvSet := flagWasSet(fs, "password-env")
	if passwordStdin && passwordEnvSet {
		return "", errors.New("cannot specify both --password-stdin and --password-env")
	}
	if !passwordStdin && !passwordEnvSet {
		return "", errors.New("must specify either --password-stdin or --password-env")
	}
	if passwordStdin {
		return readPasswordFromStdin(stdin)
	}
	return readPasswordFromEnv(passwordEnv)
}

func readPasswordFromStdin(stdin io.Reader) (string, error) {
	if stdin == nil {
		return "", errors.New("password from stdin must not be empty")
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxPasswordBytes+3))
	if err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	if bytes.HasSuffix(data, []byte("\r\n")) {
		data = data[:len(data)-2]
	} else if bytes.HasSuffix(data, []byte("\n")) {
		data = data[:len(data)-1]
	}
	if len(data) > maxPasswordBytes {
		return "", fmt.Errorf("password from stdin exceeds maximum length (%d bytes)", maxPasswordBytes)
	}
	if len(data) == 0 {
		return "", errors.New("password from stdin must not be empty")
	}
	return string(data), nil
}

func readPasswordFromEnv(varName string) (string, error) {
	if varName == "" {
		return "", errors.New("--password-env variable name must not be empty")
	}
	pw, ok := os.LookupEnv(varName)
	if !ok {
		return "", fmt.Errorf("environment variable %q is not set", varName)
	}
	if pw == "" {
		return "", fmt.Errorf("password from environment variable %q must not be empty", varName)
	}
	if len(pw) > maxPasswordBytes {
		return "", fmt.Errorf("password from environment variable %q exceeds maximum length (%d bytes)", varName, maxPasswordBytes)
	}
	return pw, nil
}

func validateCLIUsername(username string) error {
	if username == "" {
		return errors.New("--username must not be empty")
	}
	if strings.TrimSpace(username) != username {
		return fmt.Errorf("username %q must not have leading or trailing whitespace", username)
	}
	return validateControlFree("username", username)
}

func validateControlFree(label, value string) error {
	for i := range len(value) {
		b := value[i]
		if b < 0x20 || b == 0x7f {
			return fmt.Errorf("%s must not contain control characters", label)
		}
	}
	return nil
}

func withStore(ctx context.Context, dataDir string, fn func(st *store.Store) error) error {
	dbPath := filepath.Join(dataDir, dbFilename)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	err = fn(st)
	if closeErr := st.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("close store: %w", closeErr))
	}
	return err
}

func mapUserStoreError(err error, username string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("user %q already exists", username)
	}
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("user %q not found", username)
	}
	return err
}

func printTopLevelUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: headunit [command]

Commands:
  serve     Start the OpenSubsonic HTTP server (default when no command is given)
  user      Manage user accounts
  apikey    Manage OpenSubsonic API keys
  help      Show this help message`)
}

func printServeUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: headunit serve

Start the OpenSubsonic HTTP server. Configuration is read from environment
variables (PORT, MUSIC_DIR, DATA_DIR, SCAN_INTERVAL, AUTH_SECRET_KEY,
UPLOADS_ENABLED, UPLOAD_MAX_FILE_BYTES).`)
}

func printUserUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: headunit user <subcommand> [flags]

Subcommands:
  create     Create a new user account
  passwd     Update a user's password
  list       List all users
  set-admin  Grant or revoke administrator privileges
  disable    Disable a user account
  enable     Enable a user account
  delete     Delete a user account and its API keys`)
}

func printAPIKeyUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: headunit apikey <subcommand> [flags]

Subcommands:
  create     Create a new OpenSubsonic API key for a user
  list       List API keys
  revoke     Revoke an API key`)
}

func printSubcommandUsage(w io.Writer, fs *flag.FlagSet, summary string) {
	fmt.Fprintf(w, "Usage: %s\n", summary)
	hasFlags := false
	fs.VisitAll(func(*flag.Flag) {
		hasFlags = true
	})
	if hasFlags {
		fmt.Fprintln(w, "\nFlags:")
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
}
