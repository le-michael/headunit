package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.senan.xyz/taglib"

	"headunit/internal/testutil"
)

func TestLookupFormat(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantSuf  string
		wantMIME string
		wantOK   bool
	}{
		{name: "mp3", path: "/music/song.mp3", wantSuf: "mp3", wantMIME: "audio/mpeg", wantOK: true},
		{name: "flac", path: "/music/song.flac", wantSuf: "flac", wantMIME: "audio/flac", wantOK: true},
		{name: "m4a", path: "/music/song.m4a", wantSuf: "m4a", wantMIME: "audio/mp4", wantOK: true},
		{name: "aac", path: "/music/song.aac", wantSuf: "aac", wantMIME: "audio/aac", wantOK: true},
		{name: "ogg", path: "/music/song.ogg", wantSuf: "ogg", wantMIME: "audio/ogg", wantOK: true},
		{name: "opus uses audio/ogg", path: "/music/song.opus", wantSuf: "opus", wantMIME: "audio/ogg", wantOK: true},
		{name: "wav", path: "/music/song.wav", wantSuf: "wav", wantMIME: "audio/wav", wantOK: true},
		{name: "uppercase MP3", path: "/music/TRACK.MP3", wantSuf: "mp3", wantMIME: "audio/mpeg", wantOK: true},
		{name: "mixed case FlAc", path: "/music/Track.FlAc", wantSuf: "flac", wantMIME: "audio/flac", wantOK: true},
		{name: "mixed case Opus", path: "/music/Track.OpUs", wantSuf: "opus", wantMIME: "audio/ogg", wantOK: true},
		{name: "multi-dot filename", path: "/music/01.artist.v2.track.wav", wantSuf: "wav", wantMIME: "audio/wav", wantOK: true},
		{name: "unsupported image", path: "/music/cover.jpg", wantOK: false},
		{name: "unsupported wma", path: "/music/track.wma", wantOK: false},
		{name: "unsupported text", path: "/music/notes.txt", wantOK: false},
		{name: "no extension", path: "/music/track", wantOK: false},
		{name: "empty path", path: "", wantOK: false},
		{name: "trailing slash", path: "/music/album.mp3/", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotSuf, gotMIME, gotOK := LookupFormat(tc.path)
			if gotSuf != tc.wantSuf || gotMIME != tc.wantMIME || gotOK != tc.wantOK {
				t.Errorf("LookupFormat(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.path, gotSuf, gotMIME, gotOK, tc.wantSuf, tc.wantMIME, tc.wantOK)
			}
		})
	}
}

func TestIsIgnoredName(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		want  bool
	}{
		{name: "empty name", entry: "", want: true},
		{name: "dot", entry: ".", want: true},
		{name: "dotdot", entry: "..", want: true},
		{name: "DS_Store", entry: ".DS_Store", want: true},
		{name: "AppleDouble", entry: "._Track.mp3", want: true},
		{name: "syncthing temp", entry: ".syncthing.track.flac.tmp", want: true},
		{name: "hidden dir", entry: ".git", want: true},
		{name: "part suffix", entry: "song.flac.part", want: true},
		{name: "tmp suffix", entry: "song.mp3.tmp", want: true},
		{name: "crdownload suffix", entry: "song.m4a.crdownload", want: true},
		{name: "filepart suffix", entry: "song.wav.filepart", want: true},
		{name: "uppercase PART", entry: "SONG.FLAC.PART", want: true},
		{name: "uppercase TMP", entry: "SONG.TMP", want: true},
		{name: "mixed case CrDownload", entry: "album.CrDownload", want: true},
		{name: "mixed case FilePart", entry: "track.FilePart", want: true},
		{name: "normal audio file", entry: "01 - Song.mp3", want: false},
		{name: "part in middle of name", entry: "my.part.mp3", want: false},
		{name: "bare part name", entry: "part", want: false},
		{name: "bare tmp name", entry: "tmp", want: false},
		{name: "regular directory", entry: "RegularDir", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIgnoredName(tc.entry); got != tc.want {
				t.Errorf("IsIgnoredName(%q) = %v, want %v", tc.entry, got, tc.want)
			}
		})
	}
}

