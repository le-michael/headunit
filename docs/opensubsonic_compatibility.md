# Headunit OpenSubsonic Compatibility Matrix

This living document tracks **Headunit**'s compatibility against the **Subsonic REST API v1.16.1** and **OpenSubsonic** specifications across core protocol mechanics, error codes, extensions, all 87 REST endpoints, and entity schema extensions. It is updated incrementally as features and endpoints are implemented and verified.

---

## 1. Legend & Coverage Summary

### Status Legend

| Status | Meaning |
| :--- | :--- |
| `Not Implemented` | Not built yet. |
| `Stub` | Endpoint responds cleanly with an empty, no-op, or Subsonic error `70` payload for client compatibility without backing feature logic. |
| `Partial` | Core behavior works, with documented limitations or omitted optional parameters. |
| `Implemented` | Spec-compliant behavior implemented and tested. |

### Target Legend

| Target | Meaning |
| :--- | :--- |
| `MVP` | Planned for the initial Headunit MVP. |
| `MVP (Stub)` | Planned as a minimal compatibility stub in MVP so real clients (Symfonium, Feishin, Amperfy, DSub, Tempo) sync and play cleanly. |
| `Post-MVP` | Out of scope for MVP; candidate for later milestones. |
| `Deprecated` | Deprecated in the upstream Subsonic/OpenSubsonic specification. |

### Coverage Summary

*Counting rule: `Implemented / Total` counts rows with status `Implemented` only (`Stub` and `Partial` rows are tracked separately in MVP Target Progress until fully implemented).*

| Surface | Implemented / Total | MVP Target Progress | Notes |
| :--- | :---: | :---: | :--- |
| Core Protocol & Wire Features | `9 / 14` | `9 / 9` | XML/JSON, dual routes, POST form parsing, CORS, Range streaming, no-op auth seam |
| Official Error Codes | `3 / 12` | `3 / 3` | MVP emits codes `0`, `10`, `70` (`40` wired at `auth.Middleware` seam) |
| OpenSubsonic Extensions | `0 / 10` | `0 / 0` | `getOpenSubsonicExtensions` returns `[]` in MVP |
| API Endpoints (Total) | `13 / 87` | `18 / 18` (`13 / 13` full + `5 / 5` stubs) | 16 official categories (`13` MVP + `5` MVP Stub + `68` Post-MVP + `1` Deprecated) |
| OpenSubsonic Entity Extensions | `0 / 5` entity groups | `0 / 0` | `Child`, `AlbumID3`, `ArtistID3`, `Playlist`, `Error` |

---

## 2. Core Protocol & Wire Features (`9 / 14`)

| Feature | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| Dual path routing (`/rest/<method>` and `/rest/<method>.view`) | Subsonic 1.16.1 | `Implemented` | `MVP` | Registered identically in `net/http` `ServeMux` (excluding `hls.m3u8`). |
| HTTP `GET` query params & `HEAD` support | Subsonic 1.16.1 | `Implemented` | `MVP` | Go 1.22+ `GET` handlers automatically serve `HEAD` requests. |
| HTTP `POST` `application/x-www-form-urlencoded` body parsing | OpenSubsonic (`formPost`) | `Implemented` | `MVP` | Parsed via `http.MaxBytesReader` (1 MiB) + `r.ParseForm()` before auth; `formPost` extension is not advertised until full conformance. |
| XML response format (`f=xml` / default, `xmlns="http://subsonic.org/restapi"`) | Subsonic 1.16.1 | `Implemented` | `MVP` | Default response format (`application/xml; charset=utf-8`), prefixed with XML declaration; scalar fields serialized as attributes. |
| JSON response format (`f=json`, `{"subsonic-response": {...}}`) | Subsonic 1.16.1 | `Implemented` | `MVP` | `application/json; charset=utf-8`; empty collections always serialize as `[]` (never `null`). |
| JSONP response format (`f=jsonp&callback=...`) | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; modern web clients use CORS + `f=json`. |
| OpenSubsonic root envelope attributes (`openSubsonic=true`, `type`, `serverVersion`) | OpenSubsonic | `Implemented` | `MVP` | Emits `version="1.16.1"`, `type="headunit"`, `serverVersion="0.1.0"`, `openSubsonic=true` on all responses. |
| CORS headers & `OPTIONS` preflight for web clients | Web/Client Compat | `Implemented` | `MVP` | Enables browser-based clients (e.g., Feishin web); handles `OPTIONS` with `204 No Content` before auth. |
| HTTP byte-range direct streaming (`Accept-Ranges`, `Range` → `206`, `416`, `ETag`, `If-Range`, `HEAD`) | HTTP / RFC 9110 | `Implemented` | `MVP` | Backed by `DirectStreamer` using `os.OpenRoot` + `http.ServeContent` with strong `ETag`. |
| On-the-fly transcoding (`ffmpeg` `maxBitRate`/`format` negotiation) | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | `ffmpeg` is pre-bundled in the Docker image and playback routes through the `stream.Streamer` seam; MVP ignores `maxBitRate`/`format`. |
| Authentication: No-op pass-through seam (`auth.Middleware`) | Headunit MVP | `Implemented` | `MVP` | `NoopAuthenticator` accepts any or no credentials and attaches a synthetic principal until real auth is enabled. |
| Authentication: Salted MD5 token (`u` + `t` + `s`, min 6-char salt) | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Swap in an `auth.Authenticator` implementation in `cmd/headunit/main.go`. |
| Authentication: Plaintext / hex password (`u` + `p` / `enc:`) | Subsonic 1.16.1 (Deprecated) | `Not Implemented` | `Post-MVP` | Deferred; discouraged in favor of token or API key auth. |
| Authentication: API Key (`apiKey=<key>`) | OpenSubsonic (`apiKeyAuthentication`) | `Not Implemented` | `Post-MVP` | Swap in via `auth.Authenticator` seam alongside `tokenInfo`. |

