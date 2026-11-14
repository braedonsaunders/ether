package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Source is the release source parsed from a release name.
type Source string

const (
	SourceUnknown  Source = ""
	SourceCam      Source = "cam"      // CAM, TS, TC, HDCAM, telesync
	SourceScreener Source = "screener" // SCR, DVDSCR, R5, workprint
	SourceDVD      Source = "dvd"
	SourceHDTV     Source = "hdtv"
	SourceWEBRip   Source = "webrip"
	SourceWEBDL    Source = "webdl"
	SourceBluRay   Source = "bluray"
	SourceRemux    Source = "remux"
)

// RankPluginPrefix namespaces plugin-contributed ranking steps:
// "plugin:<plugin id>/<key>". A profile lists the steps it uses in
// QualityProfile.PluginScores; each adds the number its plugin returns for
// a release to that release's custom format score.
const RankPluginPrefix = "plugin:"

// IsPluginRankingStep reports whether step is plugin-namespaced
// ("plugin:<id>/<key>").
func IsPluginRankingStep(step string) bool {
	rest, ok := strings.CutPrefix(step, RankPluginPrefix)
	return ok && strings.Contains(rest, "/")
}

// ReleaseInfo is what can be parsed from a release name.
type ReleaseInfo struct {
	HardSubbed  bool     `json:"hardSubbed,omitempty"`
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
	Revision         int    `json:"revision,omitempty"`
	Cour             int    `json:"cour,omitempty"`
	Subbed           bool   `json:"subbed,omitempty"`
	Dubbed           bool   `json:"dubbed,omitempty"`
	Uncensored       bool   `json:"uncensored,omitempty"`
	Proper           bool   `json:"proper,omitempty"`
	Repack           bool   `json:"repack,omitempty"`
	Container        string `json:"container,omitempty"`
}

// Release is an indexer search result for a title.
type Release struct {
	SourceHash string      `json:"sourceHash,omitempty"` // SHA-256 of the fetched source descriptor, when known
	ID         string      `json:"id"`                   // stable: sha1(indexer + guid)
	GUID       string      `json:"guid"`                 // indexer guid
	Indexer    string      `json:"indexer"`              // indexer name
	Name       string      `json:"name"`                 // release name
	NZBURL     string      `json:"nzbUrl"`
	Size       int64       `json:"size"`
	PubDate    time.Time   `json:"pubDate"`
	Grabs      int         `json:"grabs,omitempty"`
	Password   bool        `json:"password,omitempty"` // indexer flagged passworded
	Category   int         `json:"category,omitempty"`
	Info       ReleaseInfo `json:"info"`
	Score      float64     `json:"score"`

	// IndexerID binds a result to its configured origin; Indexer is a display label.
	IndexerID string `json:"indexerId,omitempty"`
	// TitleIDs are movie/series identifiers reported by the indexer. They
	// must never be inferred from the search request's identifiers.
	TitleIDs IDs `json:"titleIds,omitempty"`
	// TitleIDsEchoed marks request IDs echoed by a source, not reported evidence.
	TitleIDsEchoed bool `json:"titleIdsEchoed,omitempty"`
	// FileName retains the add-on's matched filename for torrent file selection.
	FileName string `json:"fileName,omitempty"`

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
	// SourceLanguage is the release language of the indexer the result came
	// from (a Cardigann definition's `language`, or the owner's per-indexer
	// Release language for Newznab/Torznab/Prowlarr/Zilean), as a canonical
	// BCP 47 tag ("pt-BR"). It is evidence for a bare DUAL/MULTI whose name
	// names no dub; an explicit language in the name always wins (additive).
	SourceLanguage string `json:"sourceLanguage,omitempty"`
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
	StreamProvider string            `json:"streamProvider,omitempty"` // stored provider connection budget
	StreamURL      string            `json:"streamUrl,omitempty"`
	StreamHeaders  map[string]string `json:"streamHeaders,omitempty"`
	StreamOrigin   string            `json:"streamOrigin,omitempty"`
	// StreamOnDemand marks a StreamURL whose source prepares the release
	// when the link is opened (a Stremio add-on resolving, downloading or
	// importing it on request), so every open costs the source real work
	// (additive). See OpensOnDemand.
	StreamOnDemand bool `json:"streamOnDemand,omitempty"`
}

