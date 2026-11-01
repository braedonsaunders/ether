package core

import (
	"context"
	"io"
	"time"
)

// ---------------------------------------------------------------------------
// Catalogue (internal/catalog)

type SortOrder string

const (
	SortPopularity SortOrder = "popularity"
	SortTitle      SortOrder = "title"
	SortReleased   SortOrder = "released"
	SortRating     SortOrder = "rating"
	SortAdded      SortOrder = "added"   // movie/episode release or air date; series and seasons use the latest aired episode, then the premiere
	SortUpdated    SortOrder = "updated" // actual catalogue metadata update time
)

// StringFilter narrows a string column with a Plex string operator
// ("=", "!=", "==", "!==", "<=", ">=").
type StringFilter struct {
	Op    string `json:"op,omitempty"`
	Value string `json:"value,omitempty"`
}

// DateFilter narrows a release timestamp with a comparison operator
// ("=", "!=", "<<", "<=", ">>", ">="). SpanDay is true when the value was
// parsed from a calendar day (YYYY-MM-DD), so "=" and "!=" cover the 24-hour
// UTC day.
type DateFilter struct {
	Op      string `json:"op,omitempty"`
	Unix    int64  `json:"unix,omitempty"`
	SpanDay bool   `json:"spanDay,omitempty"`
}

// CreditFilter narrows by credited person TMDB IDs and optional credit kind
// ("cast", "director", "writer", "crew", or "" for any).
type CreditFilter struct {
	People  []int64 `json:"people,omitempty"`
	Kind    string  `json:"kind,omitempty"`
	Exclude bool    `json:"exclude,omitempty"`
}

// Filter narrows a browse or a smart collection. Zero values mean "any".
type Filter struct {
	KnownAddedDate bool `json:"-"` // only released/aired titles, before pagination
	// ExcludeKeys applies local visibility policy before counting and pagination.
	ExcludeKeys []RatingKey `json:"-"`
	// IncludeKeys, when set, restricts the page to these keys before
	// counting and pagination. An empty list matches nothing.
	IncludeKeys []RatingKey `json:"-"`
	// ExcludeGenres and ExcludeKeywords remove viewer-blocked facts before
	// counting and pagination. Matching is case-insensitive.
	ExcludeGenres         []string        `json:"-"`
	ExcludeKeywords       []string        `json:"-"`
	Type                  MediaType       `json:"type,omitempty"` // movie or show
	Genres                []string        `json:"genres,omitempty"`
	Keywords              []string        `json:"keywords,omitempty"` // every keyword must match (case-insensitive)
	YearFrom              int             `json:"yearFrom,omitempty"`
	YearTo                int             `json:"yearTo,omitempty"`
	Years                 []int           `json:"years,omitempty"`
	ExcludeYears          []int           `json:"excludeYears,omitempty"`
	MinRating             float64         `json:"minRating,omitempty"`
	MaxRating             float64         `json:"maxRating,omitempty"`
	ExcludeRatings        []float64       `json:"excludeRatings,omitempty"`
	MaxRuntime            time.Duration   `json:"maxRuntime,omitempty"`
	MinRuntime            time.Duration   `json:"minRuntime,omitempty"`
	ExcludeRuntimes       []time.Duration `json:"excludeRuntimes,omitempty"`
	Languages             []string        `json:"languages,omitempty"`
	Countries             []string        `json:"countries,omitempty"`
	ExcludeCountries      []string        `json:"excludeCountries,omitempty"`
	ContentRatings        []string        `json:"contentRatings,omitempty"`
	ExcludeContentRatings []string        `json:"excludeContentRatings,omitempty"`
	MaxContentRating      string          `json:"maxContentRating,omitempty"` // per-user limit
	Collection            int64           `json:"collection,omitempty"`
	Collections           []int64         `json:"collections,omitempty"`
	ExcludeCollections    []int64         `json:"excludeCollections,omitempty"`
	Person                int64           `json:"person,omitempty"` // cast or crew TMDB id
	Credits               []CreditFilter  `json:"credits,omitempty"`
	Studio                string          `json:"studio,omitempty"`
	StudioFilters         []StringFilter  `json:"studioFilters,omitempty"`
	FirstCharacter        string          `json:"firstCharacter,omitempty"`
	Query                 string          `json:"query,omitempty"` // title substring
	TitleFilters          []StringFilter  `json:"titleFilters,omitempty"`
	ReleasedFilters       []DateFilter    `json:"releasedFilters,omitempty"`
}

