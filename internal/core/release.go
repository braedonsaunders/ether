package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// Ranking ladder step keys for QualityProfile.Ranking.
const (
	RankRulePrefix  = "rule:"
	RankLanguage    = "language"
	RankConnection  = "connection"
	RankResolution  = "resolution"
	RankReleaseType = "releaseType"
	RankHDR         = "hdr"
	RankVerified    = "verified"
	RankVideoCodec  = "videoCodec"
	RankAudioFormat = "audioFormat"
	RankSize        = "size"
)

// DefaultRankingSteps is the built-in ranking ladder order after any Prefer rules.
var DefaultRankingSteps = []string{
	RankLanguage,
	RankConnection,
	RankResolution,
	RankReleaseType,
	RankHDR,
	RankVerified,
	RankVideoCodec,
	RankAudioFormat,
	RankSize,
}

func builtinRankingIndex(step string) int {
	return slices.Index(DefaultRankingSteps, step)
}

func isBuiltinRankingStep(step string) bool {
	return builtinRankingIndex(step) >= 0
}

// DefaultRanking returns today's default ladder order: each Prefer rule in
// rules (in slice order), followed by DefaultRankingSteps.
func DefaultRanking(rules []ReleaseRule) []string {
	out := make([]string, 0, len(rules)+len(DefaultRankingSteps))
	seen := map[string]bool{}
	for _, r := range rules {
		if r.Action == "prefer" && r.ID != "" && !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, RankRulePrefix+r.ID)
		}
	}
	out = append(out, DefaultRankingSteps...)
	return out
}

// ValidateRanking checks an explicit ranking ladder against a profile's rules.
// An empty ladder is valid and means DefaultRanking(rules).
func ValidateRanking(ranking []string, rules []ReleaseRule) error {
	if len(ranking) == 0 {
		return nil
	}
	validRules := map[string]bool{}
	for _, r := range rules {
		if r.Action == "prefer" && r.ID != "" {
			validRules[RankRulePrefix+r.ID] = true
		}
	}
	seen := map[string]bool{}
	for _, step := range ranking {
		if seen[step] {
			return fmt.Errorf("duplicate ranking step %q", step)
		}
		seen[step] = true
		if strings.HasPrefix(step, RankRulePrefix) {
			if !validRules[step] {
				return fmt.Errorf("ranking step %q does not name a prefer rule", step)
			}
			continue
		}
		if !isBuiltinRankingStep(step) {
			return fmt.Errorf("unknown ranking step %q", step)
		}
	}
	if !seen[RankConnection] || !seen[RankVerified] {
		return fmt.Errorf("ranking must include %q and %q", RankConnection, RankVerified)
	}
	return nil
}

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
	Ranking              []string       `json:"ranking,omitempty"`
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
	ConfiguredMaxHeight    int      `json:"-"`
	MinHeight              int      `json:"minHeight"` // floor, e.g. 720
	AllowHDR               bool     `json:"allowHdr"`  // only applied when the client also supports it
	AllowDolbyVision       bool     `json:"allowDolbyVision"`
	PreferredSources       []Source `json:"preferredSources"` // best first
	PreferredCodecs        []string `json:"preferredCodecs"`  // best first; codecs the client can't play are dropped anyway
	PreferredAudio         []string `json:"preferredAudio"`
	MinSizeMBPerMin        float64  `json:"minSizeMbPerMin"` // size sanity, per minute of runtime (movies, and fallback for TV)
	MaxSizeMBPerMin        float64  `json:"maxSizeMbPerMin"`
	EpisodeMinSizeMBPerMin float64  `json:"episodeMinSizeMbPerMin,omitempty"` // optional TV episode size bounds (0 uses MinSizeMBPerMin/MaxSizeMBPerMin)
	EpisodeMaxSizeMBPerMin float64  `json:"episodeMaxSizeMbPerMin,omitempty"`
	PreferLargest          bool     `json:"preferLargest,omitempty"` // prefer larger files among candidates matching the profile
	PreferredLanguages     []string `json:"preferredLanguages"`
	AllowCam               bool     `json:"allowCam"` // the only CAM switch; cinema recordings play only for profiles that allow them
	// SelectionKey is the canonical hash of this profile's configured
	// definition, computed by resolver.SelectProfile from the pristine
	// configured profile before download reshaping or client-cap
	// clamping. Release rules are part of that definition. The catalogue
	// original language is not: it is per-title evaluation context, not a
	// profile fact. Runtime-only, never persisted with the profile.
	SelectionKey string `json:"-"`
}

