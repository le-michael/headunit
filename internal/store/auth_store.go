package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	userColumns = `id, username, password_ciphertext, is_admin, disabled, created_at, updated_at`

	apiKeyJoinColumns = `k.id, k.user_id, u.username, k.name, k.key_prefix, k.key_hash, k.created_at, k.revoked_at`

	insertUserSQL = `
		INSERT INTO users (
			id, username, password_ciphertext, is_admin, disabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`

	updateUserPasswordSQL = `UPDATE users SET password_ciphertext = ?, updated_at = ? WHERE username = ?`
	updateUserAdminSQL    = `UPDATE users SET is_admin = ?, updated_at = ? WHERE username = ?`
	updateUserDisabledSQL = `UPDATE users SET disabled = ?, updated_at = ? WHERE username = ?`
	deleteUserSQL         = `DELETE FROM users WHERE username = ?`

	insertAPIKeySQL = `
		INSERT INTO api_keys (
			id, user_id, name, key_prefix, key_hash, created_at, revoked_at
		) VALUES (?, ?, ?, ?, ?, ?, '')`

	revokeAPIKeySQL = `
		UPDATE api_keys
		SET revoked_at = CASE WHEN revoked_at = '' THEN ? ELSE revoked_at END
		WHERE id = ?`
)

// GetUserByUsername returns the user matching username case-insensitively.
// It returns a wrapped ErrNotFound when no matching user exists.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	const query = `SELECT ` + userColumns + ` FROM users WHERE username = ?`

	u, err := scanUser(s.readDB.QueryRowContext(ctx, query, username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: user %q: %w", username, ErrNotFound)
		}
		return nil, fmt.Errorf("store: get user %q: %w", username, err)
	}
	return &u, nil
}

