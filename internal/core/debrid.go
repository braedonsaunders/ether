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
	// FileName is the source-matched file hint, never proof of episode identity.
	FileName string `json:"fileName,omitempty"`
	// AbsoluteEpisode is the verified anime episode number across seasons.
	AbsoluteEpisode int `json:"absoluteEpisode,omitempty"`
	// Cour and CourEpisode identify the cour (part) and relative episode
	// within a multi-cour anime season pack (0 when unmapped).
	Cour          int      `json:"cour,omitempty"`
	CourEpisode   int      `json:"courEpisode,omitempty"`
	Season        int      `json:"season,omitempty"`
	Episode       int      `json:"episode,omitempty"`
	ShowYear      int      `json:"showYear,omitempty"`
	EpisodeTitle  string   `json:"episodeTitle,omitempty"`
	ShowTitles    []string `json:"showTitles,omitempty"`
	SpecialTitles []string `json:"specialTitles,omitempty"`
	// AddonSpecialEpisode is the verified IMDb number before catalogue renumbering.
	AddonSpecialEpisode int `json:"addonSpecialEpisode,omitempty"`
	// PackID is the untargeted release ID of a FullSeason pack before
	// TargetReleaseID derives the per-episode ID (additive).
	PackID string `json:"packId,omitempty"`
	// ExpectedIDs identify the requested series; ReportedIDs are the
	// release's indexer-reported series IDs, retained for file validation.
	ExpectedIDs IDs `json:"expectedIds,omitempty"`
	ReportedIDs IDs `json:"reportedIds,omitempty"`
}

// ErrWrongContent is a request-specific release mismatch, never an account
// failure or proof that the torrent is unusable for another title.
var ErrWrongContent = fmt.Errorf("release contains different content: %w", ErrNotAvailable)

// ErrNotCached reports that no debrid provider can serve a torrent release
// instantly. It is a per-release skip, never proof the release is broken,
// so it wraps ErrNotAvailable and is not a permanent failure.
var ErrNotCached = fmt.Errorf("not cached by any debrid provider: %w", ErrNotAvailable)

// ErrDownloadQueued reports that an uncached torrent was handed to a debrid
// account to download (cached-only off): not playable yet, but it will be.
var ErrDownloadQueued = fmt.Errorf("sent to the debrid service to download: %w", ErrNotCached)

// ErrDownloadRefused reports that an uncached torrent could not be sent to
// a debrid account to download (cached-only off): the service refused it
// (a blocked file, a refused add) or the add failed. Nothing is downloading.
var ErrDownloadRefused = fmt.Errorf("the debrid service did not take it to download: %w", ErrNotCached)

// ErrDownloadPaused means Ether temporarily stopped adding torrents after
// repeated provider refusals; no download was submitted during the pause.
var ErrDownloadPaused = fmt.Errorf("torrent adds are temporarily paused after provider refusals: %w", ErrDownloadRefused)

// ErrDownloadLimit is ErrDownloadRefused because the debrid account is at
// its active-download limit (Real-Debrid too_many_active_downloads, TorBox
// ACTIVE_LIMIT): no release of any title can start downloading for now.
var ErrDownloadLimit = fmt.Errorf("the debrid account is at its active-download limit: %w", ErrDownloadRefused)

// UncachedError is a resolve that failed only because no debrid account
// holds a torrent release of the title. It wraps the per-release errors and
// says what the owner's settings and other sources contributed, so a player
// message can name the real reason.
type UncachedError struct {
	Err error
	// CachedOnly is the owner's "Cached torrents only" setting at the time.
	CachedOnly bool
	// Usenet is what Usenet contributed: UsenetNone (a Usenet provider is
	// set up but the search found no Usenet release), UsenetFailed (Usenet
	// releases were found and none played), or "" (no Usenet provider).
	Usenet string
}

// UncachedError.Usenet values.
const (
	UsenetNone   = "none"
	UsenetFailed = "failed"
)

func (e *UncachedError) Error() string { return e.Err.Error() }
func (e *UncachedError) Unwrap() error { return e.Err }

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
