package subsonic

import (
	"cmp"
	"errors"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"headunit/internal/metadata"
	"headunit/internal/store"
)

var ignoredArticlePrefixes = [...]string{"the", "el", "la", "los", "las", "le", "les"}

func (h *handler) handleGetMusicFolders(w http.ResponseWriter, r *http.Request) {
	h.writeOK(w, r, Response{
		MusicFolders: &MusicFolders{
			MusicFolder: []MusicFolder{
				{ID: musicFolderID, Name: musicFolderName},
			},
		},
	})
}

func (h *handler) handleGetArtists(w http.ResponseWriter, r *http.Request) {
	artists, err := h.store.ListArtists(r.Context())
	if err != nil {
		h.writeInternalError(w, r, "list artists", err)
		return
	}

	h.writeOK(w, r, Response{
		Artists: &ArtistsID3{
			IgnoredArticles: ignoredArticles,
			Index:           buildArtistIndexes(artists),
		},
	})
}

func (h *handler) handleGetArtist(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireIDParam(w, r)
	if !ok {
		return
	}

	artist, albums, err := h.store.GetArtist(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, r, errCodeNotFound, "Artist not found")
			return
		}
		h.writeInternalError(w, r, "get artist", err)
		return
	}

	h.writeOK(w, r, Response{
		Artist: &ArtistWithAlbumsID3{
			ID:         artist.ID,
			Name:       artist.Name,
			AlbumCount: artist.AlbumCount,
			Album:      toAlbumID3s(albums),
		},
	})
}

func (h *handler) handleGetAlbum(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireIDParam(w, r)
	if !ok {
		return
	}

	album, tracks, err := h.store.GetAlbum(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, r, errCodeNotFound, "Album not found")
			return
		}
		h.writeInternalError(w, r, "get album", err)
		return
	}

	h.writeOK(w, r, Response{
		Album: &AlbumWithSongsID3{
			ID:        album.ID,
			Name:      album.Name,
			Artist:    album.Artist,
			ArtistID:  album.ArtistID,
			SongCount: album.SongCount,
			Duration:  album.DurationSec,
			Created:   formatOptionalTimestamp(album.CreatedAt),
			Year:      album.Year,
			Genre:     album.Genre,
			Song:      toChildren(tracks),
		},
	})
}

func (h *handler) handleGetAlbumList2(w http.ResponseWriter, r *http.Request) {
	listType := strings.TrimSpace(formParam(r, "type"))
	if listType == "" {
		h.writeError(w, r, errCodeMissingParam, "Required parameter 'type' is missing")
		return
	}

	var sort store.AlbumListSort
	switch listType {
	case "alphabeticalByName":
		sort = store.AlbumSortAlphabeticalByName
	case "alphabeticalByArtist":
		sort = store.AlbumSortAlphabeticalByArtist
	case "newest":
		sort = store.AlbumSortNewest
	case "random":
		sort = store.AlbumSortRandom
	case "recent", "frequent", "highest", "starred", "byYear", "byGenre":
		// Valid Subsonic v1.16.1 list types deferred from MVP return empty lists
		// cleanly without querying the database.
		h.writeOK(w, r, Response{
			AlbumList2: &AlbumList2{Album: []AlbumID3{}},
		})
		return
	default:
		h.writeError(w, r, errCodeMissingParam, "Invalid or unsupported 'type' parameter")
		return
	}

	size := parseBoundedInt(formParam(r, "size"), defaultAlbumListSize, 0, store.MaxPageLimit)
	offset := parseMinInt(formParam(r, "offset"), 0, 0)

	albums, err := h.store.ListAlbums(r.Context(), store.AlbumListQuery{
		Sort:   sort,
		Limit:  size,
		Offset: offset,
	})
	if err != nil {
		h.writeInternalError(w, r, "list albums", err)
		return
	}

	h.writeOK(w, r, Response{
		AlbumList2: &AlbumList2{Album: toAlbumID3s(albums)},
	})
}