---

## 3. Official Subsonic / OpenSubsonic Error Codes (`3 / 12`)

All Subsonic/OpenSubsonic application errors are returned inside an HTTP `200 OK` envelope with `status="failed"` and an `<error code="..." message="..."/>` (XML) or `"error": {"code": ..., "message": "..."}` (JSON) element.

| Code | Spec Origin | Official Meaning | Status | Target | Notes |
| :---: | :--- | :--- | :--- | :--- | :--- |
| `0` | Subsonic 1.16.1 | A generic error. | `Implemented` | `MVP` | Used for malformed/oversized form bodies and unexpected server errors. |
| `10` | Subsonic 1.16.1 | Required parameter is missing. | `Implemented` | `MVP` | Returned for missing required parameters (e.g., `id`, `type`) or unknown `getAlbumList2` `type`. |
| `20` | Subsonic 1.16.1 | Incompatible Subsonic REST protocol version. Client must upgrade. | `Not Implemented` | `Post-MVP` | MVP does not enforce strict client `v` version gating. |
| `30` | Subsonic 1.16.1 | Incompatible Subsonic REST protocol version. Server must upgrade. | `Not Implemented` | `Post-MVP` | MVP does not enforce strict client `v` version gating. |
| `40` | Subsonic 1.16.1 | Wrong username or password. | `Not Implemented` | `Post-MVP` | Wired in the `auth.Middleware` seam; not emitted while `NoopAuthenticator` is active. |
| `41` | Subsonic 1.16.1 | Token authentication not supported for LDAP users. | `Not Implemented` | `Post-MVP` | Out of scope; no LDAP integration. |
| `42` | OpenSubsonic | Provided authentication mechanism not supported. | `Not Implemented` | `Post-MVP` | Deferred until real authentication mechanisms are enforced. |
| `43` | OpenSubsonic | Multiple conflicting authentication mechanisms provided. | `Not Implemented` | `Post-MVP` | Deferred until real authentication mechanisms are enforced. |
| `44` | OpenSubsonic (`apiKeyAuthentication`) | Invalid API key. | `Not Implemented` | `Post-MVP` | Deferred until `apiKeyAuthentication` is implemented. |
| `50` | Subsonic 1.16.1 | User is not authorized for the given operation. | `Not Implemented` | `Post-MVP` | Deferred until multi-user/role authorization is implemented. |
| `60` | Subsonic 1.16.1 | The trial period for the Subsonic server is over. | `Not Implemented` | `Post-MVP` | Headunit is open/self-hosted with no licensing trial. |
| `70` | Subsonic 1.16.1 | The requested data was not found. | `Implemented` | `MVP` | Returned for unknown artist/album/song IDs, missing audio files on disk (`stream`), and `getCoverArt` stub. |

---

## 4. Official OpenSubsonic Extensions (`0 / 10`)

