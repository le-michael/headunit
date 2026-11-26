package store

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned (typically wrapped) when a requested artist, album,
// or track does not exist. Callers should match it with errors.Is.
var ErrNotFound = errors.New("store: not found")

// MaxPageLimit is the upper bound enforced on paginated album and search queries.
const MaxPageLimit = 500

// AlbumListSort selects the ordering applied by Store.ListAlbums.
type AlbumListSort string

const (
	// AlbumSortAlphabeticalByName orders albums case-insensitively by album title.
	AlbumSortAlphabeticalByName AlbumListSort = "alphabeticalByName"

	// AlbumSortAlphabeticalByArtist orders albums case-insensitively by album
	// artist, then album title.
	AlbumSortAlphabeticalByArtist AlbumListSort = "alphabeticalByArtist"

	// AlbumSortNewest orders albums by most recently added track descending
	// (MAX(created_at) DESC), whereas AlbumSummary.CreatedAt reports the
	// earliest track timestamp (MIN(created_at)).
	AlbumSortNewest AlbumListSort = "newest"

	// AlbumSortRandom orders albums randomly via SQLite RANDOM().
	AlbumSortRandom AlbumListSort = "random"
)

// Track represents a single indexed audio file persisted in the tracks table.
type Track struct {
	// ID is the opaque stable track identifier ("tr_" + 32 hex chars).
	ID string

	// RelPath is the forward-slash path relative to the music root. Write
	// methods (UpsertTrack, DeleteByRelPath, ApplyScanBatch) require RelPath
	// to satisfy io/fs.ValidPath and not equal ".".
	RelPath string

	// FileSize is the audio file size in bytes.
	FileSize int64

	// MtimeNs is the file modification timestamp in Unix nanoseconds.
	MtimeNs int64

	// Title is the track title stored verbatim.
	Title string

	// Artist is the track-level artist stored verbatim.
	Artist string

	// AlbumArtist is the album-grouping artist stored verbatim.
	AlbumArtist string

	// ArtistID is the deterministic identifier derived from AlbumArtist.
	ArtistID string

	// Album is the album title stored verbatim.
	Album string

	// AlbumID is the deterministic identifier derived from AlbumArtist and Album.
	AlbumID string

	// TrackNum is the 1-based track number, or 0 when unset.
	TrackNum int

	// DiscNum is the 1-based disc number, or 0 when unset.
	DiscNum int

	// Year is the 4-digit release year, or 0 when unset.
	Year int

	// Genre is the track genre stored verbatim, or "" when unset.
	Genre string

	// DurationSec is the audio duration in whole seconds.
	DurationSec int

	// BitRate is the audio bitrate in kbps.
	BitRate int

	// CreatedAt is the UTC timestamp (second precision) when the track was first indexed.
	CreatedAt time.Time
}

// ArtistSummary holds aggregate browse metadata for a single album artist.
type ArtistSummary struct {
	// ID is the deterministic artist identifier.
	ID string

	// Name is the canonical display name (MIN(album_artist)) for the artist group.
	Name string

	// AlbumCount is the number of distinct albums under this artist.
	AlbumCount int
}

// AlbumSummary holds aggregate browse metadata for a single album.
type AlbumSummary struct {
	// ID is the deterministic album identifier.
	ID string

	// Name is the canonical display title (MIN(album)) for the album group.
	Name string

	// Artist is the canonical album artist name (MIN(album_artist)).
	Artist string

	// ArtistID is the deterministic artist identifier for the album.
	ArtistID string

	// SongCount is the number of tracks in the album.
	SongCount int

	// DurationSec is the combined duration of all tracks in whole seconds.
	DurationSec int

	// Year is the maximum non-zero track year in the album, or 0 if unset.
	Year int

	// Genre is the lexicographically smallest non-empty track genre, or "" if unset.
	Genre string

	// CreatedAt is the earliest UTC track creation timestamp in the album.
	CreatedAt time.Time
}

// TrackSnapshot holds minimal per-file sync metadata used by the library
// scanner to detect added, modified, and removed files.
type TrackSnapshot struct {
	// ID is the stable track identifier.
	ID string

	// RelPath is the forward-slash path relative to the music root.
	RelPath string

	// FileSize is the last indexed file size in bytes.
	FileSize int64

	// MtimeNs is the last indexed modification time in Unix nanoseconds.
	MtimeNs int64

	// CreatedAt is the UTC timestamp when the track was first indexed.
	CreatedAt time.Time
}

// AlbumListQuery configures sorting and pagination for Store.ListAlbums.
type AlbumListQuery struct {
	// Sort selects the album sort mode and must be one of the AlbumSort* constants.
	Sort AlbumListSort

	// Limit caps the number of albums returned. Values <= 0 return an empty
	// slice; values above MaxPageLimit are clamped to MaxPageLimit.
	Limit int

	// Offset skips the first Offset albums. Negative values are clamped to 0.
	Offset int
}

// Search3Query configures substring filtering and per-bucket pagination for
// Store.Search3.
type Search3Query struct {
	// Query is trimmed of surrounding whitespace and matched as a literal
	// ASCII case-insensitive substring (SQLite LIKE wildcards %, _, and \ are
	// escaped). Empty or whitespace-only values match all items, whereas
	// literal `""` is matched as two quote characters; HTTP callers must
	// normalize OpenSubsonic's query="" convention beforehand.
	Query string

	// ArtistCount caps matching artists returned (<= 0 skips querying; clamped to MaxPageLimit).
	ArtistCount int

	// ArtistOffset skips matching artists (negative values clamp to 0).
	ArtistOffset int

	// AlbumCount caps matching albums returned (<= 0 skips querying; clamped to MaxPageLimit).
	AlbumCount int

	// AlbumOffset skips matching albums (negative values clamp to 0).
	AlbumOffset int

	// SongCount caps matching tracks returned (<= 0 skips querying; clamped to MaxPageLimit).
	SongCount int

	// SongOffset skips matching tracks (negative values clamp to 0).
	SongOffset int
}

// Search3Result groups paginated artist, album, and track matches returned by
// Store.Search3. All three slices are always non-nil.
type Search3Result struct {
	// Artists contains matching artist summaries.
	Artists []ArtistSummary

	// Albums contains matching album summaries.
	Albums []AlbumSummary

	// Songs contains matching tracks.
	Songs []Track
}

// formatCreatedAt renders t as fixed-width UTC RFC3339 (YYYY-MM-DDTHH:MM:SSZ)
// so SQLite lexicographical MIN/MAX ordering matches chronological order.
func formatCreatedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func parseCreatedAt(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse created_at %q: %w", raw, err)
	}
	return t.UTC(), nil
}
