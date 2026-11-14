// Package livetv implements owner-defined virtual channels and IPTV playout.
// Ether supplies neither programming nor IPTV accounts.
package livetv

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv/guideprovider"
	"github.com/braedonsaunders/ether/internal/markers"
)

const Provider = "tv.plex.providers.epg.xmltv:1"

type BreakPoint = guideprovider.BreakPoint
type Item = guideprovider.Item

// ProgramSource is one weighted part of a mix. Weight 0 means 1.
// Watched is "", "watched" or "unwatched" for the whole household.
type ProgramSource struct {
	StartDate    string          `json:"startDate,omitempty"`
	ID           string          `json:"id,omitempty"`
	Kind         string          `json:"kind,omitempty"`
	ShowKey      core.RatingKey  `json:"showKey,omitempty"`
	StartSeason  int             `json:"startSeason,omitempty"`
	StartEpisode int             `json:"startEpisode,omitempty"`
	Order        string          `json:"order,omitempty"`
	Strip        bool            `json:"strip,omitempty"`
	SegmentID    string          `json:"segmentId,omitempty"`
	Personal     *PersonalRecipe `json:"personal,omitempty"`
	Sources      []ProgramSource `json:"sources,omitempty"`
	Name         string          `json:"name,omitempty"`
	Weight       int             `json:"weight,omitempty"`
	Items        []Item          `json:"items,omitempty"`
	Collection   core.RatingKey  `json:"collection,omitempty"`
	Filter       *core.Filter    `json:"filter,omitempty"`
	Watched      string          `json:"watched,omitempty"`
	// Lineup and pooled movie sources. Pool caps how many titles the smart
	// filter draws, most popular first. A lineup keeps Shows series on air at
	// once and, with RotateDays, brings the next group of the pool on air
	// every RotateDays broadcast days. Each series continues where it left off.
	Pool       int `json:"pool,omitempty"`
	Shows      int `json:"shows,omitempty"`
	RotateDays int `json:"rotateDays,omitempty"`
	// ExcludeGenres leaves titles with any of these genres out of a lineup
	// or movie pool (a sitcom lineup without cartoons, say).
	ExcludeGenres []string `json:"excludeGenres,omitempty"`
}
type Block struct {
	ID     string       `json:"id,omitempty"`
	Preset string       `json:"preset,omitempty"`
	Breaks *ChannelLook `json:"breaks,omitempty"`
	// Start and End are minutes after midnight in Timezone, or UTC when
	// the channel has no timezone. End may be 1440. Days empty means every
	// day, which is how channels saved before the weekly grid behave.
	// Days are Go weekdays: 0 Sunday … 6 Saturday, in that same zone.
	Start int   `json:"start"`
	End   int   `json:"end"`
	Days  []int `json:"days,omitempty"`
	// Name is a short grid label such as "Cartoons". It does not change
	// what plays. Known preset names are translated in the console.
	Name        string          `json:"name,omitempty"`
	Items       []Item          `json:"items"`
	Shuffle     bool            `json:"shuffle"`
	Mode        string          `json:"mode,omitempty"`  // order, shuffle, random, marathon, newest
	Align       string          `json:"align,omitempty"` // hour, half
	Fit         string          `json:"fit,omitempty"`   // cut, pad
	RepeatDays  int             `json:"repeatDays,omitempty"`
	Filler      []Item          `json:"filler,omitempty"`
	MarathonKey core.RatingKey  `json:"marathonKey,omitempty"`
	Collection  core.RatingKey  `json:"collection,omitempty"`
	Filter      *core.Filter    `json:"filter,omitempty"`
	Sources     []ProgramSource `json:"sources,omitempty"`
	Watched     string          `json:"watched,omitempty"`
}
type ChannelLook struct {
	MomentAssets         map[string]string `json:"momentAssets,omitempty"`
	BeforeBreak          []string          `json:"beforeBreak,omitempty"`
	AfterBreak           []string          `json:"afterBreak,omitempty"`
	BetweenProgrammes    []string          `json:"betweenProgrammes,omitempty"`
	BreakPlacement       string            `json:"breakPlacement,omitempty"`
	Tonight              *bool             `json:"tonight,omitempty"`
	Bug                  *core.BugSettings `json:"bug,omitempty"`
	BreakPattern         *BreakPattern     `json:"breakPattern,omitempty"`
	LookID               string            `json:"lookId"`
	Ident                bool              `json:"ident"`
	UpNext               bool              `json:"upNext"`
	Placement            string            `json:"placement,omitempty"`
	Breaks               string            `json:"breaks,omitempty"`
	BreakEvery           int               `json:"breakEvery,omitempty"`
	BreakContent         string            `json:"breakContent,omitempty"`
	ReelID               string            `json:"reelId,omitempty"`
	SegmentIDs           []string          `json:"segmentIds,omitempty"`
	BreakSeconds         int               `json:"breakSeconds,omitempty"`
	OffAir               bool              `json:"offAir"`
	Overlays             string            `json:"overlays,omitempty"`
	AllowUnratedSegments bool              `json:"allowUnratedSegments,omitempty"`
	Tagline              string            `json:"tagline,omitempty"`
	Accent               string            `json:"accent,omitempty"`
}