// ListUsers returns all users ordered case-insensitively by username with id as
// a deterministic tie-breaker. The returned slice is always non-nil.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	const query = `
		SELECT ` + userColumns + `
		FROM users
		ORDER BY lower(username) ASC, id ASC`

	rows, err := s.readDB.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()

	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate users: %w", err)
	}
	return users, nil
}

// LookupAPIKeyByHash returns the API key matching keyHash along with the owning
// user's username, admin status, and disabled status. Active, revoked, and
// disabled-user keys are all returned so callers can enforce exact auth errors.
// It returns a wrapped ErrNotFound when no matching key exists.
func (s *Store) LookupAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyLookup, error) {
	const query = `
		SELECT ` + apiKeyJoinColumns + `, u.is_admin, u.disabled
		FROM api_keys k
		INNER JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ?`

	lookup, err := scanAPIKeyLookup(s.readDB.QueryRowContext(ctx, query, keyHash))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: lookup api key: %w", ErrNotFound)
		}
		return nil, fmt.Errorf("store: lookup api key: %w", err)
	}
	return &lookup, nil
}

// ListAPIKeys returns API keys ordered deterministically. When username is
// empty, it returns all keys ordered by lower(username), created_at, id. When
// username is non-empty, it filters case-insensitively by username and orders
// by created_at, id. The returned slice is always non-nil.
func (s *Store) ListAPIKeys(ctx context.Context, username string) ([]APIKey, error) {
	const (
		allQuery = `
			SELECT ` + apiKeyJoinColumns + `
			FROM api_keys k
			INNER JOIN users u ON u.id = k.user_id
			ORDER BY lower(u.username) ASC, k.created_at ASC, k.id ASC`

		userQuery = `
			SELECT ` + apiKeyJoinColumns + `
			FROM api_keys k
			INNER JOIN users u ON u.id = k.user_id
			WHERE u.username = ?
			ORDER BY k.created_at ASC, k.id ASC`
	)

	var (
		rows *sql.Rows
		err  error
	)
	if username == "" {
		rows, err = s.readDB.QueryContext(ctx, allQuery)
	} else {
		rows, err = s.readDB.QueryContext(ctx, userQuery, username)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()

	keys := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan api key: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate api keys: %w", err)
	}
	return keys, nil
}

// CreateUser persists a new user account, generating an ID and defaulting zero
// timestamps when omitted. Duplicate usernames (case-insensitive) or IDs return
// a wrapped ErrAlreadyExists.
func (s *Store) CreateUser(ctx context.Context, u User) (*User, error) {
	if err := validateUsername(u.Username); err != nil {
		return nil, err
	}
	if len(u.PasswordCiphertext) == 0 {
		return nil, errors.New("store: password ciphertext must not be empty")
	}

	id := u.ID
	if id == "" {
		var err error
		id, err = NewUserID()
		if err != nil {
			return nil, err
		}
	}

	createdAt := u.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	createdAt = createdAt.UTC().Truncate(time.Second)

	updatedAt := u.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	updatedAt = updatedAt.UTC().Truncate(time.Second)

	ciphertext := bytes.Clone(u.PasswordCiphertext)
	_, err := s.writeDB.ExecContext(
		ctx,
		insertUserSQL,
		id,
		u.Username,
		ciphertext,
		boolToInt(u.IsAdmin),
		boolToInt(u.Disabled),
		formatCreatedAt(createdAt),
		formatCreatedAt(updatedAt),
	)
	if err != nil {
		if code, ok := sqliteErrCode(err); ok {
			switch code {
			case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
				return nil, fmt.Errorf("store: create user %q: %w", u.Username, ErrAlreadyExists)
			}
		}
		return nil, fmt.Errorf("store: create user %q: %w", u.Username, err)
	}

	return &User{
		ID:                 id,
		Username:           u.Username,
		PasswordCiphertext: bytes.Clone(ciphertext),
		IsAdmin:            u.IsAdmin,
		Disabled:           u.Disabled,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
	}, nil
}

// UpdateUserPassword replaces the encrypted password and updates updated_at for
// the user matching username case-insensitively. It returns a wrapped
// ErrNotFound when no matching user exists.
func (s *Store) UpdateUserPassword(ctx context.Context, username string, passwordCiphertext []byte) error {
	if len(passwordCiphertext) == 0 {
		return errors.New("store: password ciphertext must not be empty")
	}
	return s.execUserMutation(
		ctx,
		updateUserPasswordSQL,
		"update password",
		username,
		bytes.Clone(passwordCiphertext),
		formatCreatedAt(time.Now().UTC()),
		username,
	)
}

// SetUserAdmin sets the is_admin flag and updates updated_at for the user
// matching username case-insensitively. It returns a wrapped ErrNotFound when
// no matching user exists.
func (s *Store) SetUserAdmin(ctx context.Context, username string, isAdmin bool) error {
	return s.execUserMutation(
		ctx,
		updateUserAdminSQL,
		"set admin",
		username,
		boolToInt(isAdmin),
		formatCreatedAt(time.Now().UTC()),
		username,
	)
}

// SetUserDisabled sets the disabled flag and updates updated_at for the user
// matching username case-insensitively. It returns a wrapped ErrNotFound when
// no matching user exists.
func (s *Store) SetUserDisabled(ctx context.Context, username string, disabled bool) error {
	return s.execUserMutation(
		ctx,
		updateUserDisabledSQL,
		"set disabled",
		username,
		boolToInt(disabled),
		formatCreatedAt(time.Now().UTC()),
		username,
	)
}

// DeleteUser removes the user matching username case-insensitively along with
// any owned API keys via ON DELETE CASCADE. It returns a wrapped ErrNotFound
// when no matching user exists.
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	return s.execUserMutation(
		ctx,
		deleteUserSQL,
		"delete",
		username,
		username,
	)
}

// CreateAPIKey persists a new active API key record, generating an ID and
// defaulting zero CreatedAt when omitted. Duplicate IDs or key hashes return a
// wrapped ErrAlreadyExists, and a nonexistent UserID returns a wrapped
// ErrNotFound.
func (s *Store) CreateAPIKey(ctx context.Context, k APIKey) (*APIKey, error) {
	if k.UserID == "" {
		return nil, errors.New("store: api key user_id must not be empty")
	}
	if k.KeyPrefix == "" {
		return nil, errors.New("store: api key prefix must not be empty")
	}
	if k.KeyHash == "" {
		return nil, errors.New("store: api key hash must not be empty")
	}
	if !k.RevokedAt.IsZero() {
		return nil, errors.New("store: new api key must not have revoked_at set")
	}

	id := k.ID
	if id == "" {
		var err error
		id, err = NewAPIKeyID()
		if err != nil {
			return nil, err
		}
	}

	createdAt := k.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	createdAt = createdAt.UTC().Truncate(time.Second)

	_, err := s.writeDB.ExecContext(
		ctx,
		insertAPIKeySQL,
		id,
		k.UserID,
		k.Name,
		k.KeyPrefix,
		k.KeyHash,
		formatCreatedAt(createdAt),
	)
	if err != nil {
		if code, ok := sqliteErrCode(err); ok {
			switch code {
			case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
				return nil, fmt.Errorf("store: create api key: %w", ErrAlreadyExists)
			case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
				return nil, fmt.Errorf("store: create api key for user %q: %w", k.UserID, ErrNotFound)
			}
		}
		return nil, fmt.Errorf("store: create api key: %w", err)
	}

	return &APIKey{
		ID:        id,
		UserID:    k.UserID,
		Name:      k.Name,
		KeyPrefix: k.KeyPrefix,
		KeyHash:   k.KeyHash,
		CreatedAt: createdAt,
	}, nil
}

// RevokeAPIKey marks keyID revoked if it is not already revoked, preserving the
// initial revocation timestamp on repeat calls. It returns a wrapped
// ErrNotFound when no matching key exists.
func (s *Store) RevokeAPIKey(ctx context.Context, keyID string) error {
	if keyID == "" {
		return errors.New("store: api key id must not be empty")
	}
	res, err := s.writeDB.ExecContext(ctx, revokeAPIKeySQL, formatCreatedAt(time.Now().UTC()), keyID)
	if err != nil {
		return fmt.Errorf("store: revoke api key %q: %w", keyID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: revoke api key %q rows affected: %w", keyID, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: api key %q: %w", keyID, ErrNotFound)
	}
	return nil
}

func (s *Store) execUserMutation(ctx context.Context, query, op, username string, args ...any) error {
	if username == "" {
		return errors.New("store: username must not be empty")
	}
	res, err := s.writeDB.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: %s user %q: %w", op, username, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s user %q rows affected: %w", op, username, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: user %q: %w", username, ErrNotFound)
	}
	return nil
}

func validateUsername(username string) error {
	if username == "" {
		return errors.New("store: username must not be empty")
	}
	if strings.TrimSpace(username) != username {
		return fmt.Errorf("store: username %q must not have leading or trailing whitespace", username)
	}
	for i := range len(username) {
		b := username[i]
		if b < 0x20 || b == 0x7f {
			return errors.New("store: username must not contain control characters")
		}
	}
	return nil
}

func scanUser(scanner rowScanner) (User, error) {
	var (
		u            User
		ciphertext   []byte
		isAdminInt   int
		disabledInt  int
		createdAtRaw string
		updatedAtRaw string
	)
	if err := scanner.Scan(
		&u.ID,
		&u.Username,
		&ciphertext,
		&isAdminInt,
		&disabledInt,
		&createdAtRaw,
		&updatedAtRaw,
	); err != nil {
		return User{}, err
	}
	createdAt, err := parseTimestamp("created_at", createdAtRaw)
	if err != nil {
		return User{}, err
	}
	updatedAt, err := parseTimestamp("updated_at", updatedAtRaw)
	if err != nil {
		return User{}, err
	}
	u.PasswordCiphertext = bytes.Clone(ciphertext)
	u.IsAdmin = isAdminInt != 0
	u.Disabled = disabledInt != 0
	u.CreatedAt = createdAt
	u.UpdatedAt = updatedAt
	return u, nil
}

func scanAPIKey(scanner rowScanner) (APIKey, error) {
	var (
		k            APIKey
		createdAtRaw string
		revokedAtRaw string
	)
	if err := scanner.Scan(
		&k.ID,
		&k.UserID,
		&k.Username,
		&k.Name,
		&k.KeyPrefix,
		&k.KeyHash,
		&createdAtRaw,
		&revokedAtRaw,
	); err != nil {
		return APIKey{}, err
	}
	return finalizeAPIKey(k, createdAtRaw, revokedAtRaw)
}

func scanAPIKeyLookup(scanner rowScanner) (APIKeyLookup, error) {
	var (
		res          APIKeyLookup
		createdAtRaw string
		revokedAtRaw string
		isAdminInt   int
		disabledInt  int
	)
	if err := scanner.Scan(
		&res.Key.ID,
		&res.Key.UserID,
		&res.Key.Username,
		&res.Key.Name,
		&res.Key.KeyPrefix,
		&res.Key.KeyHash,
		&createdAtRaw,
		&revokedAtRaw,
		&isAdminInt,
		&disabledInt,
	); err != nil {
		return APIKeyLookup{}, err
	}
	k, err := finalizeAPIKey(res.Key, createdAtRaw, revokedAtRaw)
	if err != nil {
		return APIKeyLookup{}, err
	}
	res.Key = k
	res.UserIsAdmin = isAdminInt != 0
	res.UserDisabled = disabledInt != 0
	return res, nil
}

func finalizeAPIKey(k APIKey, createdAtRaw, revokedAtRaw string) (APIKey, error) {
	createdAt, err := parseTimestamp("created_at", createdAtRaw)
	if err != nil {
		return APIKey{}, err
	}
	revokedAt, err := parseOptionalTimestamp("revoked_at", revokedAtRaw)
	if err != nil {
		return APIKey{}, err
	}
	k.CreatedAt = createdAt
	k.RevokedAt = revokedAt
	return k, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func sqliteErrCode(err error) (int, bool) {
	var sqlErr *sqlite.Error
	if errors.As(err, &sqlErr) {
		return sqlErr.Code(), true
	}
	return 0, false
}
