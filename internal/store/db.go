// Package store provides Headunit's embedded SQLite WAL track and authentication store.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	schemaVersion = 2

	// writeDSNQuery configures the writer pool in WAL mode with immediate write
	// transactions so concurrent readers never block and read-to-write lock
	// upgrades cannot trigger SQLITE_BUSY.
	writeDSNQuery = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate"

	// readDSNQuery enforces read-only connections on the reader pool so
	// accidental writes fail immediately at SQLite rather than contending with
	// the serialized writer.
	readDSNQuery = "_pragma=busy_timeout(5000)&_pragma=query_only(ON)"

	writeMaxConns = 1
	readMaxConns  = 4
)

type schemaStatement struct {
	name string
	sql  string
}

var schemaV1Statements = [...]schemaStatement{
	{
		name: "create tracks table",
		sql: `CREATE TABLE IF NOT EXISTS tracks (
			id           TEXT PRIMARY KEY,
			rel_path     TEXT NOT NULL UNIQUE,
			file_size    INTEGER NOT NULL,
			mtime_ns     INTEGER NOT NULL,
			title        TEXT NOT NULL,
			artist       TEXT NOT NULL,
			album_artist TEXT NOT NULL,
			artist_id    TEXT NOT NULL,
			album        TEXT NOT NULL,
			album_id     TEXT NOT NULL,
			track_num    INTEGER NOT NULL DEFAULT 0,
			disc_num     INTEGER NOT NULL DEFAULT 0,
			year         INTEGER NOT NULL DEFAULT 0,
			genre        TEXT NOT NULL DEFAULT '',
			duration_sec INTEGER NOT NULL DEFAULT 0,
			bit_rate     INTEGER NOT NULL DEFAULT 0,
			created_at   TEXT NOT NULL
		)`,
	},
	{
		name: "create artist index",
		sql:  `CREATE INDEX IF NOT EXISTS idx_tracks_artist_id ON tracks (artist_id, album_id, disc_num, track_num, title)`,
	},
	{
		name: "create album index",
		sql:  `CREATE INDEX IF NOT EXISTS idx_tracks_album_id ON tracks (album_id, disc_num, track_num, title)`,
	},
	{
		name: "set schema user_version",
		sql:  `PRAGMA user_version = 1`,
	},
}

var schemaV2Statements = [...]schemaStatement{
	{
		name: "create users table",
		sql: `CREATE TABLE IF NOT EXISTS users (
			id                  TEXT PRIMARY KEY,
			username            TEXT NOT NULL UNIQUE COLLATE NOCASE,
			password_ciphertext BLOB NOT NULL,
			is_admin            INTEGER NOT NULL DEFAULT 0,
			disabled            INTEGER NOT NULL DEFAULT 0,
			created_at          TEXT NOT NULL,
			updated_at          TEXT NOT NULL
		)`,
	},
	{
		name: "create api_keys table",
		sql: `CREATE TABLE IF NOT EXISTS api_keys (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name       TEXT NOT NULL DEFAULT '',
			key_prefix TEXT NOT NULL,
			key_hash   TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			revoked_at TEXT NOT NULL DEFAULT ''
		)`,
	},
	{
		name: "create api_keys user_id index",
		sql:  `CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys (user_id, created_at)`,
	},
	{
		name: "set schema user_version to 2",
		sql:  `PRAGMA user_version = 2`,
	},
}

// Store manages dedicated SQLite WAL writer and read-only reader pools over
// Headunit's embedded track and authentication database. All methods on Store
// are safe for concurrent use.
type Store struct {
	writeDB *sql.DB
	readDB  *sql.DB
}