// ChannelScreens overrides the server defaults. Empty values follow them.
type ChannelScreens struct {
	OffAir   string `json:"offAir,omitempty"`
	NoSignal string `json:"noSignal,omitempty"`
}

type ClockSlot struct {
	SegmentID    string `json:"segmentId"`
	Minute       int    `json:"minute"`
	EveryMinutes int    `json:"everyMinutes"`
	From         int    `json:"from"`
	Until        int    `json:"until"`
	Days         []int  `json:"days"`
}
type PinnedSlot struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Start      int             `json:"start"`
	End        int             `json:"end"`
	Days       []int           `json:"days,omitempty"`
	Date       string          `json:"date,omitempty"`
	Sources    []ProgramSource `json:"sources,omitempty"`
	Items      []Item          `json:"items,omitempty"`
	Mode       string          `json:"mode,omitempty"`
	RepeatDays int             `json:"repeatDays,omitempty"`
	Breaks     *ChannelLook    `json:"breaks,omitempty"`
}

type Channel struct {
	Pins                 []PinnedSlot `json:"pins,omitempty"`
	programmingFill      bool
	programmingNotBefore time.Time
	programmingReels     map[string]*ReelSnapshot
	programmingBlockID   string
	programmingRule      string
	programmingPinned    bool
	Clock                []ClockSlot `json:"clock,omitempty"`
	segmentInfo          func(string) (string, string, bool)
	reel                 *ReelSnapshot
	momentHistory        *momentHistory
	DVRMinutes           int             `json:"dvrMinutes,omitempty"`
	Personal             *PersonalRecipe `json:"personal,omitempty"`
	personalViewer       *core.User
	Screens              *ChannelScreens `json:"screens,omitempty"`

	Look *ChannelLook `json:"look,omitempty"`
	// clipDuration is supplied on a channel copy; scheduling performs no I/O.
	clipDuration func(string, string) (time.Duration, bool)

	sweepKey      string
	numberMissing bool
	Health        string `json:"health,omitempty"`
	Transcode     bool   `json:"transcode"`           // IPTV: normalize codecs through the transcoder
	Stability     string `json:"stability,omitempty"` // fastest, balanced, rocksolid; "" inherits the server preset
	ID            string `json:"id"`
	// Private channels are hidden from every viewer whose policy does not
	// grant them: a restricted viewer needs it in Channels, an unrestricted
	// one in Granted. Owners always see them.
	Private       bool           `json:"private,omitempty"`
	Number        string         `json:"number"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"` // virtual or iptv
	Enabled       bool           `json:"enabled"`
	HiddenApps    []string       `json:"hiddenApps,omitempty"` // apps (Apps) that omit this channel; every other app lists it
	Favorite      bool           `json:"favorite"`
	Logo          string         `json:"logo,omitempty"`
	LogoMatched   bool           `json:"logoMatched,omitempty"`
	Rating        string         `json:"rating,omitempty"`
	SourceID      string         `json:"sourceId,omitempty"`
	GuideID       string         `json:"guideId,omitempty"`
	TvgName       string         `json:"tvgName,omitempty"`       // provider's label; a guide-matching hint
	StreamURL     string         `json:"streamUrl,omitempty"`     // owner API only
	StreamSources []StreamSource `json:"streamSources,omitempty"` // ordered inputs; independent of channel/guide identity
	Group         string         `json:"group,omitempty"`
	CustomGroup   bool           `json:"customGroup,omitempty"` // preserve owner grouping on provider refresh
	Epoch         time.Time      `json:"epoch"`
	Seed          int64          `json:"seed"`
	Shuffle       bool           `json:"shuffle"`
	Items         []Item         `json:"items"`
	Blocks        []Block        `json:"blocks,omitempty"`
	// Collection/Filter/Sources are expanded into a stable item snapshot when saved.
	Collection core.RatingKey  `json:"collection,omitempty"`
	Filter     *core.Filter    `json:"filter,omitempty"`
	Sources    []ProgramSource `json:"sources,omitempty"`
	Watched    string          `json:"watched,omitempty"` // household: watched, unwatched
	// Mode, Align, Fit, RepeatDays and Filler are the base playout. Empty
	// Mode keeps Shuffle as the original engine: false resumes in order from
	// Epoch, true is one stable shuffle. Timezone empty means block minutes
	// are UTC, every day.
	Mode        string         `json:"mode,omitempty"`
	Align       string         `json:"align,omitempty"`
	Fit         string         `json:"fit,omitempty"`
	RepeatDays  int            `json:"repeatDays,omitempty"`
	Filler      []Item         `json:"filler,omitempty"`
	Fallback    []Item         `json:"fallback,omitempty"`
	OffAir      bool           `json:"offAir,omitempty"`
	MarathonKey core.RatingKey `json:"marathonKey,omitempty"`
	Timezone    string         `json:"timezone,omitempty"`

	// GuideSourceID names the source whose XMLTV supplies GuideID's
	// programmes. Empty means SourceID. Virtual channels ignore it.
	GuideSourceID string `json:"guideSourceId,omitempty"`

	// ProviderNumber is the number the source gave this channel at the last
	// import (Xtream num, M3U tvg-chno, or the position in the owner's group
	// order). While Number still equals it the channel follows the
	// provider's renumbering; an owner edit makes Number differ and sticks.
	ProviderNumber string `json:"providerNumber,omitempty"`

	// adult is the provider's own adult flag (Xtream is_adult); import only.
	adult bool
}
type Source struct {
	LoginPool    bool            `json:"loginPool,omitempty"`
	Logins       []ProviderLogin `json:"logins,omitempty"`
	loginStreams map[string]map[string]string

	Disabled bool `json:"disabled,omitempty"`
	// LiveTV defaults to true for existing connections; VOD is opt-in.
	LiveTV      *bool      `json:"useLiveTV,omitempty"`
	VOD         bool       `json:"useVOD,omitempty"`
	VODStatus   *VODStatus `json:"vodStatus,omitempty"`
	LineupID    string     `json:"lineupId,omitempty"`
	Country     string     `json:"country,omitempty"`
	PostalCode  string     `json:"postalCode,omitempty"`
	PlexCursor  int        `json:"plexCursor,omitempty"`
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind,omitempty"` // a source kind id, e.g. plugin:<id>/channels
	PlaylistURL string     `json:"playlistUrl,omitempty"`
	GuideURL    string     `json:"guideUrl,omitempty"`
	// Inline uploads are supplied on import and are not returned by GET.
	Playlist  string    `json:"playlist,omitempty"`
	Guide     string    `json:"guide,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Auth authenticates playlist, guide and stream requests to the provider.
	// Secrets are redacted by the owner API (see auth.go).
	Auth *SourceAuth `json:"auth,omitempty"`
	// RefreshHours caps the time between automatic refreshes of URL-backed
	// sources: 0 uses the 6 hour default, a negative value means manual
	// refresh only. Sources also refresh before mapped listings run out.
	// Inline uploads never refresh automatically.
	RefreshHours int `json:"refreshHours,omitempty"`
	// ActivityRefreshMinutes allows an idle-to-active client visit to refresh
	// earlier than RefreshHours. Zero disables it; the minimum is 30 minutes.
	ActivityRefreshMinutes int `json:"activityRefreshMinutes,omitempty"`
	// LastError/LastErrorAt/RefreshFailures record failed refreshes; a
	// successful import clears them. Existing data is kept on failure.
	LastError       string     `json:"lastError,omitempty"`
	LastErrorAt     *time.Time `json:"lastErrorAt,omitempty"`
	RefreshFailures int        `json:"refreshFailures,omitempty"`
	// SkippedVOD counts VOD entries (Xtream /movie/ and /series/ streams)
	// the last playlist import left out. It is persisted with the source so
	// the console can report what the import dropped.
	SkippedVOD int `json:"skippedVod,omitempty"`
	// Read-only statistics computed by Sources(); ignored on import.
	HasPlaylist       bool       `json:"hasPlaylist"`
	HasGuide          bool       `json:"hasGuide"`
	ChannelCount      int        `json:"channelCount"`
	GuideChannelCount int        `json:"guideChannelCount"`
	ProgrammeCount    int        `json:"programmeCount"`
	GuideFrom         *time.Time `json:"guideFrom,omitempty"`
	GuideUntil        *time.Time `json:"guideUntil,omitempty"`
	NextRefresh       *time.Time `json:"nextRefresh,omitempty"`

	// Account is the provider's report on this login (Xtream panels),
	// refreshed with the lineup. Read-only.
	Account *ProviderAccount `json:"account,omitempty"`
	// Groups are the provider's channel groups from the last refresh, in
	// provider order, including the ones the owner excludes. Read-only.
	Groups []SourceGroup `json:"groups,omitempty"`
	// ExcludeGroups are group names the owner leaves out; their channels
	// are not imported at all. NewGroups "exclude" adds each group that
	// first appears on a refresh to ExcludeGroups; "" includes it.
	// HideAdult leaves out every group the provider flags as adult.
	ExcludeGroups []string `json:"excludeGroups,omitempty"`
	NewGroups     string   `json:"newGroups,omitempty"`
	HideAdult     bool     `json:"hideAdult,omitempty"`
	// GroupOrder is the owner's group order. Empty keeps the provider's
	// order and numbers; otherwise channels are numbered 1… down the
	// listed groups (unlisted groups follow in provider order).
	GroupOrder []string `json:"groupOrder,omitempty"`

	// Provider etiquette for the source's streams. MaxConnections caps
	// simultaneous provider connections below what the panel allows (0 uses
	// the panel's limit; it can never raise it). TuneGapSeconds is the least
	// time between two new provider streams: 0 is the default (2 s on a
	// connection-limited account, none otherwise), negative turns it off.
	// TunesPerHour caps new provider streams per hour; 0 is no cap.
	MaxConnections int `json:"maxConnections,omitempty"`
	TuneGapSeconds int `json:"tuneGapSeconds,omitempty"`
	TunesPerHour   int `json:"tunesPerHour,omitempty"`
}

// ProviderAccount is what an IPTV panel reports about the owner's login.
type ProviderAccount struct {
	Status            string     `json:"status,omitempty"`
	Trial             bool       `json:"trial,omitempty"`
	ExpiresAt         *time.Time `json:"expiresAt,omitempty"`
	MaxConnections    int        `json:"maxConnections,omitempty"`
	ActiveConnections int        `json:"activeConnections"`
	Formats           []string   `json:"formats,omitempty"`
	CheckedAt         time.Time  `json:"checkedAt"`
}

// SourceGroup is one provider channel group.
type SourceGroup struct {
	Name     string `json:"name"`
	Channels int    `json:"channels"`
	Adult    bool   `json:"adult,omitempty"`
	// Excluded reports whether the owner's rules leave this group out.
	Excluded bool `json:"excluded,omitempty"`
}
type Programme struct {
	BlockID          string `json:"blockId,omitempty"`
	SourceID         string `json:"sourceId,omitempty"`
	Pinned           bool   `json:"pinned,omitempty"`
	Rule             string `json:"rule,omitempty"`
	naturalBreaks    bool
	policyTag        *broadcastPolicyTag
	sourceOffset     time.Duration
	readinessViewer  int64
	graphicsRequest  *core.ClipRequest
	graphicsID       string
	graphicsFallback bool
	Reason           string    `json:"reason,omitempty"`
	Lens             string    `json:"lens,omitempty"`
	ID               string    `json:"id"`
	ChannelID        string    `json:"channelId"`
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	Item             Item      `json:"item"`
}

func (p Programme) Offset(at time.Time) time.Duration {
	if at.Before(p.Start) {
		return 0
	}
	if !at.Before(p.End) {
		return p.sourceOffset + p.End.Sub(p.Start)
	}
	return p.sourceOffset + at.Sub(p.Start)
}

// Policy is one viewer's Live TV access. It is the only channel allow-list:
// the owner edits it from the viewer's page and from each channel.
type Policy struct {
	Personal *PersonalPolicy `json:"personal,omitempty"`
	UserID   int64           `json:"userId"`
	Disabled bool            `json:"disabled"`
	// Transcode forces IPTV compatibility conversion for this viewer on new tunes.
	// It does not change channel settings or another viewer's playback.
	Transcode bool `json:"transcode"`
	// Restricted limits the viewer to Channels (none when empty), private
	// ones included. Unrestricted viewers see every public channel, including
	// ones added later, plus the private channels in Granted; Channels is
	// empty.
	Restricted bool     `json:"restricted"`
	Channels   []string `json:"channels,omitempty"`
	Granted    []string `json:"granted,omitempty"`
	Favorites  []string `json:"favorites,omitempty"`
	MaxRating  string   `json:"maxRating,omitempty"`

	channelSet map[string]struct{} // Channels as a set for long lists; see indexChannels
}
type Options struct {
	ActivityChanged func(bool) // worker lifetime transition after channels appear or disappear
	NewTimer        func(time.Duration) *time.Timer
	NewTicker       func(time.Duration) *time.Ticker

	VODChanged      func() // provider index or connection changed
	SessionsChanged func() // viewer live sessions started, stopped or expired
	SourcesChanged  func() // IPTV source health or lineup changed
	// BreakInput opens an exclusive background playback source, never a viewer session.
	// The final argument is an identity captured from an admitted save, never
	// a request context. The callback must revalidate it on each background run.
	BreakInput    func(context.Context, core.RatingKey, int64) (markers.BreakInput, error)
	BreakAnalyzer *markers.BreakAnalyzer
	GuideAI       GuideAIClient
	Reels         func(context.Context, string) (ReelSnapshot, error)
	InputUser     func(context.Context) (*core.User, error)
	StyleScreens  func(string) *config.LiveTVScreens
	// AudioTrack applies a user's stream preferences to a programme and
	// returns the chosen position in info.Streams, or -1 for no preference.
	// original is the title's original language.
	AudioTrack func(ctx context.Context, user *core.User, key core.RatingKey, info core.MediaInfo, original string) int
	Watchlist  interface{ Generation() uint64 }
	Picker     core.PersonalPicker
	State      core.State
	Users      interface {
		Users(context.Context) ([]core.User, error)
	}
	PersonalChanged func(int64)
	Graphics        core.ChannelGraphics
	GuideProvider   guideprovider.Provider

	CanPlay            func(*core.User, bool, time.Time) bool
	StreamLimit        func(*core.User) int
	DB                 *sql.DB
	Catalog            core.Catalog
	Collections        core.Collections
	Playback           core.Playback
	Transcoder         core.Transcoder
	HTTPClient         *http.Client
	LogoHTTP           *http.Client // optional test transport for the logo index; nil uses the guarded client
	LogoIndexURL       func() string
	HealthSweepMinutes func() int    // 0 defaults to 15; negative disables
	Stability          func() string // server-wide stability preset; nil or "" is balanced (reserve_policy.go)
	Now                func() time.Time
	// Logger receives credential-safe import, playback and delivery diagnostics.
	Logger *slog.Logger
	// Allowed applies additional owner title/genre policies to every surface.
	Allowed func(*core.User, *core.Title) bool
	// Warm resolves/primes a title without opening a viewer stream.
	Warm func(context.Context, core.RatingKey, time.Duration) error
	// Availability is the resolver's cached playability. Known-unavailable
	// titles are left out of the guide; unknown titles stay in place.
	Availability      core.AvailabilitySource
	CheckAvailability func(context.Context, core.RatingKey) (bool, bool)
	// Sealer protects source credentials at rest (secrets.go); nil stores
	// them as given.
	Sealer config.FieldSealer
	// Kinds overrides the source-kind registry; nil uses the built-ins.
	Kinds *KindRegistry
	// Showcase adds and refreshes the shipped decade channels.
	Showcase bool
}
type SessionInfo struct {
	ActiveSourceID string `json:"activeSourceId,omitempty"`
	// Plex's rolling subscription identity is numeric; the stream UUID remains
	// separate. These are ephemeral identities, scoped to this service lifetime.
	SubscriptionID  int64           `json:"-"`
	ActivityID      string          `json:"-"`
	ClientSessionID string          `json:"-"`
	MediaInfo       *core.MediaInfo `json:"-"` // Actual source analysis for native clients.
	ID              string          `json:"id"`
	UserID          int64           `json:"userId"`
	Channel         Channel         `json:"channel"`
	Programme       *Programme      `json:"programme,omitempty"`
	StartedAt       time.Time       `json:"startedAt"`
}

// ProviderLogin adds an account without importing another lineup.
type ProviderLogin struct {
	ID          string           `json:"id"`
	Username    string           `json:"username,omitempty"`
	Password    string           `json:"password,omitempty"`
	PlaylistURL string           `json:"playlistUrl,omitempty"`
	Account     *ProviderAccount `json:"account,omitempty"`
	Error       string           `json:"error,omitempty"`
}
