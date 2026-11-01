package core

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
)

// Release protocols (Release.Protocol).
const (
	ProtocolUsenet  = ""        // the zero value: an NZB from a Newznab indexer
	ProtocolTorrent = "torrent" // a Torznab result streamed through a debrid service
	// ProtocolUnstated is a filename preview with no protocol fact.
	// Live releases keep the empty-string Usenet default; this value
	// exists so a preview cannot report that default as evidence.
	ProtocolUnstated = "unstated"
	// ProtocolHTTP is a stream URL a Stremio addon resolved itself (often
	// through the addon owner's own debrid account), read with HTTP range
	// requests. It needs no debrid account in Ether (additive). The same
	// name is Plex for Mac / Desktop's protocol=http conversion (a chunked
	// Matroska stream on /video/:/transcode/universal/start).
	ProtocolHTTP = "http"
)

// ReleaseTarget is the episode wanted from a multi-file release such as a
// season pack.
type ReleaseTarget struct {
	Season       int    `json:"season,omitempty"`
	Episode      int    `json:"episode,omitempty"`
	ShowYear     int    `json:"showYear,omitempty"`
	EpisodeTitle string `json:"episodeTitle,omitempty"`
	// ExpectedIDs identify the requested series; ReportedIDs are the
	// release's indexer-reported series IDs, retained for file validation.
	ExpectedIDs IDs `json:"expectedIds,omitempty"`
	ReportedIDs IDs `json:"reportedIds,omitempty"`
}

// ErrNotCached reports that no debrid provider can serve a torrent release
// instantly. It is a per-release skip, never proof the release is broken,
// so it wraps ErrNotAvailable and is not a permanent failure.
var ErrNotCached = fmt.Errorf("not cached by any debrid provider: %w", ErrNotAvailable)

// ErrDownloadQueued reports that an uncached torrent was handed to a debrid
// account to download (cached-only off): not playable yet, but it will be.
var ErrDownloadQueued = fmt.Errorf("sent to the debrid service to download: %w", ErrNotCached)

// ErrNoSources reports that there is nothing to search: no enabled indexer
// at all. A configuration problem, not proof a title has no release, so
// callers must not record the title as unavailable.
var ErrNoSources = fmt.Errorf("no indexers to search: %w", ErrNotAvailable)

// ErrNoTorrentSources reports that debrid is the only way to play (no
// Usenet provider) but no enabled indexer returns torrents (Torznab, a
// Stremio addon or a torrent built-in site).
var ErrNoTorrentSources = fmt.Errorf("debrid has no torrent indexer to search: %w", ErrNoSources)

// ErrDebridAuth is a debrid service rejecting the configured API key (or an
// account that cannot use the API). A configuration problem, never a
// release failure.
var ErrDebridAuth = errors.New("debrid: authentication failed")

// TargetReleaseID derives a stable release ID for one episode of a
// multi-file release, so health, pins, cache and bad marks apply to the
// episode file rather than the whole pack.
func TargetReleaseID(releaseID string, t ReleaseTarget) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|s%de%d", releaseID, t.Season, t.Episode)))
	return hex.EncodeToString(h[:])
}
