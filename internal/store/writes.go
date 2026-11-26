package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

const (
	upsertTrackSQL = `
		INSERT INTO tracks (
			id, rel_path, file_size, mtime_ns, title, artist, album_artist,
			artist_id, album, album_id, track_num, disc_num, year, genre,
			duration_sec, bit_rate, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rel_path) DO UPDATE SET
			file_size    = excluded.file_size,
			mtime_ns     = excluded.mtime_ns,
			title        = excluded.title,
			artist       = excluded.artist,
			album_artist = excluded.album_artist,
			artist_id    = excluded.artist_id,
			album        = excluded.album,
			album_id     = excluded.album_id,
			track_num    = excluded.track_num,
			disc_num     = excluded.disc_num,
			year         = excluded.year,
			genre        = excluded.genre,
			duration_sec = excluded.duration_sec,
			bit_rate     = excluded.bit_rate`

	deleteByRelPathSQL = `DELETE FROM tracks WHERE rel_path = ?`

	deleteByDirPrefixSQL = `DELETE FROM tracks WHERE rel_path >= ? AND rel_path < ?`
)

type preparedTrack struct {
	track     Track
	createdAt string
}

func (p preparedTrack) args() []any {
	return []any{
		p.track.ID,
		p.track.RelPath,
		p.track.FileSize,
		p.track.MtimeNs,
		p.track.Title,
		p.track.Artist,
		p.track.AlbumArtist,
		p.track.ArtistID,
		p.track.Album,
		p.track.AlbumID,
		p.track.TrackNum,
		p.track.DiscNum,
		p.track.Year,
		p.track.Genre,
		p.track.DurationSec,
		p.track.BitRate,
		p.createdAt,
	}
}

// LoadSnapshots returns a map from rel_path to TrackSnapshot for every indexed
// track. It queries writeDB so scanner diffing always observes prior writes
// immediately without WAL visibility lag. The returned map is always non-nil.
func (s *Store) LoadSnapshots(ctx context.Context) (map[string]TrackSnapshot, error) {
	const query = `SELECT id, rel_path, file_size, mtime_ns, created_at FROM tracks`

	rows, err := s.writeDB.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: load snapshots: %w", err)
	}
	defer rows.Close()

	snapshots := make(map[string]TrackSnapshot)
	for rows.Next() {
		var (
			snap         TrackSnapshot
			createdAtRaw string
		)
		if err := rows.Scan(&snap.ID, &snap.RelPath, &snap.FileSize, &snap.MtimeNs, &createdAtRaw); err != nil {
			return nil, fmt.Errorf("store: scan snapshot: %w", err)
		}
		snap.CreatedAt, err = parseCreatedAt(createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("store: parse snapshot created_at for %q: %w", snap.RelPath, err)
		}
		snapshots[snap.RelPath] = snap
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate snapshots: %w", err)
	}
	return snapshots, nil
}

// UpsertTrack inserts t or updates its mutable metadata when a row with the
// same RelPath already exists, preserving the existing row's ID and CreatedAt.
// UpsertTrack always derives ArtistID and AlbumID from AlbumArtist and Album,
// generates an ID when t.ID is empty, and never mutates *t.
func (s *Store) UpsertTrack(ctx context.Context, t *Track) error {
	if t == nil {
		return errors.New("store: track must not be nil")
	}
	p, err := prepareTrackForUpsert(*t)
	if err != nil {
		return err
	}
	if _, err := s.writeDB.ExecContext(ctx, upsertTrackSQL, p.args()...); err != nil {
		return fmt.Errorf("store: upsert track %q: %w", p.track.RelPath, err)
	}
	return nil
}

// DeleteByRelPath deletes the track at relPath if present. Deleting an absent
// valid path is a no-op.
func (s *Store) DeleteByRelPath(ctx context.Context, relPath string) error {
	if err := validateRelPath(relPath); err != nil {
		return err
	}
	if _, err := s.writeDB.ExecContext(ctx, deleteByRelPathSQL, relPath); err != nil {
		return fmt.Errorf("store: delete track %q: %w", relPath, err)
	}
	return nil
}