type BrowseQuery struct {
	Filter Filter
	Sort   SortOrder
	Desc   bool
	Offset int
	Limit  int
	// NoTotal skips counting the filter's matches, which reads every title
	// the filter admits. Page.Total is then offset plus the items returned.
	NoTotal bool
}

type Page struct {
	Items []Title
	Total int
}

// Catalog is Ether's library: a local mirror of a TMDB slice plus on-demand
// fetches for anything else.
type Catalog interface {
	Get(ctx context.Context, key RatingKey) (*Title, error)
	// Children: seasons of a show, episodes of a season.
	Children(ctx context.Context, key RatingKey) ([]Title, error)
	// Grandchildren: all episodes of a show.
	Grandchildren(ctx context.Context, key RatingKey) ([]Title, error)
	Browse(ctx context.Context, q BrowseQuery) (Page, error)
	// Search reaches all of TMDB; matched titles join the browsable set.
	Search(ctx context.Context, query string, types []MediaType, limit int) ([]Title, error)
	// Lists from TMDB: "trending_day", "trending_week", "top_rated", "now_playing", "popular", "airing_today", "upcoming".
	List(ctx context.Context, list string, t MediaType, limit int) ([]Title, error)
	Similar(ctx context.Context, key RatingKey, limit int) ([]Title, error)
	Collection(ctx context.Context, tmdbCollection int64) (name string, items []Title, err error)
	// Facets for filter UIs: "genre", "year", "decade", "contentRating", "studio", "country".
	Facet(ctx context.Context, t MediaType, facet string) ([]FacetValue, error)
	// NextEpisode returns the episode after the given one, or ErrNotFound.
	NextEpisode(ctx context.Context, episode RatingKey) (*Title, error)
	// ByExternalID looks up a title by imdb ("tt123") or tvdb id.
	ByExternalID(ctx context.Context, source, id string) (*Title, error)
}

// CatalogSchedule exposes metadata-only placeholders when the owner opts in.
// Only dated, unaired episodes of an already airing regular season are returned.
// Callers must authorize the parent season and each episode for the viewer,
// render no media/playback links, and use the same scope for artwork. Ordinary
// Catalog methods remain released-only; this interface never authorizes playback.
type CatalogSchedule interface {
	ScheduledEpisodes(context.Context, RatingKey) ([]Title, error)
}

// StoredTitleLookup reads local metadata without remote discovery. Callers
// must apply their own authorization before exposing the returned title.
type StoredTitleLookup interface {
	StoredTitle(context.Context, RatingKey) (*Title, error)
}

type FacetValue struct {
	ID    string `json:"id,omitempty"` // Stable namespace-aware neutral identifier.
	Key   string `json:"key"`
	Title string `json:"title"`
	Count int    `json:"count"`
	// Labels are the equivalent values Key stands for, where a facet groups
	// them (content-rating levels list every board's label).
	Labels []string `json:"labels,omitempty"`
}

// ImageURL returns a full TMDB image URL for a path and size ("w500", "original").
type ImageURLFunc func(path, size string) string

// ---------------------------------------------------------------------------
// Collections and hubs (internal/collections, internal/recommend)

