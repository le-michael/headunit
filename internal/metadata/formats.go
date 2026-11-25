// Package metadata extracts normalized audio tags and stream properties from
// local audio files and classifies supported audio formats and ignored names.
package metadata

import (
	"path/filepath"
	"strings"
)

type formatSpec struct {
	suffix      string
	contentType string
}

var supportedFormats = map[string]formatSpec{
	".mp3":  {suffix: "mp3", contentType: "audio/mpeg"},
	".flac": {suffix: "flac", contentType: "audio/flac"},
	".m4a":  {suffix: "m4a", contentType: "audio/mp4"},
	".aac":  {suffix: "aac", contentType: "audio/aac"},
	".ogg":  {suffix: "ogg", contentType: "audio/ogg"},
	// Serve .opus as audio/ogg for broader OpenSubsonic player compatibility
	// while preserving the Subsonic suffix "opus".
	".opus": {suffix: "opus", contentType: "audio/ogg"},
	".wav":  {suffix: "wav", contentType: "audio/wav"},
}

var ignoredTempExts = map[string]struct{}{
	".part":       {},
	".tmp":        {},
	".crdownload": {},
	".filepart":   {},
}

// LookupFormat returns the lowercase OpenSubsonic file suffix (without a
// leading dot) and HTTP Content-Type for path. Extension matching is
// case-insensitive.
func LookupFormat(path string) (suffix, contentType string, ok bool) {
	spec, ok := supportedFormats[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return "", "", false
	}
	return spec.suffix, spec.contentType, true
}

// IsIgnoredName reports whether a single directory or file entry name should be
// skipped during library scanning and watching. Callers walking a directory
// tree should exempt the root directory itself.
//
// Empty names, dot-prefixed entries (such as .DS_Store, ._Track.mp3,
// .syncthing.*, and hidden directories), and partial-transfer temporary files
// ending in .part, .tmp, .crdownload, or .filepart (case-insensitive) return
// true.
func IsIgnoredName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	_, ignored := ignoredTempExts[strings.ToLower(filepath.Ext(name))]
	return ignored
}
