// Package frontend defines the protocol-facing contracts provided by Ether.
package frontend

import (
	"context"
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
type Row struct {
	ID, Title    string
	Purpose      RowPurpose
	Style        string
	Promoted     bool
	Rank         int
	ImpressionID string
	Items        []core.Title
	More         bool
	Collection   core.RatingKey
}
type RowRequest struct {
	Viewer    *core.Viewer
	Device    core.Device
	Placement Placement
	ViewID    string
	Item      core.RatingKey
	Limit     int
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
	Search(context.Context, *core.Viewer, core.Device, string, ItemQuery) (core.Page, error)
	UserData(context.Context, *core.Viewer, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
}

type PlayIntent struct {
	Estimate bool
	Options  core.DecideOptions
	Position time.Duration
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
	Report(context.Context, SessionAccess, string, Timeline) error
	Stop(context.Context, SessionAccess, string) error
	Sessions(context.Context, SessionAccess) ([]Session, error)
	Recover(context.Context, string, string, time.Duration) error
	TranscodeFor(context.Context, string) (core.HLSSession, bool)
	// Events includes events from all cluster nodes, filters by viewer unless owner access is authorized, and ends on cancellation.
	Events(context.Context, EventScope) (<-chan Event, error)
}