// Hub is a Home row or a section row.
type Hub struct {
	ID       string    `json:"id"` // stable identifier, e.g. "trending.week.movie"
	Title    string    `json:"title"`
	Type     MediaType `json:"type"`            // item type in the row ("mixed" allowed via "")
	Context  string    `json:"context"`         // Plex hub context, e.g. "hub.movie.recentlyreleased"
	Style    string    `json:"style,omitempty"` // "shelf", "hero", "clip"
	Promoted bool      `json:"promoted"`        // show on Home
	Items    []Title   `json:"items"`
	More     bool      `json:"more"`
	Key      RatingKey `json:"key,omitempty"` // backing collection, if any
	// ItemCount carries rendered availability when a front end retains the
	// actual metadata and asks only for row placement.
	ItemCount int `json:"itemCount,omitempty"`
}

type Collection struct {
	Key          RatingKey `json:"key"`
	Title        string    `json:"title"`
	Summary      string    `json:"summary,omitempty"`
	Kind         string    `json:"kind"` // "franchise", "trending", "smart", "user"
	Type         MediaType `json:"type"`
	Filter       *Filter   `json:"filter,omitempty"` // smart
	Sort         SortOrder `json:"sort,omitempty"`
	OwnerUserID  int64     `json:"ownerUserId,omitempty"` // 0 = server-wide
	PosterPath   string    `json:"posterPath,omitempty"`
	BackdropPath string    `json:"backdropPath,omitempty"`
	ChildCount   int       `json:"childCount"`
	Upcoming     bool      `json:"upcoming,omitempty"` // admits scoped future placeholders
	// UpdatedAt changes when the collection's members or artwork change, so
	// Plex clients refetch artwork (additive).
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

type Collections interface {
	List(ctx context.Context, user *User, t MediaType) ([]Collection, error)
	Get(ctx context.Context, user *User, key RatingKey) (*Collection, error)
	Items(ctx context.Context, user *User, key RatingKey, offset, limit int) (Page, error)
	CreateSmart(ctx context.Context, c Collection) (*Collection, error)
	DeleteSmart(ctx context.Context, key RatingKey) error
}

// CollectionPlaceholder resolves a future title only through a visible,
// enabled collection in which it is currently a member for this viewer.
type CollectionPlaceholder interface {
	AllowsPlaceholders(context.Context, *User, RatingKey, string) bool
	Placeholder(context.Context, *User, RatingKey, RatingKey, string) (*Title, error)
	FindPlaceholder(context.Context, *User, RatingKey, string) (*Title, RatingKey, error)
}

// Recommender produces per-user Home and section rows.
type Recommender interface {
	// HomeHubs are the rows on the Home screen across both sections.
	HomeHubs(ctx context.Context, user *User, limit int) ([]Hub, error)
	// SectionHubs are the rows at the top of a library section.
	SectionHubs(ctx context.Context, user *User, t MediaType, limit int) ([]Hub, error)
	// Related are rows on a detail page ("More like this", franchise).
	Related(ctx context.Context, user *User, key RatingKey, limit int) ([]Hub, error)
}

// ---------------------------------------------------------------------------
// Watch state (internal/state)

type WatchStatus struct {
	Key             RatingKey     `json:"key"`
	ViewOffset      time.Duration `json:"viewOffset"`
	ViewCount       int           `json:"viewCount"`
	LastViewedAt    time.Time     `json:"lastViewedAt,omitempty"`
	UserRating      float64       `json:"userRating,omitempty"` // 0-10
	InWatchlist     bool          `json:"inWatchlist,omitempty"`
	IsFavorite      bool          `json:"isFavorite,omitempty"`      // Derived from the viewer’s favorites set.
	IsNotInterested bool          `json:"isNotInterested,omitempty"` // Derived from the viewer’s recommendation exclusions.
	Duration        time.Duration `json:"duration,omitempty"`
	UpdatedAt       time.Time     `json:"updatedAt"`
}

type HistoryEntry struct {
	UserID    int64     `json:"userId"`
	Key       RatingKey `json:"key"`
	ViewedAt  time.Time `json:"viewedAt"`
	Completed bool      `json:"completed"`
}

type State interface {
	Get(ctx context.Context, userID int64, key RatingKey) (WatchStatus, error)
	GetMany(ctx context.Context, userID int64, keys []RatingKey) (map[RatingKey]WatchStatus, error)
	// Progress records a timeline update. Crossing 90% marks it watched.
	Progress(ctx context.Context, userID int64, key RatingKey, offset, duration time.Duration, state string) error
	Scrobble(ctx context.Context, userID int64, key RatingKey) error   // mark watched
	Unscrobble(ctx context.Context, userID int64, key RatingKey) error // mark unwatched
	Rate(ctx context.Context, userID int64, key RatingKey, rating float64) error
	// OnDeck: in-progress items and next episodes, most recent first.
	InProgress(ctx context.Context, userID int64, limit int) ([]WatchStatus, error)
	History(ctx context.Context, userID int64, since time.Time, limit int) ([]HistoryEntry, error)
	// InProgressAny lists keys in progress for any user (cache protection, prefetch).
	InProgressAny(ctx context.Context) ([]RatingKey, error)
}

// Playlists are user-created lists.
type Playlist struct {
	Key       RatingKey   `json:"key"`
	UserID    int64       `json:"userId"`
	Title     string      `json:"title"`
	Smart     bool        `json:"smart"`
	Items     []RatingKey `json:"items"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

type Playlists interface {
	List(ctx context.Context, userID int64) ([]Playlist, error)
	Get(ctx context.Context, userID int64, key RatingKey) (*Playlist, error)
	Create(ctx context.Context, userID int64, title string, items []RatingKey) (*Playlist, error)
	Add(ctx context.Context, userID int64, key RatingKey, items []RatingKey) error
	Remove(ctx context.Context, userID int64, key RatingKey, itemIndex int) error
	Move(ctx context.Context, userID int64, key RatingKey, from, to int) error
	Delete(ctx context.Context, userID int64, key RatingKey) error
}

// ---------------------------------------------------------------------------
// Users and auth (internal/plextv, internal/auth)

type Auth interface {
	// Authenticate maps an X-Plex-Token to a user with access to this server.
	Authenticate(ctx context.Context, token string) (*User, error)
	Users(ctx context.Context) ([]User, error)
	UpdateUser(ctx context.Context, u User) error
	Owner(ctx context.Context) (*User, error)
}

// ---------------------------------------------------------------------------
// Indexers and resolver (internal/newznab, internal/resolver)

type SearchRequest struct {
	Type          MediaType // movie or episode
	IDs           IDs       // movie ids, or the show's ids for episodes
	Title         string
	OriginalTitle string // optional original-language or canonical title (additive)
	Year          int
	Season        int
	Episode       int
	EpisodeTitle  string // optional title used to distinguish same-numbered episodes
	// AbsoluteEpisode is the episode's number counted from the series
	// start (0 = unknown), and Anime marks a series whose releases are
	// commonly numbered that way (additive).
	AbsoluteEpisode int
	Anime           bool
	// AlternativeTitles are other names of the movie or series (TMDB
	// alternative titles) a release name may use; matching only, never
	// part of a query or cache key (additive).
	AlternativeTitles []string `json:"-"`
}

type Indexer interface {
	Name() string
	Search(ctx context.Context, req SearchRequest) ([]Release, error)
	FetchNZB(ctx context.Context, r Release) ([]byte, error)
}

type ResolveRequest struct {
	Title  *Title
	Client ClientCaps
	User   *User
	// PlayedRelease is the release this viewer last played of the title
	// (the one a resume opens); Variants names Auto after it (additive).
	PlayedRelease string
	// Exclude releases already tried (failover).
	Exclude []string
	// Prefer is a release ID to try first (the viewer picked a version in
	// Plex's "Play Version" picker). It is health-checked like any other
	// candidate; when it is unavailable, normal ranking applies (additive).
	Prefer string
	// ExactHEVCTranscode permits the explicitly preferred HEVC source to reach
	// playback admission for an H.264 encode. The broker must still validate
	// the probed source and the client's complete output profile.
	ExactHEVCTranscode bool
}

type Resolver interface {
	// Resolve finds a healthy release. Returns ErrNotAvailable when nothing
	// acceptable exists.
	Resolve(ctx context.Context, req ResolveRequest) (*Resolution, error)
	// MarkBad records a release as broken so nobody gets it again.
	MarkBad(ctx context.Context, releaseID string, reason string) error
	// Available reports a cached availability answer without searching:
	// (known, available).
	Available(ctx context.Context, key RatingKey) (known bool, available bool)
}

// HealthChecker checks a sample of an NZB's articles exist (NNTP STAT).
// It also inspects archive headers: compressed or encrypted RARs and
// releases without a playable video file report healthy=false with a reason.
// Implemented by internal/stream.
type HealthChecker interface {
	Check(ctx context.Context, nzb []byte) (healthy bool, err error)
}

// ReleaseHealthChecker is a HealthChecker that is also told which release
// the NZB belongs to, so what the check reads (the PAR2 index, archive
// headers) lands in the segment cache under the ID a following Open uses.
// Optional: callers fall back to Check when it is not implemented
// (additive).
type ReleaseHealthChecker interface {
	HealthChecker
	CheckRelease(ctx context.Context, releaseID string, nzb []byte) (healthy bool, err error)
}

// CheckReleaseHealth runs hc.CheckRelease when hc implements it, else
// hc.Check.
func CheckReleaseHealth(ctx context.Context, hc HealthChecker, releaseID string, nzb []byte) (bool, error) {
	if rc, ok := hc.(ReleaseHealthChecker); ok {
		return rc.CheckRelease(ctx, releaseID, nzb)
	}
	return hc.Check(ctx, nzb)
}

// ---------------------------------------------------------------------------
// Usenet (internal/usenet)

type Priority int

const (
	PriorityLive      Priority = 0 // a viewer is waiting
	PriorityReadahead Priority = 1
	PriorityPrefetch  Priority = 2 // background; yields to live streams
)

// ArticleFetcher fetches and yEnc-decodes articles across the configured
// providers, primary first, backfilling from block accounts.
type ArticleFetcher interface {
	// Body returns the decoded payload of one article (one NZB segment).
	// Returns ErrArticleMissing when no provider has it.
	Body(ctx context.Context, messageID string, groups []string, prio Priority) ([]byte, error)
	// BodyPart is Body plus the article's yEnc =ybegin/=ypart header, which
	// gives the segment's exact byte range inside the posted file.
	BodyPart(ctx context.Context, messageID string, groups []string, prio Priority) ([]byte, ArticlePart, error)
	// Stat reports whether any provider has the article.
	Stat(ctx context.Context, messageID string) (bool, error)
}

// BurstArticleFetcher optionally borrows idle live-reserved capacity for a
// short, explicitly bounded bulk-read burst. Live waiters retain precedence.
// The deadline limits new burst admission, not already transmitted bytes.
type BurstArticleFetcher interface {
	BodyPartBurst(ctx context.Context, messageID string, groups []string, prio Priority, until time.Time) ([]byte, ArticlePart, error)
}

// ArticlePart is the yEnc header of one article. Begin and End are 1-based
// inclusive offsets in the posted file, as in =ypart; for single-part posts
// Begin is 1 and End is FileSize.
type ArticlePart struct {
	Name     string `json:"name"`
	FileSize int64  `json:"fileSize"`
	Part     int    `json:"part"`
	Total    int    `json:"total"`
	Begin    int64  `json:"begin"`
	End      int64  `json:"end"`
}

// ---------------------------------------------------------------------------
// Stream engine (internal/stream)

// MediaSource is a seekable byte stream of one playable file (the video file
// inside a release, after NZB/RAR/7z unwrapping).
type MediaSource interface {
	io.ReaderAt
	Size() int64
	Name() string // file name inside the release, e.g. "Movie.2024.1080p.mkv"
	ReleaseID() string
	Close() error
}

// OriginSource is implemented by a MediaSource that can name the service its
// bytes come from, such as "Real-Debrid" (additive). Sources without it are
// Usenet.
type OriginSource interface {
	Origin() string
}

type Engine interface {
	// Open builds the byte-offset index for the main video file of an NZB.
	Open(ctx context.Context, releaseID string, nzb []byte) (MediaSource, error)
	// Prefetch warms [offset, offset+length) at background priority.
	Prefetch(ctx context.Context, releaseID string, nzb []byte, offset, length int64) error
}

// SegmentCache stores decoded segments on disk under an LRU budget.
type SegmentCache interface {
	Get(releaseID string, segment int) ([]byte, bool)
	Put(releaseID string, segment int, data []byte) error
	// Pin protects every segment of a release from eviction.
	Pin(releaseID string, reason string) error
	Unpin(releaseID string, reason string) error
	Usage() (used, budget int64)
}

// ---------------------------------------------------------------------------
// Transcoder (internal/transcode)

type StreamInfo struct {
	// Probe hints for enhancement backends that require progressive, steady-rate input.
	Interlaced        bool `json:"interlaced,omitempty"`
	VariableFrameRate bool `json:"variableFrameRate,omitempty"`

	Index         int    `json:"index"`
	Type          string `json:"type"` // video, audio, subtitle
	Codec         string `json:"codec"`
	Profile       string `json:"profile,omitempty"`
	Level         int    `json:"level,omitempty"`
	Language      string `json:"language,omitempty"`
	Title         string `json:"title,omitempty"`
	CodecLongName string `json:"codecLongName,omitempty"`
	Default       bool   `json:"default,omitempty"`
	Forced        bool   `json:"forced,omitempty"`
	// Nil means the source did not report this ffprobe disposition.
	DefaultDisposition *bool   `json:"defaultDisposition,omitempty"`
	ForcedDisposition  *bool   `json:"forcedDisposition,omitempty"`
	HearingImpaired    *bool   `json:"hearingImpaired,omitempty"`
	Original           *bool   `json:"original,omitempty"`
	VisualImpaired     *bool   `json:"visualImpaired,omitempty"`
	Commentary         *bool   `json:"commentary,omitempty"`
	Dub                *bool   `json:"dub,omitempty"`
	Width              int     `json:"width,omitempty"`
	Height             int     `json:"height,omitempty"`
	FrameRate          float64 `json:"frameRate,omitempty"`
	// Optional probe facts are nil/empty when the source did not report them.
	AverageFrameRate   *float64 `json:"averageFrameRate,omitempty"`
	ReferenceFrameRate *float64 `json:"referenceFrameRate,omitempty"`
	DisplayAspectRatio string   `json:"displayAspectRatio,omitempty"`
	PixelFormat        string   `json:"pixelFormat,omitempty"`
	CodecTag           string   `json:"codecTag,omitempty"`
	TimeBase           string   `json:"timeBase,omitempty"`
	NALLengthSize      *int     `json:"nalLengthSize,omitempty"`
	ReferenceFrames    *int     `json:"referenceFrames,omitempty"`
	BitsPerRawSample   *int     `json:"bitsPerRawSample,omitempty"`
	ProbedLevel        *int     `json:"probedLevel,omitempty"`
	FieldOrder         string   `json:"fieldOrder,omitempty"`
	ProbedInterlaced   *bool    `json:"probedInterlaced,omitempty"`
	BitDepth           int      `json:"bitDepth,omitempty"`
	ColorTransfer      string   `json:"colorTransfer,omitempty"` // smpte2084 = HDR10, arib-std-b67 = HLG
	// Signalled picture format, in ffprobe's names; empty when the stream
	// and container leave it unspecified (decoders then assume square
	// pixels and limited range).
	SampleAspectRatio string `json:"sampleAspectRatio,omitempty"` // "N:D"
	ColorRange        string `json:"colorRange,omitempty"`        // tv, pc
	ColorPrimaries    string `json:"colorPrimaries,omitempty"`    // bt709, bt2020, smpte170m...
	ColorSpace        string `json:"colorSpace,omitempty"`        // matrix: bt709, bt2020nc, smpte170m...
	DolbyVision       bool   `json:"dolbyVision,omitempty"`
	// HDR10Plus is nil unless the probe provides affirmative or explicit negative evidence.
	HDR10Plus     *bool  `json:"hdr10Plus,omitempty"`
	Channels      int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channelLayout,omitempty"`
	SampleRate    int    `json:"sampleRate,omitempty"`
	BitrateKbps   int    `json:"bitrateKbps,omitempty"`
	External      bool   `json:"external,omitempty"` // sidecar subtitle (OpenSubtitles)
	ExternalURL   string `json:"externalUrl,omitempty"`

	// DolbyVisionProfile is the DV profile (5, 7, 8...) from the DOVI
	// configuration record; 0 when unknown or not Dolby Vision.
	DolbyVisionProfile         int    `json:"dolbyVisionProfile,omitempty"`
	DolbyVisionLevel           int    `json:"dolbyVisionLevel,omitempty"`
	DolbyVisionCompatibilityID int    `json:"dolbyVisionCompatibilityId,omitempty"`
	CodecString                string `json:"codecString,omitempty"` // RFC 6381, derived from the codec configuration

	// Configuration presence is known from a completed MP4/Matroska container
	// probe; nil means unprobed, unsupported or ambiguous. Codec tags alone do
	// not establish whether enhancement/base layers or RPU are present.
	DolbyVisionConfigurationPresent     *bool  `json:"dolbyVisionConfigurationPresent,omitempty"`
	DolbyVisionBLCompatibilityIDPresent *bool  `json:"dolbyVisionBLCompatibilityIdPresent,omitempty"`
	ProbedDolbyVisionProfile            *int   `json:"probedDolbyVisionProfile,omitempty"`
	ProbedDolbyVisionLevel              *int   `json:"probedDolbyVisionLevel,omitempty"`
	DolbyVisionVersion                  string `json:"dolbyVisionVersion,omitempty"`
	DolbyVisionBLPresent                *bool  `json:"dolbyVisionBLPresent,omitempty"`
	DolbyVisionELPresent                *bool  `json:"dolbyVisionELPresent,omitempty"`
	DolbyVisionRPUPresent               *bool  `json:"dolbyVisionRPUPresent,omitempty"`
}

type Chapter struct {
	ID    int64         `json:"id"`
	Title string        `json:"title,omitempty"`
	Start time.Duration `json:"start"`
	End   time.Duration `json:"end"`
}

// ProbeFactsVersion is the generation of facts a probe is expected to carry,
// including H.264/HEVC reference frames and descriptive audio handler titles.
// Persisted probes from an older generation are cache misses.
const ProbeFactsVersion = 2

type MediaInfo struct {
	Chapters    []Chapter     `json:"chapters,omitempty"`
	Container   string        `json:"container"` // mkv, mp4, avi, mpegts
	Duration    time.Duration `json:"duration"`
	Size        int64         `json:"size"`
	BitrateKbps int           `json:"bitrateKbps"`
	Streams     []StreamInfo  `json:"streams"`
	Estimated   bool          `json:"estimated"` // derived from release name, not probed

	// Keyframes is the container's keyframe/cue index, sorted, when it was
	// read on open or probe; empty when unknown. Not serialized: it can hold
	// thousands of points (additive).
	Keyframes []KeyframePoint `json:"-"`
}

func (m MediaInfo) Video() *StreamInfo {
	for i := range m.Streams {
		if m.Streams[i].Type == "video" {
			return &m.Streams[i]
		}
	}
	return nil
}

type DecisionKind string

const (
	DecisionDirectPlay DecisionKind = "directplay"
	DecisionRemux      DecisionKind = "copy" // Plex calls this "direct stream"
	DecisionTranscode  DecisionKind = "transcode"
)

type Decision struct {
	Enhancement VideoEnhancement `json:"enhancement,omitempty"`

	Kind       DecisionKind `json:"kind"`
	VideoCopy  bool         `json:"videoCopy"`
	AudioCopy  bool         `json:"audioCopy"`
	VideoCodec string       `json:"videoCodec"` // target codec when transcoding
	AudioCodec string       `json:"audioCodec"`
	Container  string       `json:"container"` // "mpegts" (HLS) or original
	MaxHeight  int          `json:"maxHeight"`
	// H264Level is a verified output level_idc for a constrained H.264 encode.
	// Zero retains the encoder's existing automatic selection.
	H264Level int `json:"h264Level,omitempty"`
	// H264FrameRate bounds the selected encoder output rate for that level.
	H264FrameRate  float64  `json:"h264FrameRate,omitempty"`
	BitrateKbps    int      `json:"bitrateKbps"`
	AudioStream    int      `json:"audioStream"`
	SubtitleStream int      `json:"subtitleStream"` // -1 none
	BurnSubtitles  bool     `json:"burnSubtitles"`
	Reason         string   `json:"reason"`
	AudioFilters   []string `json:"audioFilters,omitempty"`
	// AudioChannels and AudioSampleRate keep the already negotiated audio
	// decoder configuration across a source change. Zero uses the normal
	// encoder defaults.
	AudioChannels   int `json:"audioChannels,omitempty"`
	AudioSampleRate int `json:"audioSampleRate,omitempty"`

	// ToneMap converts HDR (HDR10, HLG, Dolby Vision) video to SDR while
	// transcoding, for clients without HDR support.
	ToneMap bool `json:"toneMap,omitempty"`

	// StripDolbyVision copies the compatible HDR10 base layer while
	// removing Dolby Vision RPU/configuration data; it never re-encodes.
	StripDolbyVision bool `json:"stripDolbyVision,omitempty"`
}

type HWAccel struct {
	Name     string   `json:"name"` // "videotoolbox", "nvenc", "qsv", "vaapi", "none"
	Encoders []string `json:"encoders"`
	Device   string   `json:"device,omitempty"`
	// NoGPUScale: the encoder works but the device's video processing
	// (VAAPI scale_vaapi) does not, so frames are decoded and scaled in
	// software and uploaded to the encoder.
	NoGPUScale bool `json:"noGpuScale,omitempty"`
	// NoGPUTonemap: the device's VAAPI VPP cannot run tonemap_vaapi, so HDR
	// tone-mapping falls back to the software zscale/tonemap path.
	NoGPUTonemap bool `json:"noGpuTonemap,omitempty"`
	// VAAPIQualityRC / VAAPIBitrateRC record the probed VAAPI rate-control
	// modes ("QVBR", "VBR", "CBR", "CQP") per encoder ("h264_vaapi",
	// "hevc_vaapi"); nil/empty falls back to the default QVBR/VBR modes.
	VAAPIQualityRC map[string]string `json:"vaapiQualityRc,omitempty"`
	VAAPIBitrateRC map[string]string `json:"vaapiBitrateRc,omitempty"`
}

// DelegationAuth issues expiring, server-local casting credentials. Implementations
// must enforce current membership on use and must not delegate owner privileges.
type DelegationAuth interface {
	IssueDelegation(context.Context, int64) (string, error)
}

// AuthoritativeTitleRefresher fetches current movie or show details and optionally persists them.
// It never falls back to cached data on failure and does not apply release visibility.
type AuthoritativeTitleRefresher interface {
	RefreshTitle(ctx context.Context, key RatingKey, persist bool) (*Title, error)
}
