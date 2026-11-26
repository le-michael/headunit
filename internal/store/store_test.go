package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "headunit.db")
	s, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open(%q) unexpected error: %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Store.Close() unexpected error: %v", err)
		}
	})
	return s
}

func makeTrack(relPath, title, artist, albumArtist, album string, createdAt time.Time) Track {
	return Track{
		RelPath:     relPath,
		FileSize:    1024,
		MtimeNs:     createdAt.UnixNano(),
		Title:       title,
		Artist:      artist,
		AlbumArtist: albumArtist,
		Album:       album,
		TrackNum:    1,
		DiscNum:     1,
		Year:        2020,
		Genre:       "Electronic",
		DurationSec: 180,
		BitRate:     320,
		CreatedAt:   createdAt,
	}
}

func mustUpsert(t *testing.T, s *Store, tr Track) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &tr); err != nil {
		t.Fatalf("UpsertTrack(%q) unexpected error: %v", tr.RelPath, err)
	}
}

func assertHexLower(t *testing.T, s string) {
	t.Helper()
	for i := range len(s) {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("got non-lowercase-hex char %q in %q", c, s)
		}
	}
}

func TestOpenAndClose(t *testing.T) {
	ctx := context.Background()

	t.Run("creates parent dirs, bootstraps schema and pragmas", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "nested", "data", "headunit.db")
		s, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("Open(%q) unexpected error: %v", dbPath, err)
		}
		t.Cleanup(func() { _ = s.Close() })

		// Verify tracks table and both composite indexes exist in sqlite_master.
		wantObjects := map[string]string{
			"tracks":               "table",
			"idx_tracks_artist_id": "index",
			"idx_tracks_album_id":  "index",
		}
		for name, wantType := range wantObjects {
			var gotType string
			err := s.readDB.QueryRowContext(
				ctx,
				"SELECT type FROM sqlite_master WHERE name = ?",
				name,
			).Scan(&gotType)
			if err != nil {
				t.Fatalf("sqlite_master lookup for %q failed: %v", name, err)
			}
			if gotType != wantType {
				t.Errorf("sqlite_master[%q] type = %q, want %q", name, gotType, wantType)
			}
		}

		// Verify writer pool pragmas.
		var journalMode string
		if err := s.writeDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("writeDB PRAGMA journal_mode: %v", err)
		}
		if !strings.EqualFold(journalMode, "wal") {
			t.Errorf("writeDB journal_mode = %q, want %q", journalMode, "wal")
		}

		writeIntPragmas := []struct {
			pragma string
			want   int
		}{
			{"PRAGMA synchronous", 1}, // NORMAL
			{"PRAGMA foreign_keys", 1},
			{"PRAGMA busy_timeout", 5000},
			{"PRAGMA user_version", 1},
		}
		for _, tc := range writeIntPragmas {
			var got int
			if err := s.writeDB.QueryRowContext(ctx, tc.pragma).Scan(&got); err != nil {
				t.Fatalf("writeDB %s: %v", tc.pragma, err)
			}
			if got != tc.want {
				t.Errorf("writeDB %s = %d, want %d", tc.pragma, got, tc.want)
			}
		}

		// Verify reader pool pragmas.
		readIntPragmas := []struct {
			pragma string
			want   int
		}{
			{"PRAGMA query_only", 1},
			{"PRAGMA busy_timeout", 5000},
		}
		for _, tc := range readIntPragmas {
			var got int
			if err := s.readDB.QueryRowContext(ctx, tc.pragma).Scan(&got); err != nil {
				t.Fatalf("readDB %s: %v", tc.pragma, err)
			}
			if got != tc.want {
				t.Errorf("readDB %s = %d, want %d", tc.pragma, got, tc.want)
			}
		}

		// Verify readDB rejects INSERT and DELETE mutations.
		tr := makeTrack("a.mp3", "Title", "Artist", "Artist", "Album", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		tr.ID = "tr_seed01"
		mustUpsert(t, s, tr)

		if _, err := s.readDB.ExecContext(ctx,
			`INSERT INTO tracks (id, rel_path, file_size, mtime_ns, title, artist, album_artist, artist_id, album, album_id, created_at)
			 VALUES ('tr_bad', 'bad.mp3', 1, 1, 'T', 'A', 'A', 'ar_1', 'Al', 'al_1', '2025-01-01T00:00:00Z')`,
		); err == nil {
			t.Errorf("readDB INSERT unexpectedly succeeded; want error")
		}
		if _, err := s.readDB.ExecContext(ctx, `DELETE FROM tracks WHERE id = ?`, tr.ID); err == nil {
			t.Errorf("readDB DELETE unexpectedly succeeded; want error")
		}
	})

	t.Run("special characters in database path", func(t *testing.T) {
		specialPath := filepath.Join(t.TempDir(), "my dir #1 ? 100%", "headunit.db")
		s, err := Open(ctx, specialPath)
		if err != nil {
			t.Fatalf("Open(%q) unexpected error: %v", specialPath, err)
		}
		t.Cleanup(func() { _ = s.Close() })

		if _, err := os.Stat(specialPath); err != nil {
			t.Fatalf("os.Stat(%q) failed (URI encoding mismatch): %v", specialPath, err)
		}

		ts := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
		tr := makeTrack("special.flac", "Song", "Artist", "Artist", "Album", ts)
		tr.ID = "tr_special"
		mustUpsert(t, s, tr)

		got, err := s.GetTrack(ctx, tr.ID)
		if err != nil {
			t.Fatalf("GetTrack(%q) unexpected error: %v", tr.ID, err)
		}
		if got.RelPath != tr.RelPath {
			t.Errorf("GetTrack().RelPath = %q, want %q", got.RelPath, tr.RelPath)
		}
	})

	t.Run("idempotent reopen preserves data and schema version", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "reopen.db")
		s1, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("first Open() error: %v", err)
		}
		ts := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
		tr := makeTrack("keep.mp3", "Keep Me", "Artist", "Artist", "Album", ts)
		tr.ID = "tr_keep"
		mustUpsert(t, s1, tr)
		if err := s1.Close(); err != nil {
			t.Fatalf("first Close() error: %v", err)
		}

		s2, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("second Open() error: %v", err)
		}
		t.Cleanup(func() { _ = s2.Close() })

		got, err := s2.GetTrack(ctx, tr.ID)
		if err != nil {
			t.Fatalf("GetTrack after reopen error: %v", err)
		}
		if got.Title != "Keep Me" {
			t.Errorf("GetTrack().Title = %q, want %q", got.Title, "Keep Me")
		}
	})

	t.Run("rejects invalid paths, URIs, directories, and canceled ctx", func(t *testing.T) {
		existingDir := t.TempDir()
		nonExistentDir := filepath.Join(t.TempDir(), "nonexistent")
		badPaths := []struct {
			name string
			path string
		}{
			{"empty", ""},
			{"whitespace", "   \t\n  "},
			{"memory", ":memory:"},
			{"memory uppercase with spaces", "  :MEMORY:  "},
			{"file URI lowercase", "file:headunit.db"},
			{"file URI uppercase", "FILE:///tmp/headunit.db"},
			{"existing directory", existingDir},
			{"trailing slash non-existent directory", nonExistentDir + "/"},
		}
		for _, tc := range badPaths {
			t.Run(tc.name, func(t *testing.T) {
				s, err := Open(ctx, tc.path)
				if err == nil {
					_ = s.Close()
					t.Fatalf("Open(%q) = nil error, want error", tc.path)
				}
			})
		}
		if _, err := os.Stat(nonExistentDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Stat(%q) = %v, want os.ErrNotExist (must not create file from trailing-slash path)", nonExistentDir, err)
		}

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		dbPath := filepath.Join(t.TempDir(), "canceled", "headunit.db")
		s, err := Open(canceledCtx, dbPath)
		if err == nil {
			_ = s.Close()
			t.Fatalf("Open(canceledCtx) = nil error, want error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Open(canceledCtx) err = %v, want wrapped context.Canceled", err)
		}
	})

	t.Run("rejects unsupported schema versions", func(t *testing.T) {
		for _, badVersion := range []int{-1, 2} {
			t.Run(fmt.Sprintf("user_version_%d", badVersion), func(t *testing.T) {
				dbPath := filepath.Join(t.TempDir(), "badver.db")
				s, err := Open(ctx, dbPath)
				if err != nil {
					t.Fatalf("initial Open() error: %v", err)
				}
				if err := s.Close(); err != nil {
					t.Fatalf("initial Close() error: %v", err)
				}

				rawDB, err := sql.Open("sqlite", buildDSN(dbPath, writeDSNQuery))
				if err != nil {
					t.Fatalf("raw sql.Open() error: %v", err)
				}
				if _, err := rawDB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", badVersion)); err != nil {
					_ = rawDB.Close()
					t.Fatalf("set PRAGMA user_version = %d: %v", badVersion, err)
				}
				if err := rawDB.Close(); err != nil {
					t.Fatalf("raw Close() error: %v", err)
				}

				reopened, err := Open(ctx, dbPath)
				if err == nil {
					_ = reopened.Close()
					t.Fatalf("Open() with user_version=%d unexpectedly succeeded", badVersion)
				}
			})
		}
	})

	t.Run("nil close and double close are safe", func(t *testing.T) {
		var nilStore *Store
		if err := nilStore.Close(); err != nil {
			t.Errorf("(*Store)(nil).Close() = %v, want nil", err)
		}

		dbPath := filepath.Join(t.TempDir(), "double_close.db")
		s, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("Open() error: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("first Close() = %v, want nil", err)
		}
		if err := s.Close(); err != nil {
			t.Errorf("second Close() = %v, want nil", err)
		}
	})
}

