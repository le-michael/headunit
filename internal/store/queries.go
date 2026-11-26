package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	trackColumns = `id, rel_path, file_size, mtime_ns, title, artist, album_artist,
		artist_id, album, album_id, track_num, disc_num, year, genre,
		duration_sec, bit_rate, created_at`

	albumSummaryColumns = `album_id,
		MIN(album),
		MIN(album_artist),
		MIN(artist_id),
		COUNT(*),
		SUM(duration_sec),
		COALESCE(MAX(NULLIF(year, 0)), 0),
		COALESCE(MIN(NULLIF(genre, '')), ''),
		MIN(created_at)`
)

var likeReplacer = strings.NewReplacer(
	`\`, `\\`,
	`%`, `\%`,
	`_`, `\_`,
)

type rowScanner interface {
	Scan(dest ...any) error
}

// ListArtists returns all indexed album artists ordered case-insensitively by
// name with artist_id as a deterministic tie-breaker. The returned slice is
// always non-nil.
func (s *Store) ListArtists(ctx context.Context) ([]ArtistSummary, error) {
	const query = `
		SELECT artist_id, MIN(album_artist), COUNT(DISTINCT album_id)
		FROM tracks
		GROUP BY artist_id
		ORDER BY lower(MIN(album_artist)) ASC, artist_id ASC`

	rows, err := s.readDB.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list artists: %w", err)
	}
	defer rows.Close()

	artists := []ArtistSummary{}
	for rows.Next() {
		var a ArtistSummary
		if err := rows.Scan(&a.ID, &a.Name, &a.AlbumCount); err != nil {
			return nil, fmt.Errorf("store: scan artist: %w", err)
		}
		artists = append(artists, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate artists: %w", err)
	}
	return artists, nil
}

// GetArtist returns the ArtistSummary and ordered albums for artistID. It
// returns a wrapped ErrNotFound when no tracks belong to artistID.
func (s *Store) GetArtist(ctx context.Context, artistID string) (ArtistSummary, []AlbumSummary, error) {
	const query = `
		SELECT ` + albumSummaryColumns + `
		FROM tracks
		WHERE artist_id = ?
		GROUP BY album_id
		ORDER BY lower(MIN(album)) ASC, album_id ASC`

	rows, err := s.readDB.QueryContext(ctx, query, artistID)
	if err != nil {
		return ArtistSummary{}, nil, fmt.Errorf("store: get artist %q: %w", artistID, err)
	}
	defer rows.Close()

	var albums []AlbumSummary
	for rows.Next() {
		album, err := scanAlbumSummary(rows)
		if err != nil {
			return ArtistSummary{}, nil, fmt.Errorf("store: scan album for artist %q: %w", artistID, err)
		}
		albums = append(albums, album)
	}
	if err := rows.Err(); err != nil {
		return ArtistSummary{}, nil, fmt.Errorf("store: iterate albums for artist %q: %w", artistID, err)
	}
	if len(albums) == 0 {
		return ArtistSummary{}, nil, fmt.Errorf("store: artist %q: %w", artistID, ErrNotFound)
	}

	name := albums[0].Artist
	for _, a := range albums[1:] {
		if a.Artist < name {
			name = a.Artist
		}
	}
	artist := ArtistSummary{
		ID:         artistID,
		Name:       name,
		AlbumCount: len(albums),
	}
	return artist, albums, nil
}

// GetAlbum returns the AlbumSummary and ordered tracks for albumID. Tracks are
// ordered by disc number, track number, case-insensitive title, and track ID.
// It returns a wrapped ErrNotFound when no tracks belong to albumID.
func (s *Store) GetAlbum(ctx context.Context, albumID string) (AlbumSummary, []Track, error) {
	const query = `
		SELECT ` + trackColumns + `
		FROM tracks
		WHERE album_id = ?
		ORDER BY disc_num ASC, track_num ASC, lower(title) ASC, id ASC`

	rows, err := s.readDB.QueryContext(ctx, query, albumID)
	if err != nil {
		return AlbumSummary{}, nil, fmt.Errorf("store: get album %q: %w", albumID, err)
	}
	defer rows.Close()

	var tracks []Track
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return AlbumSummary{}, nil, fmt.Errorf("store: scan track for album %q: %w", albumID, err)
		}
		tracks = append(tracks, t)
	}
	if err := rows.Err(); err != nil {
		return AlbumSummary{}, nil, fmt.Errorf("store: iterate tracks for album %q: %w", albumID, err)
	}
	if len(tracks) == 0 {
		return AlbumSummary{}, nil, fmt.Errorf("store: album %q: %w", albumID, ErrNotFound)
	}

	return aggregateAlbumSummary(albumID, tracks), tracks, nil
}

// ListAlbums returns a paginated slice of albums ordered according to q.Sort.
// Invalid or empty q.Sort returns an error even when q.Limit <= 0. When
// q.Limit <= 0 and q.Sort is valid, ListAlbums returns a non-nil empty slice.
func (s *Store) ListAlbums(ctx context.Context, q AlbumListQuery) ([]AlbumSummary, error) {
	var orderBy string
	switch q.Sort {
	case AlbumSortAlphabeticalByName:
		orderBy = "lower(MIN(album)) ASC, album_id ASC"
	case AlbumSortAlphabeticalByArtist:
		orderBy = "lower(MIN(album_artist)) ASC, lower(MIN(album)) ASC, album_id ASC"
	case AlbumSortNewest:
		orderBy = "MAX(created_at) DESC, album_id ASC"
	case AlbumSortRandom:
		orderBy = "RANDOM()"
	default:
		return nil, fmt.Errorf("store: unsupported album list sort %q", q.Sort)
	}

	if q.Limit <= 0 {
		return []AlbumSummary{}, nil
	}

	query := `
		SELECT ` + albumSummaryColumns + `
		FROM tracks
		GROUP BY album_id
		ORDER BY ` + orderBy + `
		LIMIT ? OFFSET ?`

	rows, err := s.readDB.QueryContext(ctx, query, clampLimit(q.Limit), clampOffset(q.Offset))
	if err != nil {
		return nil, fmt.Errorf("store: list albums (%s): %w", q.Sort, err)
	}
	defer rows.Close()

	albums := []AlbumSummary{}
	for rows.Next() {
		album, err := scanAlbumSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan album: %w", err)
		}
		albums = append(albums, album)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate albums: %w", err)
	}
	return albums, nil
}

// GetTrack returns the single track identified by trackID, or a wrapped
// ErrNotFound when absent.
func (s *Store) GetTrack(ctx context.Context, trackID string) (*Track, error) {
	const query = `SELECT ` + trackColumns + ` FROM tracks WHERE id = ?`

	t, err := scanTrack(s.readDB.QueryRowContext(ctx, query, trackID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: track %q: %w", trackID, ErrNotFound)
		}
		return nil, fmt.Errorf("store: get track %q: %w", trackID, err)
	}
	return &t, nil
}

// Search3 queries artists, albums, and tracks matching q.Query within a single
// read-only WAL snapshot transaction. After trimming whitespace from q.Query,
// an empty string matches all items, while non-empty strings (including literal
// `""`) are escaped and matched as literal ASCII case-insensitive SQLite LIKE
// substrings (HTTP callers must normalize OpenSubsonic's query="" convention
// before calling Search3).
//
// Artists match AlbumArtist (ordered by case-insensitive name, ArtistID);
// albums match Album or AlbumArtist while preserving full album aggregates
// (ordered by case-insensitive title, AlbumID); songs match Title, Artist,
// AlbumArtist, or Album (ordered by case-insensitive title, ID). Buckets with
// a non-positive count are skipped and returned as non-nil empty slices.
func (s *Store) Search3(ctx context.Context, q Search3Query) (Search3Result, error) {
	res := Search3Result{
		Artists: []ArtistSummary{},
		Albums:  []AlbumSummary{},
		Songs:   []Track{},
	}
	if q.ArtistCount <= 0 && q.AlbumCount <= 0 && q.SongCount <= 0 {
		return res, nil
	}

	var pattern string
	if term := strings.TrimSpace(q.Query); term != "" {
		pattern = "%" + escapeLike(term) + "%"
	}

	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Search3Result{}, fmt.Errorf("store: begin search3 tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if q.ArtistCount > 0 {
		artists, err := searchArtists(ctx, tx, pattern, clampLimit(q.ArtistCount), clampOffset(q.ArtistOffset))
		if err != nil {
			return Search3Result{}, err
		}
		res.Artists = artists
	}
	if q.AlbumCount > 0 {
		albums, err := searchAlbums(ctx, tx, pattern, clampLimit(q.AlbumCount), clampOffset(q.AlbumOffset))
		if err != nil {
			return Search3Result{}, err
		}
		res.Albums = albums
	}
	if q.SongCount > 0 {
		songs, err := searchSongs(ctx, tx, pattern, clampLimit(q.SongCount), clampOffset(q.SongOffset))
		if err != nil {
			return Search3Result{}, err
		}
		res.Songs = songs
	}

	return res, nil
}

// TrackCount returns the total number of indexed tracks in the store.
func (s *Store) TrackCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.readDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM tracks").Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count tracks: %w", err)
	}
	return count, nil
}

func searchArtists(ctx context.Context, tx *sql.Tx, pattern string, limit, offset int) ([]ArtistSummary, error) {
	const (
		unfilteredQuery = `
			SELECT artist_id, MIN(album_artist), COUNT(DISTINCT album_id)
			FROM tracks
			GROUP BY artist_id
			ORDER BY lower(MIN(album_artist)) ASC, artist_id ASC
			LIMIT ? OFFSET ?`
		filteredQuery = `
			SELECT artist_id, MIN(album_artist), COUNT(DISTINCT album_id)
			FROM tracks
			GROUP BY artist_id
			HAVING MAX(CASE WHEN album_artist LIKE ? ESCAPE '\' THEN 1 ELSE 0 END) = 1
			ORDER BY lower(MIN(album_artist)) ASC, artist_id ASC
			LIMIT ? OFFSET ?`
	)

	var (
		rows *sql.Rows
		err  error
	)
	if pattern == "" {
		rows, err = tx.QueryContext(ctx, unfilteredQuery, limit, offset)
	} else {
		rows, err = tx.QueryContext(ctx, filteredQuery, pattern, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("store: search artists: %w", err)
	}
	defer rows.Close()

	artists := []ArtistSummary{}
	for rows.Next() {
		var a ArtistSummary
		if err := rows.Scan(&a.ID, &a.Name, &a.AlbumCount); err != nil {
			return nil, fmt.Errorf("store: scan searched artist: %w", err)
		}
		artists = append(artists, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate searched artists: %w", err)
	}
	return artists, nil
}

func searchAlbums(ctx context.Context, tx *sql.Tx, pattern string, limit, offset int) ([]AlbumSummary, error) {
	const (
		unfilteredQuery = `
			SELECT ` + albumSummaryColumns + `
			FROM tracks
			GROUP BY album_id
			ORDER BY lower(MIN(album)) ASC, album_id ASC
			LIMIT ? OFFSET ?`
		filteredQuery = `
			SELECT ` + albumSummaryColumns + `
			FROM tracks
			GROUP BY album_id
			HAVING MAX(CASE WHEN album LIKE ? ESCAPE '\' OR album_artist LIKE ? ESCAPE '\' THEN 1 ELSE 0 END) = 1
			ORDER BY lower(MIN(album)) ASC, album_id ASC
			LIMIT ? OFFSET ?`
	)

	var (
		rows *sql.Rows
		err  error
	)
	if pattern == "" {
		rows, err = tx.QueryContext(ctx, unfilteredQuery, limit, offset)
	} else {
		rows, err = tx.QueryContext(ctx, filteredQuery, pattern, pattern, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("store: search albums: %w", err)
	}
	defer rows.Close()

	albums := []AlbumSummary{}
	for rows.Next() {
		album, err := scanAlbumSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan searched album: %w", err)
		}
		albums = append(albums, album)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate searched albums: %w", err)
	}
	return albums, nil
}

func searchSongs(ctx context.Context, tx *sql.Tx, pattern string, limit, offset int) ([]Track, error) {
	const (
		unfilteredQuery = `
			SELECT ` + trackColumns + `
			FROM tracks
			ORDER BY lower(title) ASC, id ASC
			LIMIT ? OFFSET ?`
		filteredQuery = `
			SELECT ` + trackColumns + `
			FROM tracks
			WHERE title LIKE ? ESCAPE '\'
			   OR artist LIKE ? ESCAPE '\'
			   OR album_artist LIKE ? ESCAPE '\'
			   OR album LIKE ? ESCAPE '\'
			ORDER BY lower(title) ASC, id ASC
			LIMIT ? OFFSET ?`
	)

	var (
		rows *sql.Rows
		err  error
	)
	if pattern == "" {
		rows, err = tx.QueryContext(ctx, unfilteredQuery, limit, offset)
	} else {
		rows, err = tx.QueryContext(ctx, filteredQuery, pattern, pattern, pattern, pattern, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("store: search songs: %w", err)
	}
	defer rows.Close()

	songs := []Track{}
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan searched song: %w", err)
		}
		songs = append(songs, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate searched songs: %w", err)
	}
	return songs, nil
}

func scanTrack(s rowScanner) (Track, error) {
	var (
		t            Track
		createdAtRaw string
	)
	err := s.Scan(
		&t.ID,
		&t.RelPath,
		&t.FileSize,
		&t.MtimeNs,
		&t.Title,
		&t.Artist,
		&t.AlbumArtist,
		&t.ArtistID,
		&t.Album,
		&t.AlbumID,
		&t.TrackNum,
		&t.DiscNum,
		&t.Year,
		&t.Genre,
		&t.DurationSec,
		&t.BitRate,
		&createdAtRaw,
	)
	if err != nil {
		return Track{}, err
	}
	t.CreatedAt, err = parseCreatedAt(createdAtRaw)
	if err != nil {
		return Track{}, err
	}
	return t, nil
}

func scanAlbumSummary(s rowScanner) (AlbumSummary, error) {
	var (
		a            AlbumSummary
		createdAtRaw string
	)
	err := s.Scan(
		&a.ID,
		&a.Name,
		&a.Artist,
		&a.ArtistID,
		&a.SongCount,
		&a.DurationSec,
		&a.Year,
		&a.Genre,
		&createdAtRaw,
	)
	if err != nil {
		return AlbumSummary{}, err
	}
	a.CreatedAt, err = parseCreatedAt(createdAtRaw)
	if err != nil {
		return AlbumSummary{}, err
	}
	return a, nil
}

// aggregateAlbumSummary derives AlbumSummary from a non-empty album track slice
// using the exact same rules as SQL albumSummaryColumns so GetAlbum avoids a
// second round-trip while remaining identical to ListAlbums/GetArtist/Search3.
func aggregateAlbumSummary(albumID string, tracks []Track) AlbumSummary {
	first := tracks[0]
	summary := AlbumSummary{
		ID:        albumID,
		Name:      first.Album,
		Artist:    first.AlbumArtist,
		ArtistID:  first.ArtistID,
		SongCount: len(tracks),
		CreatedAt: first.CreatedAt,
	}

	var hasYear bool
	for _, t := range tracks {
		if t.Album < summary.Name {
			summary.Name = t.Album
		}
		if t.AlbumArtist < summary.Artist {
			summary.Artist = t.AlbumArtist
		}
		if t.ArtistID < summary.ArtistID {
			summary.ArtistID = t.ArtistID
		}
		summary.DurationSec += t.DurationSec
		if t.Year != 0 && (!hasYear || t.Year > summary.Year) {
			summary.Year = t.Year
			hasYear = true
		}
		if t.Genre != "" && (summary.Genre == "" || t.Genre < summary.Genre) {
			summary.Genre = t.Genre
		}
		if t.CreatedAt.Before(summary.CreatedAt) {
			summary.CreatedAt = t.CreatedAt
		}
	}
	return summary
}

func escapeLike(s string) string {
	return likeReplacer.Replace(s)
}

func clampLimit(limit int) int {
	if limit < 0 {
		return 0
	}
	if limit > MaxPageLimit {
		return MaxPageLimit
	}
	return limit
}

func clampOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}