Advertised via `/rest/getOpenSubsonicExtensions`. Headunit returns an empty list (`[]`) in MVP and will only advertise an extension once it conforms completely to that extension's specification.

| Extension Name | Spec Versions | Endpoints Added / Modified | Status | Target | Description |
| :--- | :---: | :--- | :--- | :--- | :--- |
| `apiKeyAuthentication` | `[1]` | Adds `tokenInfo`; global auth | `Not Implemented` | `Post-MVP` | Authenticates requests using an `apiKey` query/form parameter instead of username/token credentials. |
| `formPost` | `[1]` | All REST endpoints | `Not Implemented` | `Post-MVP` | Standardizes `POST` `application/x-www-form-urlencoded` requests; MVP parses POST forms, but `getOpenSubsonicExtensions` returns `[]` until full extension verification. |
| `getPodcastEpisode` | `[1]` | Adds `getPodcastEpisode` | `Not Implemented` | `Post-MVP` | Retrieves metadata for a single podcast episode by ID. |
| `indexBasedQueue` | `[1]` | Adds `getPlayQueueByIndex`, `savePlayQueueByIndex` | `Not Implemented` | `Post-MVP` | Saves and restores server-side play queues using zero-based track indices (supporting duplicate tracks in a queue). |
| `playbackReport` | `[1]` | Adds `reportPlayback`; extends `getNowPlaying` | `Not Implemented` | `Post-MVP` | Reports structured client playback state/progress and exposes richer now-playing metadata. |
| `songLyrics` | `[1, 2]` | Adds `getLyricsBySongId` | `Not Implemented` | `Post-MVP` | Retrieves structured synced and unsynced lyrics by song ID. |
| `sonicSimilarity` | `[1]` | Adds `getSonicSimilarTracks`, `findSonicPath` | `Not Implemented` | `Post-MVP` | Audio-feature-based track similarity search and sonic transition paths. |
| `topSongsByArtistId` | `[1]` | Modifies `getTopSongs` | `Not Implemented` | `Post-MVP` | Allows `getTopSongs` to look up top tracks by `artistId` rather than artist name string. |
| `transcodeOffset` | `[1]` | Modifies `stream` | `Not Implemented` | `Post-MVP` | Supports time-based seeking (`timeOffset`) during transcoded audio streaming. |
| `transcoding` | `[1]` | Adds `getTranscodeDecision`, `getTranscodeStream` | `Not Implemented` | `Post-MVP` | Client-capability negotiation for direct play vs. server-side transcoding/remuxing. |

---

## 5. Complete Endpoints Compatibility Matrix (`13 / 87`)

All implemented endpoints are served under both `/rest/<method>` and `/rest/<method>.view` (except `hls.m3u8`, whose canonical path is `/rest/hls.m3u8`).

### 5.1 System (`3 / 4`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `ping` | Subsonic 1.16.1 | `Implemented` | `MVP` | Bare `status="ok"` envelope (Docker `HEALTHCHECK` probes unauthenticated `/healthz`). |
| `getLicense` | Subsonic 1.16.1 | `Implemented` | `MVP` | Returns `license: {valid: true}`. |
| `getOpenSubsonicExtensions` | OpenSubsonic | `Implemented` | `MVP` | Returns empty `openSubsonicExtensions: []` (zero extension claims until `apiKeyAuthentication` is wired). Bypasses `auth.Middleware` directly per OpenSubsonic spec while preserving CORS and form parsing. |
| `tokenInfo` | OpenSubsonic (`apiKeyAuthentication`) | `Partial` | `Post-MVP` | Handler and routes implemented; returns `tokenInfo.username` for `AuthMethodAPIKey` principals and error `42` otherwise. Marked `Partial` until `SubsonicAuthenticator` is wired in `cmd/headunit/main.go`. |