// DeleteByDirPrefix deletes all tracks strictly beneath the relative directory
// relDir ("<dir>/..."), leaving an exact-name track ("<dir>") and sibling
// prefixes untouched. relDir must be a non-empty, non-root relative directory
// path: OS separators are converted to "/", trailing "/" characters are
// trimmed, and empty inputs, leading "/", ".", "..", or other non-io/fs.ValidPath
// paths are rejected. Deleting an absent valid directory is a no-op.
func (s *Store) DeleteByDirPrefix(ctx context.Context, relDir string) error {
	prefix, err := parseDirPrefix(relDir)
	if err != nil {
		return err
	}
	// Because '0' (0x30) immediately follows '/' (0x2F) in UTF-8/ASCII, the
	// half-open range [prefix+"/", prefix+"0") uses the rel_path B-tree index
	// without matching sibling prefixes like "Rock2/" or LIKE wildcards.
	if _, err := s.writeDB.ExecContext(ctx, deleteByDirPrefixSQL, prefix+"/", prefix+"0"); err != nil {
		return fmt.Errorf("store: delete tracks under %q: %w", prefix, err)
	}
	return nil
}

// ApplyScanBatch atomically applies deleteRelPaths followed by upserts inside a
// single immediate write transaction. All paths and tracks are validated and
// prepared before the transaction begins; caller slices are never mutated.
func (s *Store) ApplyScanBatch(ctx context.Context, upserts []Track, deleteRelPaths []string) error {
	if len(upserts) == 0 && len(deleteRelPaths) == 0 {
		return nil
	}

	for _, relPath := range deleteRelPaths {
		if err := validateRelPath(relPath); err != nil {
			return err
		}
	}
	prepared := make([]preparedTrack, len(upserts))
	for i, t := range upserts {
		p, err := prepareTrackForUpsert(t)
		if err != nil {
			return err
		}
		prepared[i] = p
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin scan batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := execDeletes(ctx, tx, deleteRelPaths); err != nil {
		return err
	}
	if err := execUpserts(ctx, tx, prepared); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit scan batch tx: %w", err)
	}
	return nil
}

func execDeletes(ctx context.Context, tx *sql.Tx, relPaths []string) error {
	if len(relPaths) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, deleteByRelPathSQL)
	if err != nil {
		return fmt.Errorf("store: prepare batch delete: %w", err)
	}
	for _, relPath := range relPaths {
		if _, err := stmt.ExecContext(ctx, relPath); err != nil {
			_ = stmt.Close()
			return fmt.Errorf("store: batch delete %q: %w", relPath, err)
		}
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("store: close batch delete stmt: %w", err)
	}
	return nil
}

func execUpserts(ctx context.Context, tx *sql.Tx, tracks []preparedTrack) error {
	if len(tracks) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, upsertTrackSQL)
	if err != nil {
		return fmt.Errorf("store: prepare batch upsert: %w", err)
	}
	for _, p := range tracks {
		if _, err := stmt.ExecContext(ctx, p.args()...); err != nil {
			_ = stmt.Close()
			return fmt.Errorf("store: batch upsert %q: %w", p.track.RelPath, err)
		}
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("store: close batch upsert stmt: %w", err)
	}
	return nil
}

func prepareTrackForUpsert(t Track) (preparedTrack, error) {
	if err := validateRelPath(t.RelPath); err != nil {
		return preparedTrack{}, err
	}
	if t.ID == "" {
		id, err := NewTrackID()
		if err != nil {
			return preparedTrack{}, err
		}
		t.ID = id
	}
	t.ArtistID = ArtistID(t.AlbumArtist)
	t.AlbumID = AlbumID(t.AlbumArtist, t.Album)

	createdAt := t.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return preparedTrack{
		track:     t,
		createdAt: formatCreatedAt(createdAt),
	}, nil
}

func validateRelPath(relPath string) error {
	if !fs.ValidPath(relPath) || relPath == "." {
		return fmt.Errorf("store: invalid rel_path %q", relPath)
	}
	return nil
}

// parseDirPrefix converts OS path separators to forward slashes, rejects empty
// inputs or leading "/", trims trailing "/" only, and requires the resulting
// prefix to satisfy io/fs.ValidPath and not equal ".".
func parseDirPrefix(relDir string) (string, error) {
	s := filepath.ToSlash(relDir)
	if s == "" || strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("store: invalid directory prefix %q", relDir)
	}
	s = strings.TrimRight(s, "/")
	if s == "" || s == "." || !fs.ValidPath(s) {
		return "", fmt.Errorf("store: invalid directory prefix %q", relDir)
	}
	return s, nil
}
