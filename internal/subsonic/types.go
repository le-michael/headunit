// Package subsonic implements Headunit's OpenSubsonic v1.16.1 HTTP API.
package subsonic

import "encoding/xml"

const (
	apiVersion       = "1.16.1"
	serverType       = "headunit"
	serverVersion    = "0.1.0"
	xmlNamespace     = "http://subsonic.org/restapi"
	statusOK         = "ok"
	statusFailed     = "failed"
	defaultUsername  = "headunit"
	errCodeGeneric   = 0
	errCodeNotFound  = 70
	maxFormBodyBytes = 1 << 20 // 1 MiB
)

type jsonEnvelope struct {
	SubsonicResponse Response `json:"subsonic-response"`
}

// Response is the root Subsonic v1.16.1 / OpenSubsonic response envelope.
type Response struct {
	// XMLName sets the XML root element name to subsonic-response.
	XMLName xml.Name `xml:"subsonic-response" json:"-"`
	// Xmlns specifies the Subsonic REST XML namespace attribute.
	Xmlns string `xml:"xmlns,attr" json:"-"`
	// Status reports whether the request succeeded ("ok") or failed ("failed").
	Status string `xml:"status,attr" json:"status"`
	// Version reports the implemented Subsonic REST protocol version.
	Version string `xml:"version,attr" json:"version"`
	// Type identifies the server implementation name.
	Type string `xml:"type,attr" json:"type"`
	// ServerVersion reports the Headunit server version.
	ServerVersion string `xml:"serverVersion,attr" json:"serverVersion"`
	// OpenSubsonic advertises OpenSubsonic support on every response.
	OpenSubsonic bool `xml:"openSubsonic,attr" json:"openSubsonic"`
	// Error carries application error details when Status is "failed".
	Error *APIError `xml:"error,omitempty" json:"error,omitempty"`
	// License carries the getLicense payload.
	License *License `xml:"license,omitempty" json:"license,omitempty"`
	// OpenSubsonicExtensions carries the getOpenSubsonicExtensions payload.
	OpenSubsonicExtensions *[]OpenSubsonicExtension `xml:"openSubsonicExtensions,omitempty" json:"openSubsonicExtensions,omitempty"`
	// User carries the getUser payload.
	User *User `xml:"user,omitempty" json:"user,omitempty"`
	// ScanStatus carries the getScanStatus payload.
	ScanStatus *ScanStatus `xml:"scanStatus,omitempty" json:"scanStatus,omitempty"`
	// Playlists carries the getPlaylists stub payload.
	Playlists *Playlists `xml:"playlists,omitempty" json:"playlists,omitempty"`
	// Genres carries the getGenres stub payload.
	Genres *Genres `xml:"genres,omitempty" json:"genres,omitempty"`
	// Starred2 carries the getStarred2 stub payload.
	Starred2 *Starred2 `xml:"starred2,omitempty" json:"starred2,omitempty"`
}

// APIError represents a Subsonic/OpenSubsonic application error element.
// Neither Code nor Message uses omitempty so code 0 always serializes.
type APIError struct {
	// Code is the numeric Subsonic error code.
	Code int `xml:"code,attr" json:"code"`
	// Message is the human-readable error description.
	Message string `xml:"message,attr" json:"message"`
}

// License represents the getLicense response element.
type License struct {
	// Valid indicates whether the server license is active.
	Valid bool `xml:"valid,attr" json:"valid"`
}

// OpenSubsonicExtension describes a single advertised OpenSubsonic extension.
type OpenSubsonicExtension struct {
	// Name is the OpenSubsonic extension identifier.
	Name string `xml:"name,attr" json:"name"`
	// Versions lists the supported specification version numbers for Name.
	Versions []int `xml:"versions" json:"versions"`
}

// User represents the Subsonic v1.16.1 getUser payload.
// All 12 *Role booleans and ScrobblingEnabled omit omitempty so false values
// always serialize.
type User struct {
	// Username is the authenticated or requested Subsonic username.
	Username string `xml:"username,attr" json:"username"`
	// ScrobblingEnabled reports whether server-side scrobbling is enabled.
	ScrobblingEnabled bool `xml:"scrobblingEnabled,attr" json:"scrobblingEnabled"`
	// AdminRole reports whether the user has administrative privileges.
	AdminRole bool `xml:"adminRole,attr" json:"adminRole"`
	// SettingsRole reports whether the user may change server settings.
	SettingsRole bool `xml:"settingsRole,attr" json:"settingsRole"`
	// DownloadRole reports whether the user may download original files.
	DownloadRole bool `xml:"downloadRole,attr" json:"downloadRole"`
	// UploadRole reports whether the user may upload media files.
	UploadRole bool `xml:"uploadRole,attr" json:"uploadRole"`
	// PlaylistRole reports whether the user may manage playlists.
	PlaylistRole bool `xml:"playlistRole,attr" json:"playlistRole"`
	// CoverArtRole reports whether the user may update cover art.
	CoverArtRole bool `xml:"coverArtRole,attr" json:"coverArtRole"`
	// CommentRole reports whether the user may edit media comments.
	CommentRole bool `xml:"commentRole,attr" json:"commentRole"`
	// PodcastRole reports whether the user may manage podcasts.
	PodcastRole bool `xml:"podcastRole,attr" json:"podcastRole"`
	// StreamRole reports whether the user may stream audio tracks.
	StreamRole bool `xml:"streamRole,attr" json:"streamRole"`
	// JukeboxRole reports whether the user may control jukebox playback.
	JukeboxRole bool `xml:"jukeboxRole,attr" json:"jukeboxRole"`
	// ShareRole reports whether the user may create public shares.
	ShareRole bool `xml:"shareRole,attr" json:"shareRole"`
	// VideoConversionRole reports whether the user may trigger video conversion.
	VideoConversionRole bool `xml:"videoConversionRole,attr" json:"videoConversionRole"`
	// Folder lists the music folder IDs accessible to the user.
	Folder []int `xml:"folder" json:"folder"`
}

// ScanStatus represents the getScanStatus payload.
// Scanning and Count never use omitempty; LastScan is omitted when zero.
type ScanStatus struct {
	// Scanning reports whether a library scan is currently running.
	Scanning bool `xml:"scanning,attr" json:"scanning"`
	// Count is the number of indexed audio tracks in the library.
	Count int64 `xml:"count,attr" json:"count"`
	// LastScan is the UTC RFC3339 timestamp of the most recent completed scan.
	LastScan string `xml:"lastScan,attr,omitempty" json:"lastScan,omitempty"`
}

// Playlists represents the empty getPlaylists stub payload.
// Callers must initialize Playlist to a non-nil slice so JSON emits [].
type Playlists struct {
	// Playlist holds the list of user playlists.
	Playlist []struct{} `xml:"playlist" json:"playlist"`
}

// Genres represents the empty getGenres stub payload.
// Callers must initialize Genre to a non-nil slice so JSON emits [].
type Genres struct {
	// Genre holds the list of library genres.
	Genre []struct{} `xml:"genre" json:"genre"`
}

// Starred2 represents the empty getStarred2 stub payload.
// Callers must initialize Artist, Album, and Song to non-nil slices so JSON emits [].
type Starred2 struct {
	// Artist holds starred ID3 artists.
	Artist []struct{} `xml:"artist" json:"artist"`
	// Album holds starred ID3 albums.
	Album []struct{} `xml:"album" json:"album"`
	// Song holds starred songs.
	Song []struct{} `xml:"song" json:"song"`
}
