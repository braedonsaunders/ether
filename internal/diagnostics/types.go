// Package diagnostics records a bounded, owner-confirmed support capture and
// builds one redacted ZIP. It does not persist captures, change playback, or
// accept paths, URLs, or commands from the caller.
package diagnostics

import "time"

// Schema is the archive manifest version.
const Schema = "ether.support.v1"

// Limits applied before an event is retained and before an archive is built.
const (
	maxEvents             = 2048
	maxCaptureEvents      = maxEvents
	maxDevices            = 64
	maxSessions           = 256
	maxCaptures           = 2
	maxDiscovery          = 16
	maxString             = 240
	maxExcerpt            = 512
	maxNoteRunes          = 500
	maxCandidates         = 8
	maxPlaybackCandidates = 24
	maxDuration           = 10 * time.Minute
	preRoll               = 90 * time.Second
	retainFor             = 15 * time.Minute
	coalesceWindow        = 2 * time.Second
	// deviceRecent is how long a sighting stays on the recent-device list.
	// Older ids are stale: they are not offered, and starting a capture
	// with one returns ErrStaleDevice.
	deviceRecent    = 30 * time.Minute
	maxUncompressed = 2 << 20
	maxZip          = 2 << 20
	maxLogBytes     = 1 << 20
	maxLogLines     = 3000
	redactMask      = "[redacted]"
)

// Kind is one observed fact. ClassifyRequest never returns a source
// rejection or an encoder result; those are recorded only by an explicit
// Observe from the component that actually decided.
type Kind string

const (
	KindDevice             Kind = "device.seen"
	KindPlaybackRequested  Kind = "playback.requested"
	KindSourceChosen       Kind = "source.chosen"
	KindSourceRejected     Kind = "source.rejected"
	KindStreamRequested    Kind = "stream.requested"
	KindEncoderStarted     Kind = "encoder.started"
	KindEncoderFailed      Kind = "encoder.failed"
	KindMediaRequest       Kind = "media.request"
	KindMediaBytes         Kind = "media.bytes"
	KindTimelineReceived   Kind = "timeline.received"
	KindPlaybackReported   Kind = "playback.reported"
	KindPoll               Kind = "client.poll"
	KindTranscodeDecision  Kind = "transcode.decision"
	KindPlaybackDecision   Kind = "playback.decision"
	KindRequest            Kind = "request"
	KindRequestStarted     Kind = "request.started"
	KindClientCapabilities Kind = "client.capabilities"
)

// Event is one neutral observation. String fields are clipped before the
// event is stored.
type Event struct {
	Time time.Time
	// Until is the last arrival merged into this event. A coalesced span
	// is bounded by Time, not by the previous arrival. Zero means Time.
	Until            time.Time
	Kind             Kind
	Protocol         string
	DeviceKey        string
	SessionKey       string
	AttemptKey       string
	Method           string
	Route            string
	Status           int
	Duration         time.Duration
	Query            string
	Cancelled        bool
	DBQueries        int64
	DBTime           time.Duration
	Response         *ResponseSummary
	Bytes            int64
	Range            string
	Repeat           int
	Product          string
	Platform         string
	Model            string
	DeviceName       string
	Device           string
	AppVersion       string
	QualityRequested string
	QualitySelected  string
	Profile          string
	Decision         string
	Reason           string
	Rejection        string
	ErrorExcerpt     string
	FFmpegExcerpt    string
	Title            string
	SourceName       string
	Username         string
	ViewerID         int64 // correlation only; never exported
	Peer             string
	// Uncorrelated is set when a correlation id was too long to retain.
	// The id is not stored, so it cannot be merged with another id.
	Uncorrelated bool
	Candidates   []Candidate
	Media        *MediaFacts
	// Playback is the structured resolution trace for KindPlaybackDecision.
	// Release names stay only when the titles flag is on. It must not carry
	// a username or address.
	Playback *PlaybackDecision
	Client   *ClientFacts
}