### 5.2 Browsing (`5 / 20`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getMusicFolders` | Subsonic 1.16.1 | `Implemented` | `MVP` | Returns single virtual folder `musicFolder: [{id: 1, name: "Music"}]`. |
| `getIndexes` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; Headunit organizes tracks via ID3 tags rather than mirroring host directories. |
| `getMusicDirectory` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; folder-based browsing is out of scope for MVP. |
| `getGenres` | Subsonic 1.16.1 | `Stub` | `MVP (Stub)` | Returns empty `genres: {genre: []}` to prevent client sync errors. |
| `getArtists` | Subsonic 1.16.1 | `Implemented` | `MVP` | ID3 artists derived from `album_artist` (`ar_*`), bucketed `A`–`Z` + `#` (`ignoredArticles: "The El La Los Las Le Les"`). |
| `getArtist` | Subsonic 1.16.1 | `Implemented` | `MVP` | Returns artist + albums (`al_*`) by `id`; missing `id` → error `10`, unknown → error `70`. |
| `getAlbum` | Subsonic 1.16.1 | `Implemented` | `MVP` | Returns album + songs (`tr_*`) ordered by `disc_num, track_num, title`; missing `id` → error `10`, unknown → error `70`. |
| `getSong` | Subsonic 1.16.1 | `Implemented` | `MVP` | Returns single `Child` song entity by `id`; missing `id` → error `10`, unknown → error `70`. |
| `getVideos` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Headunit indexes audio files only in MVP. |
| `getVideoInfo` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Headunit indexes audio files only in MVP. |
| `getArtistInfo` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no external metadata/biography scrapers in MVP. |
| `getArtistInfo2` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no external metadata/biography scrapers in MVP. |
| `getAlbumInfo` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no external album notes/Last.fm integration in MVP. |
| `getAlbumInfo2` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no external album notes/Last.fm integration in MVP. |
| `getSimilarSongs` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no similarity engine in MVP. |
| `getSimilarSongs2` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no similarity engine in MVP. |
| `getTopSongs` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no external top-track metadata or play-count ranking in MVP. |
| `getPodcastEpisode` | OpenSubsonic (`getPodcastEpisode`) | `Not Implemented` | `Post-MVP` | Deferred; podcasts are out of scope for MVP *(officially categorized under Browsing in OpenSubsonic)*. |
| `getSonicSimilarTracks` | OpenSubsonic (`sonicSimilarity`) | `Not Implemented` | `Post-MVP` | Deferred; requires `sonicSimilarity` audio analysis. |
| `findSonicPath` | OpenSubsonic (`sonicSimilarity`) | `Not Implemented` | `Post-MVP` | Deferred; requires `sonicSimilarity` audio analysis. |

### 5.3 Lists (`1 / 7`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getAlbumList` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; folder-based album listing is out of scope for MVP (use `getAlbumList2`). |
| `getAlbumList2` | Subsonic 1.16.1 | `Implemented` | `MVP` | Supports `alphabeticalByName`, `alphabeticalByArtist`, `newest`, `random` (`size` def `10`/max `500`, `offset` def `0`); deferred valid types (`recent`, `frequent`, `highest`, `starred`, `byYear`, `byGenre`) return empty `album: []`; missing/unknown `type` → error `10`. |
| `getRandomSongs` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Explicitly deferred from MVP. |
| `getSongsByGenre` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred alongside full genre browsing. |
| `getNowPlaying` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no live playback session tracking in MVP. |
| `getStarred` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; folder-based favorites are out of scope for MVP. |
| `getStarred2` | Subsonic 1.16.1 | `Stub` | `MVP (Stub)` | Returns empty `starred2: {artist: [], album: [], song: []}` to prevent client sync errors. |

### 5.4 Searching (`1 / 3`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `search` | Subsonic 1.16.1 (Deprecated) | `Not Implemented` | `Deprecated` | Deprecated in Subsonic 1.16.1; superseded by `search2` and `search3`. |
| `search2` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; folder-based search is out of scope for MVP (use `search3`). |
| `search3` | Subsonic 1.16.1 (+ OpenSubsonic) | `Implemented` | `MVP` | ID3 substring search across artists, albums, and songs; implements OpenSubsonic empty-query bulk sync (normalizes `query=` and literal `query=""` / `%22%22` to match-all); defaults omitted counts to `20` (max `500`), respects explicit `0` counts, and orders with deterministic ID tie-breakers. |

### 5.5 Playlists (`0 / 5`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getPlaylists` | Subsonic 1.16.1 | `Stub` | `MVP (Stub)` | Returns empty `playlists: {playlist: []}` to prevent client sync errors. |
| `getPlaylist` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; playlists persistence is out of scope for MVP. |
| `createPlaylist` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; playlists persistence is out of scope for MVP. |
| `updatePlaylist` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; playlists persistence is out of scope for MVP. |
| `deletePlaylist` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; playlists persistence is out of scope for MVP. |

