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
			id, source, rel_path, file_size, mtime_ns, title, artist, album_artist,
			artist_id, album, album_id, track_num, disc_num, year, genre,
			duration_sec, bit_rate, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, rel_path) DO UPDATE SET
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

	deleteByRelPathSQL = `DELETE FROM tracks WHERE source = ? AND rel_path = ?`

	deleteByDirPrefixSQL = `DELETE FROM tracks WHERE source = ? AND rel_path >= ? AND rel_path < ?`
)

type preparedTrack struct {
	track     Track
	createdAt string
}

func (p preparedTrack) args() []any {
	return []any{
		p.track.ID,
		p.track.Source,
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
// track under source (empty source defaults to SourceMusic). It queries writeDB
// so scanner diffing always observes prior writes immediately without WAL
// visibility lag. The returned map is always non-nil.
func (s *Store) LoadSnapshots(ctx context.Context, source string) (map[string]TrackSnapshot, error) {
	src, err := normalizeSource(source)
	if err != nil {
		return nil, err
	}
	const query = `SELECT id, rel_path, file_size, mtime_ns, created_at FROM tracks WHERE source = ?`

	rows, err := s.writeDB.QueryContext(ctx, query, src)
	if err != nil {
		return nil, fmt.Errorf("store: load snapshots (%s): %w", src, err)
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
		return nil, fmt.Errorf("store: iterate snapshots (%s): %w", src, err)
	}
	return snapshots, nil
}

// UpsertTrack inserts t or updates its mutable metadata when a row with the
// same (Source, RelPath) already exists, preserving the existing row's ID,
// Source, and CreatedAt. Empty t.Source defaults to SourceMusic on the stored
// row. UpsertTrack always derives ArtistID and AlbumID from AlbumArtist and
// Album, generates an ID when t.ID is empty, and never mutates *t.
func (s *Store) UpsertTrack(ctx context.Context, t *Track) error {
	if t == nil {
		return errors.New("store: track must not be nil")
	}
	p, err := prepareTrackForUpsert(*t)
	if err != nil {
		return err
	}
	if _, err := s.writeDB.ExecContext(ctx, upsertTrackSQL, p.args()...); err != nil {
		return fmt.Errorf("store: upsert track (%s, %q): %w", p.track.Source, p.track.RelPath, err)
	}
	return nil
}

// GetTrackBySourceRelPath returns the track identified by (source, relPath),
// or a wrapped ErrNotFound when absent. Empty source defaults to SourceMusic.
// It queries writeDB so callers immediately observe prior upserts on the writer
// pool without WAL visibility lag.
func (s *Store) GetTrackBySourceRelPath(ctx context.Context, source, relPath string) (*Track, error) {
	src, err := normalizeSource(source)
	if err != nil {
		return nil, err
	}
	if err := validateRelPath(relPath); err != nil {
		return nil, err
	}

	const query = `SELECT ` + trackColumns + ` FROM tracks WHERE source = ? AND rel_path = ?`
	t, err := scanTrack(s.writeDB.QueryRowContext(ctx, query, src, relPath))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: track (%s, %q): %w", src, relPath, ErrNotFound)
		}
		return nil, fmt.Errorf("store: get track (%s, %q): %w", src, relPath, err)
	}
	return &t, nil
}

// DeleteByRelPath deletes the track at (source, relPath) if present. Empty
// source defaults to SourceMusic. Deleting an absent valid path is a no-op.
func (s *Store) DeleteByRelPath(ctx context.Context, source, relPath string) error {
	src, err := normalizeSource(source)
	if err != nil {
		return err
	}
	if err := validateRelPath(relPath); err != nil {
		return err
	}
	if _, err := s.writeDB.ExecContext(ctx, deleteByRelPathSQL, src, relPath); err != nil {
		return fmt.Errorf("store: delete track (%s, %q): %w", src, relPath, err)
	}
	return nil
}

// DeleteByDirPrefix deletes all tracks under source strictly beneath the
// relative directory relDir ("<dir>/..."), leaving an exact-name track
// ("<dir>"), sibling prefixes, and tracks under other sources untouched. Empty
// source defaults to SourceMusic. relDir must be a non-empty, non-root relative
// directory path: OS separators are converted to "/", trailing "/" characters
// are trimmed, and empty inputs, leading "/", ".", "..", or other
// non-io/fs.ValidPath paths are rejected. Deleting an absent valid directory is
// a no-op.
func (s *Store) DeleteByDirPrefix(ctx context.Context, source, relDir string) error {
	src, err := normalizeSource(source)
	if err != nil {
		return err
	}
	prefix, err := parseDirPrefix(relDir)
	if err != nil {
		return err
	}
	// Because '0' (0x30) immediately follows '/' (0x2F) in UTF-8/ASCII, the
	// half-open range [prefix+"/", prefix+"0") uses the (source, rel_path) B-tree
	// index without matching sibling prefixes like "Rock2/" or LIKE wildcards.
	if _, err := s.writeDB.ExecContext(ctx, deleteByDirPrefixSQL, src, prefix+"/", prefix+"0"); err != nil {
		return fmt.Errorf("store: delete tracks under (%s, %q): %w", src, prefix, err)
	}
	return nil
}

// ApplyScanBatch atomically applies deleteRelPaths followed by upserts under
// source inside a single immediate write transaction. Empty source defaults to
// SourceMusic. All sources, paths, and tracks are validated and prepared before
// the transaction begins; caller slices are never mutated.
func (s *Store) ApplyScanBatch(ctx context.Context, source string, upserts []Track, deleteRelPaths []string) error {
	src, err := normalizeSource(source)
	if err != nil {
		return err
	}
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
		if t.Source == "" {
			t.Source = src
		} else if t.Source != src {
			return fmt.Errorf("store: batch track %q source %q does not match batch source %q", t.RelPath, t.Source, src)
		}
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

	if err := execDeletes(ctx, tx, src, deleteRelPaths); err != nil {
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

func execDeletes(ctx context.Context, tx *sql.Tx, source string, relPaths []string) error {
	if len(relPaths) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, deleteByRelPathSQL)
	if err != nil {
		return fmt.Errorf("store: prepare batch delete: %w", err)
	}
	for _, relPath := range relPaths {
		if _, err := stmt.ExecContext(ctx, source, relPath); err != nil {
			_ = stmt.Close()
			return fmt.Errorf("store: batch delete (%s, %q): %w", source, relPath, err)
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
			return fmt.Errorf("store: batch upsert (%s, %q): %w", p.track.Source, p.track.RelPath, err)
		}
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("store: close batch upsert stmt: %w", err)
	}
	return nil
}

func prepareTrackForUpsert(t Track) (preparedTrack, error) {
	src, err := normalizeSource(t.Source)
	if err != nil {
		return preparedTrack{}, err
	}
	t.Source = src
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

// normalizeSource defaults an empty source to SourceMusic and rejects any
// value other than SourceMusic or SourceUploads.
func normalizeSource(source string) (string, error) {
	switch source {
	case "":
		return SourceMusic, nil
	case SourceMusic, SourceUploads:
		return source, nil
	default:
		return "", fmt.Errorf("store: invalid track source %q", source)
	}
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