func (h *handler) handleGetSong(w http.ResponseWriter, r *http.Request) {
	id, ok := h.requireIDParam(w, r)
	if !ok {
		return
	}

	track, err := h.store.GetTrack(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, r, errCodeNotFound, "Song not found")
			return
		}
		h.writeInternalError(w, r, "get song", err)
		return
	}
	if track == nil {
		h.writeError(w, r, errCodeNotFound, "Song not found")
		return
	}

	song := toChild(*track)
	h.writeOK(w, r, Response{Song: &song})
}

func (h *handler) handleSearch3(w http.ResponseWriter, r *http.Request) {
	q := store.Search3Query{
		Query:        parseSearch3Query(formParam(r, "query")),
		ArtistCount:  parseBoundedInt(formParam(r, "artistCount"), defaultSearch3Count, 0, store.MaxPageLimit),
		ArtistOffset: parseMinInt(formParam(r, "artistOffset"), 0, 0),
		AlbumCount:   parseBoundedInt(formParam(r, "albumCount"), defaultSearch3Count, 0, store.MaxPageLimit),
		AlbumOffset:  parseMinInt(formParam(r, "albumOffset"), 0, 0),
		SongCount:    parseBoundedInt(formParam(r, "songCount"), defaultSearch3Count, 0, store.MaxPageLimit),
		SongOffset:   parseMinInt(formParam(r, "songOffset"), 0, 0),
	}

	res, err := h.store.Search3(r.Context(), q)
	if err != nil {
		h.writeInternalError(w, r, "search3", err)
		return
	}

	h.writeOK(w, r, Response{
		SearchResult3: &SearchResult3{
			Artist: toArtistID3s(res.Artists),
			Album:  toAlbumID3s(res.Albums),
			Song:   toChildren(res.Songs),
		},
	})
}

func (h *handler) requireIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(formParam(r, "id"))
	if id == "" {
		h.writeError(w, r, errCodeMissingParam, "Required parameter 'id' is missing")
		return "", false
	}
	return id, true
}

func (h *handler) writeInternalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.logger.Error("subsonic: internal handler error", "op", op, "error", err)
	h.writeError(w, r, errCodeGeneric, "Internal server error")
}

type artistIndexEntry struct {
	dto          ArtistID3
	lowerSortKey string
	lowerName    string
}

func buildArtistIndexes(artists []store.ArtistSummary) []IndexID3 {
	buckets := make(map[string][]artistIndexEntry)
	for _, a := range artists {
		sortKey := artistSortKey(a.Name)
		bucket := artistIndexBucket(sortKey)
		buckets[bucket] = append(buckets[bucket], artistIndexEntry{
			dto:          toArtistID3(a),
			lowerSortKey: strings.ToLower(sortKey),
			lowerName:    strings.ToLower(a.Name),
		})
	}

	indexes := []IndexID3{}
	appendBucket := func(name string) {
		entries := buckets[name]
		if len(entries) == 0 {
			return
		}
		slices.SortFunc(entries, func(a, b artistIndexEntry) int {
			if c := cmp.Compare(a.lowerSortKey, b.lowerSortKey); c != 0 {
				return c
			}
			if c := cmp.Compare(a.lowerName, b.lowerName); c != 0 {
				return c
			}
			return cmp.Compare(a.dto.ID, b.dto.ID)
		})
		out := make([]ArtistID3, len(entries))
		for i, e := range entries {
			out[i] = e.dto
		}
		indexes = append(indexes, IndexID3{
			Name:   name,
			Artist: out,
		})
	}

	for ch := byte('A'); ch <= 'Z'; ch++ {
		appendBucket(string(ch))
	}
	appendBucket("#")
	return indexes
}

