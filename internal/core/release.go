package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
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
	AbsoluteEpisodes []int `json:"absoluteEpisodes,omitempty"`
	// AirDate is the episode broadcast date parsed from YYMMDD, YYYYMMDD, or
	// YYYY.MM.DD / YYYY-MM-DD tokens, normalized to "YYYY-MM-DD" (additive).
	AirDate    string `json:"airDate,omitempty"`
	Revision   int    `json:"revision,omitempty"`
	Cour       int    `json:"cour,omitempty"`
	Subbed     bool   `json:"subbed,omitempty"`
	Dubbed     bool   `json:"dubbed,omitempty"`
	Uncensored bool   `json:"uncensored,omitempty"`
	Proper     bool   `json:"proper,omitempty"`
	Repack     bool   `json:"repack,omitempty"`
	Container  string `json:"container,omitempty"`
	// CourRelative marks episodes counted within Cour ("S3 Part 2 - 03"),
	// not within the season ("S04.Part.2.S04E09") (additive).
	CourRelative bool `json:"courRelative,omitempty"`
}

// Release is an indexer search result for a title.
type Release struct {
	SourceHash string      `json:"sourceHash,omitempty"` // SHA-256 of the fetched source descriptor, when known
	ContentID  string      `json:"contentId,omitempty"`  // cross-indexer content identity ("nzb:<sha256>" once NZB is known)
	Poster     string      `json:"poster,omitempty"`     // Usenet poster reported by indexer or NZB
	UsenetDate time.Time   `json:"usenetDate,omitempty"` // Usenet upload timestamp when reported separately from PubDate
	Fallbacks  []Release   `json:"-"`                    // alternate indexer copies merged during multi-indexer search
	ID         string      `json:"id"`                   // stable: sha1(indexer + guid)
	GUID       string      `json:"guid"`                 // indexer guid
	Indexer    string      `json:"indexer"`              // indexer name
	Name       string      `json:"name"`                 // release name
	NZBURL     string      `json:"nzbUrl"`
	NZBRef     string      `json:"-"` // opaque plugin NZB reference
	TorrentRef string      `json:"-"` // opaque plugin torrent descriptor reference
	Size       int64       `json:"size"`
	PubDate    time.Time   `json:"pubDate"`
	Grabs      int         `json:"grabs,omitempty"`
	Password   bool        `json:"password,omitempty"` // indexer flagged passworded
	Category   int         `json:"category,omitempty"`
	Info       ReleaseInfo `json:"info"`
	Score      float64     `json:"score"`

	// IndexerID binds a result to its configured origin; Indexer is a display label.
	IndexerID string `json:"indexerId,omitempty"`
	// SourceOrder is the 1-based position of this result in the source's own
	// response (set for Stremio add-on results so later cache sorting or
	// batching cannot lose it). Zero means unset.
	SourceOrder int `json:"sourceOrder,omitempty"`
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
	// TorrentFiles holds the file list parsed from a .torrent metainfo
	// document during ResolveTorrent, when available. In-memory only.
	TorrentFiles []ReleaseFile `json:"-"`
	// FileCount hints how many files a torrent contains when TorrentFiles has
	// not been parsed (from indexer metadata or a plugin release hint). Debrid
	// providers that zip high-file-count torrents use it to avoid trusting a
	// cache-check miss on zip-only torrents.
	FileCount int `json:"fileCount,omitempty"`
	// Private marks a private-tracker torrent (info.private == 1 or a
	// Cardigann definition with type: private). Debrid providers may only
	// check or play an existing cache by bare info hash.
	Private bool `json:"private,omitempty"`
	Seeders int  `json:"seeders,omitempty"`
	Peers   int  `json:"peers,omitempty"`
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
	PluginStream bool `json:"-"` // direct HTTP link supplied by an Ether plugin
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
	// AddonSubtitles contains sealed references for sidecars offered with this file.
	AddonSubtitles []AddonSubtitle `json:"addonSubtitles,omitempty"`
	// IndexerSubtitles are the subtitle languages the indexer lists for the
	// release (the Newznab "subs" attribute), as release language names.
	// Empty means the indexer said nothing, not that there are none (additive).
	IndexerSubtitles []string `json:"indexerSubtitles,omitempty"`
}

const AddonSubtitleSealContext = "addon-stream-subtitle"

type AddonSubtitle struct {
	Language  string `json:"language"`
	Reference string `json:"reference"`
	Forced    bool   `json:"forced,omitempty"`
}

