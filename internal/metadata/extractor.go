package metadata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.senan.xyz/taglib"
)

const (
	unknownTitle  = "Unknown Title"
	unknownArtist = "Unknown Artist"
	unknownAlbum  = "Unknown Album"
	minYear       = 1000
	maxYear       = 9999
)

// leadingTrackNumRe strips 1-3 digit track prefixes (e.g., "01 - ", "2. ",
// "03_", "4) ", "05 ") while preserving 4-digit years ("1984.mp3") and
// alphanumeric titles ("24K Magic.mp3").
var leadingTrackNumRe = regexp.MustCompile(`^\s*\d{1,3}\s*(?:[-._)]\s*|\s+)`)

// Info holds normalized audio tags and stream properties extracted from a file.
type Info struct {
	// Title is the track title, falling back to the cleaned filename stem or
	// "Unknown Title". Always non-empty.
	Title string

	// Artist is the track-level artist (ARTIST, then ALBUMARTIST, then
	// "Unknown Artist"). Always non-empty.
	Artist string

	// AlbumArtist is the album-grouping artist (ALBUMARTIST, then ARTIST, then
	// "Unknown Artist"). Always non-empty.
	AlbumArtist string

	// Album is the album title, falling back to "Unknown Album". Always non-empty.
	Album string

	// TrackNum is the 1-based track number, or 0 when absent or invalid.
	TrackNum int

	// DiscNum is the 1-based disc number, or 0 when absent or invalid.
	DiscNum int

	// Year is the 4-digit release year in [1000, 9999], or 0 when absent or invalid.
	Year int

	// Genre is the trimmed primary genre tag, or "" when absent.
	Genre string

	// DurationSec is the audio duration rounded to whole seconds, or 0 when unknown.
	DurationSec int

	// BitRate is the audio bitrate in kbps, or 0 when unknown.
	BitRate int
}

// Extract reads and normalizes audio tags and stream properties from absPath
// using pure-Go TagLib (Wasm).
//
// Extract follows a two-tier error contract: OS, permission, non-regular file,
// and unexpected TagLib runtime errors return (Info{}, err). Readable regular
// files that TagLib cannot parse (taglib.ErrInvalidFile) degrade gracefully
// with err == nil. Tags and stream properties are read independently so
// untagged audio retains DurationSec and BitRate.
//
// Callers should filter paths with IsIgnoredName, LookupFormat, and a non-zero
// file size check before calling Extract, because any readable non-audio or
// empty regular file degrades to FallbackFromFilename with err == nil.
//
// Extract resolves symlinks before invoking TagLib so cross-directory symlinks
// remain accessible within Wazero's per-directory WASI mount, while preserving
// absPath for error messages and filename fallback. Extract is safe for
// concurrent use.
func Extract(absPath string) (Info, error) {
	return extract(absPath, taglib.ReadTags, taglib.ReadProperties)
}

func extract(
	absPath string,
	readTags func(string) (map[string][]string, error),
	readProps func(string) (taglib.Properties, error),
) (Info, error) {
	if err := checkReadableRegularFile(absPath); err != nil {
		return Info{}, err
	}

	readPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return Info{}, fmt.Errorf("resolve %q: %w", absPath, err)
	}

	tags, err := readTags(readPath)
	if err != nil {
		if !errors.Is(err, taglib.ErrInvalidFile) {
			return Info{}, fmt.Errorf("read tags %q: %w", absPath, err)
		}
		if err := checkReadableRegularFile(absPath); err != nil {
			return Info{}, err
		}
		tags = nil
	}

	props, err := readProps(readPath)
	if err != nil {
		if !errors.Is(err, taglib.ErrInvalidFile) {
			return Info{}, fmt.Errorf("read properties %q: %w", absPath, err)
		}
		if err := checkReadableRegularFile(absPath); err != nil {
			return Info{}, err
		}
		props = taglib.Properties{}
	} else if hasNoProperties(props) {
		// taglib.ReadProperties returns zero Properties with nil error for
		// missing, unreadable, or unparseable files; re-check readability to
		// catch TOCTOU removal or permission loss after ReadTags.
		if err := checkReadableRegularFile(absPath); err != nil {
			return Info{}, err
		}
	}

	return normalize(absPath, tags, props), nil
}