// Open opens or creates the SQLite WAL database at the filesystem path dbPath,
// ensures its parent directory exists, verifies WAL mode, bootstraps or
// migrates the schema to version 2 atomically, and initializes dedicated writer
// and read-only reader pools. It rejects empty paths, directory paths, SQLite
// :memory:/file: URIs, and databases whose PRAGMA user_version is not 0, 1, or 2.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	absPath, err := resolveDBPath(dbPath)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("store: open %q: %w", absPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return nil, fmt.Errorf("store: create parent directory for %q: %w", absPath, err)
	}

	writeDB, err := sql.Open("sqlite", buildDSN(absPath, writeDSNQuery))
	if err != nil {
		return nil, fmt.Errorf("store: open write pool: %w", err)
	}
	writeDB.SetMaxOpenConns(writeMaxConns)
	writeDB.SetMaxIdleConns(writeMaxConns)

	if err := writeDB.PingContext(ctx); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: ping write pool: %w", err)
	}

	var journalMode string
	if err := writeDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: check journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: expected WAL journal_mode, got %q", journalMode)
	}

	if err := bootstrapSchema(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		return nil, err
	}

	readDB, err := sql.Open("sqlite", buildDSN(absPath, readDSNQuery))
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: open read pool: %w", err)
	}
	readDB.SetMaxOpenConns(readMaxConns)
	readDB.SetMaxIdleConns(readMaxConns)

	if err := readDB.PingContext(ctx); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, fmt.Errorf("store: ping read pool: %w", err)
	}

	return &Store{
		writeDB: writeDB,
		readDB:  readDB,
	}, nil
}

// Close closes the reader pool before the writer pool so SQLite can cleanly
// checkpoint WAL state on final connection close. Safe to call on a nil Store
// or multiple times.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var readErr, writeErr error
	if s.readDB != nil {
		readErr = s.readDB.Close()
	}
	if s.writeDB != nil {
		writeErr = s.writeDB.Close()
	}
	return errors.Join(readErr, writeErr)
}

func resolveDBPath(dbPath string) (string, error) {
	trimmed := strings.TrimSpace(dbPath)
	if trimmed == "" {
		return "", errors.New("store: database path must not be empty")
	}
	// Reject SQLite URI and in-memory specs because WAL mode and separate
	// writer/reader pools require an on-disk database file with sidecar files.
	lower := strings.ToLower(trimmed)
	if lower == ":memory:" || strings.HasPrefix(lower, "file:") {
		return "", fmt.Errorf("store: database path %q must be a filesystem path", dbPath)
	}
	// Reject trailing path separators before filepath.Abs strips them so a
	// non-existent directory path cannot silently create a file of that name.
	if strings.HasSuffix(dbPath, "/") || strings.HasSuffix(dbPath, string(filepath.Separator)) {
		return "", fmt.Errorf("store: database path %q must not end with a path separator", dbPath)
	}
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("store: resolve database path %q: %w", dbPath, err)
	}
	if st, err := os.Stat(absPath); err == nil && st.IsDir() {
		return "", fmt.Errorf("store: database path %q is a directory", absPath)
	}
	return absPath, nil
}

// buildDSN encodes absPath into a SQLite file: URI so reserved characters such
// as spaces, '?', '#', and '%' never corrupt DSN query parameters.
func buildDSN(absPath, rawQuery string) string {
	slashPath := filepath.ToSlash(absPath)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := url.URL{
		Scheme:   "file",
		Path:     slashPath,
		RawQuery: rawQuery,
	}
	return u.String()
}

func bootstrapSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin schema bootstrap tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	switch version {
	case 0:
		if err := execSchemaStatements(ctx, tx, schemaV1Statements[:]); err != nil {
			return err
		}
		if err := execSchemaStatements(ctx, tx, schemaV2Statements[:]); err != nil {
			return err
		}
	case 1:
		if err := execSchemaStatements(ctx, tx, schemaV2Statements[:]); err != nil {
			return err
		}
	case schemaVersion:
		// Already initialized at current schema version.
	default:
		return fmt.Errorf("store: unsupported schema version %d (want 0, 1, or %d)", version, schemaVersion)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit schema bootstrap tx: %w", err)
	}
	return nil
}

func execSchemaStatements(ctx context.Context, tx *sql.Tx, stmts []schemaStatement) error {
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt.sql); err != nil {
			return fmt.Errorf("store: %s: %w", stmt.name, err)
		}
	}
	return nil
}
