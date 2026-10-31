// Package livetv implements owner-defined virtual channels and IPTV playout.
// Ether supplies neither programming nor IPTV accounts.
package livetv

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

const Provider = "tv.plex.providers.epg.xmltv:1"

type Item struct {
	Key        core.RatingKey `json:"key"`
	Title      string         `json:"title"`
	DurationMS int64          `json:"durationMs"`
	Summary    string         `json:"summary,omitempty"`
	Rating     string         `json:"rating,omitempty"`
	Thumb      string         `json:"thumb,omitempty"`
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
type Channel struct {
	Transcode     bool           `json:"transcode"` // IPTV: normalize codecs through the transcoder
	ID            string         `json:"id"`
	Number        string         `json:"number"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"` // virtual or iptv
	Enabled       bool           `json:"enabled"`
	Favorite      bool           `json:"favorite"`
	Logo          string         `json:"logo,omitempty"`
	Rating        string         `json:"rating,omitempty"`
	AllowedUsers  []int64        `json:"allowedUsers,omitempty"`
	SourceID      string         `json:"sourceId,omitempty"`
	GuideID       string         `json:"guideId,omitempty"`
	StreamURL     string         `json:"streamUrl,omitempty"`     // owner API only
	StreamSources []StreamSource `json:"streamSources,omitempty"` // ordered inputs; independent of channel/guide identity
	Group         string         `json:"group,omitempty"`
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
}
type Source struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
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
	// Read-only statistics computed by Sources(); ignored on import.
	HasPlaylist       bool       `json:"hasPlaylist"`
	HasGuide          bool       `json:"hasGuide"`
	ChannelCount      int        `json:"channelCount"`
	GuideChannelCount int        `json:"guideChannelCount"`
	ProgrammeCount    int        `json:"programmeCount"`
	GuideFrom         *time.Time `json:"guideFrom,omitempty"`
	GuideUntil        *time.Time `json:"guideUntil,omitempty"`
	NextRefresh       *time.Time `json:"nextRefresh,omitempty"`
}
type Programme struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channelId"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Item      Item      `json:"item"`
}

func (p Programme) Offset(at time.Time) time.Duration {
	if at.Before(p.Start) {
		return 0
	}
	if !at.Before(p.End) {
		return p.End.Sub(p.Start)
	}
	return at.Sub(p.Start)
}

type Policy struct {
	UserID    int64    `json:"userId"`
	Disabled  bool     `json:"disabled"`
	Channels  []string `json:"channels,omitempty"` // empty permits all
	Favorites []string `json:"favorites,omitempty"`
	MaxRating string   `json:"maxRating,omitempty"`
}
type Options struct {
	CanPlay     func(*core.User, bool, time.Time) bool
	StreamLimit func(*core.User) int
	DB          *sql.DB
	Catalog     core.Catalog
	Collections core.Collections
	Playback    core.Playback
	Transcoder  core.Transcoder
	HTTPClient  *http.Client
	Now         func() time.Time
	// Allowed applies additional owner title/genre policies to every surface.
	Allowed func(*core.User, *core.Title) bool
	// Warm resolves/primes a title without opening a viewer stream.
	Warm func(context.Context, core.RatingKey) error
	// Sealer protects source credentials at rest (secrets.go); nil stores
	// them as given.
	Sealer config.FieldSealer
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