func TestIDs(t *testing.T) {
	t.Run("ArtistID format, determinism, and normalization", func(t *testing.T) {
		id1 := ArtistID("Daft Punk")
		id2 := ArtistID("  daft punk\t")
		id3 := ArtistID("DAFT PUNK")
		other := ArtistID("Justice")

		if !strings.HasPrefix(id1, "ar_") || len(id1) != 19 {
			t.Fatalf("ArtistID() = %q (len %d), want prefix ar_ and len 19", id1, len(id1))
		}
		assertHexLower(t, strings.TrimPrefix(id1, "ar_"))

		if id1 != id2 || id1 != id3 {
			t.Errorf("ArtistID normalization mismatch: %q, %q, %q", id1, id2, id3)
		}
		if id1 == other {
			t.Errorf("ArtistID collision between distinct artists: %q", id1)
		}
	})

	t.Run("AlbumID format, normalization, and boundary separation", func(t *testing.T) {
		id1 := AlbumID("Daft Punk", "Discovery")
		id2 := AlbumID(" daft punk ", "  DISCOVERY\n")
		diffArtist := AlbumID("Other Artist", "Discovery")
		boundaryA := AlbumID("a", "bc")
		boundaryB := AlbumID("ab", "c")

		if !strings.HasPrefix(id1, "al_") || len(id1) != 19 {
			t.Fatalf("AlbumID() = %q (len %d), want prefix al_ and len 19", id1, len(id1))
		}
		assertHexLower(t, strings.TrimPrefix(id1, "al_"))

		if id1 != id2 {
			t.Errorf("AlbumID normalization mismatch: %q != %q", id1, id2)
		}
		if id1 == diffArtist {
			t.Errorf("AlbumID same album under different artists collided: %q", id1)
		}
		if boundaryA == boundaryB {
			t.Errorf("AlbumID boundary collision between (a,bc) and (ab,c): %q", boundaryA)
		}
	})

	t.Run("NewTrackID format and uniqueness", func(t *testing.T) {
		seen := make(map[string]struct{}, 100)
		for range 100 {
			id, err := NewTrackID()
			if err != nil {
				t.Fatalf("NewTrackID() unexpected error: %v", err)
			}
			if !strings.HasPrefix(id, "tr_") || len(id) != 35 {
				t.Fatalf("NewTrackID() = %q (len %d), want prefix tr_ and len 35", id, len(id))
			}
			assertHexLower(t, strings.TrimPrefix(id, "tr_"))
			if _, dup := seen[id]; dup {
				t.Fatalf("NewTrackID() generated duplicate ID %q", id)
			}
			seen[id] = struct{}{}
		}
	})
}