// Resolution is the resolver's answer for a title and client: the winning
// release, already health-checked, plus fallbacks for mid-stream failover.
type Resolution struct {
	// HealthVerified means the complete volume sample passed, rather than an
	// optimistic start or an inconclusive health check.
	HealthVerified  bool      `json:"healthVerified,omitempty"`
	QualityFallback string    `json:"qualityFallback,omitempty"`
	Title           RatingKey `json:"title"`
	Release         Release   `json:"release"`
	NZB             []byte    `json:"-"`
	Alternates      []Release `json:"alternates"` // next candidates, best first
	ResolvedAt      time.Time `json:"resolvedAt"`
}

// Media scopes a quality profile or rule. Every profile serves exactly one
// media; a client rule may leave it empty to apply to every media.
const (
	MediaAny   = ""
	MediaMovie = "movie"
	MediaTV    = "tv"
)

// ProfileMediaTypes lists the media a quality profile can serve, in the
// order the console shows them. It is the one list: validation, the
// upgrade split, shared-name resolution and the console's media options
// (PROFILE_MEDIA in web/src/lib/releases.ts) all follow it. Adding a media
// such as anime means adding its constant here and to PROFILE_MEDIA, plus
// classifying titles into it (titleMedia in the resolver); nothing else
// is restructured.
var ProfileMediaTypes = []string{MediaMovie, MediaTV}

// IsProfileMedia reports whether media is one a profile can serve.
func IsProfileMedia(media string) bool {
	for _, m := range ProfileMediaTypes {
		if m == media {
			return true
		}
	}
	return false
}

// FindProfile returns the profile named name that serves media. Names are
// unique per media, so the same name can serve movies and shows as two
// profiles; with no media it is the first profile of that name.
func FindProfile(profiles []QualityProfile, name, media string) (QualityProfile, bool) {
	if name == "" {
		return QualityProfile{}, false
	}
	for _, p := range profiles {
		if p.Name == name && MediaApplies(p.MediaType, media) {
			return p, true
		}
	}
	return QualityProfile{}, false
}