func TestFallbackFromFilename(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantTitle string
	}{
		{name: "dash separator", path: "/music/01 - Song Name.flac", wantTitle: "Song Name"},
		{name: "dot separator", path: "/music/1. Track One.mp3", wantTitle: "Track One"},
		{name: "underscore separator", path: "/music/02_Underscore Track.wav", wantTitle: "Underscore Track"},
		{name: "paren separator", path: "/music/03) Paren Track.m4a", wantTitle: "Paren Track"},
		{name: "space separator", path: "/music/04 Space Track.ogg", wantTitle: "Space Track"},
		{name: "three-digit track number", path: "/music/100 - Bonus Track.opus", wantTitle: "Bonus Track"},
		{name: "leading whitespace before track number", path: "/music/  05 - Spaced.mp3", wantTitle: "Spaced"},
		{name: "four-digit year only preserved", path: "/music/1984.mp3", wantTitle: "1984"},
		{name: "four-digit year prefix preserved", path: "/music/1999 - Prince.mp3", wantTitle: "1999 - Prince"},
		{name: "alphanumeric leading digits preserved", path: "/music/24K Magic.mp3", wantTitle: "24K Magic"},
		{name: "track number only preserved", path: "/music/01.mp3", wantTitle: "01"},
		{name: "prefix only falls back to trimmed stem", path: "/music/01 - .flac", wantTitle: "01 -"},
		{name: "dots in stem", path: "/music/01 - Track.v2.final.mp3", wantTitle: "Track.v2.final"},
		{name: "no extension", path: "/music/07 - No Extension", wantTitle: "No Extension"},
		{name: "dotfile with empty stem falls back to base", path: "/music/.mp3", wantTitle: ".mp3"},
		{name: "empty path", path: "", wantTitle: "Unknown Title"},
		{name: "whitespace path", path: "   ", wantTitle: "Unknown Title"},
		{name: "whitespace basename", path: "/music/   ", wantTitle: "Unknown Title"},
		{name: "dotdot", path: "..", wantTitle: "Unknown Title"},
		{name: "dot", path: ".", wantTitle: "Unknown Title"},
		{name: "root separator", path: "/", wantTitle: "Unknown Title"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := Info{
				Title:       tc.wantTitle,
				Artist:      "Unknown Artist",
				AlbumArtist: "Unknown Artist",
				Album:       "Unknown Album",
			}
			if got := FallbackFromFilename(tc.path); got != want {
				t.Errorf("FallbackFromFilename(%q) = %+v, want %+v", tc.path, got, want)
			}
		})
	}
}

