// Package frontend defines the protocol-facing contracts provided by Ether.
package frontend

import (
	"context"
	"encoding/json"
	"errors"
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
	Images                 []Image
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

// FavoriteKind namespaces stable, policy-checked entity identifiers.
type FavoriteKind string

const (
	FavoriteTitle      FavoriteKind = "title"
	FavoritePerson     FavoriteKind = "person"
	FavoriteCollection FavoriteKind = "collection"
	FavoriteChannel    FavoriteKind = "channel"
)

// FavoriteRef identifies an entity in its own namespace.
type FavoriteRef struct {
	Kind FavoriteKind
	ID   string
}

type ItemRequest struct {
	Viewer *core.Viewer
	Device core.Device
	Key    core.RatingKey
}
type ItemQuery struct {
	// AdjacentTo selects the anchor and its immediately neighboring visible items.
	AdjacentTo core.RatingKey
	// StartAt selects an inclusive starting item after authorization, filtering
	// and ordering. Offset and Limit apply to the resulting suffix. It cannot
	// be combined with AdjacentTo; an absent or filtered anchor is not found.
	StartAt       core.RatingKey
	Offset, Limit int
	Sort          core.SortOrder
	Desc          bool
	Types         []core.MediaType
	Recursive     bool
	Season        *int // Optional presented season number.
	Filter        core.Filter
	Watched       *bool
	Favorite      *bool
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

// Image identifies an image authorized for an item. Path is an opaque,
// server-owned source reference: callers must neither interpret it as a URL nor
// construct one themselves. Tag changes when the selected artwork changes.
// Older adapters may still return a TMDB path while migrating to opaque refs.
type Image struct {
	Kind, Path, Tag string
	// Subject identifies an entity within the authorized item, such as the
	// tmdb:<id> person whose credit owns a portrait. It is part of the grant.
	Subject string
	// Width and Height describe the cached source pixels; zero means unknown.
	Width, Height int
	// BlurHash is a cached preview of the source; empty means unavailable.
	BlurHash string
	// ThumbHash is padded base64 of the cached source's standard ThumbHash;
	// empty means the background computation has not completed.
	ThumbHash string
}

// SameImage compares stable authorization identity, excluding optional metadata.
func SameImage(a, b Image) bool {
	return a.Kind == b.Kind && a.Path == b.Path && a.Tag == b.Tag && a.Subject == b.Subject
}

const (
	ImagePoster   = "poster"
	ImageBackdrop = "backdrop"
	ImageLogo     = "logo"
	ImageThumb    = "thumb"
	ImageBanner   = "banner"
	ImageStill    = "still"
	ImagePerson   = "person"
	ImageChannel  = "channel"
)

type Version struct {
	ID, Name string
	// AddedAt is release publication time when known, otherwise the title date
	// used for Plex AddedAt (updated time, then original release time).
	AddedAt time.Time
	Media   core.MediaInfo
	// Source facts are absent unless they are known for this version. FileName
	// is a basename only; DateModified is never a catalogue fallback date.
	FileName     string     `json:"fileName,omitempty"`
	PartCount    *int       `json:"partCount,omitempty"`
	ETag         string     `json:"etag,omitempty"`
	DateModified *time.Time `json:"dateModified,omitempty"`
	PublishedAt  *time.Time `json:"publishedAt,omitempty"`
	// FirstObservedAt is the first committed observation of this candidate offer.
	// Nil means that no trustworthy observation was recorded.
	FirstObservedAt *time.Time `json:"firstObservedAt,omitempty"`
}
type Marker struct {
	Type       string
	Start, End time.Duration
}

// SessionMarkers describes only a complete set bound to the current source.
// Known=false means the source has not established a complete marker set.
type SessionMarkers struct {
	Known      bool
	SourceETag string
	Markers    []Marker
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
	Collection *core.Collection
	Playlist   *core.Playlist
	Title      core.Title
	UserData   core.WatchStatus
	// CanPlay reflects current playback authorization when known. Nil means the
	// verified peer or policy result is unavailable. It does not guarantee a
	// source, codec, stream slot or transcode capacity will be available.
	CanPlay *bool
	Tracks  []core.StreamInfo
	// TracksEstimated marks a set containing synthesized playback estimates.
	// These streams must not be presented as facts about a resolved source.
	TracksEstimated bool
	Images          []Image
	// ImagesAuthoritative reports whether Images describes the currently selected
	// authorized artwork. Background discovery may add alternatives later.
	ImagesAuthoritative bool
	Versions            []Version
	Markers             []Marker
	Extras              []Extra
}

// ResolvedEventSubject is viewer-authorized current data for an event entity.
// PersonFacetID shares the namespace used by person facets.
type ResolvedEventSubject struct {
	Ref           FavoriteRef
	PersonFacetID string
	Favorite      bool
	UserData      *core.WatchStatus
}

// ExternalRequest identifies an IMDb title with an optional canonical episode.
// Season and Episode must either both be set or both omitted.
type ExternalRequest struct {
	Source          string
	ID              string
	Type            core.MediaType
	Season, Episode *int
}

// CollectionQuery requests viewer-scoped collection pagination. A nonpositive
// Limit defaults to 50; larger requests are capped at 500.
type CollectionQuery struct {
	Types         []core.MediaType
	Offset, Limit int
}
type CollectionPage struct {
	Items []core.Collection
	Total int
}

// PageQuery defaults nonpositive Limit to 50 and caps it at 500.
type PageQuery struct{ Offset, Limit int }
type NotInterestedEntry struct {
	Key     core.RatingKey
	AddedAt time.Time
}
type NotInterestedPage struct {
	Items []NotInterestedEntry
	Total int
}

// Experience applies viewer policy before returning any data. Browse operations
// must not resolve or open a release. Policy is applied before pagination and limits.
type Experience interface {
	Views(context.Context, *core.Viewer) ([]View, error)
	Collections(context.Context, *core.Viewer, core.Device, CollectionQuery) (CollectionPage, error)
	NotInterested(context.Context, *core.Viewer, core.Device, PageQuery) (NotInterestedPage, error)
	NotInterestedItem(context.Context, *core.Viewer, core.Device, core.RatingKey) (NotInterestedEntry, error)
	// ResolveExternal resolves an IMDb ID without opening a release, and returns
	// a key only after enforcing current viewer and inherited-show policy.
	ResolveExternal(context.Context, *core.Viewer, core.Device, ExternalRequest) (core.RatingKey, error)
	Rows(context.Context, RowRequest) ([]Row, error)
	Item(context.Context, ItemRequest) (ItemDetail, error)
	Children(context.Context, *core.Viewer, core.Device, core.RatingKey, ItemQuery) (core.Page, error)
	LibraryItems(context.Context, *core.Viewer, core.Device, string, ItemQuery) (core.Page, error)
	Similar(context.Context, *core.Viewer, core.Device, core.RatingKey, ItemQuery) (SearchPage, error)
	Ancestors(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]core.Title, error)
	Facets(context.Context, *core.Viewer, core.Device, string, string) ([]core.FacetValue, error)
	ResolveFacet(context.Context, *core.Viewer, core.Device, string, string, string) (core.FacetValue, error)
	// PersonFacetIDs returns IDs only for credited people of an authorized title.
	// Episode-only credits may not appear in library-wide Facets or ResolveFacet.
	// Requests are bounded to 256 positive TMDB IDs; missing credits are omitted.
	PersonFacetIDs(context.Context, *core.Viewer, core.Device, core.RatingKey, []int64) (map[int64]string, error)
	// NamedFacetIDs resolves exact genre or studio names on one visible title.
	// A request contains at most 256 nonempty names; missing names are omitted.
	NamedFacetIDs(context.Context, *core.Viewer, core.Device, core.RatingKey, string, []string) (map[string]string, error)
	Search(context.Context, *core.Viewer, core.Device, string, ItemQuery) (SearchPage, error)
	UserData(context.Context, *core.Viewer, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
	SetPlayed(context.Context, *core.Viewer, core.RatingKey) error
	SetUnplayed(context.Context, *core.Viewer, core.RatingKey) error
	SetFavorite(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetNotInterested(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetWatchlist(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetEntityFavorite(context.Context, *core.Viewer, FavoriteRef, bool) error
	EntityFavorites(context.Context, *core.Viewer, []FavoriteRef) (map[FavoriteRef]bool, error)
	ResolveEventSubject(context.Context, *core.Viewer, core.Device, FavoriteRef) (ResolvedEventSubject, error)
	SetRating(context.Context, *core.Viewer, core.RatingKey, float64) error
	SetLikes(context.Context, *core.Viewer, core.RatingKey, *bool) error
	RemoveFromContinue(context.Context, *core.Viewer, core.RatingKey) error
}

type PlayIntent struct {
	Estimate bool
	Options  core.DecideOptions
	// ExactVersion treats PlanRequest.Version as a scoped opaque handle and
	// refuses any substitute release, including during later source failover.
	ExactVersion bool
	// Set flags distinguish omitted choices from stream index zero and
	// an explicit subtitle-off choice (SubtitleStream == -1).
	AudioSet    bool
	SubtitleSet bool
	// AudioOrdinal selects a zero-based audio-only position from the opened,
	// probed source. SubtitleStreamIndex is an absolute source stream index;
	// -1 explicitly disables subtitles. Nil preserves normal preferences.
	AudioOrdinal        *int
	SubtitleStreamIndex *int
	Position            time.Duration
	ClientSessionID     string
	PlaybackID          string
	// AllowTranscoding is nil for the existing default; false forbids video
	// or audio transcoding while retaining any allowed direct-stream remux.
	AllowTranscoding *bool
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

// ErrTranscodeCapacity classifies an exhausted shared encoder budget.
var ErrTranscodeCapacity = errors.New("transcode capacity exhausted")

// TranscodeCapacityError carries a suggested retry delay. Capacity is checked
// atomically at stream start and is not reserved by an earlier Plan.
type TranscodeCapacityError struct {
	RetryAfter time.Duration
	Cause      error
}

func (e *TranscodeCapacityError) Error() string        { return ErrTranscodeCapacity.Error() }
func (e *TranscodeCapacityError) Unwrap() error        { return e.Cause }
func (e *TranscodeCapacityError) Is(target error) bool { return target == ErrTranscodeCapacity }

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
	// Version is a resolver-issued variant ID in legacy mode, or a scoped
	// VersionHandles handle when ExactVersion is set. Empty selects automatically.
	Version string
	// ExpectedSourceETag binds playback to a previously offered resolved source.
	// It is not an authorization credential. A missing or changed source fails
	// closed instead of resolving a different release.
	ExpectedSourceETag string
	// DetailSource is an opaque, scoped handle returned by DetailPlan.
	DetailSource string
	Viewer       *core.Viewer
	Device       core.Device
	Protocol     Protocol
	Item         core.RatingKey
	// Channel is the stable Host.LiveTV channel ID. Exactly one of Item and
	// Channel must be supplied.
	Channel string
	Caps    core.ClientCaps
	Intent  PlayIntent
}
type PlayPlan struct {
	ID        string
	Estimated bool
	Media     core.MediaInfo
	Decision  core.Decision
	// Alternatives are evaluated against this same source and client policy.
	// DirectStream denotes copy-only remux, without encoding either stream.
	DirectPlay, DirectStream, Transcoding bool
	// HLS reports whether this resolved source can be delivered as HLS under
	// the declared client profile. Encoder capacity is checked at stream start.
	HLS bool
	// HLSContainer is the authoritative segment container from the resolved HLS
	// output decision (mpegts, fmp4, or mkv). Empty means unknown or ineligible;
	// it is not the source container or an encoder-capacity reservation.
	HLSContainer string
	// ETag identifies the resolved immutable source when available; it is
	// empty when the backend cannot prove a stable source identity.
	ETag string
	// DetailSource is a scoped, expiring handle for the resolved source.
	DetailSource string
}

// DetailPlanner resolves a playable single item within a bounded admission
// budget. It returns source facts without creating a frontend session. The
// caller must separately obtain a scoped lazy stream capability.
type DetailPlanner interface {
	DetailPlan(ctx context.Context, request PlanRequest) (PlayPlan, error)
	ResolveDetailSource(ctx context.Context, scope VersionScope, handle string) (DetailSourceTarget, error)
	MatchDetailSession(ctx context.Context, scope VersionScope, handle, sessionID string) error
}

type DetailSourceTarget struct {
	Item RatingKey
	ETag string
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
	// ClientSessionID is a non-secret protocol installation/session identifier.
	ClientSessionID string
	Device          core.Device
	Item            core.RatingKey
	Channel         string
	StartedAt       time.Time
	Capabilities    *SessionCapabilities
}

// SessionCapabilities are declared by an authenticated device on one surface.
// URL fields are metadata and must never be fetched by the broker.
type SessionCapabilities struct {
	PlayableMediaTypes           []string
	SupportedCommands            []string
	SupportsMediaControl         *bool
	SupportsPersistentIdentifier *bool
	SupportsRemoteControl        *bool
	DeviceProfile                json.RawMessage
	AppStoreURL                  string
	IconURL                      string
}
type Timeline struct {
	// PlaybackID may be empty for clients that do not send one. The broker
	// preserves its no-ID supersession semantics.
	PlaybackID      string
	ClientSessionID string
	// Sequence is optional; zero preserves the legacy unordered behavior.
	Sequence           uint64
	Position, Duration time.Duration
	State              string
}

type CommandKind string

const (
	CommandStop        CommandKind = "stop"
	CommandPause       CommandKind = "pause"
	CommandUnpause     CommandKind = "unpause"
	CommandSeek        CommandKind = "seek"
	CommandSetAudio    CommandKind = "setAudio"
	CommandSetSubtitle CommandKind = "setSubtitle"
	CommandMessage     CommandKind = "message"
)

type Command struct {
	Kind                CommandKind
	Position            time.Duration
	StreamIndex         int
	Message             string
	ControllerSessionID string
}
type Event struct {
	Type     string
	ViewerID int64
	// ActorViewerID is set from the authenticated actor for SessionCommand;
	// zero denotes a system-originated stop without an authenticated actor.
	ActorViewerID int64
	Item          core.RatingKey
	SessionID     string
	// SubjectKind and SubjectID identify non-title user data such as a
	// favorite person, collection or channel. Item remains the title key.
	SubjectKind     string
	SubjectID       string
	OriginProtocol  Protocol
	OriginSessionID string
	OriginClientID  string
	Command         *Command
	TargetProtocol  Protocol
	TargetDeviceID  string
	Session         *Session
	Timeline        *Timeline
}

// TrickplayOffer is Jellyfin sprite geometry for one generated width.
// Width and Height are one thumbnail in pixels. Columns and Rows are the
// grid inside each sheet. Interval is the time between thumbnails.
type TrickplayOffer struct {
	Width, Height int
	Columns, Rows int
	Count         int
	Interval      time.Duration
	Bandwidth     int
}

// TrickplayTiles serves preview sheets for an item the viewer may play.
// The broker owns release identity and generation. A false offer means no
// frame exists yet, which clients render as an empty trickplay map.
type TrickplayTiles interface {
	TrickplayOffer(context.Context, SessionAccess, core.RatingKey) (TrickplayOffer, bool)
	TrickplaySheet(context.Context, SessionAccess, core.RatingKey, int) ([]byte, error)
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

// ScopedMedia serves a stream only to the authenticated device that owns it.
// The scoped URL's grant and current playback policy are also checked.
type ScopedMedia interface {
	ServeMediaAccess(http.ResponseWriter, *http.Request, SessionAccess)
}

// Owner requests owner scope; the subscriber must verify active owner status.
type EventScope struct {
	Viewer *core.Viewer
	Owner  bool
	// Protocol suppresses this surface's own user-data invalidations.
	Protocol Protocol
	// SessionID selects the authenticated client session for targeted commands.
	SessionID string
	// Device binds a socket before a playback session exists and remains valid
	// when this authenticated device's current session changes.
	Device core.Device
}

// Sessions owns playback across protocols. Open revalidates the broker-owned
// plan, protocol, viewer, device and admission; estimated plans cannot be opened.
// MediaPath is scoped and unguessable and never contains account credentials.
type Sessions interface {
	Plan(context.Context, PlanRequest) (PlayPlan, error)
	Open(context.Context, OpenRequest) (Session, error)
	// StartStream revalidates access to the session and returns its scoped URL.
	StartStream(context.Context, SessionAccess, string, StreamRequest) (string, error)
	// SubtitleURL returns a scoped URL for a subtitle stream index and format.
	SubtitleURL(context.Context, SessionAccess, string, int, string) (string, error)
	// Markers returns source-bound ranges for the authorized playback session.
	Markers(context.Context, SessionAccess, string) (SessionMarkers, error)
	// ServeMedia authorizes the scoped grant and serves direct, HLS or DASH data.
	ServeMedia(http.ResponseWriter, *http.Request)
	Report(context.Context, SessionAccess, string, Timeline) error
	Stop(context.Context, SessionAccess, string) error
	Command(context.Context, *core.Viewer, string, Command) error
	UpdateCapabilities(context.Context, SessionAccess, SessionCapabilities) error
	Capabilities(context.Context, SessionAccess) (SessionCapabilities, error)
	Sessions(context.Context, SessionAccess) ([]Session, error)
	Recover(context.Context, string, string, time.Duration) error
	TranscodeFor(context.Context, string) (core.HLSSession, bool)
	// Events includes events from all cluster nodes, filters by viewer unless owner access is authorized, and ends on cancellation.
	Events(context.Context, EventScope) (<-chan Event, error)
}