// ProfileServesEveryMedia reports whether a profile named name exists for
// every profile media, which makes the name shared policy before a title's
// media is known.
func ProfileServesEveryMedia(profiles []QualityProfile, name string) bool {
	for _, m := range ProfileMediaTypes {
		found := false
		for _, p := range profiles {
			if p.Name == name && p.MediaType == m {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// MediaApplies reports whether a scope (a profile or rule's MediaType)
// applies when choosing for media. An empty media means the caller has no
// title context and accepts any scope.
func MediaApplies(scope, media string) bool {
	if media == MediaAny {
		return true
	}
	return scope == MediaAny || scope == media
}

// MediaTypeForApp maps a Starr app to the media its profiles describe.
func MediaTypeForApp(app string) string {
	switch strings.ToLower(strings.TrimSpace(app)) {
	case "radarr":
		return MediaMovie
	case "sonarr":
		return MediaTV
	}
	return MediaAny
}

// ProfileImport records where a synced quality profile came from. Manual
// profiles leave it nil; a profile with Import set is updated in place by
// the guide sync instead of being duplicated.
type ProfileImport struct {
	Source   string    `json:"source"`             // "trash" or "dictionarry"
	App      string    `json:"app"`                // "radarr" or "sonarr"
	Profile  string    `json:"profile"`            // source profile id/slug
	TrashID  string    `json:"trashId,omitempty"`  // source trash_id
	ScoreSet string    `json:"scoreSet,omitempty"` // TRaSH score set
	Groups   []string  `json:"groups,omitempty"`   // selected cf-group trash IDs
	SyncedAt time.Time `json:"syncedAt,omitempty"`
}

// A quality profile's Language values beside a specific language.
const (
	ProfileLanguageOriginal = "original" // each title's original language
	ProfileLanguageAny      = "any"      // every language
)

// QualityProfile is an owner-editable rule set ranking releases for a class
// of client.
type QualityProfile struct {
	ReleaseRules []ReleaseRule `json:"releaseRules,omitempty"`
	// PluginScores lists the plugin ranking steps the profile uses, each
	// with its settings ({} for none). A step adds the number its plugin
	// returns for a release to the release's custom format score.
	PluginScores     map[string]json.RawMessage `json:"pluginScores,omitempty"`
	AllowedQualities []string                   `json:"allowedQualities,omitempty"` // imported source-resolution gates
	// QualityOrder ranks qualities best first, as Radarr and Sonarr do: a
	// release ranks by the first item listing its quality, qualities in one
	// item (a group) rank equally, and a quality no item lists ranks below
	// all of them. It is the first ranking key; custom format scores order
	// releases of equal quality (docs/quality-ranking.md).
	QualityOrder []QualityItem `json:"qualityOrder,omitempty"`
	// CustomFormatScores scores every format the profile uses, keyed by
	// format id: the owner's and imported custom formats, Ether's built-in
	// formats ("ether:…", BuiltinFormats) and Prefer rules ("rule:<id>").
	// A format the map does not list scores 0, as in Radarr and Sonarr.
	CustomFormatScores map[string]int `json:"customFormatScores,omitempty"`
	// MinCustomFormatScore rejects a release whose custom formats (the
	// owner's and imported ones, not Ether's built-ins, rules or plugins)
	// score strictly lower. nil sets no minimum.
	MinCustomFormatScore *int           `json:"minCustomFormatScore,omitempty"`
	Name                 string         `json:"name"`
	MediaType            string         `json:"mediaType,omitempty"` // "movie" or "tv" (ProfileMediaTypes); empty only in configs saved before profiles were per media
	Import               *ProfileImport `json:"import,omitempty"`    // set on imported, syncable profiles
	MaxHeight            int            `json:"maxHeight"`           // cap, e.g. 1080
	// ConfiguredMaxHeight is MaxHeight as the owner configured it,
	// before download reshaping or a client's caps clamped the
	// effective MaxHeight. -1 means the owner set no ceiling ("Any");
	// 0 means unset (a profile built outside SelectProfile), in which
	// case MaxHeight itself is the configured value. Runtime-only,
	// never persisted with the profile.
	ConfiguredMaxHeight    int      `json:"-"`
	FallbackBelowMin       bool     `json:"fallbackBelowMin,omitempty"`
	MinHeight              int      `json:"minHeight"` // floor, e.g. 720
	AllowHDR               bool     `json:"allowHdr"`  // only applied when the client also supports it
	AllowDolbyVision       bool     `json:"allowDolbyVision"`
	MinSizeMBPerMin        float64  `json:"minSizeMbPerMin"` // size sanity, per minute of runtime (movies, and fallback for TV)
	MaxSizeMBPerMin        float64  `json:"maxSizeMbPerMin"`
	EpisodeMinSizeMBPerMin float64  `json:"episodeMinSizeMbPerMin,omitempty"` // optional TV episode size bounds (0 uses MinSizeMBPerMin/MaxSizeMBPerMin)
	EpisodeMaxSizeMBPerMin float64  `json:"episodeMaxSizeMbPerMin,omitempty"`
	PreferredLanguages     []string `json:"preferredLanguages"`
	// Language is the profile's audio language, as in Radarr: "" or
	// ProfileLanguageOriginal accepts each title's original language
	// alongside the preferred ones, ProfileLanguageAny accepts every
	// language, and any other value names the one language a release's
	// audio must be in (an ISO code or a language name; multi/dual-audio
	// releases pass).
	Language string `json:"language,omitempty"`
	AllowCam bool   `json:"allowCam"` // the only pre-release switch; CAM/TS/TC and screener/R5/workprint copies play only for profiles that allow them
	// SelectionKey is the canonical hash of this profile's configured
	// definition, computed by resolver.SelectProfile from the pristine
	// configured profile before download reshaping or client-cap
	// clamping. Release rules are part of that definition. The catalogue
	// original language is not: it is per-title evaluation context, not a
	// profile fact. Runtime-only, never persisted with the profile.
	SelectionKey string `json:"-"`
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
// edit (heights, HDR/DV, sizes, qualities, scores, release rules, plugin
// steps) changes it; client caps never enter it.
func (p QualityProfile) ComputeSelectionKey() string {
	p.SelectionKey = ""
	// Provenance is not part of the ranking definition: a sync updating
	// SyncedAt must not change the key that keys resumes and warm entries.
	p.Import = nil
	// Neither is the media a profile serves: splitting a shared profile
	// into its movie and TV copies keeps every remembered release valid.
	p.MediaType = ""
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// QualityItem is one entry of a profile's quality order: a single quality
// or a named group of qualities that rank equally. A quality is
// "<source>-<height>" ("webdl-1080", "remux-2160"); source is a Source
// value, or "unknown" for a release that names a resolution but no source.
type QualityItem struct {
	Name      string   `json:"name,omitempty"`
	Qualities []string `json:"qualities"`
}

// ReportedTitleIDs excludes identifiers copied from a search request.
func (r Release) ReportedTitleIDs() IDs {
	if r.TitleIDsEchoed {
		return IDs{}
	}
	return r.TitleIDs
}