type ResponseSummary struct {
	// Manifest contains bounded, allowlisted DASH facts; never raw URLs or tokens.
	Manifest  []string     `json:"manifest,omitempty"`
	Size      int          `json:"size"`
	ItemCount int          `json:"itemCount,omitempty"`
	Decision  string       `json:"decision,omitempty"`
	ErrorCode string       `json:"errorCode,omitempty"`
	Error     string       `json:"error,omitempty"`
	TotalSize *int         `json:"totalSize,omitempty"`
	More      *bool        `json:"more,omitempty"`
	Items     []string     `json:"items,omitempty"`
	Hubs      []HubSummary `json:"hubs,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

type HubSummary struct {
	Identifier string   `json:"identifier"`
	Title      string   `json:"title,omitempty"`
	Style      string   `json:"style,omitempty"`
	Type       string   `json:"type,omitempty"`
	Size       int      `json:"size"`
	More       bool     `json:"more"`
	ItemCount  int      `json:"itemCount"`
	Items      []string `json:"items,omitempty"`
}

// Candidate is a bounded, URL-free source fact.
type Candidate struct {
	Protocol   string
	Height     int
	VideoCodec string
	AudioCodec string
	Size       int64
	Score      float64
	Accepted   bool
	Rejected   bool
	Reason     string
	Name       string
}

// PlaybackDecision is a privacy-safe explanation of one resolution:
// the profile ceiling, who chose, the candidates that mattered, and
// the Play Version part ids. Release names are included only when the
// owner includes titles. Otherwise each name is a neutral token.
type PlaybackDecision struct {
	// TraceScope distinguishes title-wide historical traces from request-bound facts.
	TraceScope         string              `json:"traceScope,omitempty"`
	Profile            string              `json:"profile,omitempty"`
	ProfileMaxHeight   int                 `json:"profileMaxHeight,omitempty"`
	ClientMaxHeight    int                 `json:"clientMaxHeight,omitempty"`
	EffectiveMaxHeight int                 `json:"effectiveMaxHeight,omitempty"`
	AllowHDR           bool                `json:"allowHdr"`
	AllowDolbyVision   bool                `json:"allowDolbyVision"`
	Chooser            string              `json:"chooser,omitempty"`
	ChosenRelease      string              `json:"chosenRelease,omitempty"`
	RequestedPartID    int64               `json:"requestedPartId,omitempty"`
	Candidates         []PlaybackCandidate `json:"candidates,omitempty"`
	Variants           []PlaybackVariant   `json:"variants,omitempty"`
}

// PlaybackCandidate is one release in a playback decision.
type PlaybackCandidate struct {
	Name          string  `json:"name,omitempty"`
	Height        int     `json:"height,omitempty"`
	Size          int64   `json:"size,omitempty"`
	Score         float64 `json:"score,omitempty"`
	Rejected      bool    `json:"rejected,omitempty"`
	Reason        string  `json:"reason,omitempty"`
	Healthy       bool    `json:"healthy,omitempty"`
	HealthChecked bool    `json:"healthChecked,omitempty"`
}

// PlaybackVariant maps a Play Version part id to the release it starts.
type PlaybackVariant struct {
	PartID    int64  `json:"partId,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	Height    int    `json:"height,omitempty"`
	Release   string `json:"release,omitempty"`
	Requested bool   `json:"requested,omitempty"`
}

// MediaFacts is probe information safe to export. It has no URLs.
type MediaFacts struct {
	Container   string `json:"container,omitempty"`
	VideoCodec  string `json:"videoCodec,omitempty"`
	AudioCodec  string `json:"audioCodec,omitempty"`
	Height      int    `json:"height,omitempty"`
	Width       int    `json:"width,omitempty"`
	BitrateKbps int    `json:"bitrateKbps,omitempty"`
	Channels    string `json:"channels,omitempty"`
}

// DeviceInfo is the owner-console card. ID is an opaque hash, not a client id.
type DeviceInfo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Model         string    `json:"model,omitempty"`
	Product       string    `json:"product,omitempty"`
	Version       string    `json:"version,omitempty"`
	Platform      string    `json:"platform,omitempty"`
	LastSeen      time.Time `json:"lastSeen"`
	SuggestedKind string    `json:"suggestedKind,omitempty"`
}

