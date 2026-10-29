// Package frontend defines the protocol-facing contracts provided by Ether.
package frontend

import (
	"context"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Protocol identifies a surface independently of client product or platform.
type Protocol = core.Protocol

// Placement identifies where rows are rendered.
type Placement string

const (
	PlacementHome    Placement = "home"
	PlacementLibrary Placement = "library"
	PlacementDetail  Placement = "detail"
)

// RowPurpose describes why a row exists, independent of wire-level hub IDs.
type RowPurpose = core.RowPurpose

const (
	RowContinue         RowPurpose = "continue"
	RowNextUp           RowPurpose = "nextUp"
	RowRecentlyReleased RowPurpose = "recentlyReleased"
	RowCollection       RowPurpose = "collection"
	RowRecommendation   RowPurpose = "recommendation"
	RowLive             RowPurpose = "live"
	RowWatchlist        RowPurpose = "watchlist"
)

type View struct {
	ID   string
	Name string
	Type core.MediaType
}

// RowEntryKind identifies a neutral entry independently of wire formats.
type RowEntryKind string

const (
	RowEntryTitle       RowEntryKind = "title"
	RowEntryCollection  RowEntryKind = "collection"
	RowEntryPlaylist    RowEntryKind = "playlist"
	RowEntryLiveChannel RowEntryKind = "liveChannel"
	RowEntryDirectory   RowEntryKind = "directory"
)

// RowEntry is a tagged union: Kind identifies the one non-nil payload.
// Handle is an opaque, server-issued interaction capability scoped to the
// viewer and device; it is not an account credential.
type RowEntry struct {
	Kind        RowEntryKind
	Handle      string
	Title       *core.Title
	Collection  *core.Collection
	Playlist    *core.Playlist
	LiveChannel *RowLiveChannel
	Directory   *RowDirectory
}

// RowLiveChannel contains viewer-visible channel metadata, never source URLs.
type RowLiveChannel struct {
	ID, Number, Name, Logo string
	GuideReference         string
	Current, Next          *RowProgramme
}

// RowProgramme describes one guide entry without an upstream stream URL.
type RowProgramme struct {
	ID, Title, EpisodeTitle string
	Start, End              time.Time
	Season, Episode         int
	Item                    core.RatingKey
}

// RowDirectory identifies a browsable destination in the experience.
type RowDirectory struct {
	ID, Title, Description, Image string
	Kind                          string
}

// RowLayout describes Home Designer presentation selected by experience.
type RowLayout struct {
	Order int
	Style string
	Paint RowPaint
}

// RowPaint carries optional presentation colors and artwork references.
type RowPaint struct {
	Background, Foreground, Accent string
	Artwork                        string
}

type Row struct {
	ID, Title    string
	Purpose      RowPurpose
	Kind         string // Stable semantic producer kind, independent of protocol.
	Layout       RowLayout
	Style        string
	Promoted     bool
	Rank         int
	ImpressionID string
	// Handle is a server-issued row interaction capability.
	Handle string
	// Entries is the ordered heterogeneous row; Items remains for compatibility.
	Entries    []RowEntry
	Items      []core.Title
	More       bool
	Collection core.RatingKey
}
type RowRequest struct {
	Viewer    *core.Viewer
	Device    core.Device
	Placement Placement
	ViewID    string
	Item      core.RatingKey
	Limit     int
}

// RowInteractionKind is a viewer action on an issued row or entry.
type RowInteractionKind string

const (
	RowImpression RowInteractionKind = "impression"
	RowClick      RowInteractionKind = "click"
	RowPlay       RowInteractionKind = "play"
)

type RowInteraction struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
	Handle   string
	Kind     RowInteractionKind
}

// RowInteractionReporter validates issued handles, membership and device scope
// before recording actions. Clients must never construct handles or contexts.
type RowInteractionReporter interface {
	ReportRowInteraction(context.Context, RowInteraction) error
}

type ItemRequest struct {
	Viewer *core.Viewer
	Device core.Device
	Key    core.RatingKey
}
type ItemQuery struct {
	Offset, Limit int
	Sort          core.SortOrder
	Desc          bool
}

// Continuation distinguishes a known next page from exhaustion and a
// truncated source that cannot determine whether more matches exist.
type Continuation string

const (
	ContinuationMore      Continuation = "more"
	ContinuationExhausted Continuation = "exhausted"
	ContinuationUnknown   Continuation = "unknown"
)

type SearchPage struct {
	Page         core.Page
	TotalExact   bool
	Continuation Continuation
}
type Image struct{ Kind, Path, Tag string }
type Version struct {
	ID, Name string
	Media    core.MediaInfo
}
type Marker struct {
	Type       string
	Start, End time.Duration
}

