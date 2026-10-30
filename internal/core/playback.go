package core

import (
	"context"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// Transcoder (internal/transcode)

type HLSRequest struct {
	// RequireIndependentProof emits HLS independence tags only when the
	// session can guarantee every advertised segment is independently decodable.
	// Zero preserves the legacy playlist contract for existing callers.
	RequireIndependentProof bool
	SessionID               string
	InputURL                string // ffmpeg input; Ether serves sources on loopback HTTP with range support
	// LiveInput permits playlist demuxers for owner-configured live sources.
	// Ordinary resolved media must leave this false, even if duration is unknown.
	LiveInput bool
	// InputHeaders are optional HTTP headers ffmpeg must send when reading
	// InputURL (e.g. for resolved trailer URLs). Transcoders that cannot
	// send headers ignore them; most resolved URLs need none (additive).
	InputHeaders map[string]string
	Info         MediaInfo
	Decision     Decision
	StartOffset  time.Duration
	// SegmentDuration defaults to 4s when zero (5s for DASH).
	SegmentDuration time.Duration
	// Protocol is ProtocolHLS (default when empty) or ProtocolDASH. A DASH
	// session also implements DASHSession; Transcoder.Session returns it as
	// an HLSSession, so type-assert to reach the DASH methods.
	Protocol string
}

// Streaming protocols of HLSRequest.Protocol.
const (
	ProtocolHLS  = "hls"
	ProtocolDASH = "dash"
)

// DASHSession is a running ffmpeg remux or transcode producing MPEG-DASH:
// a static, full-length MPD with separate video and audio representations
// of fMP4 segments addressed by number ($Number$, from 0). Its HLSSession
// methods map onto the first representation; ServePlaylist writes the MPD.
type DASHSession interface {
	HLSSession
	// ServeManifest writes the MPD.
	ServeManifest(w http.ResponseWriter, r *http.Request)
	// ServeInit writes the initialization segment of a representation.
	ServeInit(w http.ResponseWriter, r *http.Request, rep int)
	// ServeMedia writes media segment n of a representation, waiting for
	// ffmpeg and restarting it at the right offset when a seek jumps ahead.
	ServeMedia(w http.ResponseWriter, r *http.Request, rep, n int)
}

// HLSSession is one running ffmpeg remux or transcode producing HLS (or
// DASH; see DASHSession).
type HLSSession interface {
	ID() string
	// ServePlaylist writes the media playlist (index.m3u8).
	ServePlaylist(w http.ResponseWriter, r *http.Request)
	// ServeSegment writes segment n, waiting for ffmpeg to produce it and
	// restarting ffmpeg at the right offset when a seek jumps ahead.
	ServeSegment(w http.ResponseWriter, r *http.Request, n int)
	// SwapInput restarts the session on a new input at the given position
	// (mid-stream failover). Segment numbering continues.
	SwapInput(ctx context.Context, inputURL string, info MediaInfo, at time.Duration) error
	LastAccess() time.Time
	Close() error
}

type Transcoder interface {
	Probe(ctx context.Context, inputURL string) (*MediaInfo, error)
	Decide(info MediaInfo, caps ClientCaps, opts DecideOptions) Decision
	// StartHLS starts an HLS session, or a DASH session (a DASHSession)
	// when req.Protocol is ProtocolDASH.
	StartHLS(ctx context.Context, req HLSRequest) (HLSSession, error)
	Session(id string) (HLSSession, bool)
	Stop(id string)
	HWAccel() HWAccel
}

// DecideOptions are per-request choices from the Plex decision request.
type DecideOptions struct {
	// DisableVideoTranscode prevents optional enhancement from promoting direct playback.
	DisableVideoTranscode bool

	DirectPlay     bool     // client allows direct play
	DirectStream   bool     // client allows remux
	MaxBitrateKbps int      // client-requested cap (maxVideoBitrate)
	MaxHeight      int      // from videoResolution
	AudioStream    int      // selected stream index, -1 default
	SubtitleStream int      // -1 none
	AudioFilters   []string // trusted owner/viewer audio processing; forces audio encoding
	BurnSubtitles  bool
}

// ---------------------------------------------------------------------------
// Playback orchestration (internal/playback)

// PlaySession binds a user, a title and a client to a resolved, open source.
type PlaySession struct {
	PlaybackGroup string `json:"playbackGroup,omitempty"`
	// PreparedOnly marks a source reserved for planning but not opened by a
	// frontend. Cluster snapshots carry it for owner Activity.
	PreparedOnly bool `json:"preparedOnly,omitempty"`
	ID           string
	UserID       int64
	ClientID     string
	Key          RatingKey
	Resolution   *Resolution
	Source       MediaSource // swaps on failover; read through Playback.Source
	Info         MediaInfo
	StartedAt    time.Time

	// Status for the admin dashboard and /status/sessions, filled on the
	// snapshots Playback returns.
	TitleName      string       // "Movie (2024)" or "Show - S01E02 - Episode"
	Username       string       // user display name
	Client         ClientCaps   // the client's product, device and limits
	DecisionReason string       `json:"decisionReason,omitempty"`
	Decision       DecisionKind // directplay once bytes go straight to the client; "" until known
	BytesServed    int64        // source bytes delivered (direct play and ffmpeg input)
	SpeedBps       int64        // recent delivery rate, bytes per second
	LastActive     time.Time
	// ViewOffset is the client's last reported playhead in milliseconds.
	// Nil means no timeline has arrived; source bytes do not imply progress.
	ViewOffset *int64 `json:"viewOffset,omitempty"`
	Failovers  int    // mid-stream release swaps so far
	// Origin names where the current release streams from: "Usenet" or the
	// debrid service, such as "Real-Debrid" (additive).
	Origin string `json:"origin,omitempty"`
	// DownFailovers counts speed-driven swaps to a lower-bitrate release
	// (a subset of Failovers) (additive).
	DownFailovers int `json:"downFailovers,omitempty"`
	// RequiresRemux is set when a direct-play failover could not splice or
	// find an exact-size replacement: the next decision/start for this
	// session must be a zero-encode remux (additive).
	RequiresRemux bool `json:"requiresRemux,omitempty"`
	// Node is the cluster node serving a session reported by another node
	// (empty for this node's own sessions and on a standalone server).
	Node string `json:"node,omitempty"`
}

type Playback interface {
	// Estimate is the virtual media info shown before a title is resolved.
	Estimate(ctx context.Context, t *Title, caps ClientCaps) MediaInfo
	// Prepare resolves and opens a source for the user and client, enforcing
	// stream limits. Reuses an existing session for the same user/client/key.
	Prepare(ctx context.Context, user *User, key RatingKey, caps ClientCaps) (*PlaySession, error)
	Session(id string) (*PlaySession, bool)
	// SessionFor finds the live session of a client for a title.
	SessionFor(userID int64, clientID string, key RatingKey) (*PlaySession, bool)
	// ServeRange serves the session's current source with HTTP range
	// support, failing over to the next release when articles are missing.
	ServeRange(w http.ResponseWriter, r *http.Request, sessionID string)
	// InputURL is a loopback URL ffmpeg can read the session's source from.
	InputURL(sessionID string) string
	// Failover swaps the session to the next candidate release, keeping the
	// timestamp. Returns the new media info.
	Failover(ctx context.Context, sessionID string, at time.Duration) (*PlaySession, error)
	Stop(sessionID string)
	Sessions() []*PlaySession
}

// SurfaceScopedPlayback separates client identifiers by their authenticated
// protocol surface, even when different clients supply identical IDs.
type SurfaceScopedPlayback interface {
	PrepareSurface(ctx context.Context, user *User, key RatingKey, caps ClientCaps, releaseID string, exact bool) (*PlaySession, error)
	SessionForSurface(surface string, userID int64, clientID string, key RatingKey, releaseID string, exact bool) (*PlaySession, bool)
	MarkSurfaceOpened(id string) bool
	MarkSurfacePrepared(id string) bool
}

// PlaybackRevisionPin keeps an opened generation from silently failing over
// while a client consumes metadata bound to its source revision.
type PlaybackRevisionPin interface {
	PinSourceRevision(ctx context.Context, id, revision string) error
	UnpinSourceRevision(id string)
}

// PlaybackSourceObservation contains scalar facts from one opened source
// generation. Empty Revision means its identity cannot be established safely;
// nil HDR and empty codec/zero height remain unknown.
type PlaybackSourceObservation struct {
	Revision, ReleaseID string
	VideoCodec          string
	Height              int
	HDR                 *bool
}

// GuardedPlaybackSource keeps a source generation current until fn completes.
// Callers must bound ctx and must not call Stop or Failover from fn.
type GuardedPlaybackSource interface {
	WithCurrentSource(context.Context, string, func(context.Context, PlaybackSourceObservation) error) error
}