// Settings is an explicit safe projection. Callers must not put secrets,
// filesystem paths, or URLs in it.
type Settings struct {
	LogLevel              string            `json:"logLevel,omitempty"`
	FriendlyName          string            `json:"friendlyName,omitempty"`
	QualityDefault        string            `json:"qualityDefault,omitempty"`
	QualityRemote         string            `json:"qualityRemote,omitempty"`
	ProfileName           string            `json:"profileName,omitempty"`
	ProfileMaxHeight      int               `json:"profileMaxHeight,omitempty"`
	ProfileAllowHDR       bool              `json:"profileAllowHdr,omitempty"`
	HWAccel               string            `json:"hwAccel,omitempty"`
	MaxTranscodes         int               `json:"maxTranscodes,omitempty"`
	RemoteMaxBitrateKbps  int               `json:"remoteMaxBitrateKbps,omitempty"`
	SecureConnections     string            `json:"secureConnections,omitempty"`
	RemoteAccess          bool              `json:"remoteAccess,omitempty"`
	CertificateConfigured bool              `json:"certificateConfigured,omitempty"`
	CacheBudgetBytes      int64             `json:"cacheBudgetBytes,omitempty"`
	ReleaseMode           string            `json:"releaseMode,omitempty"`
	ShowUpcomingEpisodes  bool              `json:"showUpcomingEpisodes"`
	ShowUpcomingMovies    bool              `json:"showUpcomingMovies"`
	VisibilityDefault     string            `json:"visibilityDefault,omitempty"`
	CatalogRules          []CatalogRuleFact `json:"catalogRules,omitempty"`
	CatalogRulesTruncated bool              `json:"catalogRulesTruncated,omitempty"`
}

type CatalogRuleFact struct {
	Action     string                 `json:"action"`
	MediaType  string                 `json:"mediaType"`
	Match      string                 `json:"match"`
	Scope      string                 `json:"scope"`
	Conditions []CatalogConditionFact `json:"conditions"`
}
type CatalogConditionFact struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value,omitempty"`
}

// SessionFact is one playback session before projection. DeviceKey selects
// correlation and is not exported. Username, title, and peer are omitted
// from the session file; usernames and addresses are exported only from the
// identity preview when the owner opts in.
type SessionFact struct {
	DeviceKey string
	State     string
	Decision  string
	Quality   string
	Title     string
	Username  string
	Peer      string
	Bytes     int64
}

// HardwareFact has no device node paths.
type HardwareFact struct {
	Name      string `json:"name,omitempty"`
	FFmpeg    string `json:"ffmpeg,omitempty"`
	Available bool   `json:"available"`
}

// StorageFact has no cache directory.
// DatabaseObjectFact describes a table or index allocation, never its row data.
type DatabaseObjectFact struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// DatabaseTableFact is one table's allocation: its own pages and its
// indexes' (FTS5 shadow tables count toward their virtual table). Rows is
// the planner's estimate from the last ANALYZE or PRAGMA optimize, or an
// exact count when RowsExact is set.
type DatabaseTableFact struct {
	Name       string `json:"name"`
	TableBytes int64  `json:"tableBytes"`
	IndexBytes int64  `json:"indexBytes"`
	Indexes    int    `json:"indexes"`
	Rows       int64  `json:"rows"`
	RowsExact  bool   `json:"rowsExact,omitempty"`
}

// CatalogCountFact counts catalogue rows: by type and browsable state
// (Detailed nil), and by browsable and detail state (Type empty). What the
// mirror holds, never which titles.
type CatalogCountFact struct {
	Type      string `json:"type,omitempty"`
	Browsable bool   `json:"browsable"`
	Detailed  *bool  `json:"detailed,omitempty"`
	Rows      int64  `json:"rows"`
}