func TestUpsertTrack_InPlaceEditAndValidation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertTrack(ctx, nil); err == nil {
		t.Fatalf("UpsertTrack(nil) = nil error, want error")
	}

	invalidPaths := []string{
		"",
		".",
		"..",
		"../outside.mp3",
		"/abs/song.mp3",
		"dir/song.mp3/",
		"dir//song.mp3",
		"dir/./song.mp3",
		"dir/../song.mp3",
	}
	for _, bad := range invalidPaths {
		tr := makeTrack(bad, "T", "A", "A", "Al", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		if err := s.UpsertTrack(ctx, &tr); err == nil {
			t.Errorf("UpsertTrack(relPath=%q) = nil error, want error", bad)
		}
	}

	// Insert with empty ID, stale IDs, non-UTC CreatedAt with subseconds, and verbatim strings.
	est := time.FixedZone("EST", -5*3600)
	origCreated := time.Date(2025, 4, 5, 10, 20, 30, 987654321, est)
	input := Track{
		ID:          "",
		RelPath:     "Daft Punk/Discovery/01 One More Time.flac",
		FileSize:    50_000_000,
		MtimeNs:     1700000000123456789,
		Title:       "  One More Time  ",
		Artist:      "Daft Punk feat. Romanthony",
		AlbumArtist: "Daft Punk",
		ArtistID:    "stale_artist_id",
		Album:       "Discovery",
		AlbumID:     "stale_album_id",
		TrackNum:    1,
		DiscNum:     1,
		Year:        2001,
		Genre:       "French House",
		DurationSec: 320,
		BitRate:     950,
		CreatedAt:   origCreated,
	}
	inputBefore := input

	if err := s.UpsertTrack(ctx, &input); err != nil {
		t.Fatalf("UpsertTrack() unexpected error: %v", err)
	}
	if !reflect.DeepEqual(input, inputBefore) {
		t.Errorf("UpsertTrack() mutated caller Track:\ngot:  %+v\nwant: %+v", input, inputBefore)
	}

	snaps, err := s.LoadSnapshots(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshots() error: %v", err)
	}
	snap, ok := snaps[input.RelPath]
	if !ok {
		t.Fatalf("snapshot for %q missing", input.RelPath)
	}
	if !strings.HasPrefix(snap.ID, "tr_") || len(snap.ID) != 35 {
		t.Fatalf("generated ID = %q, want tr_ + 32 hex chars", snap.ID)
	}

	got, err := s.GetTrack(ctx, snap.ID)
	if err != nil {
		t.Fatalf("GetTrack(%q) error: %v", snap.ID, err)
	}
	wantCreated := origCreated.UTC().Truncate(time.Second)
	want := Track{
		ID:          snap.ID,
		RelPath:     input.RelPath,
		FileSize:    input.FileSize,
		MtimeNs:     input.MtimeNs,
		Title:       input.Title, // verbatim
		Artist:      input.Artist,
		AlbumArtist: input.AlbumArtist,
		ArtistID:    ArtistID(input.AlbumArtist),
		Album:       input.Album,
		AlbumID:     AlbumID(input.AlbumArtist, input.Album),
		TrackNum:    input.TrackNum,
		DiscNum:     input.DiscNum,
		Year:        input.Year,
		Genre:       input.Genre,
		DurationSec: input.DurationSec,
		BitRate:     input.BitRate,
		CreatedAt:   wantCreated,
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("GetTrack() after insert:\ngot:  %+v\nwant: %+v", *got, want)
	}

	// In-place edit on same RelPath updates all 14 mutable columns while preserving ID & CreatedAt.
	updated := Track{
		ID:          "tr_should_be_ignored_on_conflict",
		RelPath:     input.RelPath,
		FileSize:    51_000_000,
		MtimeNs:     1700000000999999999,
		Title:       "One More Time (Remastered)",
		Artist:      "Daft Punk",
		AlbumArtist: "Daft Punk Remasters",
		ArtistID:    "ignored",
		Album:       "Discovery (Deluxe)",
		AlbumID:     "ignored",
		TrackNum:    2,
		DiscNum:     2,
		Year:        2021,
		Genre:       "House",
		DurationSec: 325,
		BitRate:     1024,
		CreatedAt:   time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := s.UpsertTrack(ctx, &updated); err != nil {
		t.Fatalf("UpsertTrack() in-place edit error: %v", err)
	}

	count, err := s.TrackCount(ctx)
	if err != nil {
		t.Fatalf("TrackCount() error: %v", err)
	}
	if count != 1 {
		t.Fatalf("TrackCount() = %d, want 1", count)
	}

	gotEdited, err := s.GetTrack(ctx, snap.ID)
	if err != nil {
		t.Fatalf("GetTrack() after edit error: %v", err)
	}
	wantEdited := Track{
		ID:          snap.ID, // preserved
		RelPath:     updated.RelPath,
		FileSize:    updated.FileSize,
		MtimeNs:     updated.MtimeNs,
		Title:       updated.Title,
		Artist:      updated.Artist,
		AlbumArtist: updated.AlbumArtist,
		ArtistID:    ArtistID(updated.AlbumArtist),
		Album:       updated.Album,
		AlbumID:     AlbumID(updated.AlbumArtist, updated.Album),
		TrackNum:    updated.TrackNum,
		DiscNum:     updated.DiscNum,
		Year:        updated.Year,
		Genre:       updated.Genre,
		DurationSec: updated.DurationSec,
		BitRate:     updated.BitRate,
		CreatedAt:   wantCreated, // preserved
	}
	if !reflect.DeepEqual(*gotEdited, wantEdited) {
		t.Errorf("GetTrack() after in-place edit:\ngot:  %+v\nwant: %+v", *gotEdited, wantEdited)
	}

	// Same-ID in-place edit on existing RelPath succeeds and preserves CreatedAt.
	sameIDUpdate := updated
	sameIDUpdate.ID = snap.ID
	sameIDUpdate.Title = "One More Time (Same-ID Edit)"
	sameIDUpdate.CreatedAt = time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.UpsertTrack(ctx, &sameIDUpdate); err != nil {
		t.Fatalf("UpsertTrack() same-ID in-place edit error: %v", err)
	}
	gotSameID, err := s.GetTrack(ctx, snap.ID)
	if err != nil {
		t.Fatalf("GetTrack() after same-ID edit error: %v", err)
	}
	wantSameID := wantEdited
	wantSameID.Title = sameIDUpdate.Title
	if !reflect.DeepEqual(*gotSameID, wantSameID) {
		t.Errorf("GetTrack() after same-ID edit:\ngot:  %+v\nwant: %+v", *gotSameID, wantSameID)
	}

	// Zero CreatedAt defaults to current UTC time truncated to seconds.
	before := time.Now().UTC().Truncate(time.Second)
	zeroTimeTrack := makeTrack("zero_time.mp3", "Now", "NowArtist", "NowArtist", "NowAlbum", time.Time{})
	zeroTimeTrack.ID = "tr_zero_time"
	mustUpsert(t, s, zeroTimeTrack)
	after := time.Now().UTC().Add(time.Second)

	gotZero, err := s.GetTrack(ctx, zeroTimeTrack.ID)
	if err != nil {
		t.Fatalf("GetTrack(tr_zero_time) error: %v", err)
	}
	if gotZero.CreatedAt.Before(before) || gotZero.CreatedAt.After(after) {
		t.Errorf("zero CreatedAt defaulted to %v, want within [%v, %v]", gotZero.CreatedAt, before, after)
	}
}

func TestBrowseQueries_GroupingAndAggregates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Empty store checks.
	emptyArtists, err := s.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists() on empty store error: %v", err)
	}
	if emptyArtists == nil || len(emptyArtists) != 0 {
		t.Errorf("ListArtists() on empty store = %#v, want non-nil empty slice", emptyArtists)
	}
	if count, err := s.TrackCount(ctx); err != nil || count != 0 {
		t.Errorf("TrackCount() on empty store = (%d, %v), want (0, nil)", count, err)
	}
	if _, _, err := s.GetArtist(ctx, "ar_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetArtist(missing) err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.GetAlbum(ctx, "al_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetAlbum(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetTrack(ctx, "tr_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTrack(missing) err = %v, want ErrNotFound", err)
	}

	t1 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2025, 1, 3, 10, 0, 0, 0, time.UTC)

	seed := []Track{
		// Compilation album with mixed-case AlbumArtist/Album, different track-level Artist,
		// unset Year/Genre on one track, and multi-disc ordering with tie-breakers.
		{
			ID: "tr_comp_d2_t1", RelPath: "comp/d2_01.mp3", FileSize: 100, MtimeNs: 1,
			Title: "Final Encore", Artist: "AAA Soloist", AlbumArtist: "various artists",
			Album: "summer hits", TrackNum: 1, DiscNum: 2, Year: 0, Genre: "",
			DurationSec: 200, BitRate: 256, CreatedAt: t3,
		},
		{
			ID: "tr_comp_d1_t2", RelPath: "comp/d1_02.mp3", FileSize: 100, MtimeNs: 2,
			Title: "Second Song", Artist: "Guest B", AlbumArtist: "Various Artists",
			Album: "Summer Hits", TrackNum: 2, DiscNum: 1, Year: 2022, Genre: "Dance",
			DurationSec: 150, BitRate: 320, CreatedAt: t1,
		},
		{
			ID: "tr_comp_d1_t1_b", RelPath: "comp/d1_01b.mp3", FileSize: 100, MtimeNs: 3,
			Title: "Alpha", Artist: "Guest C", AlbumArtist: "Various Artists",
			Album: "Summer Hits", TrackNum: 1, DiscNum: 1, Year: 2019, Genre: "Pop",
			DurationSec: 120, BitRate: 192, CreatedAt: t2,
		},
		{
			ID: "tr_comp_d1_t1_a", RelPath: "comp/d1_01a.mp3", FileSize: 100, MtimeNs: 4,
			Title: "alpha", Artist: "Guest D", AlbumArtist: "various artists",
			Album: "summer hits", TrackNum: 1, DiscNum: 1, Year: 0, Genre: "",
			DurationSec: 130, BitRate: 192, CreatedAt: t2,
		},
		// Second album under same artist with ALL Year==0 and Genre=="", using lowercase
		// Album/AlbumArtist so GetArtist verifies case-insensitive album ordering and
		// ArtistSummary.Name update from a later album.
		{
			ID: "tr_comp_unset", RelPath: "comp/unset.mp3", FileSize: 100, MtimeNs: 5,
			Title: "Mystery", Artist: "Unknown Guest", AlbumArtist: "various artists",
			Album: "ambient demos", TrackNum: 1, DiscNum: 1, Year: 0, Genre: "",
			DurationSec: 90, BitRate: 128, CreatedAt: t2,
		},
		// Separate artist sorting before "Various Artists".
		{
			ID: "tr_boards", RelPath: "boc/01.flac", FileSize: 500, MtimeNs: 6,
			Title: "Roygbiv", Artist: "Boards of Canada", AlbumArtist: "boards of canada",
			Album: "Music Has the Right to Children", TrackNum: 1, DiscNum: 1, Year: 1998, Genre: "IDM",
			DurationSec: 151, BitRate: 800, CreatedAt: t1,
		},
	}
	if err := s.ApplyScanBatch(ctx, seed, nil); err != nil {
		t.Fatalf("ApplyScanBatch() seed error: %v", err)
	}

	// ListArtists: case-insensitive order and accurate AlbumCount.
	artists, err := s.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists() error: %v", err)
	}
	wantArtists := []ArtistSummary{
		{ID: ArtistID("Boards of Canada"), Name: "boards of canada", AlbumCount: 1},
		{ID: ArtistID("Various Artists"), Name: "Various Artists", AlbumCount: 2},
	}
	if !reflect.DeepEqual(artists, wantArtists) {
		t.Errorf("ListArtists() =\n%+v\nwant:\n%+v", artists, wantArtists)
	}

	// GetArtist: albums sorted case-insensitively by album title.
	vaID := ArtistID("Various Artists")
	gotArtist, vaAlbums, err := s.GetArtist(ctx, vaID)
	if err != nil {
		t.Fatalf("GetArtist(%q) error: %v", vaID, err)
	}
	if gotArtist != wantArtists[1] {
		t.Errorf("GetArtist() summary = %+v, want %+v", gotArtist, wantArtists[1])
	}
	if len(vaAlbums) != 2 {
		t.Fatalf("GetArtist() album count = %d, want 2", len(vaAlbums))
	}
	if vaAlbums[0].Name != "ambient demos" || vaAlbums[1].Name != "Summer Hits" {
		t.Errorf("GetArtist() album order = [%q, %q], want [ambient demos, Summer Hits]", vaAlbums[0].Name, vaAlbums[1].Name)
	}
	if vaAlbums[0].Year != 0 || vaAlbums[0].Genre != "" {
		t.Errorf("unset album summary got Year=%d Genre=%q, want 0 and empty", vaAlbums[0].Year, vaAlbums[0].Genre)
	}

	// GetAlbum: track sort order and Go-aggregated AlbumSummary.
	compAlbumID := AlbumID("Various Artists", "Summer Hits")
	gotAlbum, tracks, err := s.GetAlbum(ctx, compAlbumID)
	if err != nil {
		t.Fatalf("GetAlbum(%q) error: %v", compAlbumID, err)
	}
	wantTrackOrder := []string{"tr_comp_d1_t1_a", "tr_comp_d1_t1_b", "tr_comp_d1_t2", "tr_comp_d2_t1"}
	gotTrackOrder := make([]string, len(tracks))
	for i, tr := range tracks {
		gotTrackOrder[i] = tr.ID
	}
	if !reflect.DeepEqual(gotTrackOrder, wantTrackOrder) {
		t.Errorf("GetAlbum() track order = %v, want %v", gotTrackOrder, wantTrackOrder)
	}

	wantCompSummary := AlbumSummary{
		ID:          compAlbumID,
		Name:        "Summer Hits",     // MIN("Summer Hits", "summer hits")
		Artist:      "Various Artists", // MIN(album_artist), NOT track-level Artist "AAA Soloist"
		ArtistID:    vaID,
		SongCount:   4,
		DurationSec: 600,
		Year:        2022,
		Genre:       "Dance",
		CreatedAt:   t1,
	}
	if gotAlbum != wantCompSummary {
		t.Errorf("GetAlbum() summary =\n%+v\nwant:\n%+v", gotAlbum, wantCompSummary)
	}

	// Go-vs-SQL AlbumSummary parity across GetAlbum, GetArtist, ListAlbums, and Search3.
	listedAlbums, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: 10})
	if err != nil {
		t.Fatalf("ListAlbums() error: %v", err)
	}
	listedByID := make(map[string]AlbumSummary, len(listedAlbums))
	for _, a := range listedAlbums {
		listedByID[a.ID] = a
	}
	searched, err := s.Search3(ctx, Search3Query{Query: "", AlbumCount: 10})
	if err != nil {
		t.Fatalf("Search3() error: %v", err)
	}
	searchedByID := make(map[string]AlbumSummary, len(searched.Albums))
	for _, a := range searched.Albums {
		searchedByID[a.ID] = a
	}
	for _, a := range vaAlbums {
		fromGetAlbum, _, err := s.GetAlbum(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetAlbum(%q) error: %v", a.ID, err)
		}
		if fromGetAlbum != a {
			t.Errorf("Go-vs-SQL parity mismatch for %q: GetAlbum=%+v, GetArtist=%+v", a.ID, fromGetAlbum, a)
		}
		if fromList := listedByID[a.ID]; fromGetAlbum != fromList {
			t.Errorf("Go-vs-SQL parity mismatch for %q: GetAlbum=%+v, ListAlbums=%+v", a.ID, fromGetAlbum, fromList)
		}
		if fromSearch := searchedByID[a.ID]; fromGetAlbum != fromSearch {
			t.Errorf("Go-vs-SQL parity mismatch for %q: GetAlbum=%+v, Search3=%+v", a.ID, fromGetAlbum, fromSearch)
		}
	}

	// Corrupt created_at error propagation across all 6 timestamp-reading methods.
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE tracks SET created_at = ? WHERE id = ?`, "not-a-timestamp", "tr_boards"); err != nil {
		t.Fatalf("corrupt created_at UPDATE error: %v", err)
	}
	boardsArtistID := ArtistID("Boards of Canada")
	boardsAlbumID := AlbumID("Boards of Canada", "Music Has the Right to Children")

	corruptChecks := []struct {
		name string
		fn   func() error
	}{
		{"GetTrack", func() error { _, err := s.GetTrack(ctx, "tr_boards"); return err }},
		{"GetAlbum", func() error { _, _, err := s.GetAlbum(ctx, boardsAlbumID); return err }},
		{"GetArtist", func() error { _, _, err := s.GetArtist(ctx, boardsArtistID); return err }},
		{"ListAlbums", func() error {
			_, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: 10})
			return err
		}},
		{"Search3 albums", func() error {
			_, err := s.Search3(ctx, Search3Query{Query: "Boards", AlbumCount: 10})
			return err
		}},
		{"Search3 songs", func() error {
			_, err := s.Search3(ctx, Search3Query{Query: "Roygbiv", SongCount: 10})
			return err
		}},
		{"LoadSnapshots", func() error { _, err := s.LoadSnapshots(ctx); return err }},
	}
	for _, tc := range corruptChecks {
		t.Run("corrupt_created_at_"+tc.name, func(t *testing.T) {
			err := tc.fn()
			if err == nil {
				t.Fatalf("%s() = nil error on corrupt created_at, want error", tc.name)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("%s() returned ErrNotFound on corrupt created_at, want parse error: %v", tc.name, err)
			}
		})
	}
}

func TestListAlbums(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Invalid or empty Sort must error even when Limit <= 0.
	invalidSorts := []AlbumListQuery{
		{Sort: "", Limit: 0},
		{Sort: "", Limit: -10},
		{Sort: "byGenre", Limit: 0},
		{Sort: "alphabeticalByName ", Limit: 5},
	}
	for _, q := range invalidSorts {
		if _, err := s.ListAlbums(ctx, q); err == nil {
			t.Errorf("ListAlbums(%+v) = nil error, want error", q)
		}
	}

	t10 := time.Date(2025, 1, 1, 0, 0, 10, 0, time.UTC)
	t20 := time.Date(2025, 1, 1, 0, 0, 20, 0, time.UTC)
	t30 := time.Date(2025, 1, 1, 0, 0, 30, 0, time.UTC)
	t50 := time.Date(2025, 1, 1, 0, 0, 50, 0, time.UTC)

	// Album A has oldest MIN(created_at)=t10 but newest MAX(created_at)=t50.
	// Album B has MIN=MAX=t30.
	// Album C and Album D share album name "Echoes" under different artists and share MAX(created_at)=t20.
	seed := []Track{
		makeTrack("a/1.mp3", "A1", "Zebra", "Zebra", "Zeta", t10),
		makeTrack("a/2.mp3", "A2", "Zebra", "Zebra", "Zeta", t50),
		makeTrack("b/1.mp3", "B1", "Alpha", "Alpha", "Beta", t30),
		makeTrack("c/1.mp3", "C1", "Bravo", "Bravo", "Echoes", t20),
		makeTrack("d/1.mp3", "D1", "alpha", "alpha", "echoes", t20),
	}
	if err := s.ApplyScanBatch(ctx, seed, nil); err != nil {
		t.Fatalf("ApplyScanBatch() seed error: %v", err)
	}

	idA := AlbumID("Zebra", "Zeta")
	idB := AlbumID("Alpha", "Beta")
	idC := AlbumID("Bravo", "Echoes")
	idD := AlbumID("alpha", "echoes")

	// Deterministic tie-breaker order for idC vs idD.
	tieFirst, tieSecond := idC, idD
	if idD < idC {
		tieFirst, tieSecond = idD, idC
	}

	extractIDs := func(albums []AlbumSummary) []string {
		ids := make([]string, len(albums))
		for i, a := range albums {
			ids[i] = a.ID
		}
		return ids
	}

	t.Run("alphabeticalByName with tie-breaker", func(t *testing.T) {
		got, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: 10})
		if err != nil {
			t.Fatalf("ListAlbums() error: %v", err)
		}
		want := []string{idB, tieFirst, tieSecond, idA}
		if gotIDs := extractIDs(got); !reflect.DeepEqual(gotIDs, want) {
			t.Errorf("alphabeticalByName = %v, want %v", gotIDs, want)
		}
	})

	t.Run("alphabeticalByArtist", func(t *testing.T) {
		got, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByArtist, Limit: 10})
		if err != nil {
			t.Fatalf("ListAlbums() error: %v", err)
		}
		want := []string{idB, idD, idC, idA}
		if gotIDs := extractIDs(got); !reflect.DeepEqual(gotIDs, want) {
			t.Errorf("alphabeticalByArtist = %v, want %v", gotIDs, want)
		}
	})

	t.Run("newest orders by MAX(created_at) DESC while AlbumSummary.CreatedAt is MIN(created_at)", func(t *testing.T) {
		got, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortNewest, Limit: 10})
		if err != nil {
			t.Fatalf("ListAlbums() error: %v", err)
		}
		want := []string{idA, idB, tieFirst, tieSecond}
		if gotIDs := extractIDs(got); !reflect.DeepEqual(gotIDs, want) {
			t.Errorf("newest = %v, want %v", gotIDs, want)
		}
		if !got[0].CreatedAt.Equal(t10) {
			t.Errorf("newest[0].CreatedAt = %v, want MIN(created_at)=%v", got[0].CreatedAt, t10)
		}
		if !got[1].CreatedAt.Equal(t30) {
			t.Errorf("newest[1].CreatedAt = %v, want %v", got[1].CreatedAt, t30)
		}
	})

	t.Run("random returns valid distinct albums", func(t *testing.T) {
		got, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortRandom, Limit: 3})
		if err != nil {
			t.Fatalf("ListAlbums(random) error: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(random) = %d, want 3", len(got))
		}
		valid := map[string]bool{idA: true, idB: true, idC: true, idD: true}
		seen := map[string]bool{}
		for _, a := range got {
			if !valid[a.ID] || seen[a.ID] {
				t.Errorf("unexpected or duplicate random album ID %q", a.ID)
			}
			seen[a.ID] = true
		}
	})

	t.Run("limit <= 0 and negative offset clamping", func(t *testing.T) {
		for _, limit := range []int{0, -1} {
			got, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: limit})
			if err != nil {
				t.Fatalf("ListAlbums(limit=%d) error: %v", limit, err)
			}
			if got == nil || len(got) != 0 {
				t.Errorf("ListAlbums(limit=%d) = %#v, want non-nil empty slice", limit, got)
			}
		}

		got, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: 2, Offset: -5})
		if err != nil {
			t.Fatalf("ListAlbums(offset=-5) error: %v", err)
		}
		want := []string{idB, tieFirst}
		if gotIDs := extractIDs(got); !reflect.DeepEqual(gotIDs, want) {
			t.Errorf("offset=-5 = %v, want %v", gotIDs, want)
		}
	})

	t.Run("MaxPageLimit 505 clamps to 500", func(t *testing.T) {
		sLarge := openTestStore(t)
		const total = 505
		batch := make([]Track, total)
		baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := range total {
			batch[i] = makeTrack(
				fmt.Sprintf("dir/track_%03d.mp3", i),
				fmt.Sprintf("Song %03d", i),
				fmt.Sprintf("Artist %03d", i),
				fmt.Sprintf("Artist %03d", i),
				fmt.Sprintf("Album %03d", i),
				baseTime.Add(time.Duration(i)*time.Second),
			)
		}
		if err := sLarge.ApplyScanBatch(ctx, batch, nil); err != nil {
			t.Fatalf("ApplyScanBatch(%d) error: %v", total, err)
		}

		page1, err := sLarge.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: 505})
		if err != nil {
			t.Fatalf("ListAlbums(limit=505) error: %v", err)
		}
		if len(page1) != MaxPageLimit {
			t.Errorf("len(page1) = %d, want %d", len(page1), MaxPageLimit)
		}

		page2, err := sLarge.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: 50, Offset: 500})
		if err != nil {
			t.Fatalf("ListAlbums(offset=500) error: %v", err)
		}
		if len(page2) != 5 {
			t.Errorf("len(page2) = %d, want 5", len(page2))
		}
	})
}

func TestSearch3(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ts := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	seed := []Track{
		{
			ID: "tr_1", RelPath: "1.mp3", FileSize: 10, MtimeNs: 1,
			Title: "100% Pure Love", Artist: "Crystal Waters", AlbumArtist: "Crystal Waters",
			Album: "Storyteller", DurationSec: 180, CreatedAt: ts,
		},
		{
			ID: "tr_2", RelPath: "2.mp3", FileSize: 10, MtimeNs: 2,
			Title: "1000 Pure Drops", Artist: "Other Band", AlbumArtist: "Other Band",
			Album: "Drops", DurationSec: 200, CreatedAt: ts,
		},
		{
			ID: "tr_3", RelPath: "3.mp3", FileSize: 10, MtimeNs: 3,
			Title: "a_b track", Artist: `Back\Slash`, AlbumArtist: `Back\Slash`,
			Album: "Under_Score", DurationSec: 210, CreatedAt: ts,
		},
		{
			ID: "tr_4", RelPath: "4.mp3", FileSize: 10, MtimeNs: 4,
			Title: "axb track", Artist: "BackXSlash", AlbumArtist: "BackXSlash",
			Album: "UnderXScore", DurationSec: 220, CreatedAt: ts,
		},
		{
			ID: "tr_5", RelPath: "5.mp3", FileSize: 10, MtimeNs: 5,
			Title: `Say ""Hello""`, Artist: "Featured Star", AlbumArtist: "Host Artist",
			Album: "Mix Tape", DurationSec: 100, CreatedAt: ts,
		},
		{
			ID: "tr_6", RelPath: "6.mp3", FileSize: 10, MtimeNs: 6,
			Title: "Second Cut", Artist: "Host Artist", AlbumArtist: "Host Artist",
			Album: "Mix Tape", DurationSec: 150, CreatedAt: ts,
		},
	}
	if err := s.ApplyScanBatch(ctx, seed, nil); err != nil {
		t.Fatalf("ApplyScanBatch() seed error: %v", err)
	}

	t.Run("all non-positive counts return non-nil empty slices", func(t *testing.T) {
		got, err := s.Search3(ctx, Search3Query{
			Query:       "",
			ArtistCount: 0,
			AlbumCount:  -1,
			SongCount:   0,
		})
		if err != nil {
			t.Fatalf("Search3() error: %v", err)
		}
		if got.Artists == nil || len(got.Artists) != 0 {
			t.Errorf("Artists = %#v, want non-nil empty", got.Artists)
		}
		if got.Albums == nil || len(got.Albums) != 0 {
			t.Errorf("Albums = %#v, want non-nil empty", got.Albums)
		}
		if got.Songs == nil || len(got.Songs) != 0 {
			t.Errorf("Songs = %#v, want non-nil empty", got.Songs)
		}
	})

	t.Run("empty and whitespace queries return all items", func(t *testing.T) {
		for _, qStr := range []string{"", "   \t  "} {
			got, err := s.Search3(ctx, Search3Query{
				Query:       qStr,
				ArtistCount: 20,
				AlbumCount:  20,
				SongCount:   20,
			})
			if err != nil {
				t.Fatalf("Search3(%q) error: %v", qStr, err)
			}
			if len(got.Artists) != 5 || len(got.Albums) != 5 || len(got.Songs) != 6 {
				t.Errorf("Search3(%q) counts = (%d, %d, %d), want (5, 5, 6)", qStr, len(got.Artists), len(got.Albums), len(got.Songs))
			}
		}
	})

	t.Run("literal double quotes treated as literal substring", func(t *testing.T) {
		got, err := s.Search3(ctx, Search3Query{
			Query:       `""`,
			ArtistCount: 20,
			AlbumCount:  20,
			SongCount:   20,
		})
		if err != nil {
			t.Fatalf("Search3(quotes) error: %v", err)
		}
		if len(got.Artists) != 0 || len(got.Albums) != 0 || len(got.Songs) != 1 || got.Songs[0].ID != "tr_5" {
			t.Errorf("Search3(quotes) = artists:%d albums:%d songs:%+v, want only tr_5", len(got.Artists), len(got.Albums), got.Songs)
		}
	})

	t.Run("LIKE wildcards %, _, and backslash are escaped", func(t *testing.T) {
		tests := []struct {
			name        string
			query       string
			wantArtist  int
			wantAlbum   int
			wantSongIDs []string
		}{
			{
				name:        "percent literal",
				query:       "100%",
				wantArtist:  0,
				wantAlbum:   0,
				wantSongIDs: []string{"tr_1"},
			},
			{
				name:        "underscore literal",
				query:       "a_b",
				wantArtist:  0,
				wantAlbum:   0,
				wantSongIDs: []string{"tr_3"},
			},
			{
				name:        "underscore in album",
				query:       "under_score",
				wantArtist:  0,
				wantAlbum:   1,
				wantSongIDs: []string{"tr_3"},
			},
			{
				name:        "backslash literal",
				query:       `back\slash`,
				wantArtist:  1,
				wantAlbum:   1,
				wantSongIDs: []string{"tr_3"},
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got, err := s.Search3(ctx, Search3Query{
					Query:       tc.query,
					ArtistCount: 10,
					AlbumCount:  10,
					SongCount:   10,
				})
				if err != nil {
					t.Fatalf("Search3(%q) error: %v", tc.query, err)
				}
				if len(got.Artists) != tc.wantArtist {
					t.Errorf("len(Artists) = %d, want %d", len(got.Artists), tc.wantArtist)
				}
				if len(got.Albums) != tc.wantAlbum {
					t.Errorf("len(Albums) = %d, want %d", len(got.Albums), tc.wantAlbum)
				}
				gotSongIDs := make([]string, len(got.Songs))
				for i, song := range got.Songs {
					gotSongIDs[i] = song.ID
				}
				if !reflect.DeepEqual(gotSongIDs, tc.wantSongIDs) {
					t.Errorf("song IDs = %v, want %v", gotSongIDs, tc.wantSongIDs)
				}
			})
		}
	})

	t.Run("case-insensitive buckets and full group aggregates", func(t *testing.T) {
		// "featured star" matches Song.Artist on tr_5 only (not AlbumArtist or Album).
		gotFeatured, err := s.Search3(ctx, Search3Query{
			Query:       "FEATURED STAR",
			ArtistCount: 10,
			AlbumCount:  10,
			SongCount:   10,
		})
		if err != nil {
			t.Fatalf("Search3(FEATURED STAR) error: %v", err)
		}
		if len(gotFeatured.Artists) != 0 || len(gotFeatured.Albums) != 0 || len(gotFeatured.Songs) != 1 || gotFeatured.Songs[0].ID != "tr_5" {
			t.Errorf("Search3(FEATURED STAR) mismatch: artists=%d albums=%d songs=%d", len(gotFeatured.Artists), len(gotFeatured.Albums), len(gotFeatured.Songs))
		}

		// "host artist" matches Artist, Album (preserving full 2-song duration=250), and both Songs.
		gotHost, err := s.Search3(ctx, Search3Query{
			Query:       "host artist",
			ArtistCount: 10,
			AlbumCount:  10,
			SongCount:   10,
		})
		if err != nil {
			t.Fatalf("Search3(host artist) error: %v", err)
		}
		if len(gotHost.Artists) != 1 || len(gotHost.Albums) != 1 || len(gotHost.Songs) != 2 {
			t.Fatalf("Search3(host artist) counts = (%d,%d,%d), want (1,1,2)", len(gotHost.Artists), len(gotHost.Albums), len(gotHost.Songs))
		}
		if gotHost.Albums[0].SongCount != 2 || gotHost.Albums[0].DurationSec != 250 {
			t.Errorf("matched album aggregate = (songs:%d, dur:%d), want (2, 250)", gotHost.Albums[0].SongCount, gotHost.Albums[0].DurationSec)
		}

		// Surrounding whitespace on non-empty queries is trimmed before matching.
		gotPadded, err := s.Search3(ctx, Search3Query{
			Query:       "  host artist  ",
			ArtistCount: 10,
			AlbumCount:  10,
			SongCount:   10,
		})
		if err != nil {
			t.Fatalf("Search3(padded host artist) error: %v", err)
		}
		if !reflect.DeepEqual(gotPadded, gotHost) {
			t.Errorf("Search3(padded) =\n%+v\nwant:\n%+v", gotPadded, gotHost)
		}

		// Zero-match queries return non-nil empty slices for every bucket.
		gotNone, err := s.Search3(ctx, Search3Query{
			Query:       "no-such-match-xyz",
			ArtistCount: 10,
			AlbumCount:  10,
			SongCount:   10,
		})
		if err != nil {
			t.Fatalf("Search3(no match) error: %v", err)
		}
		if gotNone.Artists == nil || len(gotNone.Artists) != 0 ||
			gotNone.Albums == nil || len(gotNone.Albums) != 0 ||
			gotNone.Songs == nil || len(gotNone.Songs) != 0 {
			t.Errorf("Search3(no match) = %#v, want non-nil empty slices", gotNone)
		}
	})

	t.Run("partial counts, negative offsets, and deterministic tie pagination", func(t *testing.T) {
		// Skip artists (0) and albums (-5); paginate songs with negative offset clamped to 0.
		got, err := s.Search3(ctx, Search3Query{
			Query:        "",
			ArtistCount:  0,
			ArtistOffset: -2,
			AlbumCount:   -5,
			AlbumOffset:  -2,
			SongCount:    2,
			SongOffset:   -3,
		})
		if err != nil {
			t.Fatalf("Search3() error: %v", err)
		}
		if got.Artists == nil || len(got.Artists) != 0 || got.Albums == nil || len(got.Albums) != 0 {
			t.Errorf("skipped buckets should be non-nil empty: artists=%#v albums=%#v", got.Artists, got.Albums)
		}
		if len(got.Songs) != 2 || got.Songs[0].ID != "tr_1" || got.Songs[1].ID != "tr_2" {
			t.Errorf("songs page 0 = %+v, want [tr_1, tr_2]", got.Songs)
		}

		// Negative artist and album offsets clamp to 0.
		gotNeg, err := s.Search3(ctx, Search3Query{
			Query:        "",
			ArtistCount:  2,
			ArtistOffset: -4,
			AlbumCount:   2,
			AlbumOffset:  -4,
			SongCount:    0,
		})
		if err != nil {
			t.Fatalf("Search3(neg offsets) error: %v", err)
		}
		if len(gotNeg.Artists) != 2 || len(gotNeg.Albums) != 2 {
			t.Errorf("neg offset counts = (%d, %d), want (2, 2)", len(gotNeg.Artists), len(gotNeg.Albums))
		}

		// Add tied song titles and tied album titles across artists and paginate across the ties.
		mustUpsert(t, s, Track{
			ID: "tr_tie_b", RelPath: "tie_b.mp3", FileSize: 1, MtimeNs: 1,
			Title: "Twin", Artist: "Tie B", AlbumArtist: "Tie B", Album: "SameName", CreatedAt: ts,
		})
		mustUpsert(t, s, Track{
			ID: "tr_tie_a", RelPath: "tie_a.mp3", FileSize: 1, MtimeNs: 1,
			Title: "twin", Artist: "Tie A", AlbumArtist: "Tie A", Album: "samename", CreatedAt: ts,
		})
		p1, err := s.Search3(ctx, Search3Query{Query: "twin", SongCount: 1, SongOffset: 0})
		if err != nil {
			t.Fatalf("Search3(twin page 1) error: %v", err)
		}
		p2, err := s.Search3(ctx, Search3Query{Query: "twin", SongCount: 1, SongOffset: 1})
		if err != nil {
			t.Fatalf("Search3(twin page 2) error: %v", err)
		}
		if len(p1.Songs) != 1 || p1.Songs[0].ID != "tr_tie_a" || len(p2.Songs) != 1 || p2.Songs[0].ID != "tr_tie_b" {
			t.Errorf("tied song pagination = (%+v, %+v), want (tr_tie_a, tr_tie_b)", p1.Songs, p2.Songs)
		}

		tieAl1, tieAl2 := AlbumID("Tie A", "samename"), AlbumID("Tie B", "SameName")
		if tieAl2 < tieAl1 {
			tieAl1, tieAl2 = tieAl2, tieAl1
		}
		ap1, err := s.Search3(ctx, Search3Query{Query: "samename", AlbumCount: 1, AlbumOffset: 0})
		if err != nil {
			t.Fatalf("Search3(samename page 1) error: %v", err)
		}
		ap2, err := s.Search3(ctx, Search3Query{Query: "samename", AlbumCount: 1, AlbumOffset: 1})
		if err != nil {
			t.Fatalf("Search3(samename page 2) error: %v", err)
		}
		if len(ap1.Albums) != 1 || ap1.Albums[0].ID != tieAl1 || len(ap2.Albums) != 1 || ap2.Albums[0].ID != tieAl2 {
			t.Errorf("tied album pagination = (%+v, %+v), want (%q, %q)", ap1.Albums, ap2.Albums, tieAl1, tieAl2)
		}
	})

	t.Run("MaxPageLimit 505 clamps all three buckets to 500", func(t *testing.T) {
		sLarge := openTestStore(t)
		const total = 505
		batch := make([]Track, total)
		for i := range total {
			batch[i] = makeTrack(
				fmt.Sprintf("s/t_%03d.mp3", i),
				fmt.Sprintf("Title %03d", i),
				fmt.Sprintf("Artist %03d", i),
				fmt.Sprintf("Artist %03d", i),
				fmt.Sprintf("Album %03d", i),
				ts,
			)
		}
		if err := sLarge.ApplyScanBatch(ctx, batch, nil); err != nil {
			t.Fatalf("ApplyScanBatch(%d) error: %v", total, err)
		}

		got, err := sLarge.Search3(ctx, Search3Query{
			Query:       "",
			ArtistCount: 505,
			AlbumCount:  505,
			SongCount:   505,
		})
		if err != nil {
			t.Fatalf("Search3(505) error: %v", err)
		}
		if len(got.Artists) != MaxPageLimit || len(got.Albums) != MaxPageLimit || len(got.Songs) != MaxPageLimit {
			t.Errorf("clamped counts = (%d, %d, %d), want (%d, %d, %d)",
				len(got.Artists), len(got.Albums), len(got.Songs), MaxPageLimit, MaxPageLimit, MaxPageLimit)
		}
	})
}

func TestDeletesAndSnapshots(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	emptySnaps, err := s.LoadSnapshots(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshots() on empty store error: %v", err)
	}
	if emptySnaps == nil || len(emptySnaps) != 0 {
		t.Errorf("LoadSnapshots() on empty store = %#v, want non-nil empty map", emptySnaps)
	}

	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	tracks := []Track{
		{ID: "tr_rock_1", RelPath: "Rock/a.mp3", FileSize: 101, MtimeNs: 1001, Title: "A", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_rock_2", RelPath: "Rock/90s/b.mp3", FileSize: 102, MtimeNs: 1002, Title: "B", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_rock_exact", RelPath: "Rock", FileSize: 100, MtimeNs: 1000, Title: "Exact", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_rock2", RelPath: "Rock2/c.mp3", FileSize: 103, MtimeNs: 1003, Title: "C", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_rock0", RelPath: "Rock0/d.mp3", FileSize: 104, MtimeNs: 1004, Title: "D", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_rockdash", RelPath: "Rock-Classic/e.mp3", FileSize: 105, MtimeNs: 1005, Title: "E", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_rockfile", RelPath: "Rock.mp3", FileSize: 106, MtimeNs: 1006, Title: "F", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_wild_match", RelPath: "100%_Rock/dir_1/g.mp3", FileSize: 107, MtimeNs: 1007, Title: "G", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
		{ID: "tr_wild_other", RelPath: "1000XRock/dirX1/h.mp3", FileSize: 108, MtimeNs: 1008, Title: "H", Artist: "A", AlbumArtist: "A", Album: "A", CreatedAt: ts},
	}
	if err := s.ApplyScanBatch(ctx, tracks, nil); err != nil {
		t.Fatalf("ApplyScanBatch() seed error: %v", err)
	}

	snaps, err := s.LoadSnapshots(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshots() error: %v", err)
	}
	if len(snaps) != len(tracks) {
		t.Fatalf("len(LoadSnapshots) = %d, want %d", len(snaps), len(tracks))
	}
	wantSnap := TrackSnapshot{
		ID:        "tr_rock_1",
		RelPath:   "Rock/a.mp3",
		FileSize:  101,
		MtimeNs:   1001,
		CreatedAt: ts,
	}
	if got := snaps["Rock/a.mp3"]; got != wantSnap {
		t.Errorf("LoadSnapshots()[Rock/a.mp3] = %+v, want %+v", got, wantSnap)
	}

	// DeleteByRelPath: invalid paths rejected, absent valid path is idempotent, existing path deleted.
	for _, bad := range []string{"", ".", "../a.mp3", "/Rock/a.mp3", "Rock/"} {
		if err := s.DeleteByRelPath(ctx, bad); err == nil {
			t.Errorf("DeleteByRelPath(%q) = nil error, want error", bad)
		}
	}
	if err := s.DeleteByRelPath(ctx, "Does/Not/Exist.mp3"); err != nil {
		t.Fatalf("DeleteByRelPath(absent) error: %v", err)
	}
	if err := s.DeleteByRelPath(ctx, "Rock.mp3"); err != nil {
		t.Fatalf("DeleteByRelPath(Rock.mp3) error: %v", err)
	}
	if _, err := s.GetTrack(ctx, "tr_rockfile"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTrack(tr_rockfile) after delete = %v, want ErrNotFound", err)
	}

	// DeleteByDirPrefix: invalid prefixes rejected (including leading slash).
	badPrefixes := []string{
		"",
		"/",
		"///",
		"/music/Rock",
		"/Rock/",
		".",
		"./",
		"./Rock",
		"..",
		"Rock/../Pop",
		"Rock//Pop",
	}
	for _, bad := range badPrefixes {
		if err := s.DeleteByDirPrefix(ctx, bad); err == nil {
			t.Errorf("DeleteByDirPrefix(%q) = nil error, want error", bad)
		}
	}

	// Delete "Rock/" (trailing slash normalized) leaves Rock2/, Rock0/, Rock-Classic/ untouched.
	if err := s.DeleteByDirPrefix(ctx, "Rock/"); err != nil {
		t.Fatalf("DeleteByDirPrefix(Rock/) error: %v", err)
	}
	// Idempotent delete on already-empty directory.
	if err := s.DeleteByDirPrefix(ctx, "Rock///"); err != nil {
		t.Fatalf("DeleteByDirPrefix(Rock///) idempotent error: %v", err)
	}

	// Delete literal % and _ directory name leaves look-alike sibling untouched.
	if err := s.DeleteByDirPrefix(ctx, "100%_Rock/dir_1"); err != nil {
		t.Fatalf("DeleteByDirPrefix(100%%_Rock/dir_1) error: %v", err)
	}

	remaining, err := s.LoadSnapshots(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshots() after deletes error: %v", err)
	}
	wantRemaining := map[string]bool{
		"Rock":                  true,
		"Rock2/c.mp3":           true,
		"Rock0/d.mp3":           true,
		"Rock-Classic/e.mp3":    true,
		"1000XRock/dirX1/h.mp3": true,
	}
	if len(remaining) != len(wantRemaining) {
		t.Fatalf("remaining tracks = %v, want keys %v", remaining, wantRemaining)
	}
	for relPath := range wantRemaining {
		if _, ok := remaining[relPath]; !ok {
			t.Errorf("expected remaining track %q missing", relPath)
		}
	}
}

func TestApplyScanBatch_Atomicity(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Empty batch is a no-op.
	if err := s.ApplyScanBatch(ctx, nil, nil); err != nil {
		t.Fatalf("ApplyScanBatch(nil, nil) error: %v", err)
	}

	t1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 2, 2, 0, 0, 0, 0, time.UTC)

	seed := []Track{
		{ID: "tr_old", RelPath: "reinsert.mp3", FileSize: 10, MtimeNs: 1, Title: "Old", Artist: "A", AlbumArtist: "A", Album: "Al", CreatedAt: t1},
		{ID: "tr_keep", RelPath: "keep.mp3", FileSize: 20, MtimeNs: 2, Title: "Keep", Artist: "A", AlbumArtist: "A", Album: "Al", CreatedAt: t1},
		{ID: "tr_fixed", RelPath: "fixed.mp3", FileSize: 30, MtimeNs: 3, Title: "Fixed", Artist: "A", AlbumArtist: "A", Album: "Al", CreatedAt: t1},
	}
	if err := s.ApplyScanBatch(ctx, seed, nil); err != nil {
		t.Fatalf("ApplyScanBatch(seed) error: %v", err)
	}

	// Delete + reinsert same rel_path in one batch: delete runs first, so new ID and CreatedAt take effect.
	// Also verify caller upserts slice is never mutated.
	upserts := []Track{
		{
			ID:          "",
			RelPath:     "reinsert.mp3",
			FileSize:    99,
			MtimeNs:     99,
			Title:       "Fresh",
			Artist:      "B",
			AlbumArtist: "B",
			ArtistID:    "stale",
			Album:       "NewAlbum",
			AlbumID:     "stale",
			CreatedAt:   t2,
		},
	}
	upsertsCopy := append([]Track(nil), upserts...)

	if err := s.ApplyScanBatch(ctx, upserts, []string{"reinsert.mp3"}); err != nil {
		t.Fatalf("ApplyScanBatch(delete+reinsert) error: %v", err)
	}
	if !reflect.DeepEqual(upserts, upsertsCopy) {
		t.Errorf("ApplyScanBatch mutated caller upserts slice:\ngot:  %+v\nwant: %+v", upserts, upsertsCopy)
	}
	if _, err := s.GetTrack(ctx, "tr_old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTrack(tr_old) after delete+reinsert = %v, want ErrNotFound", err)
	}
	snaps, err := s.LoadSnapshots(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshots() error: %v", err)
	}
	reSnap, ok := snaps["reinsert.mp3"]
	if !ok {
		t.Fatalf("reinsert.mp3 missing after delete+reinsert")
	}
	if reSnap.ID == "tr_old" || !reSnap.CreatedAt.Equal(t2) {
		t.Errorf("reinsert.mp3 snapshot = %+v, want new ID and CreatedAt=%v", reSnap, t2)
	}

	// Pre-transaction validation failures leave DB untouched.
	if err := s.ApplyScanBatch(ctx, []Track{makeTrack("new_never.mp3", "X", "X", "X", "X", t2)}, []string{"/bad/delete.mp3"}); err == nil {
		t.Fatalf("ApplyScanBatch(bad delete path) = nil error, want error")
	}
	if err := s.ApplyScanBatch(ctx, []Track{makeTrack("../bad_upsert.mp3", "X", "X", "X", "X", t2)}, []string{"keep.mp3"}); err == nil {
		t.Fatalf("ApplyScanBatch(bad upsert path) = nil error, want error")
	}
	if _, err := s.GetTrack(ctx, "tr_keep"); err != nil {
		t.Fatalf("tr_keep missing after pre-tx validation failure: %v", err)
	}
	afterBadSnaps, err := s.LoadSnapshots(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshots() after pre-tx failure error: %v", err)
	}
	if _, exists := afterBadSnaps["new_never.mp3"]; exists {
		t.Errorf("new_never.mp3 persisted despite pre-tx delete validation failure")
	}

	// Mid-transaction PRIMARY KEY(id) collision rolls back both prior delete and prior valid upsert.
	collidingBatch := []Track{
		{ID: "tr_should_rollback", RelPath: "rollback.mp3", FileSize: 1, MtimeNs: 1, Title: "Roll", Artist: "A", AlbumArtist: "A", Album: "Al", CreatedAt: t2},
		{ID: "tr_fixed", RelPath: "different_path_same_id.mp3", FileSize: 1, MtimeNs: 1, Title: "Collide", Artist: "A", AlbumArtist: "A", Album: "Al", CreatedAt: t2},
	}
	if err := s.ApplyScanBatch(ctx, collidingBatch, []string{"keep.mp3"}); err == nil {
		t.Fatalf("ApplyScanBatch(PK collision) = nil error, want error")
	}
	if _, err := s.GetTrack(ctx, "tr_keep"); err != nil {
		t.Errorf("tr_keep was not rolled back after mid-tx failure: %v", err)
	}
	if _, err := s.GetTrack(ctx, "tr_should_rollback"); !errors.Is(err, ErrNotFound) {
		t.Errorf("tr_should_rollback persisted despite mid-tx failure: err=%v", err)
	}

	// Delete-only batch succeeds without upserts.
	if err := s.ApplyScanBatch(ctx, nil, []string{"keep.mp3"}); err != nil {
		t.Fatalf("ApplyScanBatch(delete-only) error: %v", err)
	}
	if _, err := s.GetTrack(ctx, "tr_keep"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTrack(tr_keep) after delete-only batch = %v, want ErrNotFound", err)
	}
}

func TestConcurrency_WALIsolationAndRace(t *testing.T) {
	ctx := context.Background()

	t.Run("held_write_tx_wal_snapshot", func(t *testing.T) {
		s := openTestStore(t)
		ts := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)

		seeded := makeTrack("seeded/01.mp3", "Seed Song", "Seed Artist", "Seed Artist", "Seed Album", ts)
		seeded.ID = "tr_seeded"
		mustUpsert(t, s, seeded)

		seededArtistID := ArtistID(seeded.AlbumArtist)
		seededAlbumID := AlbumID(seeded.AlbumArtist, seeded.Album)

		// Hold the sole writeDB connection open inside an uncommitted write transaction.
		tx, err := s.writeDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("writeDB.BeginTx() error: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		uncommitted := makeTrack("pending/02.mp3", "Pending Song", "Pending Artist", "Pending Artist", "Pending Album", ts.Add(time.Minute))
		uncommitted.ID = "tr_pending"
		p, err := prepareTrackForUpsert(uncommitted)
		if err != nil {
			t.Fatalf("prepareTrackForUpsert() error: %v", err)
		}
		if _, err := tx.ExecContext(ctx, upsertTrackSQL, p.args()...); err != nil {
			t.Fatalf("uncommitted insert error: %v", err)
		}

		// Exercise ALL 7 readDB methods under a bounded timeout; any accidental
		// writeDB usage would block on MaxOpenConns(1) and time out cleanly.
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		count, err := s.TrackCount(readCtx)
		if err != nil {
			t.Fatalf("TrackCount() while write tx held error: %v", err)
		}
		if count != 1 {
			t.Errorf("TrackCount() while write tx held = %d, want 1", count)
		}

		artists, err := s.ListArtists(readCtx)
		if err != nil {
			t.Fatalf("ListArtists() while write tx held error: %v", err)
		}
		if len(artists) != 1 || artists[0].ID != seededArtistID {
			t.Errorf("ListArtists() while write tx held = %+v, want only %q", artists, seededArtistID)
		}

		gotArtist, artistAlbums, err := s.GetArtist(readCtx, seededArtistID)
		if err != nil {
			t.Fatalf("GetArtist(seeded) while write tx held error: %v", err)
		}
		if gotArtist.ID != seededArtistID || len(artistAlbums) != 1 {
			t.Errorf("GetArtist(seeded) = (%+v, len=%d), want seeded artist and 1 album", gotArtist, len(artistAlbums))
		}
		if _, _, err := s.GetArtist(readCtx, p.track.ArtistID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetArtist(uncommitted) = %v, want ErrNotFound", err)
		}

		gotAlbum, albumTracks, err := s.GetAlbum(readCtx, seededAlbumID)
		if err != nil {
			t.Fatalf("GetAlbum(seeded) while write tx held error: %v", err)
		}
		if gotAlbum.ID != seededAlbumID || len(albumTracks) != 1 {
			t.Errorf("GetAlbum(seeded) = (%+v, len=%d), want seeded album and 1 track", gotAlbum, len(albumTracks))
		}
		if _, _, err := s.GetAlbum(readCtx, p.track.AlbumID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetAlbum(uncommitted) = %v, want ErrNotFound", err)
		}

		listedAlbums, err := s.ListAlbums(readCtx, AlbumListQuery{Sort: AlbumSortAlphabeticalByName, Limit: 10})
		if err != nil {
			t.Fatalf("ListAlbums() while write tx held error: %v", err)
		}
		if len(listedAlbums) != 1 || listedAlbums[0].ID != seededAlbumID {
			t.Errorf("ListAlbums() while write tx held = %+v, want only %q", listedAlbums, seededAlbumID)
		}

		gotTrack, err := s.GetTrack(readCtx, seeded.ID)
		if err != nil {
			t.Fatalf("GetTrack(seeded) while write tx held error: %v", err)
		}
		if gotTrack.ID != seeded.ID {
			t.Errorf("GetTrack(seeded).ID = %q, want %q", gotTrack.ID, seeded.ID)
		}
		if _, err := s.GetTrack(readCtx, p.track.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetTrack(uncommitted) = %v, want ErrNotFound", err)
		}

		searchRes, err := s.Search3(readCtx, Search3Query{
			Query:       "",
			ArtistCount: 10,
			AlbumCount:  10,
			SongCount:   10,
		})
		if err != nil {
			t.Fatalf("Search3() while write tx held error: %v", err)
		}
		if len(searchRes.Artists) != 1 || len(searchRes.Albums) != 1 || len(searchRes.Songs) != 1 {
			t.Errorf("Search3() while write tx held counts = (%d,%d,%d), want (1,1,1)",
				len(searchRes.Artists), len(searchRes.Albums), len(searchRes.Songs))
		}

		// Commit the write transaction and confirm readers observe the new row.
		if err := tx.Commit(); err != nil {
			t.Fatalf("tx.Commit() error: %v", err)
		}
		afterCount, err := s.TrackCount(ctx)
		if err != nil {
			t.Fatalf("TrackCount() after commit error: %v", err)
		}
		if afterCount != 2 {
			t.Errorf("TrackCount() after commit = %d, want 2", afterCount)
		}
		if _, err := s.GetTrack(ctx, p.track.ID); err != nil {
			t.Errorf("GetTrack(committed) after commit error: %v", err)
		}
	})

	t.Run("barrier_synchronized_race_stress", func(t *testing.T) {
		s := openTestStore(t)
		ts := time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)

		// Seed stable rows that writers never delete so readers always find valid IDs.
		stable := makeTrack("stable/01.mp3", "Stable Song", "Stable Artist", "Stable Artist", "Stable Album", ts)
		stable.ID = "tr_stable"
		mustUpsert(t, s, stable)
		stableArtistID := ArtistID(stable.AlbumArtist)
		stableAlbumID := AlbumID(stable.AlbumArtist, stable.Album)

		const (
			numReaders = 8
			numWriters = 4
			iterations = 25
		)
		start := make(chan struct{})
		errCh := make(chan error, (numReaders+numWriters)*iterations)

		var wg sync.WaitGroup
		for r := range numReaders {
			wg.Add(1)
			go func(readerID int) {
				defer wg.Done()
				<-start
				for i := range iterations {
					if _, err := s.ListArtists(ctx); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d ListArtists: %w", readerID, i, err)
						return
					}
					if _, _, err := s.GetArtist(ctx, stableArtistID); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d GetArtist: %w", readerID, i, err)
						return
					}
					if _, _, err := s.GetAlbum(ctx, stableAlbumID); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d GetAlbum: %w", readerID, i, err)
						return
					}
					if _, err := s.ListAlbums(ctx, AlbumListQuery{Sort: AlbumSortNewest, Limit: 20}); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d ListAlbums: %w", readerID, i, err)
						return
					}
					if _, err := s.GetTrack(ctx, stable.ID); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d GetTrack: %w", readerID, i, err)
						return
					}
					if _, err := s.Search3(ctx, Search3Query{
						Query:       "Stable",
						ArtistCount: 10,
						AlbumCount:  10,
						SongCount:   10,
					}); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d Search3: %w", readerID, i, err)
						return
					}
					if _, err := s.TrackCount(ctx); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d TrackCount: %w", readerID, i, err)
						return
					}
				}
			}(r)
		}

		for w := range numWriters {
			wg.Add(1)
			go func(writerID int) {
				defer wg.Done()
				<-start
				for i := range iterations {
					relPath := fmt.Sprintf("worker_%d/track_%d.mp3", writerID, i)
					tr := makeTrack(
						relPath,
						fmt.Sprintf("Track %d-%d", writerID, i),
						fmt.Sprintf("Worker %d", writerID),
						fmt.Sprintf("Worker %d", writerID),
						fmt.Sprintf("Album %d", writerID),
						ts.Add(time.Duration(i)*time.Second),
					)
					if i%2 == 0 {
						if err := s.UpsertTrack(ctx, &tr); err != nil {
							errCh <- fmt.Errorf("writer %d iter %d UpsertTrack: %w", writerID, i, err)
							return
						}
					} else {
						prevPath := fmt.Sprintf("worker_%d/track_%d.mp3", writerID, i-1)
						if err := s.ApplyScanBatch(ctx, []Track{tr}, []string{prevPath}); err != nil {
							errCh <- fmt.Errorf("writer %d iter %d ApplyScanBatch: %w", writerID, i, err)
							return
						}
						if err := s.DeleteByRelPath(ctx, relPath); err != nil {
							errCh <- fmt.Errorf("writer %d iter %d DeleteByRelPath: %w", writerID, i, err)
							return
						}
						if i%5 == 0 {
							if err := s.DeleteByDirPrefix(ctx, fmt.Sprintf("worker_%d", writerID)); err != nil {
								errCh <- fmt.Errorf("writer %d iter %d DeleteByDirPrefix: %w", writerID, i, err)
								return
							}
						}
					}
					if _, err := s.LoadSnapshots(ctx); err != nil {
						errCh <- fmt.Errorf("writer %d iter %d LoadSnapshots: %w", writerID, i, err)
						return
					}
				}
			}(w)
		}

		close(start)
		wg.Wait()
		close(errCh)

		for err := range errCh {
			t.Error(err)
		}
	})
}
