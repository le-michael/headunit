package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	artistIDPrefix = "ar_"
	albumIDPrefix  = "al_"
	trackIDPrefix  = "tr_"

	derivedIDBytes = 8
	trackIDBytes   = 16
)

// ArtistID derives a deterministic, case- and whitespace-normalized artist
// identifier ("ar_" + 16 hex chars) from albumArtist.
func ArtistID(albumArtist string) string {
	norm := strings.ToLower(strings.TrimSpace(albumArtist))
	sum := sha256.Sum256([]byte(norm))
	return artistIDPrefix + hex.EncodeToString(sum[:derivedIDBytes])
}

// AlbumID derives a deterministic, case- and whitespace-normalized album
// identifier ("al_" + 16 hex chars) from albumArtist and album.
func AlbumID(albumArtist, album string) string {
	normArtist := strings.ToLower(strings.TrimSpace(albumArtist))
	normAlbum := strings.ToLower(strings.TrimSpace(album))
	// Separate normalized inputs with NUL so boundary shifts like ("a", "bc")
	// and ("ab", "c") hash to distinct IDs.
	sum := sha256.Sum256([]byte(normArtist + "\x00" + normAlbum))
	return albumIDPrefix + hex.EncodeToString(sum[:derivedIDBytes])
}

// NewTrackID generates an opaque random track identifier ("tr_" + 32 hex chars)
// using crypto/rand.
func NewTrackID() (string, error) {
	var buf [trackIDBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("store: generate track id: %w", err)
	}
	return trackIDPrefix + hex.EncodeToString(buf[:]), nil
}