### 5.6 Media Retrieval (`1 / 8`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `stream` | Subsonic 1.16.1 (+ OpenSubsonic) | `Implemented` | `MVP` | Raw direct-stream via `DirectStreamer` (`os.OpenRoot`) with HTTP Range (`200`/`206`/`416`/`HEAD`, `ETag`); `maxBitRate`/`format` ignored in MVP; never scrobbles/increments play counts per OpenSubsonic spec; missing `id` → HTTP 200 error `10`, missing track/file → HTTP 200 error `70` written before sending audio headers. |
| `download` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; `getUser` reports `downloadRole: false` in MVP. |
| `hls.m3u8` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; canonical path `/rest/hls.m3u8` (no `.view` suffix); no HLS segmentation/transcoding in MVP. |
| `getCaptions` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; video captions are out of scope for MVP. |
| `getCoverArt` | Subsonic 1.16.1 | `Stub` | `MVP (Stub)` | Returns HTTP `200 OK` + Subsonic error `70` as a defensive guard for clients probing cover art. |
| `getLyrics` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; lyrics extraction/serving is out of scope for MVP. |
| `getAvatar` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; no user profiles/avatars in MVP. |
| `getLyricsBySongId` | OpenSubsonic (`songLyrics`) | `Not Implemented` | `Post-MVP` | Deferred until `songLyrics` extension is implemented. |

### 5.7 Media Annotation (`0 / 5`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `star` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; read-only library state in MVP. |
| `unstar` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; read-only library state in MVP. |
| `setRating` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; user ratings are out of scope for MVP. |
| `scrobble` | Subsonic 1.16.1 | `Stub` | `MVP (Stub)` | No-op bare `status="ok"` so clients calling `scrobble` on playback start/end do not error. |
| `reportPlayback` | OpenSubsonic (`playbackReport`) | `Not Implemented` | `Post-MVP` | Deferred until `playbackReport` extension is implemented. |

### 5.8 Sharing (`0 / 4`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getShares` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; public link sharing is out of scope for MVP. |
| `createShare` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; public link sharing is out of scope for MVP. |
| `updateShare` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; public link sharing is out of scope for MVP. |
| `deleteShare` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; public link sharing is out of scope for MVP. |

### 5.9 Podcast (`0 / 7`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getPodcasts` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; podcast management is out of scope for MVP. |
| `getNewestPodcasts` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; podcast management is out of scope for MVP. |
| `refreshPodcasts` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; podcast management is out of scope for MVP. |
| `createPodcastChannel` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; podcast management is out of scope for MVP. |
| `deletePodcastChannel` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; podcast management is out of scope for MVP. |
| `deletePodcastEpisode` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; podcast management is out of scope for MVP. |
| `downloadPodcastEpisode` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; podcast management is out of scope for MVP. |

### 5.10 Jukebox (`0 / 1`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `jukeboxControl` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; server-side audio playback is out of scope for MVP. |

### 5.11 Internet Radio (`0 / 4`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getInternetRadioStations` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; internet radio stations are out of scope for MVP. |
| `createInternetRadioStation` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; internet radio stations are out of scope for MVP. |
| `updateInternetRadioStation` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; internet radio stations are out of scope for MVP. |
| `deleteInternetRadioStation` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; internet radio stations are out of scope for MVP. |

### 5.12 Chat (`0 / 2`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getChatMessages` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; multi-user chat is out of scope for MVP. |
| `addChatMessage` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; multi-user chat is out of scope for MVP. |

### 5.13 User Management (`1 / 6`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getUser` | Subsonic 1.16.1 | `Implemented` | `MVP` | Returns synthetic user (echoes `username` or context principal) with `streamRole: true`, `downloadRole: false`, all other `*Role: false`, `scrobblingEnabled: false`, `folder: [1]`. |
| `getUsers` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred until multi-user account management is implemented. |
| `createUser` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred until multi-user account management is implemented. |
| `updateUser` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred until multi-user account management is implemented. |
| `deleteUser` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred until multi-user account management is implemented. |
| `changePassword` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred until user authentication is implemented. |

### 5.14 Bookmarks (`0 / 7`)