func TestExtract_RealFixtures(t *testing.T) {
	t.Run("tagged MP3 full fields", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "03 - Ignored Filename.mp3")
		testutil.WriteMinimalMP3(t, p)
		testutil.WriteTags(t, p, map[string][]string{
			taglib.Title:       {"Get Lucky"},
			taglib.Artist:      {"Daft Punk feat. Pharrell Williams"},
			taglib.AlbumArtist: {"Daft Punk"},
			taglib.Album:       {"Random Access Memories"},
			taglib.TrackNumber: {"03/12"},
			taglib.DiscNumber:  {"2/3"},
			taglib.Date:        {"2013-05-17"},
			taglib.Genre:       {"Disco"},
		})

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		want := Info{
			Title:       "Get Lucky",
			Artist:      "Daft Punk feat. Pharrell Williams",
			AlbumArtist: "Daft Punk",
			Album:       "Random Access Memories",
			TrackNum:    3,
			DiscNum:     2,
			Year:        2013,
			Genre:       "Disco",
			DurationSec: 2,
			BitRate:     128,
		}
		if got != want {
			t.Errorf("Extract(%q) = %+v, want %+v", p, got, want)
		}
	})

	t.Run("tagged FLAC compilation album artist only", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "01 - Track.flac")
		testutil.WriteMinimalFLAC(t, p)
		testutil.WriteTags(t, p, map[string][]string{
			taglib.Title:       {"Compilation Track"},
			taglib.AlbumArtist: {"Various Artists"},
			taglib.Album:       {"Late Night Mix"},
		})

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		if got.Artist != "Various Artists" || got.AlbumArtist != "Various Artists" {
			t.Errorf("artist fields = (%q, %q), want (%q, %q)", got.Artist, got.AlbumArtist, "Various Artists", "Various Artists")
		}
		if got.Title != "Compilation Track" || got.Album != "Late Night Mix" {
			t.Errorf("title/album = (%q, %q), want (%q, %q)", got.Title, got.Album, "Compilation Track", "Late Night Mix")
		}
		if got.DurationSec != 2 {
			t.Errorf("DurationSec = %d, want 2", got.DurationSec)
		}
	})

	t.Run("tagged FLAC artist only fills album artist", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "02 - Blue.flac")
		testutil.WriteMinimalFLAC(t, p)
		testutil.WriteTags(t, p, map[string][]string{
			taglib.Title:  {"So What"},
			taglib.Artist: {"Miles Davis"},
			taglib.Album:  {"Kind of Blue"},
		})

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		if got.Artist != "Miles Davis" || got.AlbumArtist != "Miles Davis" {
			t.Errorf("artist fields = (%q, %q), want (%q, %q)", got.Artist, got.AlbumArtist, "Miles Davis", "Miles Davis")
		}
		if got.DurationSec != 2 {
			t.Errorf("DurationSec = %d, want 2", got.DurationSec)
		}
	})

	t.Run("tagged FLAC YEAR fallback and invalid dates", func(t *testing.T) {
		tests := []struct {
			name     string
			tags     map[string][]string
			wantYear int
		}{
			{
				name:     "YEAR fallback without DATE",
				tags:     map[string][]string{"YEAR": {"1997"}},
				wantYear: 1997,
			},
			{
				name:     "unparseable DATE falls back to YEAR",
				tags:     map[string][]string{taglib.Date: {"99"}, "YEAR": {"2004"}},
				wantYear: 2004,
			},
			{
				name:     "invalid DATE and YEAR produce zero",
				tags:     map[string][]string{taglib.Date: {"99"}, "YEAR": {"20245"}},
				wantYear: 0,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "01 - Year Test.flac")
				testutil.WriteMinimalFLAC(t, p)
				testutil.WriteTags(t, p, tc.tags)

				got, err := Extract(p)
				if err != nil {
					t.Fatalf("Extract(%q) unexpected error: %v", p, err)
				}
				if got.Year != tc.wantYear {
					t.Errorf("Year = %d, want %d", got.Year, tc.wantYear)
				}
			})
		}
	})

	t.Run("tagged FLAC whitespace and malformed tags", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "04 - Clean Fallback.flac")
		testutil.WriteMinimalFLAC(t, p)
		testutil.WriteTags(t, p, map[string][]string{
			taglib.Title:       {"   "},
			taglib.Artist:      {"  Trimmed Artist  "},
			taglib.Album:       {"\t  "},
			taglib.TrackNumber: {"-5/10"},
			taglib.DiscNumber:  {"abc/2"},
			taglib.Date:        {"0999"},
			taglib.Genre:       {"  ", "  Jazz  ", "Funk"},
		})

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		if got.Title != "Clean Fallback" {
			t.Errorf("Title = %q, want %q", got.Title, "Clean Fallback")
		}
		if got.Artist != "Trimmed Artist" || got.AlbumArtist != "Trimmed Artist" {
			t.Errorf("artist fields = (%q, %q), want (%q, %q)", got.Artist, got.AlbumArtist, "Trimmed Artist", "Trimmed Artist")
		}
		if got.Album != "Unknown Album" {
			t.Errorf("Album = %q, want %q", got.Album, "Unknown Album")
		}
		if got.TrackNum != 0 || got.DiscNum != 0 || got.Year != 0 {
			t.Errorf("numerics = (track=%d, disc=%d, year=%d), want (0, 0, 0)", got.TrackNum, got.DiscNum, got.Year)
		}
		if got.Genre != "Jazz" {
			t.Errorf("Genre = %q, want %q", got.Genre, "Jazz")
		}
		if got.DurationSec != 2 {
			t.Errorf("DurationSec = %d, want 2", got.DurationSec)
		}
	})

	t.Run("untagged MP3 retains duration and bitrate", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "01 - Raw Beats.mp3")
		testutil.WriteMinimalMP3(t, p)

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		want := Info{
			Title:       "Raw Beats",
			Artist:      "Unknown Artist",
			AlbumArtist: "Unknown Artist",
			Album:       "Unknown Album",
			DurationSec: 2,
			BitRate:     128,
		}
		if got != want {
			t.Errorf("Extract(%q) = %+v, want %+v", p, got, want)
		}
	})

	t.Run("untagged FLAC retains duration and bitrate", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "01 - Song Name.flac")
		testutil.WriteMinimalFLAC(t, p)

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		if got.Title != "Song Name" || got.Artist != "Unknown Artist" || got.AlbumArtist != "Unknown Artist" || got.Album != "Unknown Album" {
			t.Errorf("fallback metadata = %+v, want Song Name / Unknown Artist / Unknown Album", got)
		}
		if got.DurationSec != 2 || got.BitRate != 64 {
			t.Errorf("properties = (DurationSec=%d, BitRate=%d), want (2, 64)", got.DurationSec, got.BitRate)
		}
	})

	t.Run("untagged WAV retains duration and bitrate", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "01 - Pure Wave.wav")
		testutil.WriteMinimalWAV(t, p)

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		want := Info{
			Title:       "Pure Wave",
			Artist:      "Unknown Artist",
			AlbumArtist: "Unknown Artist",
			Album:       "Unknown Album",
			DurationSec: 2,
			BitRate:     128,
		}
		if got != want {
			t.Errorf("Extract(%q) = %+v, want %+v", p, got, want)
		}
	})

	t.Run("tagged WAV preserves tags and properties", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "09 - Ignored.wav")
		testutil.WriteMinimalWAV(t, p)
		testutil.WriteTags(t, p, map[string][]string{
			taglib.Title:       {"Studio Master"},
			taglib.Artist:      {"Wave Artist"},
			taglib.AlbumArtist: {"Wave Artist"},
			taglib.Album:       {"Test Pressing"},
			taglib.TrackNumber: {"1/4"},
			taglib.Date:        {"2022"},
		})

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		want := Info{
			Title:       "Studio Master",
			Artist:      "Wave Artist",
			AlbumArtist: "Wave Artist",
			Album:       "Test Pressing",
			TrackNum:    1,
			Year:        2022,
			DurationSec: 2,
			BitRate:     128,
		}
		if got != want {
			t.Errorf("Extract(%q) = %+v, want %+v", p, got, want)
		}
	})

	t.Run("cross-directory symlink extracts tags and preserves link name for fallback", func(t *testing.T) {
		root := t.TempDir()
		targetDir := filepath.Join(root, "real")
		linkDir := filepath.Join(root, "linked")
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q): %v", targetDir, err)
		}
		if err := os.MkdirAll(linkDir, 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q): %v", linkDir, err)
		}

		realMP3 := filepath.Join(targetDir, "source.mp3")
		testutil.WriteMinimalMP3(t, realMP3)
		testutil.WriteTags(t, realMP3, map[string][]string{
			taglib.Title:  {"Linked Song"},
			taglib.Artist: {"Linked Artist"},
		})
		linkMP3 := filepath.Join(linkDir, "01 - Alias.mp3")
		if err := os.Symlink(realMP3, linkMP3); err != nil {
			t.Fatalf("os.Symlink(%q, %q): %v", realMP3, linkMP3, err)
		}

		gotMP3, err := Extract(linkMP3)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", linkMP3, err)
		}
		if gotMP3.Title != "Linked Song" || gotMP3.Artist != "Linked Artist" || gotMP3.DurationSec != 2 || gotMP3.BitRate != 128 {
			t.Errorf("Extract(symlink MP3) = %+v, want Linked Song / Linked Artist / 2s / 128kbps", gotMP3)
		}

		realWAV := filepath.Join(targetDir, "raw.wav")
		testutil.WriteMinimalWAV(t, realWAV)
		linkWAV := filepath.Join(linkDir, "02 - Link Fallback Name.wav")
		if err := os.Symlink(realWAV, linkWAV); err != nil {
			t.Fatalf("os.Symlink(%q, %q): %v", realWAV, linkWAV, err)
		}

		gotWAV, err := Extract(linkWAV)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", linkWAV, err)
		}
		if gotWAV.Title != "Link Fallback Name" || gotWAV.DurationSec != 2 || gotWAV.BitRate != 128 {
			t.Errorf("Extract(symlink WAV) = %+v, want Title=%q / 2s / 128kbps", gotWAV, "Link Fallback Name")
		}
	})

	t.Run("corrupt regular file degrades to filename fallback", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "02 - Corrupt Track.aac")
		if err := os.WriteFile(p, []byte("not valid audio bytes"), 0o600); err != nil {
			t.Fatalf("os.WriteFile(%q): %v", p, err)
		}

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		want := FallbackFromFilename(p)
		if got != want {
			t.Errorf("Extract(%q) = %+v, want %+v", p, got, want)
		}
	})

	t.Run("empty regular file degrades to filename fallback", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "03 - Empty.mp3")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatalf("os.WriteFile(%q): %v", p, err)
		}

		got, err := Extract(p)
		if err != nil {
			t.Fatalf("Extract(%q) unexpected error: %v", p, err)
		}
		want := FallbackFromFilename(p)
		if got != want {
			t.Errorf("Extract(%q) = %+v, want %+v", p, got, want)
		}
	})
}

