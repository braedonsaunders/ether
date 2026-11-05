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
	"github.com/braedonsaunders/ether/internal/markers"
)

const Provider = "tv.plex.providers.epg.xmltv:1"

type BreakPoint struct {
	AtMS       int64   `json:"atMs"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence,omitempty"`
}

type Item struct {
	BreakReleaseID string         `json:"breakReleaseId,omitempty"`
	BreakRevision  string         `json:"breakRevision,omitempty"`
	BreakPoints    []BreakPoint   `json:"breakPoints,omitempty"`
	BreakPointsMS  []int64        `json:"breakPointsMs,omitempty"`
	ReelID         string         `json:"reelId,omitempty"`
	ReelClipID     string         `json:"reelClipId,omitempty"`
	EpisodeTitle   string         `json:"episodeTitle,omitempty"`
	ShowTitle      string         `json:"showTitle,omitempty"`
	Art            string         `json:"art,omitempty"`
	Genres         []string       `json:"genres,omitempty"`
	GUID           string         `json:"guid,omitempty"`
	ShowGUID       string         `json:"showGuid,omitempty"`
	MediaType      string         `json:"mediaType,omitempty"`
	Premiere       bool           `json:"premiere,omitempty"`
	Live           bool           `json:"live,omitempty"`
	Key            core.RatingKey `json:"key"`
	Title          string         `json:"title"`
	DurationMS     int64          `json:"durationMs"`
	Summary        string         `json:"summary,omitempty"`
	Rating         string         `json:"rating,omitempty"`
	Thumb          string         `json:"thumb,omitempty"`
	// Show metadata keeps episodes in season order inside a shuffle,
	// marathon or newest-first playout. Zero values are movies and items
	// saved before weekly scheduling, which stay in their stored order.
	ShowKey    core.RatingKey `json:"showKey,omitempty"`
	Season     int            `json:"season,omitempty"`
	Episode    int            `json:"episode,omitempty"`
	ReleasedMS int64          `json:"releasedMs,omitempty"`
	// Slate is "offair" for a guide entry that fills a gap. It is not a
	// catalogue title and is never stored on a channel.
	Slate string `json:"slate,omitempty"`
}

// ProgramSource is one weighted part of a mix. Weight 0 means 1.
// Watched is "", "watched" or "unwatched" for the whole household.
type ProgramSource struct {
	Name       string         `json:"name,omitempty"`
	Weight     int            `json:"weight,omitempty"`
	Items      []Item         `json:"items,omitempty"`
	Collection core.RatingKey `json:"collection,omitempty"`
	Filter     *core.Filter   `json:"filter,omitempty"`
	Watched    string         `json:"watched,omitempty"`
}
type Block struct {
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
	BreakPlacement       string            `json:"breakPlacement,omitempty"`
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
type Channel struct {
	Clock          []ClockSlot `json:"clock,omitempty"`
	segmentInfo    func(string) (string, string, bool)
	reel           *ReelSnapshot
	momentHistory  *momentHistory
	DVRMinutes     int             `json:"dvrMinutes,omitempty"`
	Personal       *PersonalRecipe `json:"personal,omitempty"`
	personalViewer *core.User
	Screens        *ChannelScreens `json:"screens,omitempty"`

	Look *ChannelLook `json:"look,omitempty"`
	// clipDuration is supplied on a channel copy; scheduling performs no I/O.
	clipDuration func(string, string) (time.Duration, bool)

	sweepKey      string
	numberMissing bool
	Health        string `json:"health,omitempty"`
	Transcode     bool   `json:"transcode"` // IPTV: normalize codecs through the transcoder
	ID            string `json:"id"`
	// Private channels are hidden from every viewer whose policy does not
	// grant them: a restricted viewer needs it in Channels, an unrestricted
	// one in Granted. Owners always see them.
	Private       bool           `json:"private,omitempty"`
	Number        string         `json:"number"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"` // virtual or iptv
	Enabled       bool           `json:"enabled"`
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
	LineupID    string `json:"lineupId,omitempty"`
	Country     string `json:"country,omitempty"`
	PostalCode  string `json:"postalCode,omitempty"`
	PlexCursor  int    `json:"plexCursor,omitempty"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind,omitempty"` // a source kind id, e.g. plugin:<id>/channels
	PlaylistURL string `json:"playlistUrl,omitempty"`
	GuideURL    string `json:"guideUrl,omitempty"`
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
	policyTag       *broadcastPolicyTag
	sourceOffset    time.Duration
	graphicsRequest *core.ClipRequest
	graphicsID      string
	Reason          string    `json:"reason,omitempty"`
	Lens            string    `json:"lens,omitempty"`
	ID              string    `json:"id"`
	ChannelID       string    `json:"channelId"`
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	Item            Item      `json:"item"`
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
	// BreakInput opens an exclusive background playback source, never a viewer session.
	BreakInput    func(context.Context, core.RatingKey) (markers.BreakInput, error)
	BreakAnalyzer *markers.BreakAnalyzer
	GuideAI       GuideAIClient
	Reels         func(context.Context, string) (ReelSnapshot, error)
	InputUser     func(context.Context) (*core.User, error)
	DefaultStyle  func() string
	StyleScreens  func(string) *config.LiveTVScreens
	Screens       func() config.LiveTVScreens
	Watchlist     interface{ Generation() uint64 }
	Picker        core.PersonalPicker
	State         core.State
	Users         interface {
		Users(context.Context) ([]core.User, error)
	}
	PersonalChanged func(int64)
	Graphics        core.ChannelGraphics
	PlexGuideToken  func() string
	PlexGuideURL    string // test endpoint

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
	HealthSweepMinutes func() int // 0 defaults to 15; negative disables
	Now                func() time.Time
	// Logger receives credential-safe import, playback and delivery diagnostics.
	Logger *slog.Logger
	// Allowed applies additional owner title/genre policies to every surface.
	Allowed func(*core.User, *core.Title) bool
	// Warm resolves/primes a title without opening a viewer stream.
	Warm func(context.Context, core.RatingKey, time.Duration) error
	// Availability is the resolver's cached playability. Known-unavailable
	// titles are left out of the guide; unknown titles stay in place.
	Availability core.AvailabilitySource
	// Sealer protects source credentials at rest (secrets.go); nil stores
	// them as given.
	Sealer config.FieldSealer
	// Kinds overrides the source-kind registry; nil uses the built-ins.
	Kinds *KindRegistry
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