// AssetKindFact totals stored On Air assets of one kind.
type AssetKindFact struct {
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

// CatalogSettingsFact is the catalogue's browsable bar. HideObscure is the
// console's "Hide obscure titles": either minimum set.
type CatalogSettingsFact struct {
	MinPopularity      float64 `json:"minPopularity"`
	MinVotes           int     `json:"minVotes"`
	HideObscure        bool    `json:"hideObscure"`
	IncludeAdult       bool    `json:"includeAdult"`
	ShowUpcomingMovies bool    `json:"showUpcomingMovies"`
}

// FileSetFact totals a set of files beside the database.
type FileSetFact struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

type StorageFact struct {
	DatabaseEngine           string               `json:"databaseEngine,omitempty"`
	DatabaseAllocationStatus string               `json:"databaseAllocationStatus,omitempty"`
	DatabasePageSize         int64                `json:"databasePageSize,omitempty"`
	DatabasePageCount        int64                `json:"databasePageCount,omitempty"`
	DatabaseFreePages        int64                `json:"databaseFreePages"`
	DatabaseObjectsStatus    string               `json:"databaseObjectsStatus,omitempty"`
	DatabaseObjects          []DatabaseObjectFact `json:"databaseLargestObjects,omitempty"`
	CacheBudgetBytes         int64                `json:"cacheBudgetBytes,omitempty"`
	DiskFreeBytes            int64                `json:"diskFreeBytes,omitempty"`
	DiskFreeKnown            bool                 `json:"diskFreeKnown"`
	DatabaseBytes            int64                `json:"databaseBytes,omitempty"`
	WALBytes                 int64                `json:"walBytes,omitempty"`
	DatabaseKnown            bool                 `json:"databaseKnown"`
	WALKnown                 bool                 `json:"walKnown"`
	Filesystem               string               `json:"filesystem,omitempty"`
	CacheFreeBytes           int64                `json:"cacheFreeBytes,omitempty"`
	CacheFreeKnown           bool                 `json:"cacheFreeKnown"`
	CacheFilesystem          string               `json:"cacheFilesystem,omitempty"`
	// DatabaseAutoVacuum and DatabaseJournalMode are SQLite's settings.
	DatabaseAutoVacuum  string `json:"databaseAutoVacuum,omitempty"`
	DatabaseJournalMode string `json:"databaseJournalMode,omitempty"`
	// DatabaseTables is every table measured, largest first. Status is
	// complete, partial (the time budget ran out; the largest tables by
	// row estimate are measured first) or unavailable.
	DatabaseTablesStatus string               `json:"databaseTablesStatus,omitempty"`
	DatabaseTables       []DatabaseTableFact  `json:"databaseTables,omitempty"`
	DatabaseTablesMillis int64                `json:"databaseTablesMillis,omitempty"`
	CatalogCountsStatus  string               `json:"catalogCountsStatus,omitempty"`
	CatalogCounts        []CatalogCountFact   `json:"catalogCounts,omitempty"`
	CatalogSettings      *CatalogSettingsFact `json:"catalogSettings,omitempty"`
	OnAirAssets          []AssetKindFact      `json:"onairAssets,omitempty"`
	PreMigrateSnapshots  FileSetFact          `json:"preMigrateSnapshots"`
	Backups              FileSetFact          `json:"backups"`
}

// ConnectivityFact has no public host, custom URL, or certificate path.
type ConnectivityFact struct {
	PlexRegistration      map[string]any `json:"plexRegistration,omitempty"`
	RemoteAccess          bool           `json:"remoteAccess"`
	SecureConnections     string         `json:"secureConnections,omitempty"`
	CertificateConfigured bool           `json:"certificateConfigured"`
	PublicPort            int            `json:"publicPort,omitempty"`
}

type ViewerSection struct {
	ID    int    `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
}
type ViewerCandidate struct {
	Key      int64  `json:"key"`
	Title    string `json:"title,omitempty"`
	OffsetMS int64  `json:"offsetMs"`
	Reason   string `json:"reason"`
	Included bool   `json:"included"`
}
type ViewerLayoutRow struct {
	Layer   string `json:"layer"`
	Section string `json:"section"`
	Row     string `json:"row"`
	Status  string `json:"status"`
	Slot    int    `json:"slot,omitempty"`
}
type ViewerFacts struct {
	Sections             []ViewerSection   `json:"sections"`
	Candidates           []ViewerCandidate `json:"candidates"`
	Layout               []ViewerLayoutRow `json:"layout"`
	WatchSample          int               `json:"watchSample"`
	InProgress           int               `json:"inProgress"`
	Watched              int               `json:"watched"`
	WatchSampleTruncated bool              `json:"watchSampleTruncated"`
	CandidatesTruncated  bool              `json:"candidatesTruncated"`
	SectionsTruncated    bool              `json:"sectionsTruncated"`
	LayoutTruncated      bool              `json:"layoutTruncated"`
	Notes                []string          `json:"notes,omitempty"`
}

// Deps are the only inputs the archive collectors may use.
type Deps struct {
	Now         func() time.Time
	Version     string
	Commit      string
	BuildDate   string
	Platform    string
	Container   string // "yes", "no", or "" when the caller could not tell
	InstallType string
	Uptime      func() time.Duration
	Secrets     func() []string
	Settings    func() (Settings, error)
	// Sessions returns facts for deviceKey and a count of sessions that
	// belonged to someone else and were not copied into the slice.
	Sessions     func(deviceKey string) (matched []SessionFact, omitted int, err error)
	Hardware     func() (HardwareFact, error)
	Storage      func() (StorageFact, error)
	Connectivity func() (ConnectivityFact, error)
	Viewer       func(userID int64) (ViewerFacts, error)
	// LogExtract returns structured records. Callers correlate on exact
	// fields inside [From, Until] and must not match substrings.
	LogExtract func(q LogQuery) (records []LogRecord, truncated bool, err error)
}

// StartIn opens a capture. DeviceID empty means the device is not known yet.
// A device id that is not in the recent table is stale and does not open a
// capture. Notes are kept, truncated, and treated as untrusted data.
type StartIn struct {
	DeviceID         string `json:"deviceId"`
	Category         string `json:"category"`
	Note             string `json:"note"`
	IncludeTitles    bool   `json:"includeTitles"`
	IncludeUsernames bool   `json:"includeUsernames"`
	IncludeAddresses bool   `json:"includeAddresses"`
}

// FinishIn replaces the inclusion flags used for the ZIP. The finish
// response previews the exact usernames and addresses that will be inside.
type FinishIn struct {
	IncludeTitles    bool `json:"includeTitles"`
	IncludeUsernames bool `json:"includeUsernames"`
	IncludeAddresses bool `json:"includeAddresses"`
	SetFlags         bool `json:"-"`
}

// ClientFacts records the capability calculation, not raw HTTP credentials.
// Inputs records header/query presence only; no client identifiers or arbitrary
// profile directives are copied. MaxHeight describes the exact caps handed to
// playback when Stage is admission. Stage output adds the final protocol and
// target codecs after profile processing; request describes declarations.
type ClientFacts struct {
	OutputProtocol       string   `json:"outputProtocol,omitempty"`
	PlayerProfile        string   `json:"playerProfile,omitempty"`
	TranscodeVideoCodecs []string `json:"transcodeVideoCodecs,omitempty"`
	TranscodeAudioCodecs []string `json:"transcodeAudioCodecs,omitempty"`
	Stage                string   `json:"stage"`
	Registry             string   `json:"registry"`
	RegistryMaxHeight    int      `json:"registryMaxHeight"`
	// OwnerMaxHeight is the registry ceiling after client overrides, not the owner quality profile.
	OwnerMaxHeight       int           `json:"ownerMaxHeight"`
	DeclaredMaxHeight    int           `json:"declaredMaxHeight"`
	MaxHeight            int           `json:"maxHeight"`
	ExplicitMaxHeight    bool          `json:"explicitMaxHeight"`
	MaxBitrateKbps       int           `json:"maxBitrateKbps"`
	DefaultRemoteBitrate bool          `json:"defaultRemoteBitrate"`
	Remote               bool          `json:"remote"`
	HDR10                bool          `json:"hdr10"`
	DolbyVision          bool          `json:"dolbyVision"`
	VideoCodecs          []string      `json:"videoCodecs"`
	AudioCodecs          []string      `json:"audioCodecs"`
	Containers           []string      `json:"containers"`
	Inputs               []ClientInput `json:"inputs,omitempty"`
}

type ClientInput struct {
	Name        string `json:"name"`
	Header      bool   `json:"header"`
	Query       bool   `json:"query"`
	HeaderEmpty bool   `json:"headerEmpty,omitempty"`
	QueryEmpty  bool   `json:"queryEmpty,omitempty"`
}