func hasNoProperties(p taglib.Properties) bool {
	return p.Format == "" &&
		p.InnerCodec == "" &&
		p.Length == 0 &&
		p.Channels == 0 &&
		p.SampleRate == 0 &&
		p.BitRate == 0 &&
		p.BitDepth == 0
}

// FallbackFromFilename builds a minimal Info from absPath when embedded tags
// are absent or unreadable, deriving Title from the cleaned filename stem and
// filling default "Unknown Artist" and "Unknown Album" values.
func FallbackFromFilename(absPath string) Info {
	return Info{
		Title:       titleFromFilename(absPath),
		Artist:      unknownArtist,
		AlbumArtist: unknownArtist,
		Album:       unknownAlbum,
	}
}

func checkReadableRegularFile(absPath string) error {
	st, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat %q: %w", absPath, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %q", absPath)
	}
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("open %q: %w", absPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", absPath, err)
	}
	return nil
}

func normalize(absPath string, tags map[string][]string, props taglib.Properties) Info {
	title := firstTagValue(tags[taglib.Title])
	if title == "" {
		title = titleFromFilename(absPath)
	}

	rawArtist := firstTagValue(tags[taglib.Artist])
	rawAlbumArtist := firstTagValue(tags[taglib.AlbumArtist])

	artist := rawArtist
	if artist == "" {
		artist = rawAlbumArtist
	}
	if artist == "" {
		artist = unknownArtist
	}

	albumArtist := rawAlbumArtist
	if albumArtist == "" {
		albumArtist = rawArtist
	}
	if albumArtist == "" {
		albumArtist = unknownArtist
	}

	album := firstTagValue(tags[taglib.Album])
	if album == "" {
		album = unknownAlbum
	}

	var durationSec int
	if props.Length > 0 {
		durationSec = int(props.Length.Round(time.Second) / time.Second)
	}

	return Info{
		Title:       title,
		Artist:      artist,
		AlbumArtist: albumArtist,
		Album:       album,
		TrackNum:    parseIndex(firstTagValue(tags[taglib.TrackNumber])),
		DiscNum:     parseIndex(firstTagValue(tags[taglib.DiscNumber])),
		Year:        parseYear(tags),
		Genre:       firstTagValue(tags[taglib.Genre]),
		DurationSec: durationSec,
		BitRate:     int(props.BitRate),
	}
}

func titleFromFilename(absPath string) string {
	if strings.TrimSpace(absPath) == "" {
		return unknownTitle
	}
	base := filepath.Base(absPath)
	if base == "." || base == ".." || base == string(filepath.Separator) || strings.TrimSpace(base) == "" {
		return unknownTitle
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	cleaned := strings.TrimSpace(leadingTrackNumRe.ReplaceAllString(stem, ""))
	if cleaned != "" {
		return cleaned
	}
	if trimmedStem := strings.TrimSpace(stem); trimmedStem != "" {
		return trimmedStem
	}
	return strings.TrimSpace(base)
}

func firstTagValue(values []string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseIndex(raw string) int {
	head, _, _ := strings.Cut(raw, "/")
	n, err := strconv.Atoi(strings.TrimSpace(head))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func parseYear(tags map[string][]string) int {
	for _, key := range [...]string{taglib.Date, "YEAR"} {
		for _, raw := range tags[key] {
			if year := extractYear(raw); year != 0 {
				return year
			}
		}
	}
	return 0
}

func extractYear(raw string) int {
	s := strings.TrimSpace(raw)
	if len(s) < 4 {
		return 0
	}
	if len(s) > 4 && s[4] >= '0' && s[4] <= '9' {
		return 0
	}
	for i := range 4 {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
	}
	year, err := strconv.Atoi(s[:4])
	if err != nil || year < minYear || year > maxYear {
		return 0
	}
	return year
}