func TestExtract_PartialAndUnexpectedErrors(t *testing.T) {
	newTempFile := func(t *testing.T, name string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte("stub"), 0o600); err != nil {
			t.Fatalf("os.WriteFile(%q): %v", p, err)
		}
		return p
	}

	t.Run("readTags ErrInvalidFile retains properties", func(t *testing.T) {
		p := newTempFile(t, "05 - Untagged Audio.aac")
		got, err := extract(
			p,
			func(string) (map[string][]string, error) { return nil, taglib.ErrInvalidFile },
			func(string) (taglib.Properties, error) {
				return taglib.Properties{Length: 185 * time.Second, BitRate: 256}, nil
			},
		)
		if err != nil {
			t.Fatalf("extract() unexpected error: %v", err)
		}
		want := Info{
			Title:       "Untagged Audio",
			Artist:      "Unknown Artist",
			AlbumArtist: "Unknown Artist",
			Album:       "Unknown Album",
			DurationSec: 185,
			BitRate:     256,
		}
		if got != want {
			t.Errorf("extract() = %+v, want %+v", got, want)
		}
	})

	t.Run("readProps ErrInvalidFile preserves tags", func(t *testing.T) {
		p := newTempFile(t, "06 - Tagged Only.mp3")
		got, err := extract(
			p,
			func(string) (map[string][]string, error) {
				return map[string][]string{
					taglib.Title:  {"Tagged Only"},
					taglib.Artist: {"Artist"},
					taglib.Album:  {"Album"},
				}, nil
			},
			func(string) (taglib.Properties, error) { return taglib.Properties{}, taglib.ErrInvalidFile },
		)
		if err != nil {
			t.Fatalf("extract() unexpected error: %v", err)
		}
		want := Info{
			Title:       "Tagged Only",
			Artist:      "Artist",
			AlbumArtist: "Artist",
			Album:       "Album",
		}
		if got != want {
			t.Errorf("extract() = %+v, want %+v", got, want)
		}
	})

	t.Run("TOCTOU deletion during readTags returns ErrNotExist without calling readProps", func(t *testing.T) {
		p := newTempFile(t, "01 - Vanishing.mp3")
		readPropsCalled := false

		_, err := extract(
			p,
			func(string) (map[string][]string, error) {
				if rmErr := os.Remove(p); rmErr != nil {
					t.Errorf("os.Remove(%q): %v", p, rmErr)
				}
				return nil, taglib.ErrInvalidFile
			},
			func(string) (taglib.Properties, error) {
				readPropsCalled = true
				return taglib.Properties{}, nil
			},
		)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("extract() error = %v, want errors.Is(_, os.ErrNotExist)", err)
		}
		if readPropsCalled {
			t.Errorf("readProps was called after TOCTOU deletion in readTags, want skipped")
		}
	})

	t.Run("TOCTOU deletion before real ReadProperties returns ErrNotExist", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "02 - Vanishing.mp3")
		testutil.WriteMinimalMP3(t, p)

		_, err := extract(
			p,
			taglib.ReadTags,
			func(readPath string) (taglib.Properties, error) {
				if rmErr := os.Remove(p); rmErr != nil {
					t.Errorf("os.Remove(%q): %v", p, rmErr)
				}
				return taglib.ReadProperties(readPath)
			},
		)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("extract() error = %v, want errors.Is(_, os.ErrNotExist)", err)
		}
	})

	t.Run("TOCTOU deletion during readProps ErrInvalidFile returns ErrNotExist", func(t *testing.T) {
		p := newTempFile(t, "02 - Vanishing.mp3")

		_, err := extract(
			p,
			func(string) (map[string][]string, error) { return map[string][]string{}, nil },
			func(string) (taglib.Properties, error) {
				if rmErr := os.Remove(p); rmErr != nil {
					t.Errorf("os.Remove(%q): %v", p, rmErr)
				}
				return taglib.Properties{}, taglib.ErrInvalidFile
			},
		)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("extract() error = %v, want errors.Is(_, os.ErrNotExist)", err)
		}
	})

	t.Run("unexpected error from readTags propagates", func(t *testing.T) {
		p := newTempFile(t, "03 - Boom.mp3")
		sentinel := errors.New("wasm runtime init failed")
		readPropsCalled := false

		_, err := extract(
			p,
			func(string) (map[string][]string, error) { return nil, sentinel },
			func(string) (taglib.Properties, error) {
				readPropsCalled = true
				return taglib.Properties{}, nil
			},
		)
		if !errors.Is(err, sentinel) {
			t.Fatalf("extract() error = %v, want errors.Is(_, %v)", err, sentinel)
		}
		if readPropsCalled {
			t.Errorf("readProps was called after unexpected readTags error, want skipped")
		}
	})

	t.Run("unexpected error from readProps propagates", func(t *testing.T) {
		p := newTempFile(t, "04 - Boom.mp3")
		sentinel := errors.New("wasm call failed")

		_, err := extract(
			p,
			func(string) (map[string][]string, error) { return map[string][]string{}, nil },
			func(string) (taglib.Properties, error) { return taglib.Properties{}, sentinel },
		)
		if !errors.Is(err, sentinel) {
			t.Fatalf("extract() error = %v, want errors.Is(_, %v)", err, sentinel)
		}
	})

	t.Run("duration rounding boundaries", func(t *testing.T) {
		tests := []struct {
			name    string
			length  time.Duration
			wantSec int
		}{
			{name: "zero", length: 0, wantSec: 0},
			{name: "negative clamped to zero", length: -500 * time.Millisecond, wantSec: 0},
			{name: "499ms rounds down to 0", length: 499 * time.Millisecond, wantSec: 0},
			{name: "500ms rounds up to 1", length: 500 * time.Millisecond, wantSec: 1},
			{name: "1499ms rounds down to 1", length: 1499 * time.Millisecond, wantSec: 1},
			{name: "1500ms rounds up to 2", length: 1500 * time.Millisecond, wantSec: 2},
		}

		p := newTempFile(t, "01 - Rounding.mp3")
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got, err := extract(
					p,
					func(string) (map[string][]string, error) { return nil, nil },
					func(string) (taglib.Properties, error) {
						return taglib.Properties{Length: tc.length}, nil
					},
				)
				if err != nil {
					t.Fatalf("extract() unexpected error: %v", err)
				}
				if got.DurationSec != tc.wantSec {
					t.Errorf("DurationSec for %v = %d, want %d", tc.length, got.DurationSec, tc.wantSec)
				}
			})
		}
	})

	t.Run("DATE and YEAR boundary cases", func(t *testing.T) {
		tests := []struct {
			name     string
			tags     map[string][]string
			wantYear int
		}{
			{name: "lower bound 1000", tags: map[string][]string{taglib.Date: {"1000"}}, wantYear: 1000},
			{name: "upper bound 9999", tags: map[string][]string{taglib.Date: {"9999"}}, wantYear: 9999},
			{name: "below lower bound 0999", tags: map[string][]string{taglib.Date: {"0999"}}, wantYear: 0},
			{name: "five digits rejected", tags: map[string][]string{taglib.Date: {"20135"}}, wantYear: 0},
			{name: "signed prefix rejected", tags: map[string][]string{taglib.Date: {"+202"}}, wantYear: 0},
			{name: "padded ISO date", tags: map[string][]string{taglib.Date: {"  2024-02-29  "}}, wantYear: 2024},
			{name: "multi-value slice skips blank and invalid", tags: map[string][]string{taglib.Date: {"   ", "n/a", "1988-09-01"}}, wantYear: 1988},
			{name: "DATE takes precedence over YEAR", tags: map[string][]string{taglib.Date: {"2015-01-01"}, "YEAR": {"2010"}}, wantYear: 2015},
			{name: "invalid DATE falls back to YEAR", tags: map[string][]string{taglib.Date: {"bad"}, "YEAR": {"  1995  "}}, wantYear: 1995},
		}

		p := newTempFile(t, "01 - Years.mp3")
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got, err := extract(
					p,
					func(string) (map[string][]string, error) { return tc.tags, nil },
					func(string) (taglib.Properties, error) { return taglib.Properties{}, nil },
				)
				if err != nil {
					t.Fatalf("extract() unexpected error: %v", err)
				}
				if got.Year != tc.wantYear {
					t.Errorf("Year = %d, want %d", got.Year, tc.wantYear)
				}
			})
		}
	})
}