// Extra describes a playable trailer or clip. Key is the broker-facing item
// identity; Source and SourceID describe the upstream reference, not a resolved
// URL or credentials. Plex media and part identifiers remain adapter-owned.
type Extra struct {
	Key, ParentKey                          core.RatingKey
	Title, Kind, Source, SourceID, Language string
	Official                                bool
	PublishedAt                             time.Time
	Duration                                time.Duration
	Height                                  int
}
type ItemDetail struct {
	Title    core.Title
	UserData core.WatchStatus
	Tracks   []core.StreamInfo
	Images   []Image
	Versions []Version
	Markers  []Marker
	Extras   []Extra
}

// Experience applies viewer policy before returning any data. Browse operations
// must not resolve or open a release. Policy is applied before pagination and limits.
type Experience interface {
	Views(context.Context, *core.Viewer) ([]View, error)
	Rows(context.Context, RowRequest) ([]Row, error)
	Item(context.Context, ItemRequest) (ItemDetail, error)
	Children(context.Context, *core.Viewer, core.Device, core.RatingKey, ItemQuery) (core.Page, error)
	Search(context.Context, *core.Viewer, core.Device, string, ItemQuery) (SearchPage, error)
	UserData(context.Context, *core.Viewer, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
	SetPlayed(context.Context, *core.Viewer, core.RatingKey) error
	SetUnplayed(context.Context, *core.Viewer, core.RatingKey) error
	SetFavorite(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetRating(context.Context, *core.Viewer, core.RatingKey, float64) error
	RemoveFromContinue(context.Context, *core.Viewer, core.RatingKey) error
}

type PlayIntent struct {
	Estimate        bool
	Options         core.DecideOptions
	Position        time.Duration
	ClientSessionID string
	PlaybackID      string
	// PreferRemux asks for a copy-only remux when source changes would
	// otherwise disrupt the client. The broker still decides whether it applies.
	PreferRemux bool
	Plex        *PlexHints
}

// PlexHints carries Plex-specific selections that cannot be represented by
// neutral stream indexes. Only the Plex adapter should populate it.
type PlexHints struct {
	Collection        string
	MediaIndex        int
	PartReleaseID     string
	AudioSelection    int
	SubtitleSelection int
	SubtitleSet       bool
	SubtitleStream    *core.StreamInfo
}

type StreamFormat string

const (
	StreamDirect StreamFormat = "direct"
	StreamHLS    StreamFormat = "hls"
	StreamDASH   StreamFormat = "dash"
)

// StreamRequest describes output, not an upstream URL or credential. The
// broker owns the input, decision and stream grant.
type StreamRequest struct {
	Format          StreamFormat
	Position        time.Duration
	SegmentDuration time.Duration
}
type PlanRequest struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
	Item     core.RatingKey
	Caps     core.ClientCaps
	Intent   PlayIntent
}
type PlayPlan struct {
	ID        string
	Estimated bool
	Media     core.MediaInfo
	Decision  core.Decision
}
type OpenRequest struct {
	Protocol Protocol
	PlanID   string
	Viewer   *core.Viewer
	Device   core.Device
}
type Session struct {
	ID, MediaPath string
	Protocol      Protocol
	ViewerID      int64
	Device        core.Device
	Item          core.RatingKey
	StartedAt     time.Time
}
type Timeline struct {
	// PlaybackID may be empty for clients that do not send one. The broker
	// preserves its no-ID supersession semantics.
	PlaybackID         string
	ClientSessionID    string
	Position, Duration time.Duration
	State              string
}
type Event struct {
	Type      string
	ViewerID  int64
	Item      core.RatingKey
	SessionID string
}

// SessionAccess binds client operations to an authenticated viewer and surface.
// Owner requests owner scope; it is not proof of authorization. The broker
// verifies the authenticated viewer is an active owner.
type SessionAccess struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
	Owner    bool
}

// Owner requests owner scope; the subscriber must verify active owner status.
type EventScope struct {
	Viewer *core.Viewer
	Owner  bool
}

// Sessions owns playback across protocols. Open revalidates the broker-owned
// plan, protocol, viewer, device and admission; estimated plans cannot be opened.
// MediaPath is scoped and unguessable and never contains account credentials.
type Sessions interface {
	Plan(context.Context, PlanRequest) (PlayPlan, error)
	Open(context.Context, OpenRequest) (Session, error)
	// StartStream revalidates access to the session and returns its scoped URL.
	StartStream(context.Context, SessionAccess, string, StreamRequest) (string, error)
	// ServeMedia authorizes the scoped grant and serves direct, HLS or DASH data.
	ServeMedia(http.ResponseWriter, *http.Request)
	Report(context.Context, SessionAccess, string, Timeline) error
	Stop(context.Context, SessionAccess, string) error
	Sessions(context.Context, SessionAccess) ([]Session, error)
	Recover(context.Context, string, string, time.Duration) error
	TranscodeFor(context.Context, string) (core.HLSSession, bool)
	// Events includes events from all cluster nodes, filters by viewer unless owner access is authorized, and ends on cancellation.
	Events(context.Context, EventScope) (<-chan Event, error)
}
