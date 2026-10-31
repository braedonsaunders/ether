package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Source is the release source parsed from a release name.
type Source string

const (
	SourceUnknown  Source = ""
	SourceCam      Source = "cam" // CAM, TS, TC, HDCAM, telesync
	SourceScreener Source = "screener"
	SourceDVD      Source = "dvd"
	SourceHDTV     Source = "hdtv"
	SourceWEBRip   Source = "webrip"
	SourceWEBDL    Source = "webdl"
	SourceBluRay   Source = "bluray"
	SourceRemux    Source = "remux"
)

// ReleaseInfo is what can be parsed from a release name.
type ReleaseInfo struct {
	Title       string   `json:"title"`
	Edition     string   `json:"edition,omitempty"`
	Year        int      `json:"year,omitempty"`
	Seasons     []int    `json:"seasons,omitempty"`
	Episodes    []int    `json:"episodes,omitempty"`
	FullSeason  bool     `json:"fullSeason,omitempty"`
	Height      int      `json:"height,omitempty"` // 480, 720, 1080, 2160
	Source      Source   `json:"source,omitempty"`
	VideoCodec  string   `json:"videoCodec,omitempty"` // h264, hevc, av1
	HDR10       bool     `json:"hdr10,omitempty"`
	DolbyVision bool     `json:"dolbyVision,omitempty"`
	AudioCodec  string   `json:"audioCodec,omitempty"` // aac, ac3, eac3, truehd, dts, dtshd
	Atmos       bool     `json:"atmos,omitempty"`
	Channels    string   `json:"channels,omitempty"` // "5.1"
	Group       string   `json:"group,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	// SubtitleLanguages are subtitle languages a release declares without
	// claiming the audio (VOSTFR, VOSE, SUBFRENCH) (additive).
	SubtitleLanguages []string `json:"subtitleLanguages,omitempty"`
	// AbsoluteEpisodes are episode numbers counted from the series start,
	// as anime releases number them ("Show - 1071") (additive).
	AbsoluteEpisodes []int  `json:"absoluteEpisodes,omitempty"`
	Proper           bool   `json:"proper,omitempty"`
	Repack           bool   `json:"repack,omitempty"`
	Container        string `json:"container,omitempty"`
}

// Release is an indexer search result for a title.
type Release struct {
	ID       string      `json:"id"`      // stable: sha1(indexer + guid)
	GUID     string      `json:"guid"`    // indexer guid
	Indexer  string      `json:"indexer"` // indexer name
	Name     string      `json:"name"`    // release name
	NZBURL   string      `json:"nzbUrl"`
	Size     int64       `json:"size"`
	PubDate  time.Time   `json:"pubDate"`
	Grabs    int         `json:"grabs,omitempty"`
	Password bool        `json:"password,omitempty"` // indexer flagged passworded
	Category int         `json:"category,omitempty"`
	Info     ReleaseInfo `json:"info"`
	Score    float64     `json:"score"`
	// TitleIDs are movie/series identifiers reported by the indexer. They
	// must never be inferred from the search request's identifiers.
	TitleIDs IDs `json:"titleIds,omitempty"`

	// Torrent releases (additive). Protocol is "" for Usenet and
	// ProtocolTorrent for Torznab results; torrents are streamed through
	// an owner-configured debrid service (internal/debrid).
	Protocol  string `json:"protocol,omitempty"`
	InfoHash  string `json:"infoHash,omitempty"` // lower-case hex BitTorrent v1 info hash
	MagnetURL string `json:"magnetUrl,omitempty"`
	Seeders   int    `json:"seeders,omitempty"`
	Peers     int    `json:"peers,omitempty"`
	// Target narrows a multi-file release (a season pack) to the wanted
	// episode; set by the resolver on candidates before fetching.
	Target *ReleaseTarget `json:"target,omitempty"`
	// DebridCached and DebridUncached name the debrid services (config
	// Debrid* kinds) the source reported as holding, or not holding, the
	// torrent, such as a Stremio addon's cached marker (additive). Hints
	// only: a provider's own answer always wins.
	DebridCached   []string `json:"debridCached,omitempty"`
	DebridUncached []string `json:"debridUncached,omitempty"`
	// HTTP stream releases (ProtocolHTTP, additive). StreamURL is the URL
	// the addon answered with; it and StreamHeaders (request headers the
	// addon requires) can carry the addon's credentials, so they never
	// leave the server. StreamOrigin is the owner-configured addon origin
	// (scheme://host:port), the only private address the URL may reach.
	StreamURL     string            `json:"streamUrl,omitempty"`
	StreamHeaders map[string]string `json:"streamHeaders,omitempty"`
	StreamOrigin  string            `json:"streamOrigin,omitempty"`
}

// Resolution is the resolver's answer for a title and client: the winning
// release, already health-checked, plus fallbacks for mid-stream failover.
type Resolution struct {
	Title      RatingKey `json:"title"`
	Release    Release   `json:"release"`
	NZB        []byte    `json:"-"`
	Alternates []Release `json:"alternates"` // next candidates, best first
	ResolvedAt time.Time `json:"resolvedAt"`
}

// QualityProfile is an owner-editable rule set ranking releases for a class
// of client.
type QualityProfile struct {
	ReleaseRules         []ReleaseRule  `json:"releaseRules,omitempty"`
	AllowedQualities     []string       `json:"allowedQualities,omitempty"` // imported source-resolution gates
	CustomFormatScores   map[string]int `json:"customFormatScores,omitempty"`
	MinCustomFormatScore *int           `json:"minCustomFormatScore,omitempty"`
	Name                 string         `json:"name"`
	MaxHeight            int            `json:"maxHeight"` // cap, e.g. 1080
	// ConfiguredMaxHeight is MaxHeight as the owner configured it,
	// before download reshaping or a client's caps clamped the
	// effective MaxHeight. -1 means the owner set no ceiling ("Any");
	// 0 means unset (a profile built outside SelectProfile), in which
	// case MaxHeight itself is the configured value. Runtime-only,
	// never persisted with the profile.
	ConfiguredMaxHeight int      `json:"-"`
	MinHeight           int      `json:"minHeight"` // floor, e.g. 720
	AllowHDR            bool     `json:"allowHdr"`  // only applied when the client also supports it
	AllowDolbyVision    bool     `json:"allowDolbyVision"`
	PreferredSources    []Source `json:"preferredSources"` // best first
	PreferredCodecs     []string `json:"preferredCodecs"`  // best first; codecs the client can't play are dropped anyway
	PreferredAudio      []string `json:"preferredAudio"`
	MinSizeMBPerMin     float64  `json:"minSizeMbPerMin"` // size sanity, per minute of runtime
	MaxSizeMBPerMin     float64  `json:"maxSizeMbPerMin"`
	PreferredLanguages  []string `json:"preferredLanguages"`
	AllowCam            bool     `json:"allowCam"` // also gated by the owner's global cam setting
	// SelectionKey is the canonical hash of this profile's configured
	// definition, computed by resolver.SelectProfile from the pristine
	// configured profile before download reshaping or client-cap
	// clamping. Release rules are part of that definition. The catalogue
	// original language is not: it is per-title evaluation context, not a
	// profile fact. Runtime-only, never persisted with the profile.
	SelectionKey string `json:"-"`
}

// ComputeSelectionKey returns the canonical key of the configured
// definition in p: sha256 of its JSON (runtime-only fields tagged
// json:"-" are excluded) truncated to 16 hex chars. Any selection-field
// edit (heights, HDR/DV, codecs, sources, audio, languages, sizes,
// qualities, scores, release rules) changes it; client caps never enter it.
func (p QualityProfile) ComputeSelectionKey() string {
	p.SelectionKey = ""
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}