// IsAddonHTTPStream reports whether an add-on supplies the opened HTTP file.
func (r Release) IsAddonHTTPStream() bool {
	return r.Protocol == ProtocolHTTP && r.StreamOrigin != "" && r.StreamProvider == ""
}

// KnownFileCount returns the torrent's known file count from FileCount or
// parsed TorrentFiles, or 0 when unknown.
func (r Release) KnownFileCount() int {
	return max(r.FileCount, len(r.TorrentFiles))
}

// PreFetchContentIDPrefix namespaces Usenet pre-fetch content identities
// ("pre:<sizeBucket>|<poster>|<uploadDay>").
const PreFetchContentIDPrefix = "pre:"

// NormalizeReleasePoster normalizes a Usenet poster string for content identity matching.
func NormalizeReleasePoster(poster string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(poster)))
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

// ReleaseSizeBucket maps a positive byte size to a logarithmic ~10% bucket so
// small yEnc/NFO accounting differences across indexers land in the same bucket.
func ReleaseSizeBucket(size int64) int64 {
	if size <= 0 {
		return 0
	}
	b := int64(math.Round(math.Log(float64(size)) / math.Log(1.10)))
	if b < 1 {
		return 1
	}
	return b
}

// PreFetchContentIDFor builds a pre-fetch Usenet content identity from a byte
// size, poster, and upload date. Missing fields return "".
func PreFetchContentIDFor(size int64, poster string, uploadDate time.Time) string {
	bucket := ReleaseSizeBucket(size)
	normPoster := NormalizeReleasePoster(poster)
	if bucket <= 0 || normPoster == "" || uploadDate.IsZero() {
		return ""
	}
	return fmt.Sprintf("%s%d|%s|%s", PreFetchContentIDPrefix, bucket, normPoster, uploadDate.UTC().Format("2006-01-02"))
}