func artistSortKey(name string) string {
	trimmed := strings.TrimSpace(name)
	for _, art := range ignoredArticlePrefixes {
		if len(trimmed) <= len(art) || !strings.EqualFold(trimmed[:len(art)], art) {
			continue
		}
		after := trimmed[len(art):]
		r, _ := utf8.DecodeRuneInString(after)
		if r == utf8.RuneError || !unicode.IsSpace(r) {
			continue
		}
		if rest := strings.TrimLeftFunc(after, unicode.IsSpace); rest != "" {
			return rest
		}
		return trimmed
	}
	return trimmed
}

func artistIndexBucket(sortKey string) string {
	r, _ := utf8.DecodeRuneInString(sortKey)
	if 'a' <= r && r <= 'z' {
		return string(r - 'a' + 'A')
	}
	if 'A' <= r && r <= 'Z' {
		return string(r)
	}
	return "#"
}

func toArtistID3(a store.ArtistSummary) ArtistID3 {
	return ArtistID3{
		ID:         a.ID,
		Name:       a.Name,
		AlbumCount: a.AlbumCount,
	}
}

func toArtistID3s(artists []store.ArtistSummary) []ArtistID3 {
	out := make([]ArtistID3, len(artists))
	for i, a := range artists {
		out[i] = toArtistID3(a)
	}
	return out
}

func toAlbumID3(a store.AlbumSummary) AlbumID3 {
	return AlbumID3{
		ID:        a.ID,
		Name:      a.Name,
		Artist:    a.Artist,
		ArtistID:  a.ArtistID,
		SongCount: a.SongCount,
		Duration:  a.DurationSec,
		Created:   formatOptionalTimestamp(a.CreatedAt),
		Year:      a.Year,
		Genre:     a.Genre,
	}
}

func toAlbumID3s(albums []store.AlbumSummary) []AlbumID3 {
	out := make([]AlbumID3, len(albums))
	for i, a := range albums {
		out[i] = toAlbumID3(a)
	}
	return out
}

func toChild(t store.Track) Child {
	suffix, contentType, ok := metadata.LookupFormat(t.RelPath)
	if !ok {
		contentType = "application/octet-stream"
		suffix = strings.TrimPrefix(strings.ToLower(path.Ext(t.RelPath)), ".")
	}

	return Child{
		ID:          t.ID,
		Parent:      t.AlbumID,
		IsDir:       false,
		Title:       t.Title,
		Album:       t.Album,
		Artist:      t.Artist,
		Track:       t.TrackNum,
		Year:        t.Year,
		Genre:       t.Genre,
		Size:        t.FileSize,
		ContentType: contentType,
		Suffix:      suffix,
		Duration:    t.DurationSec,
		BitRate:     t.BitRate,
		Path:        t.RelPath,
		IsVideo:     false,
		DiscNumber:  t.DiscNum,
		Created:     formatOptionalTimestamp(t.CreatedAt),
		AlbumID:     t.AlbumID,
		ArtistID:    t.ArtistID,
		Type:        mediaTypeMusic,
	}
}

func toChildren(tracks []store.Track) []Child {
	out := make([]Child, len(tracks))
	for i, t := range tracks {
		out[i] = toChild(t)
	}
	return out
}

func formatOptionalTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

// parseSearch3Query trims surrounding whitespace and maps literal `""`
// (sent by OpenSubsonic clients for bulk library sync) to "" so Store.Search3
// treats it as a match-all query rather than two literal quote characters.
func parseSearch3Query(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == `""` {
		return ""
	}
	return trimmed
}

func parseOptionalInt(raw string, def int) int {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return def
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return def
	}
	return n
}

func parseMinInt(raw string, def, minVal int) int {
	n := parseOptionalInt(raw, def)
	if n < minVal {
		return minVal
	}
	return n
}

func parseBoundedInt(raw string, def, minVal, maxVal int) int {
	n := parseMinInt(raw, def, minVal)
	if n > maxVal {
		return maxVal
	}
	return n
}