func TestExtract_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "does-not-exist.mp3")
		_, err := Extract(p)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Extract(%q) error = %v, want errors.Is(_, os.ErrNotExist)", p, err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := Extract(""); err == nil {
			t.Fatalf("Extract(%q) succeeded, want error", "")
		}
	})

	t.Run("directory path", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Extract(dir); err == nil {
			t.Fatalf("Extract(%q) succeeded on directory, want error", dir)
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permission checks")
		}
		p := filepath.Join(t.TempDir(), "01 - Locked.mp3")
		testutil.WriteMinimalMP3(t, p)
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatalf("os.Chmod(%q, 0o000): %v", p, err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(p, 0o600)
		})

		_, err := Extract(p)
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("Extract(%q) error = %v, want errors.Is(_, os.ErrPermission)", p, err)
		}
	})
}

func TestExtract_Concurrent(t *testing.T) {
	dir := t.TempDir()

	mp3Path := filepath.Join(dir, "01 - MP3.mp3")
	testutil.WriteMinimalMP3(t, mp3Path)
	testutil.WriteTags(t, mp3Path, map[string][]string{
		taglib.Title:  {"Concurrent MP3"},
		taglib.Artist: {"MP3 Artist"},
		taglib.Album:  {"MP3 Album"},
	})

	flacPath := filepath.Join(dir, "02 - FLAC.flac")
	testutil.WriteMinimalFLAC(t, flacPath)
	testutil.WriteTags(t, flacPath, map[string][]string{
		taglib.Title:  {"Concurrent FLAC"},
		taglib.Artist: {"FLAC Artist"},
		taglib.Album:  {"FLAC Album"},
	})

	wavPath := filepath.Join(dir, "03 - Concurrent WAV.wav")
	testutil.WriteMinimalWAV(t, wavPath)

	corruptPath := filepath.Join(dir, "04 - Corrupt.aac")
	if err := os.WriteFile(corruptPath, []byte("corrupt payload"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", corruptPath, err)
	}

	type target struct {
		path      string
		wantTitle string
		wantDur   int
	}
	targets := []target{
		{path: mp3Path, wantTitle: "Concurrent MP3", wantDur: 2},
		{path: flacPath, wantTitle: "Concurrent FLAC", wantDur: 2},
		{path: wavPath, wantTitle: "Concurrent WAV", wantDur: 2},
		{path: corruptPath, wantTitle: "Corrupt", wantDur: 0},
	}

	const workers = 12
	results := make([]Info, workers)
	errs := make([]error, workers)
	start := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(id int) {
			defer wg.Done()
			<-start
			results[id], errs[id] = Extract(targets[id%len(targets)].path)
		}(i)
	}

	close(start)
	wg.Wait()

	for i := range workers {
		want := targets[i%len(targets)]
		if errs[i] != nil {
			t.Errorf("worker %d Extract(%q) unexpected error: %v", i, want.path, errs[i])
			continue
		}
		if results[i].Title != want.wantTitle || results[i].DurationSec != want.wantDur {
			t.Errorf("worker %d Extract(%q) = (Title=%q, DurationSec=%d), want (%q, %d)",
				i, want.path, results[i].Title, results[i].DurationSec, want.wantTitle, want.wantDur)
		}
	}
}