// parseBTIHGUIDSuffix parses a Stremio-style torrent GUID suffix from
// "btih:<hash>/s<season>e<episode>" or "btih:<hash>/<fileIndex>".
func parseBTIHGUIDSuffix(guid string) (season, episode int, hasEpisode bool, fileIndex int, hasFileIndex bool) {
	guid = strings.TrimSpace(guid)
	if len(guid) <= len("btih:") || !strings.EqualFold(guid[:len("btih:")], "btih:") {
		return 0, 0, false, 0, false
	}
	rest := guid[len("btih:"):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash+1 >= len(rest) {
		return 0, 0, false, 0, false
	}
	suffix := rest[slash+1:]
	if len(suffix) >= 4 && (suffix[0] == 's' || suffix[0] == 'S') {
		ePos := strings.IndexAny(suffix[1:], "eE")
		if ePos > 0 {
			s, sOK := parseNonNegDigits(suffix[1 : 1+ePos])
			e, eOK := parseNonNegDigits(suffix[1+ePos+1:])
			if sOK && eOK && e > 0 {
				return s, e, true, 0, false
			}
		}
	}
	if idx, ok := parseNonNegDigits(suffix); ok {
		return 0, 0, false, idx, true
	}
	return 0, 0, false, 0, false
}

func parseNonNegDigits(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func (r Release) explicitFileSelectionKey() string {
	if r.Target == nil {
		return ""
	}
	return r.Target.fileSelectionKey()
}

func (r Release) guidFileSelectionKey() string {
	_, _, _, idx, ok := parseBTIHGUIDSuffix(r.GUID)
	if !ok {
		return ""
	}
	return (ReleaseTarget{FileIndex: &idx}).fileSelectionKey()
}

// EpisodeScope returns the single season and episode targeted by r when r
// addresses one episode (via Target, a Stremio-style btih:<hash>/sNeM GUID,
// or a single-episode torrent parse). Target takes precedence over parsed
// fields so retargeted or vaulted packs use the requested episode. Untargeted
// Usenet releases return ok=false so single-episode NZBs preserve unscoped
// cross-indexer identities.
func (r Release) EpisodeScope() (season, episode int, ok bool) {
	if r.Target != nil && r.Target.Season >= 0 && r.Target.Episode > 0 {
		return r.Target.Season, r.Target.Episode, true
	}
	if s, e, hasEp, _, _ := parseBTIHGUIDSuffix(r.GUID); hasEp {
		return s, e, true
	}
	if (r.Protocol == ProtocolTorrent || strings.TrimSpace(r.InfoHash) != "") && !r.Info.FullSeason {
		if len(r.Info.Episodes) == 1 && r.Info.Episodes[0] > 0 {
			season := 0
			if len(r.Info.Seasons) == 1 && r.Info.Seasons[0] >= 0 {
				season = r.Info.Seasons[0]
			}
			return season, r.Info.Episodes[0], true
		}
		if len(r.Info.Episodes) == 0 && len(r.Info.AbsoluteEpisodes) == 1 && r.Info.AbsoluteEpisodes[0] > 0 {
			return 0, r.Info.AbsoluteEpisodes[0], true
		}
	}
	return 0, 0, false
}

// HasExplicitFileTarget reports whether r narrows a multi-file release to a
// specific file, archive member, or track rather than an episode.
func (r Release) HasExplicitFileTarget() bool {
	if r.explicitFileSelectionKey() != "" {
		return true
	}
	if _, _, ok := r.EpisodeScope(); ok {
		return false
	}
	return r.guidFileSelectionKey() != ""
}

// HasFileOrEpisodeScope reports whether r narrows a multi-file release to a
// single file/member/track or a single episode.
func (r Release) HasFileOrEpisodeScope() bool {
	if r.explicitFileSelectionKey() != "" {
		return true
	}
	if _, _, ok := r.EpisodeScope(); ok {
		return true
	}
	return r.guidFileSelectionKey() != ""
}

// ScopedContentID narrows base to the targeted file or episode when r selects
// one entry inside a multi-file release (an explicit file/member/track target,
// an episode target/GUID/single-episode torrent, or a btih:<hash>/<fileIdx>
// GUID), so health, blocklist, and probe cache state do not bleed across other
// files or episodes of the same pack.
func (r Release) ScopedContentID(base string) string {
	if base == "" {
		return ""
	}
	if sel := r.explicitFileSelectionKey(); sel != "" {
		suffix := "|file:" + sel
		if strings.HasSuffix(base, suffix) {
			return base
		}
		return base + suffix
	}
	if season, episode, ok := r.EpisodeScope(); ok {
		return TargetReleaseID(base, ReleaseTarget{Season: season, Episode: episode})
	}
	if sel := r.guidFileSelectionKey(); sel != "" {
		suffix := "|file:" + sel
		if strings.HasSuffix(base, suffix) {
			return base
		}
		return base + suffix
	}
	return base
}

// PreFetchContentID returns r's pre-fetch Usenet content identity
// (size bucket + poster + upload day), or "" when those facts are absent.
func (r Release) PreFetchContentID() string {
	if r.Protocol != ProtocolUsenet {
		return ""
	}
	ts := r.UsenetDate
	if ts.IsZero() {
		ts = r.PubDate
	}
	return r.ScopedContentID(PreFetchContentIDFor(r.Size, r.Poster, ts))
}

// EffectiveContentID returns r's message-ID content identity once known,
// falling back to PreFetchContentID before fetching.
func (r Release) EffectiveContentID() string {
	if r.ContentID != "" {
		return r.ScopedContentID(r.ContentID)
	}
	return r.PreFetchContentID()
}

// ProbeContentKeys returns the durable content-identity keys under which
// probed container metadata for r may be stored or looked up, ordered from
// strongest (exact content identity) to weakest (pre-fetch identity). Multi-file
// and per-episode releases are scoped via ScopedContentID; untargeted season
// packs return nil so they never read or pollute a shared bare key.
func (r Release) ProbeContentKeys() []string {
	if r.Info.FullSeason && !r.HasFileOrEpisodeScope() {
		return nil
	}
	var out []string
	add := func(k string) {
		if k == "" {
			return
		}
		for _, existing := range out {
			if existing == k {
				return
			}
		}
		out = append(out, k)
	}
	if r.ContentID != "" {
		add(r.ScopedContentID(r.ContentID))
	}
	if hash := strings.ToLower(strings.TrimSpace(r.InfoHash)); hash != "" {
		add(r.ScopedContentID("bt:" + hash))
	}
	if pre := r.PreFetchContentID(); pre != "" {
		add(pre)
	}
	if len(out) == 0 && r.ID != "" {
		add(r.ScopedContentID("rel:" + r.ID))
	}
	return out
}

// Resolution is the resolver's answer for a title and client: the winning
// release, already health-checked, plus fallbacks for mid-stream failover.
type Resolution struct {
	AutoSelectionReason AutoSelectionReason `json:"autoSelectionReason,omitempty"`
	// VersionSelector is the configured slot that this concrete release fulfilled.
	VersionSelector string `json:"versionSelector,omitempty"`
	// HealthVerified means the complete volume sample passed, rather than an
	// optimistic start or an inconclusive health check.
	HealthVerified  bool      `json:"healthVerified,omitempty"`
	QualityFallback string    `json:"qualityFallback,omitempty"`
	Title           RatingKey `json:"title"`
	Release         Release   `json:"release"`
	NZB             []byte    `json:"-"`
	Alternates      []Release `json:"alternates"` // next candidates, best first
	FailedReleases  []string  `json:"-"`          // concrete release IDs rejected during this resolve
	// PreferenceFailed is the owner's preferred release (a pin with fallback)
	// that this resolve tried and could not use before ranking took over.
	PreferenceFailed string    `json:"-"`
	ResolvedAt       time.Time `json:"resolvedAt"`
}

// MatchesVersion compares concrete picks and resolver-validated logical slots.
func (r *Resolution) MatchesVersion(id string) bool {
	return r != nil && (r.Release.ID == id || id != "" && r.VersionSelector == id)
}

// Media scopes a client quality rule to the films or the episodes it
// applies to; an empty scope applies to every title. Quality profiles have
// no media: a library chooses its profile.
const (
	MediaAny   = ""
	MediaMovie = "movie"
	MediaTV    = "tv"
)

// FindProfile returns the profile named name.
func FindProfile(profiles []QualityProfile, name string) (QualityProfile, bool) {
	if name == "" {
		return QualityProfile{}, false
	}
	for _, p := range profiles {
		if p.Name == name {
			return p, true
		}
	}
	return QualityProfile{}, false
}

// MediaApplies reports whether a rule's media scope applies when choosing
// for media. An empty media means the caller has no title context and
// accepts any scope.
func MediaApplies(scope, media string) bool {
	if media == MediaAny {
		return true
	}
	return scope == MediaAny || scope == media
}

// ProfileImport records where a synced quality profile came from. Manual
// profiles leave it nil; a profile with Import set is updated in place by
// the guide sync instead of being duplicated.
type ProfileImport struct {
	Source   string   `json:"source"`             // "trash" or "dictionarry"
	App      string   `json:"app"`                // "radarr" or "sonarr"
	Profile  string   `json:"profile"`            // source profile id/slug
	TrashID  string   `json:"trashId,omitempty"`  // source trash_id
	ScoreSet string   `json:"scoreSet,omitempty"` // TRaSH score set
	Groups   []string `json:"groups,omitempty"`   // selected cf-group trash IDs
	// Database is the custom Profilarr database (config ProfileDatabase.ID)
	// a "dictionarry" import came from; "" is the official Dictionarry.
	Database string `json:"database,omitempty"`
	// NoSync keeps the provenance but stops QualitySync updating the profile.
	NoSync   bool      `json:"noSync,omitempty"`
	SyncedAt time.Time `json:"syncedAt,omitempty"`
}

// A quality profile's Language values beside a specific language.
const (
	ProfileLanguageOriginal = "original" // each title's original language
	ProfileLanguageAny      = "any"      // every language
)

// A quality profile's AudioLanguageMode controls how a specific Language
// applies. Existing profiles without this field keep AudioLanguageModeRequired
// (strict filtering); new profiles default to AudioLanguageModePreferred
// (ranking first, falling back to other releases, original language first).
const (
	AudioLanguageModeRequired  = "required"
	AudioLanguageModePreferred = "preferred"
)

// QualityProfile is an owner-editable rule set ranking releases for the
// libraries that choose it. One profile serves films and episodes alike;
// the Episode* fields only apply to episodes.
type QualityProfile struct {
	ReleaseRules []ReleaseRule `json:"releaseRules,omitempty"`
	// PluginScores lists the plugin ranking steps the profile uses, each
	// with its settings ({} for none). A step adds the number its plugin
	// returns for a release to the release's custom format score.
	PluginScores     map[string]json.RawMessage `json:"pluginScores,omitempty"`
	AllowedQualities []string                   `json:"allowedQualities,omitempty"` // preferred imported source-resolution pairs
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
	// MinCustomFormatScore prefers releases whose custom formats (the
	// owner's and imported ones, not Ether's built-ins, rules or plugins)
	// reach this score. Lower scores remain playable. nil sets no preference.
	MinCustomFormatScore *int           `json:"minCustomFormatScore,omitempty"`
	Name                 string         `json:"name"`
	Import               *ProfileImport `json:"import,omitempty"` // set on imported, syncable profiles
	MaxHeight            int            `json:"maxHeight"`        // cap, e.g. 1080
	// ConfiguredMaxHeight is MaxHeight as the owner configured it,
	// before download reshaping or a client's caps clamped the
	// effective MaxHeight. -1 means the owner set no ceiling ("Any");
	// 0 means unset (a profile built outside SelectProfile), in which
	// case MaxHeight itself is the configured value. Runtime-only,
	// never persisted with the profile.
	ConfiguredMaxHeight int `json:"-"`
	// CeilingSource names the runtime declaration, registry default, owner
	// override, quality rule, or download reshape that lowered MaxHeight
	// below ConfiguredMaxHeight. Runtime-only, never persisted.
	CeilingSource          string   `json:"-"`
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
	// language, and any other value names a specific language (an ISO code or
	// a language name) whose strictness is governed by AudioLanguageMode.
	Language string `json:"language,omitempty"`
	// AudioLanguageMode controls how a specific Language applies: "" or
	// AudioLanguageModeRequired strictly filters out non-matching releases
	// (existing profiles keep this behaviour); AudioLanguageModePreferred
	// ranks matching releases first and keeps other releases playable as
	// fallbacks.
	AudioLanguageMode string `json:"audioLanguageMode,omitempty"`
	AllowCam          bool   `json:"allowCam"` // the only pre-release switch; CAM/TS/TC and screener/R5/workprint copies play only for profiles that allow them
	// SelectionKey is the canonical hash of this profile's configured
	// definition, computed by resolver.SelectProfile from the pristine
	// configured profile before download reshaping or client-cap
	// clamping. Release rules are part of that definition. The catalogue
	// original language is not: it is per-title evaluation context, not a
	// profile fact. Runtime-only, never persisted with the profile.
	SelectionKey string `json:"-"`
}

// EffectiveAudioLanguageMode returns the resolved mode for a specific profile
// Language: AudioLanguageModePreferred only when explicitly configured, and
// AudioLanguageModeRequired otherwise so existing profiles keep strict
// filtering.
func (p QualityProfile) EffectiveAudioLanguageMode() string {
	if strings.EqualFold(strings.TrimSpace(p.AudioLanguageMode), AudioLanguageModePreferred) {
		return AudioLanguageModePreferred
	}
	return AudioLanguageModeRequired
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
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	// Ranking semantics are part of policy identity: discard remembered hard-floor verdicts.
	sum := sha256.Sum256(append([]byte("soft-quality-v1:"), b...))
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

// SameReleaseContent reports whether a and b represent the same underlying
// media release (same ID, content/source/info/stream identity, GUID on the
// same indexer, or normalized release name). Content-level rejections such as
// a probed audio language mismatch use this to avoid retrying reposts or
// vault/indexer copies of the same release within one start without preventing
// broken-upload repost failover.
func SameReleaseContent(a, b Release) bool {
	if a.ID != "" && a.ID == b.ID {
		return true
	}
	if ac, bc := a.EffectiveContentID(), b.EffectiveContentID(); ac != "" && ac == bc {
		return true
	}
	if a.InfoHash != "" && strings.EqualFold(a.InfoHash, b.InfoHash) {
		return true
	}
	if a.SourceHash != "" && a.SourceHash == b.SourceHash {
		return true
	}
	if a.StreamURL != "" && a.StreamURL == b.StreamURL {
		return true
	}
	if a.GUID != "" && a.GUID == b.GUID && a.Indexer != "" && a.Indexer == b.Indexer {
		return true
	}
	an, bn := normalizedReleaseIdentity(a.Name), normalizedReleaseIdentity(b.Name)
	return an != "" && an == bn
}

// ReleaseContentKey returns a canonical string summarizing a release's
// content-equivalence keys for deduplication and flight keying.
func (r Release) ReleaseContentKey() string {
	return strings.Join([]string{
		r.ID,
		r.EffectiveContentID(),
		strings.ToLower(r.InfoHash),
		r.SourceHash,
		r.StreamURL,
		normalizedReleaseIdentity(r.Name),
	}, "\x00")
}

func normalizedReleaseIdentity(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return ""
	}
	if dot := strings.LastIndexByte(s, '.'); dot > 0 {
		switch strings.ToLower(s[dot+1:]) {
		case "mkv", "mp4", "m4v", "avi", "ts", "m2ts", "mov", "webm", "nzb":
			s = s[:dot]
		}
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
