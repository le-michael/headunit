// Package subsonic implements Headunit's OpenSubsonic v1.16.1 HTTP API.
package subsonic

import "encoding/xml"

const (
	apiVersion           = "1.16.1"
	serverType           = "headunit"
	serverVersion        = "0.1.0"
	xmlNamespace         = "http://subsonic.org/restapi"
	statusOK             = "ok"
	statusFailed         = "failed"
	errCodeGeneric       = 0
	errCodeMissingParam  = 10
	errCodeNotAuthorized = 50
	errCodeNotFound      = 70
	maxFormBodyBytes     = 1 << 20 // 1 MiB
	musicFolderID        = 1
	musicFolderName      = "Music"
	ignoredArticles      = "The El La Los Las Le Les"
	defaultAlbumListSize = 10
	defaultSearch3Count  = 20
	mediaTypeMusic       = "music"
	healthzBody          = "ok\n"
	extAPIKeyAuth        = "apiKeyAuthentication"
	msgUnsupportedAuth   = "Provided authentication mechanism not supported"
	msgNotAuthorized     = "User is not authorized for the given operation"
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
	// TokenInfo carries the tokenInfo payload.
	TokenInfo *TokenInfo `xml:"tokenInfo,omitempty" json:"tokenInfo,omitempty"`
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
	// MusicFolders carries the getMusicFolders payload.
	MusicFolders *MusicFolders `xml:"musicFolders,omitempty" json:"musicFolders,omitempty"`
	// Artists carries the getArtists ID3 index payload.
	Artists *ArtistsID3 `xml:"artists,omitempty" json:"artists,omitempty"`
	// Artist carries the getArtist payload.
	Artist *ArtistWithAlbumsID3 `xml:"artist,omitempty" json:"artist,omitempty"`
	// Album carries the getAlbum payload.
	Album *AlbumWithSongsID3 `xml:"album,omitempty" json:"album,omitempty"`
	// AlbumList2 carries the getAlbumList2 payload.
	AlbumList2 *AlbumList2 `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	// Song carries the getSong payload.
	Song *Child `xml:"song,omitempty" json:"song,omitempty"`
	// SearchResult3 carries the search3 payload.
	SearchResult3 *SearchResult3 `xml:"searchResult3,omitempty" json:"searchResult3,omitempty"`
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

// TokenInfo represents the OpenSubsonic tokenInfo payload.
type TokenInfo struct {
	// Username is the username associated with the authenticated API key.
	Username string `xml:"username,attr" json:"username"`
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

// MusicFolders represents the getMusicFolders payload.
// Callers must initialize MusicFolder to a non-nil slice so JSON emits [].
type MusicFolders struct {
	// MusicFolder lists the virtual music folders exposed by Headunit.
	MusicFolder []MusicFolder `xml:"musicFolder" json:"musicFolder"`
}

// MusicFolder describes a single top-level virtual music folder.
type MusicFolder struct {
	// ID is the numeric music folder identifier (never omitted even if zero).
	ID int `xml:"id,attr" json:"id"`
	// Name is the human-readable display name of the music folder.
	Name string `xml:"name,attr" json:"name"`
}

// ArtistsID3 represents the getArtists ID3 artist index payload.
// Callers must initialize Index to a non-nil slice so JSON emits [].
type ArtistsID3 struct {
	// IgnoredArticles lists space-delimited leading articles stripped when sorting.
	IgnoredArticles string `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	// Index holds non-empty alphabetical artist buckets ("A"-"Z", followed by "#").
	Index []IndexID3 `xml:"index" json:"index"`
}

// IndexID3 groups ID3 artists sharing a single index bucket.
type IndexID3 struct {
	// Name is the single-character index heading ("A"-"Z" or "#").
	Name string `xml:"name,attr" json:"name"`
	// Artist lists the artists sorted within this index bucket.
	Artist []ArtistID3 `xml:"artist" json:"artist"`
}

// ArtistID3 represents an ID3 artist summary without nested albums.
type ArtistID3 struct {
	// ID is the deterministic artist identifier ("ar_" prefix).
	ID string `xml:"id,attr" json:"id"`
	// Name is the canonical album artist display name.
	Name string `xml:"name,attr" json:"name"`
	// AlbumCount is the number of distinct albums under this artist.
	AlbumCount int `xml:"albumCount,attr" json:"albumCount"`
}

// ArtistWithAlbumsID3 represents the getArtist payload containing an artist
// and all of their albums. Callers must initialize Album to a non-nil slice.
type ArtistWithAlbumsID3 struct {
	// ID is the deterministic artist identifier ("ar_" prefix).
	ID string `xml:"id,attr" json:"id"`
	// Name is the canonical album artist display name.
	Name string `xml:"name,attr" json:"name"`
	// AlbumCount is the number of distinct albums under this artist.
	AlbumCount int `xml:"albumCount,attr" json:"albumCount"`
	// Album lists the artist's albums ordered deterministically.
	Album []AlbumID3 `xml:"album" json:"album"`
}

