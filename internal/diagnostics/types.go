// Package diagnostics records a bounded, owner-confirmed support capture and
// builds one redacted ZIP. It does not persist captures, change playback, or
// accept paths, URLs, or commands from the caller.
package diagnostics

import "time"

// Schema is the archive manifest version.
const Schema = "ether.support.v1"

// Limits applied before an event is retained and before an archive is built.
const (
	maxEvents        = 2048
	maxCaptureEvents = maxEvents
	maxDevices       = 64
	maxSessions      = 256
	maxCaptures      = 2
	maxDiscovery     = 16
	maxString        = 240
	maxExcerpt       = 512
	maxNoteRunes     = 500
	maxCandidates    = 8
	maxDuration      = 3 * time.Minute
	preRoll          = 90 * time.Second
	retainFor        = 15 * time.Minute
	coalesceWindow   = 2 * time.Second
	// deviceRecent is how long a sighting stays on the recent-device list.
	// Older ids are stale: they are not offered, and starting a capture
	// with one returns ErrStaleDevice.
	deviceRecent    = 30 * time.Minute
	maxUncompressed = 2 << 20
	maxZip          = 2 << 20
	maxLogBytes     = 64 << 10
	maxLogLines     = 200
	redactMask      = "[redacted]"
)

// Kind is one observed fact. ClassifyRequest never returns a source
// rejection or an encoder result; those are recorded only by an explicit
// Observe from the component that actually decided.
type Kind string

const (
	KindDevice            Kind = "device.seen"
	KindPlaybackRequested Kind = "playback.requested"
	KindSourceChosen      Kind = "source.chosen"
	KindSourceRejected    Kind = "source.rejected"
	KindStreamRequested   Kind = "stream.requested"
	KindEncoderStarted    Kind = "encoder.started"
	KindEncoderFailed     Kind = "encoder.failed"
	KindMediaRequest      Kind = "media.request"
	KindMediaBytes        Kind = "media.bytes"
	KindTimelineReceived  Kind = "timeline.received"
	KindPlaybackReported  Kind = "playback.reported"
	KindPoll              Kind = "client.poll"
	KindTranscodeDecision Kind = "transcode.decision"
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
	Bytes            int64
	Range            string
	Repeat           int
	Product          string
	Platform         string
	Model            string
	DeviceName       string
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
	Peer             string
	// Uncorrelated is set when a correlation id was too long to retain.
	// The id is not stored, so it cannot be merged with another id.
	Uncorrelated bool
	Candidates   []Candidate
	Media        *MediaFacts
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
	LogLevel              string `json:"logLevel,omitempty"`
	FriendlyName          string `json:"friendlyName,omitempty"`
	QualityDefault        string `json:"qualityDefault,omitempty"`
	QualityRemote         string `json:"qualityRemote,omitempty"`
	ProfileName           string `json:"profileName,omitempty"`
	ProfileMaxHeight      int    `json:"profileMaxHeight,omitempty"`
	ProfileAllowHDR       bool   `json:"profileAllowHdr,omitempty"`
	HWAccel               string `json:"hwAccel,omitempty"`
	MaxTranscodes         int    `json:"maxTranscodes,omitempty"`
	RemoteMaxBitrateKbps  int    `json:"remoteMaxBitrateKbps,omitempty"`
	SecureConnections     string `json:"secureConnections,omitempty"`
	RemoteAccess          bool   `json:"remoteAccess,omitempty"`
	CertificateConfigured bool   `json:"certificateConfigured,omitempty"`
	CacheBudgetBytes      int64  `json:"cacheBudgetBytes,omitempty"`
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
type StorageFact struct {
	CacheBudgetBytes int64 `json:"cacheBudgetBytes,omitempty"`
	DiskFreeBytes    int64 `json:"diskFreeBytes,omitempty"`
	DiskFreeKnown    bool  `json:"diskFreeKnown"`
}

// ConnectivityFact has no public host, custom URL, or certificate path.
type ConnectivityFact struct {
	RemoteAccess          bool   `json:"remoteAccess"`
	SecureConnections     string `json:"secureConnections,omitempty"`
	CertificateConfigured bool   `json:"certificateConfigured"`
	PublicPort            int    `json:"publicPort,omitempty"`
}

// Deps are the only inputs the archive collectors may use.
type Deps struct {
	Now       func() time.Time
	Version   string
	Commit    string
	Platform  string
	Container string // "yes", "no", or "" when the caller could not tell
	Secrets   func() []string
	Settings  func() (Settings, error)
	// Sessions returns facts for deviceKey and a count of sessions that
	// belonged to someone else and were not copied into the slice.
	Sessions     func(deviceKey string) (matched []SessionFact, omitted int, err error)
	Hardware     func() (HardwareFact, error)
	Storage      func() (StorageFact, error)
	Connectivity func() (ConnectivityFact, error)
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