// EffectiveRanking returns the complete ranking ladder for p: DefaultRanking
// when Ranking is empty, or p.Ranking with any unmentioned Prefer rules
// prepended and any omitted built-in steps placed in their default relative
// positions.
func (p QualityProfile) EffectiveRanking() []string {
	if len(p.Ranking) == 0 {
		return DefaultRanking(p.ReleaseRules)
	}
	validRules := map[string]bool{}
	var preferOrder []string
	for _, r := range p.ReleaseRules {
		if r.Action == "prefer" && r.ID != "" {
			key := RankRulePrefix + r.ID
			if !validRules[key] {
				validRules[key] = true
				preferOrder = append(preferOrder, key)
			}
		}
	}
	mentioned := map[string]bool{}
	var filtered []string
	for _, step := range p.Ranking {
		if mentioned[step] {
			continue
		}
		if strings.HasPrefix(step, RankRulePrefix) {
			if !validRules[step] {
				continue
			}
		} else if !isBuiltinRankingStep(step) {
			continue
		}
		mentioned[step] = true
		filtered = append(filtered, step)
	}
	out := make([]string, 0, len(preferOrder)+len( DefaultRankingSteps))
	for _, key := range preferOrder {
		if !mentioned[key] {
			mentioned[key] = true
			out = append(out, key)
		}
	}
	out = append(out, filtered...)
	for bi, b := range DefaultRankingSteps {
		if mentioned[b] {
			continue
		}
		inserted := false
		for i, step := range out {
			if si := builtinRankingIndex(step); si > bi {
				out = slices.Insert(out, i, b)
				inserted = true
				break
			}
		}
		if !inserted {
			out = append(out, b)
		}
	}
	return out
}

// SizeBoundsForKey returns the configured (min, max) MB/min bounds for a rating key.
// TV episodes use EpisodeMinSizeMBPerMin / EpisodeMaxSizeMBPerMin when positive,
// falling back to MinSizeMBPerMin / MaxSizeMBPerMin.
func (p QualityProfile) SizeBoundsForKey(key RatingKey) (minMBPerMin, maxMBPerMin float64) {
	minMBPerMin, maxMBPerMin = p.MinSizeMBPerMin, p.MaxSizeMBPerMin
	if key.Kind() == KindEpisode {
		if p.EpisodeMinSizeMBPerMin > 0 {
			minMBPerMin = p.EpisodeMinSizeMBPerMin
		}
		if p.EpisodeMaxSizeMBPerMin > 0 {
			maxMBPerMin = p.EpisodeMaxSizeMBPerMin
		}
	}
	return minMBPerMin, maxMBPerMin
}

// SizeBoundsFor returns the configured (min, max) MB/min bounds for a title.
func (p QualityProfile) SizeBoundsFor(t *Title) (minMBPerMin, maxMBPerMin float64) {
	if t != nil && (t.Type == TypeEpisode || t.Key.Kind() == KindEpisode) {
		return p.SizeBoundsForKey(EpisodeKey(1, 1, 1))
	}
	return p.MinSizeMBPerMin, p.MaxSizeMBPerMin
}

// ComputeSelectionKey returns the canonical key of the configured
// definition in p: sha256 of its JSON (runtime-only fields tagged
// json:"-" are excluded) truncated to 16 hex chars. Any selection-field
// edit (heights, HDR/DV, codecs, sources, audio, languages, sizes,
// qualities, scores, release rules, ranking) changes it; client caps
// never enter it.
func (p QualityProfile) ComputeSelectionKey() string {
	p.SelectionKey = ""
	eff := p.EffectiveRanking()
	if slices.Equal(eff, DefaultRanking(p.ReleaseRules)) {
		p.Ranking = nil
	} else {
		p.Ranking = eff
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}