// AlbumID3 represents an ID3 album summary.
type AlbumID3 struct {
	// ID is the deterministic album identifier ("al_" prefix).
	ID string `xml:"id,attr" json:"id"`
	// Name is the canonical album title.
	Name string `xml:"name,attr" json:"name"`
	// Artist is the canonical album artist name.
	Artist string `xml:"artist,attr" json:"artist"`
	// ArtistID is the deterministic identifier for the album artist.
	ArtistID string `xml:"artistId,attr" json:"artistId"`
	// SongCount is the number of tracks in the album.
	SongCount int `xml:"songCount,attr" json:"songCount"`
	// Duration is the total album duration in whole seconds.
	Duration int `xml:"duration,attr" json:"duration"`
	// Created is the earliest UTC RFC3339 timestamp among tracks in the album.
	Created string `xml:"created,attr,omitempty" json:"created,omitempty"`
	// Year is the album release year, omitted when zero.
	Year int `xml:"year,attr,omitempty" json:"year,omitempty"`
	// Genre is the album genre, omitted when empty.
	Genre string `xml:"genre,attr,omitempty" json:"genre,omitempty"`
}

// AlbumWithSongsID3 represents the getAlbum payload containing an album and
// its ordered tracks. Callers must initialize Song to a non-nil slice.
type AlbumWithSongsID3 struct {
	// ID is the deterministic album identifier ("al_" prefix).
	ID string `xml:"id,attr" json:"id"`
	// Name is the canonical album title.
	Name string `xml:"name,attr" json:"name"`
	// Artist is the canonical album artist name.
	Artist string `xml:"artist,attr" json:"artist"`
	// ArtistID is the deterministic identifier for the album artist.
	ArtistID string `xml:"artistId,attr" json:"artistId"`
	// SongCount is the number of tracks in the album.
	SongCount int `xml:"songCount,attr" json:"songCount"`
	// Duration is the total album duration in whole seconds.
	Duration int `xml:"duration,attr" json:"duration"`
	// Created is the earliest UTC RFC3339 timestamp among tracks in the album.
	Created string `xml:"created,attr,omitempty" json:"created,omitempty"`
	// Year is the album release year, omitted when zero.
	Year int `xml:"year,attr,omitempty" json:"year,omitempty"`
	// Genre is the album genre, omitted when empty.
	Genre string `xml:"genre,attr,omitempty" json:"genre,omitempty"`
	// Song lists the album's tracks ordered by disc, track number, title, and ID.
	Song []Child `xml:"song" json:"song"`
}

// AlbumList2 represents the getAlbumList2 payload.
// Callers must initialize Album to a non-nil slice so JSON emits [].
type AlbumList2 struct {
	// Album lists the paginated album summaries matching the requested sort.
	Album []AlbumID3 `xml:"album" json:"album"`
}

// Child represents a Subsonic v1.16.1 song entity across getAlbum, getSong, and
// search3 responses, matching the 21 standard MVP fields.
type Child struct {
	// ID is the stable track identifier ("tr_" prefix).
	ID string `xml:"id,attr" json:"id"`
	// Parent links the track to its parent ID3 album ID.
	Parent string `xml:"parent,attr" json:"parent"`
	// IsDir indicates whether the entry is a directory (always false for songs).
	IsDir bool `xml:"isDir,attr" json:"isDir"`
	// Title is the track title.
	Title string `xml:"title,attr" json:"title"`
	// Album is the album title.
	Album string `xml:"album,attr" json:"album"`
	// Artist is the track-level artist name.
	Artist string `xml:"artist,attr" json:"artist"`
	// Track is the 1-based track number, omitted when zero.
	Track int `xml:"track,attr,omitempty" json:"track,omitempty"`
	// Year is the 4-digit release year, omitted when zero.
	Year int `xml:"year,attr,omitempty" json:"year,omitempty"`
	// Genre is the track genre, omitted when empty.
	Genre string `xml:"genre,attr,omitempty" json:"genre,omitempty"`
	// Size is the audio file size in bytes.
	Size int64 `xml:"size,attr" json:"size"`
	// ContentType is the HTTP MIME type used when streaming the track.
	ContentType string `xml:"contentType,attr" json:"contentType"`
	// Suffix is the lowercase file extension without leading dot, omitted only if empty.
	Suffix string `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	// Duration is the audio duration in whole seconds.
	Duration int `xml:"duration,attr" json:"duration"`
	// BitRate is the audio bitrate in kbps, omitted when zero.
	BitRate int `xml:"bitRate,attr,omitempty" json:"bitRate,omitempty"`
	// Path is the forward-slash path relative to the music root.
	Path string `xml:"path,attr" json:"path"`
	// IsVideo indicates whether the entry is a video (always false for audio).
	IsVideo bool `xml:"isVideo,attr" json:"isVideo"`
	// DiscNumber is the 1-based disc number, omitted when zero.
	DiscNumber int `xml:"discNumber,attr,omitempty" json:"discNumber,omitempty"`
	// Created is the UTC RFC3339 timestamp when the track was first indexed.
	Created string `xml:"created,attr,omitempty" json:"created,omitempty"`
	// AlbumID is the deterministic parent album identifier.
	AlbumID string `xml:"albumId,attr" json:"albumId"`
	// ArtistID is the deterministic album artist identifier.
	ArtistID string `xml:"artistId,attr" json:"artistId"`
	// Type identifies the Subsonic media type ("music").
	Type string `xml:"type,attr" json:"type"`
}

// SearchResult3 represents the search3 payload.
// Callers must initialize Artist, Album, and Song to non-nil slices so JSON emits [].
type SearchResult3 struct {
	// Artist holds matching ID3 artists.
	Artist []ArtistID3 `xml:"artist" json:"artist"`
	// Album holds matching ID3 albums.
	Album []AlbumID3 `xml:"album" json:"album"`
	// Song holds matching tracks.
	Song []Child `xml:"song" json:"song"`
}