*Official OpenSubsonic category covering both track bookmarks and server-side play queues.*

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getBookmarks` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; track bookmarks are out of scope for MVP. |
| `createBookmark` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; track bookmarks are out of scope for MVP. |
| `deleteBookmark` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; track bookmarks are out of scope for MVP. |
| `getPlayQueue` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; server-side play queue sync is out of scope for MVP. |
| `savePlayQueue` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; server-side play queue sync is out of scope for MVP. |
| `getPlayQueueByIndex` | OpenSubsonic (`indexBasedQueue`) | `Not Implemented` | `Post-MVP` | Deferred until `indexBasedQueue` extension is implemented. |
| `savePlayQueueByIndex` | OpenSubsonic (`indexBasedQueue`) | `Not Implemented` | `Post-MVP` | Deferred until `indexBasedQueue` extension is implemented. |

### 5.15 Media Library Scanning (`1 / 2`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getScanStatus` | Subsonic 1.16.1 | `Implemented` | `MVP` | Reports atomic `scanning` flag and track `count` from `scanner.Service`, plus de-facto/Navidrome-compatible `lastScan` (RFC3339, emitted after first scan completes); `folderCount` is omitted because Headunit flattens host folders. |
| `startScan` | Subsonic 1.16.1 | `Not Implemented` | `Post-MVP` | Deferred; MVP scans automatically at startup, via live `fsnotify` watcher, and periodic `SCAN_INTERVAL` poll. |

### 5.16 Transcoding (`0 / 2`)

| Endpoint | Spec Origin | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `getTranscodeDecision` | OpenSubsonic (`transcoding`) | `Not Implemented` | `Post-MVP` | Deferred until `transcoding` extension (`ffmpeg` `Streamer` wrapper) is implemented. |
| `getTranscodeStream` | OpenSubsonic (`transcoding`) | `Not Implemented` | `Post-MVP` | Deferred until `transcoding` extension (`ffmpeg` `Streamer` wrapper) is implemented. |

---

## 6. OpenSubsonic Entity & Data-Model Extensions (`0 / 5` Entity Groups)

> [!NOTE]
> **OpenSubsonic Feature-Detection Rule:** Clients detect server support for OpenSubsonic entity extensions by checking whether extended fields are present in responses. If a server supports an extended field, it **must always emit that field** (e.g., empty string, `0`, or `[]` when unset) rather than omitting it. Headunit MVP emits standard Subsonic v1.16.1 fields plus basic audio tags/duration only (and omits `coverArt` across all entity payloads), so all formal OpenSubsonic entity extension fields tracked below are omitted in MVP (aside from the de-facto `scanStatus.lastScan` attribute on `getScanStatus`).

| Entity Group | OpenSubsonic Extended Fields | Status | Target | Notes |
| :--- | :--- | :--- | :--- | :--- |
| **`Child` (Song)** | `mediaType`, `musicBrainzId`, `isrc`, `sortName`, `played`, `genres`, `artists`, `displayArtist`, `albumArtists`, `displayAlbumArtist`, `contributors`, `displayComposer`, `bitDepth`, `samplingRate`, `channelCount`, `replayGain`, `bpm`, `comment`, `moods`, `groupings`, `explicitStatus`, `works`, `movements` | `Not Implemented` | `Post-MVP` | MVP emits standard Subsonic 1.16.1 `Child` fields (`id`, `parent`, `isDir`, `isVideo`, `title`, `album`, `artist`, `albumId`, `artistId`, `track`, `discNumber`, `year`, `genre`, `size`, `suffix`, `contentType`, `duration`, `bitRate`, `path`, `type`, `created`). |
| **`AlbumID3`** | `played`, `userRating`, `musicBrainzId`, `sortName`, `version`, `genres`, `artists`, `displayArtist`, `moods`, `releaseTypes`, `isCompilation`, `explicitStatus`, `releaseDate`, `originalReleaseDate`, `recordLabels`, `discTitles` | `Not Implemented` | `Post-MVP` | MVP emits standard Subsonic 1.16.1 `AlbumID3` fields (`id`, `name`, `artist`, `artistId`, `songCount`, `duration`, `created`, `year`, `genre`). |
| **`ArtistID3`** | `musicBrainzId`, `sortName`, `disambiguation`, `roles` | `Not Implemented` | `Post-MVP` | MVP emits standard Subsonic 1.16.1 `ArtistID3` fields (`id`, `name`, `albumCount`). |
| **`Playlist`** | `readonly`, `validUntil` | `Not Implemented` | `Post-MVP` | MVP returns an empty playlist array from `getPlaylists` stub. |
| **`Error`** | `helpUrl` | `Not Implemented` | `Post-MVP` | MVP emits standard `code` and `message` attributes only. |
