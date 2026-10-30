package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
)

// BrokerDeps are the explicit dependencies shared by all protocol surfaces.
type BrokerDeps struct {
	Coordination *Coordination
	// MaxPendingStarts bounds active or waiting neutral stream starts.
	// Zero selects the default; the broker rejects values above 4096.
	MaxPendingStarts int
	DB               *sql.DB
	Cluster          clusterbus.Node
	Live             LiveBackend
	Variants         func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error)
	SelectTracks     func(context.Context, *core.Viewer, *core.Title, core.MediaInfo, core.DecideOptions, bool, bool) (core.DecideOptions, error)
	// SelectScopedTracks receives the authenticated device as well. If present,
	// it replaces SelectTracks and must preserve the unscoped behavior.
	SelectScopedTracks func(context.Context, *core.Viewer, core.Device, *core.Title, core.MediaInfo, core.DecideOptions, bool, bool) (core.DecideOptions, error)
	Lookup             func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
	// MediaLookup checks current title policy using local facts only; it must
	// never resolve a release or fetch remote metadata on a segment request.
	// If nil, Lookup must satisfy the same requirement.
	MediaLookup        func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
	Playback           core.Playback
	Transcoder         core.Transcoder
	Progress           ProgressWriter
	ActiveViewer       func(context.Context, int64) (*core.Viewer, error)
	CanPlay            func(*core.Viewer, bool, time.Time) bool
	BitrateLimit       func(*core.Viewer) int
	RemoteBitrateLimit func() int
	OnTimeline         func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string)
	Events             *EventBus
	OnEventError       func(error)
	OpenSubtitle       func(string, string) (io.ReadCloser, string, error)
	ExtractSubtitle    func(context.Context, string, int, string, io.Writer) error
	Now                func() time.Time
}

type brokerPlan struct {
	request       frontend.PlanRequest
	source        *core.PlaySession
	decision      core.Decision
	created       time.Time
	etag          string
	live          bool
	exactRelease  string
	versionHandle string
}
type brokerSession struct {
	public           frontend.Session
	grant            string
	playID           string
	clientID         string
	playbackID       string
	caps             core.ClientCaps
	opts             core.DecideOptions
	decision         core.Decision
	allowTranscoding *bool
	format           frontend.StreamFormat
	streamPosition   time.Duration
	segmentDuration  time.Duration
	live             bool
	sequence         uint64
	versionHandle    string
	exactRelease     string
}

type retiredSession struct {
	viewerID int64
	protocol frontend.Protocol
	deviceID string
	until    time.Time
}

// Broker coordinates neutral sessions with the shared playback admission manager.
// Plex's legacy timeline tracker is not yet included in Sessions; owner
// Activity continues to use the shared playback manager until that move lands.
type Broker struct {
	deps                BrokerDeps
	startSlots          chan struct{}
	shutdownTimeout     time.Duration
	mu                  sync.Mutex
	plans               map[string]brokerPlan
	sessions            map[string]*brokerSession
	retired             map[string]retiredSession
	lazySessions        map[string]lazySession
	lazyCleanup         map[string]lazyCleanupEntry
	lazyCleanupRunning  bool
	lazyCleanupWait     time.Duration
	lazyCleanupReserved int
	lazyCleanupCancel   context.CancelFunc
	lazyCleanupDone     chan struct{}
	devices             map[deviceKey]frontend.SessionCapabilities
	owners              *PlaybackOwners
	gates               *Lifecycle
	bindings            *BindingTable[frontend.PlayIntent]
	closed              bool
	pendingPlans        int
	cluster             *brokerCluster
}

func NewBroker(d BrokerDeps) (*Broker, error) {
	if d.Lookup == nil || d.Playback == nil || d.ActiveViewer == nil || d.Events == nil || d.CanPlay == nil || d.BitrateLimit == nil || d.Progress == nil || d.OnTimeline == nil {
		return nil, errors.New("sessions: missing required dependency")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	coord := d.Coordination
	if coord == nil {
		coord = NewCoordination()
	}
	if coord.Owners == nil || coord.Gates == nil {
		return nil, errors.New("sessions: invalid coordination")
	}
	limit := d.MaxPendingStarts
	if limit <= 0 {
		limit = 128
	}
	if limit > 4096 {
		return nil, errors.New("sessions: max pending starts exceeds 4096")
	}
	b := &Broker{deps: d, startSlots: make(chan struct{}, limit), shutdownTimeout: 10 * time.Second, lazyCleanupWait: 5 * time.Second, plans: make(map[string]brokerPlan), sessions: make(map[string]*brokerSession), retired: make(map[string]retiredSession), devices: make(map[deviceKey]frontend.SessionCapabilities), owners: coord.Owners, gates: coord.Gates, bindings: NewBindingTable[frontend.PlayIntent]()}
	if d.DB != nil && d.Cluster != nil {
		var err error
		b.cluster, err = startBrokerCluster(b, d.DB, d.Cluster)
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}
func (b *Broker) viewer(ctx context.Context, v *core.Viewer) (*core.Viewer, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	active, err := b.deps.ActiveViewer(ctx, v.ID)
	if err != nil || active == nil || active.ID != v.ID {
		return nil, core.ErrUnauthorized
	}
	return active, nil
}
func (b *Broker) permitted(v *core.Viewer, caps core.ClientCaps) bool {
	return b.deps.CanPlay == nil || b.deps.CanPlay(v, caps.Remote, b.deps.Now())
}
func (b *Broker) publish(ctx context.Context, event frontend.Event) {
	if err := b.deps.Events.Publish(ctx, event); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
}
func clientKey(protocol frontend.Protocol, device core.Device) string {
	return fmt.Sprintf("%d:%s%s", len(protocol), protocol, device.ID)
}
func copyRequest(r frontend.PlanRequest) frontend.PlanRequest {
	if r.Intent.AllowTranscoding != nil {
		allowed := *r.Intent.AllowTranscoding
		r.Intent.AllowTranscoding = &allowed
	}
	r.Caps.VideoCodecs = slices.Clone(r.Caps.VideoCodecs)
	r.Caps.AudioCodecs = slices.Clone(r.Caps.AudioCodecs)
	r.Caps.Containers = slices.Clone(r.Caps.Containers)
	r.Caps.HLSContainers = slices.Clone(r.Caps.HLSContainers)
	r.Caps.SubtitleCodecs = slices.Clone(r.Caps.SubtitleCodecs)
	r.Caps.DVProfiles = slices.Clone(r.Caps.DVProfiles)
	r.Caps.VideoLevels = maps.Clone(r.Caps.VideoLevels)
	r.Caps.VideoProfiles = maps.Clone(r.Caps.VideoProfiles)
	for k, v := range r.Caps.VideoProfiles {
		r.Caps.VideoProfiles[k] = slices.Clone(v)
	}
	r.Caps.Profiles = cloneClientProfiles(r.Caps.Profiles)
	r.Intent.Options.AudioFilters = slices.Clone(r.Intent.Options.AudioFilters)
	if r.Intent.Plex != nil {
		p := *r.Intent.Plex
		if p.SubtitleStream != nil {
			s := copyStream(*p.SubtitleStream)
			p.SubtitleStream = &s
		}
		r.Intent.Plex = &p
	}
	return r
}
func copyStream(stream core.StreamInfo) core.StreamInfo {
	for _, field := range []**bool{&stream.DefaultDisposition, &stream.ForcedDisposition, &stream.HearingImpaired, &stream.Original, &stream.VisualImpaired, &stream.Commentary, &stream.Dub, &stream.HDR10Plus, &stream.DolbyVisionBLPresent, &stream.DolbyVisionELPresent, &stream.DolbyVisionRPUPresent, &stream.ProbedInterlaced, &stream.DolbyVisionConfigurationPresent, &stream.DolbyVisionBLCompatibilityIDPresent} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	for _, field := range []**int{&stream.NALLengthSize, &stream.ReferenceFrames, &stream.BitsPerRawSample, &stream.ProbedLevel, &stream.ProbedDolbyVisionProfile, &stream.ProbedDolbyVisionLevel} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	for _, field := range []**float64{&stream.AverageFrameRate, &stream.ReferenceFrameRate} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	return stream
}
func publicMedia(info core.MediaInfo) core.MediaInfo {
	info.Streams = slices.Clone(info.Streams)
	for i := range info.Streams {
		info.Streams[i] = copyStream(info.Streams[i])
		info.Streams[i].ExternalURL = ""
	}
	info.Chapters = slices.Clone(info.Chapters)
	info.Keyframes = slices.Clone(info.Keyframes)
	return info
}
func (b *Broker) Plan(ctx context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	req = copyRequest(req)
	v, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if req.Protocol == "" || req.Device.ID == "" || (req.Item == 0) == (req.Channel == "") {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	// Plex-specific version and stream identifiers remain on the existing Plex
	// path until its adapter can translate them into neutral selections.
	if req.Intent.Plex != nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	viewerCopy := *v
	req.Viewer = &viewerCopy
	req.Caps.Protocol = req.Protocol
	req.Caps.ClientID = clientKey(req.Protocol, req.Device)
	applySubtitleProfiles(&req.Caps)
	if req.Caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (req.Caps.MaxBitrateKbps == 0 || limit < req.Caps.MaxBitrateKbps) {
			req.Caps.MaxBitrateKbps = limit
		}
	}
	if !b.permitted(v, req.Caps) {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	if b.deps.BitrateLimit != nil {
		if limit := b.deps.BitrateLimit(v); limit > 0 {
			if req.Caps.MaxBitrateKbps == 0 || limit < req.Caps.MaxBitrateKbps {
				req.Caps.MaxBitrateKbps = limit
			}
			if req.Intent.Options.MaxBitrateKbps == 0 || limit < req.Intent.Options.MaxBitrateKbps {
				req.Intent.Options.MaxBitrateKbps = limit
			}
		}
	}
	if req.Channel != "" {
		if req.Version != "" || req.Intent.ExactVersion {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		return b.planLive(ctx, req, v)
	}
	title, err := b.deps.Lookup(ctx, v, req.Device, req.Item)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if title == nil || title.Key != req.Item {
		return frontend.PlayPlan{}, core.ErrNotFound
	}
	if !title.Released.IsZero() && title.Released.After(b.deps.Now()) {
		return frontend.PlayPlan{}, core.ErrUnreleased
	}
	if req.Intent.Estimate && req.Intent.ExactVersion {
		info := b.deps.Playback.Estimate(ctx, title, req.Caps)
		dec := core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: info.Container}
		if b.deps.Transcoder != nil {
			dec = b.deps.Transcoder.Decide(info, req.Caps, req.Intent.Options)
		}
		return frontend.PlayPlan{Estimated: true, Media: publicMedia(info), Decision: dec}, nil
	}
	versionHandle := ""
	if req.Intent.ExactVersion {
		if req.Version == "" || b.deps.DB == nil {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		h := &versionHandles{db: b.deps.DB, broker: b, protocol: req.Protocol}
		item, release, err := h.resolveRecord(ctx, frontend.VersionScope{Viewer: v, Device: req.Device, Protocol: req.Protocol, Caps: req.Caps}, req.Version)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		if item != req.Item {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
		versionHandle, req.Version = normalizeVersionHandle(req.Version), release
	}
	var chosen *core.ReleaseVariant
	if req.Version != "" {
		if b.deps.Variants == nil {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		variants, err := b.deps.Variants(ctx, v, title, req.Caps)
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		for j := range variants {
			if variants[j].ID == req.Version {
				chosen = &variants[j]
				break
			}
		}
		if chosen == nil {
			return frontend.PlayPlan{}, core.ErrNotFound
		}
	}
	if req.Intent.Estimate {
		var info core.MediaInfo
		if chosen != nil {
			info = chosen.Info
		} else {
			info = b.deps.Playback.Estimate(ctx, title, req.Caps)
		}
		dec := core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: info.Container}
		if b.deps.Transcoder != nil {
			dec = b.deps.Transcoder.Decide(info, req.Caps, req.Intent.Options)
		}
		return frontend.PlayPlan{Estimated: true, Media: publicMedia(info), Decision: dec}, nil
	}
	b.mu.Lock()
	for key, p := range b.plans {
		if b.deps.Now().Sub(p.created) > 2*time.Minute {
			delete(b.plans, key)
		}
	}
	if b.closed || len(b.plans)+b.pendingPlans >= 1024 {
		b.mu.Unlock()
		return frontend.PlayPlan{}, core.ErrStreamLimit
	}
	b.pendingPlans++
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.pendingPlans--; b.mu.Unlock() }()
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", v.ID, req.Caps.ClientID))
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	defer unlock()
	previous, existed := b.deps.Playback.SessionFor(v.ID, req.Caps.ClientID, req.Item)
	var ps *core.PlaySession
	if chosen != nil && req.Intent.ExactVersion {
		vp, ok := b.deps.Playback.(core.ExactVariantPlayback)
		if !ok {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		ps, err = vp.PrepareExactVariant(ctx, v, req.Item, req.Caps, chosen.ID)
	} else if chosen != nil {
		vp, ok := b.deps.Playback.(core.VariantPlayback)
		if !ok {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		ps, err = vp.PrepareVariant(ctx, v, req.Item, req.Caps, chosen.ID)
	} else {
		ps, err = b.deps.Playback.Prepare(ctx, v, req.Item, req.Caps)
	}
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if ps == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if req.Intent.ExactVersion && (ps.Resolution == nil || ps.Resolution.Release.ID != req.Version) {
		if !existed || previous == nil || previous.ID != ps.ID {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	existed = existed && previous != nil && previous.ID == ps.ID
	if b.deps.SelectScopedTracks != nil || b.deps.SelectTracks != nil {
		var selected core.DecideOptions
		var err error
		if b.deps.SelectScopedTracks != nil {
			selected, err = b.deps.SelectScopedTracks(ctx, v, req.Device, title, ps.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
		} else {
			selected, err = b.deps.SelectTracks(ctx, v, title, ps.Info, req.Intent.Options, req.Intent.AudioSet, req.Intent.SubtitleSet)
		}
		if err != nil {
			if !existed {
				b.deps.Playback.Stop(ps.ID)
			}
			return frontend.PlayPlan{}, err
		}
		req.Intent.Options = selected
	}
	dec, err := b.decideProfile(ps, req)
	if err != nil {
		if !existed {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, err
	}
	if req.Intent.AllowTranscoding != nil && !*req.Intent.AllowTranscoding && decisionEncodes(dec, ps.Info) {
		if !existed {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if (dec.Kind == core.DecisionDirectPlay && !req.Intent.Options.DirectPlay) || (dec.Kind == core.DecisionRemux && !req.Intent.Options.DirectStream) {
		if !existed {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	id := rand.Text()
	etag := sourceRevision(ps)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		if !existed {
			b.deps.Playback.Stop(ps.ID)
		}
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	for key, p := range b.plans {
		if b.deps.Now().Sub(p.created) > 2*time.Minute {
			delete(b.plans, key)
		}
	}
	exactRelease := ""
	if req.Intent.ExactVersion {
		exactRelease = req.Version
	}
	b.plans[id] = brokerPlan{request: req, source: ps, decision: dec, created: b.deps.Now(), etag: etag, exactRelease: exactRelease, versionHandle: versionHandle}
	b.mu.Unlock()
	direct, remux, transcoding := b.planAlternatives(ps, req)
	return frontend.PlayPlan{ID: id, Media: publicMedia(ps.Info), Decision: dec, DirectPlay: direct, DirectStream: remux, Transcoding: transcoding, ETag: etag}, nil
}
func (b *Broker) Open(ctx context.Context, req frontend.OpenRequest) (frontend.Session, error) {
	v, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.Session{}, err
	}
	b.mu.Lock()
	p, ok := b.plans[req.PlanID]
	if b.closed {
		ok = false
	}
	if ok && (p.request.Viewer.ID != v.ID || p.request.Protocol != req.Protocol || req.Device.ID == "" || p.request.Device.ID != req.Device.ID) {
		ok = false
	}
	if ok {
		delete(b.plans, req.PlanID)
	}
	b.mu.Unlock()
	if !ok || b.deps.Now().Sub(p.created) > 2*time.Minute || !b.permitted(v, p.request.Caps) {
		return frontend.Session{}, core.ErrUnauthorized
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", v.ID, p.request.Caps.ClientID))
	if err != nil {
		return frontend.Session{}, err
	}
	defer unlock()
	v, err = b.viewer(ctx, req.Viewer)
	if err != nil || !b.permitted(v, p.request.Caps) {
		return frontend.Session{}, core.ErrUnauthorized
	}
	if limit := b.deps.BitrateLimit(v); limit > 0 && (p.request.Caps.MaxBitrateKbps == 0 || p.request.Caps.MaxBitrateKbps > limit) {
		return frontend.Session{}, core.ErrUnauthorized
	}
	if p.request.Caps.Remote && b.deps.RemoteBitrateLimit != nil {
		if limit := b.deps.RemoteBitrateLimit(); limit > 0 && (p.request.Caps.MaxBitrateKbps == 0 || p.request.Caps.MaxBitrateKbps > limit) {
			return frontend.Session{}, core.ErrUnauthorized
		}
	}
	if p.live {
		return b.openLive(ctx, req, p, v)
	}
	title, err := b.deps.Lookup(ctx, v, req.Device, p.request.Item)
	if err != nil {
		return frontend.Session{}, err
	}
	if title == nil || title.Key != p.request.Item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		return frontend.Session{}, core.ErrNotFound
	}
	ps, ok := b.deps.Playback.Session(p.source.ID)
	if !ok || ps == nil || ps.UserID != v.ID || ps.ClientID != p.request.Caps.ClientID || ps.Key != p.request.Item {
		return frontend.Session{}, core.ErrNotAvailable
	}
	if p.exactRelease != "" && (ps.Resolution == nil || ps.Resolution.Release.ID != p.exactRelease) {
		return frontend.Session{}, core.ErrNotAvailable
	}
	// A plan's tag must not describe a source that failed over before Open.
	// Ask the frontend to re-plan instead of handing it a stale revision.
	if p.etag != "" && sourceRevision(ps) != p.etag {
		return frontend.Session{}, core.ErrNotAvailable
	}
	id, err := sessionID(req.Protocol)
	if err != nil {
		return frontend.Session{}, err
	}
	grant := rand.Text()
	playbackID := p.request.Intent.PlaybackID
	if playbackID == "" && req.Protocol != "plex" {
		playbackID = id
	}
	session := frontend.Session{ID: id, MediaPath: "/sessions/media/" + id + "/" + grant + "/file", Protocol: req.Protocol, ViewerID: v.ID, Device: p.request.Device, Item: p.request.Item, StartedAt: b.deps.Now()}
	if err := b.enrichCapabilities(ctx, &session); err != nil {
		return frontend.Session{}, err
	}
	s := &brokerSession{public: copySession(session), grant: grant, playID: ps.ID, clientID: p.request.Caps.ClientID, playbackID: playbackID, caps: p.request.Caps, opts: p.request.Intent.Options, decision: p.decision, allowTranscoding: p.request.Intent.AllowTranscoding, versionHandle: p.versionHandle, exactRelease: p.exactRelease}
	if b.cluster != nil && session.Protocol != "plex" {
		if err := b.cluster.store.put(ctx, session, ps.ID, *v, b.deps.Now()); err != nil {
			return frontend.Session{}, err
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		if b.cluster != nil {
			_ = b.cluster.store.remove(ctx, session.ID)
		}
		return frontend.Session{}, core.ErrNotAvailable
	}
	b.bindings.Put(&Binding[frontend.PlayIntent]{ID: id, UserID: v.ID, ClientID: s.clientID, Key: session.Item, PlayID: ps.ID, PlaybackID: s.playbackID, Requested: p.request.Intent, Decision: p.decision, Info: ps.Info, Caps: s.caps, Opts: s.opts, UpdatedAt: b.deps.Now()})
	b.owners.BindFor("broker", v.ID, s.clientID, session.Item, s.playbackID, ps.ID, true, b.deps.Now())
	superseded := b.collectSupersededLocked(s)
	b.sessions[id] = s
	b.mu.Unlock()
	b.finishSuperseded(ctx, superseded, ps.ID)
	if b.cluster != nil {
		b.cluster.ownershipChanged(ctx)
	}
	b.publish(ctx, sessionEvent("SessionStarted", session, nil))
	return session, nil
}
func (b *Broker) access(ctx context.Context, a frontend.SessionAccess, id string) (*brokerSession, *core.Viewer, error) {
	return b.accessSession(ctx, a, id, true)
}

func (b *Broker) accessSession(ctx context.Context, a frontend.SessionAccess, id string, enforcePlaybackPolicy bool) (*brokerSession, *core.Viewer, error) {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, nil, core.ErrNotAvailable
	}
	s, ok := b.sessions[id]
	if ok {
		copy := *s
		s = &copy
	}
	b.mu.Unlock()
	if !ok {
		return nil, nil, core.ErrNotFound
	}
	owner := a.Owner && viewer.IsOwner && !viewer.Delegated
	if !owner && (s.public.ViewerID != viewer.ID || s.public.Protocol != a.Protocol || a.Device.ID == "" || s.public.Device.ID != a.Device.ID) {
		return nil, nil, core.ErrUnauthorized
	}
	if a.Owner && !owner {
		return nil, nil, core.ErrUnauthorized
	}
	if enforcePlaybackPolicy && !b.permitted(viewer, s.caps) && !owner {
		return nil, nil, core.ErrUnauthorized
	}
	return s, viewer, nil
}
func (b *Broker) Events(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	return b.deps.Events.Subscribe(ctx, scope)
}
func (b *Broker) Recover(ctx context.Context, id, url string, at time.Duration) error {
	return Recover(ctx, b.bindings, b.deps.Playback, id, url, at)
}

// HasBinding reports whether this broker owns an encoder ID.
func (b *Broker) HasBinding(id string) bool { _, ok := b.bindings.Get(id); return ok }
func (b *Broker) TranscodeFor(_ context.Context, id string) (core.HLSSession, bool) {
	return TranscodeFor(b.bindings, b.deps.Transcoder, id)
}
func (b *Broker) Sessions(ctx context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return nil, err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if a.Owner && !owner {
		return nil, core.ErrUnauthorized
	}
	b.mu.Lock()
	var out []frontend.Session
	for _, s := range b.sessions {
		if owner || s.public.ViewerID == v.ID {
			out = append(out, s.public)
		}
	}
	b.mu.Unlock()
	if b.cluster != nil {
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range remote {
			if owner || r.session.ViewerID == v.ID {
				out = append(out, r.session)
			}
		}
	}
	visible := out[:0]
	for i := range out {
		if b.cluster != nil && out[i].Protocol != "plex" {
			active, err := b.cluster.store.current(ctx, out[i].ID)
			if err != nil {
				return nil, err
			}
			if !active {
				continue
			}
		}
		if err := b.enrichCapabilities(ctx, &out[i]); err != nil {
			return nil, err
		}
		visible = append(visible, out[i])
	}
	return visible, nil
}
func (b *Broker) Report(ctx context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	s, v, err := b.accessSession(ctx, a, id, t.State != "stopped")
	if errors.Is(err, core.ErrNotFound) {
		retired, retiredErr := b.retiredAccess(ctx, a, id)
		if retiredErr != nil {
			return retiredErr
		}
		if retired {
			return nil
		}
	}
	if errors.Is(err, core.ErrNotFound) && b.cluster != nil {
		return b.cluster.forwardReport(ctx, a, id, t)
	}
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	s, v, err = b.accessSession(ctx, a, id, t.State != "stopped")
	if err != nil {
		return err
	}
	if v.ID != s.public.ViewerID {
		v, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
		if err != nil || v == nil {
			return core.ErrUnauthorized
		}
	}
	// A revoked session may still stop and release resources, but cannot write
	// further progress after its title or channel becomes inaccessible.
	if !b.currentMediaPolicy(ctx, v, s) {
		if t.State == "stopped" {
			return b.stopLocked(ctx, s)
		}
		return core.ErrUnauthorized
	}
	if b.cluster != nil && s.public.Protocol != "plex" {
		active, err := b.cluster.store.active(ctx, s.public.ID)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
	}
	accepted, rollbackSequence, err := b.acceptSequence(ctx, s, t.Sequence)
	if err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	if err := b.enrichCapabilities(ctx, &s.public); err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
	if !s.live {
		reportedID := t.PlaybackID
		if reportedID == s.public.ID {
			reportedID = s.playbackID
		}
		if b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, reportedID) {
			rollbackSequence()
			if b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
				return b.stopLocked(ctx, s)
			}
			return nil
		}
	}
	if s.live {
		b.publish(ctx, sessionEvent("SessionUpdated", s.public, &t))
		if t.State == "stopped" {
			return b.stopLocked(ctx, s)
		}
		return nil
	}
	if t.Duration <= 0 {
		if source, ok := b.deps.Playback.Session(s.playID); ok && source != nil && source.Info.Duration > 0 {
			t.Duration = source.Info.Duration
		}
	}
	// Keep active broker ownership alive when Plex's shared v39 history
	// expires idle entries. Never refresh a superseded binding.
	if !b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
		b.owners.BindFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID, s.playID, false, b.deps.Now())
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.public.Protocol, SessionID: s.public.ID, ClientID: s.public.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, s.public.ViewerID, s.public.Item, t.Position, t.Duration, t.State); err != nil {
		rollbackSequence()
		return err
	}
	if toucher, ok := b.deps.Playback.(interface{ Touch(string) }); ok && (t.State == "playing" || t.State == "paused") {
		toucher.Touch(s.playID)
	}
	NotifyTranscodeActivity(b.deps.Transcoder, s.public.ID, t.State)
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(s.public.ViewerID, s.clientID, s.public.Item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(v, s.public.Item, t.Position, t.Duration, t.State)
	}
	b.publish(ctx, sessionEvent("SessionUpdated", s.public, &t))
	if t.State == "stopped" {
		return b.stopLocked(ctx, s)
	}
	return nil
}
func (b *Broker) Stop(ctx context.Context, a frontend.SessionAccess, id string) error {
	s, _, err := b.accessSession(ctx, a, id, false)
	if errors.Is(err, core.ErrNotFound) {
		if b.cluster == nil {
			return nil
		}
		v, err := b.viewer(ctx, a.Viewer)
		if err != nil {
			return err
		}
		owner := a.Owner && v.IsOwner && !v.Delegated
		if a.Owner && !owner {
			return core.ErrUnauthorized
		}
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			return err
		}
		for _, r := range remote {
			if r.session.ID != id {
				continue
			}
			if !owner && (r.session.ViewerID != v.ID || r.session.Protocol != a.Protocol || a.Device.ID == "" || r.session.Device.ID != a.Device.ID) {
				return core.ErrUnauthorized
			}
			payload, err := json.Marshal(remoteStop{Node: r.node, ID: id, ActorID: v.ID, DeviceID: a.Device.ID, Protocol: a.Protocol, Owner: owner})
			if err != nil {
				return err
			}
			return b.cluster.node.Publish(ctx, brokerStopTopic, payload)
		}
		return nil
	}
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	s, _, err = b.accessSession(ctx, a, id, false)
	if err != nil {
		return err
	}
	return b.stopLocked(ctx, s)
}
func (b *Broker) stopLocked(ctx context.Context, s *brokerSession) error {
	b.mu.Lock()
	_, ok := b.sessions[s.public.ID]
	if ok {
		delete(b.sessions, s.public.ID)
		b.rememberRetiredLocked(s)
	}
	b.mu.Unlock()
	if !ok {
		return nil
	}
	if b.cluster != nil {
		if err := b.cluster.store.remove(ctx, s.public.ID); err != nil && b.deps.OnEventError != nil {
			b.deps.OnEventError(err)
		}
		b.cluster.announceRetired(ctx, s)
	}
	b.bindings.Remove(s.public.ID)
	if s.live {
		err := b.deps.Live.Stop(ctx, &core.Viewer{ID: s.public.ViewerID}, s.playID)
		b.publish(ctx, sessionEvent("SessionStopped", s.public, nil))
		return err
	}
	if b.deps.Transcoder != nil {
		b.deps.Transcoder.Stop(s.public.ID)
	}
	if !b.owners.StaleFor("broker", s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
		b.deps.Playback.Stop(s.playID)
	}
	b.publish(ctx, sessionEvent("SessionStopped", s.public, nil))
	return nil
}

// StopPlayback stops every local broker session fed by a source ID.
func (b *Broker) StopPlayback(ctx context.Context, playID string) bool {
	b.mu.Lock()
	var entries []brokerSession
	for _, s := range b.sessions {
		if s.playID == playID {
			entries = append(entries, *s)
		}
	}
	b.mu.Unlock()
	stopped := true
	for i := range entries {
		s := &entries[i]
		unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
		if err == nil {
			cmd := frontend.Command{Kind: frontend.CommandStop}
			b.publish(ctx, frontend.Event{Type: "SessionCommand", ViewerID: s.public.ViewerID, SessionID: s.public.ID, TargetProtocol: s.public.Protocol, TargetDeviceID: s.public.Device.ID, Command: &cmd})
			_ = b.stopLocked(ctx, s)
			unlock()
		} else {
			stopped = false
		}
	}
	if len(entries) == 0 && b.cluster != nil {
		remote, err := b.cluster.remote(ctx)
		if err != nil {
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(err)
			}
			return false
		}
		for _, r := range remote {
			if r.sourceID != "" && r.sourceID == playID {
				payload, err := json.Marshal(remoteStop{Node: r.node, SourceID: playID})
				if err == nil {
					err = b.cluster.node.Publish(ctx, brokerStopTopic, payload)
				}
				if err != nil && b.deps.OnEventError != nil {
					b.deps.OnEventError(err)
				}
				return err == nil
			}
		}
	}
	return len(entries) > 0 && stopped
}

// Close stops local broker sessions and prevents new plans or opens.
func (b *Broker) Close(ctx context.Context) {
	b.mu.Lock()
	if b.closed && len(b.sessions) == 0 && len(b.plans) == 0 {
		b.mu.Unlock()
		return
	}
	firstClose := !b.closed
	b.closed = true
	lazyCancel, lazyDone := b.lazyCleanupCancel, b.lazyCleanupDone
	var ids, planned []string
	seen := make(map[string]bool)
	for _, s := range b.sessions {
		if !seen[s.playID] {
			ids = append(ids, s.playID)
			seen[s.playID] = true
		}
	}
	for _, p := range b.plans {
		if p.source != nil {
			planned = append(planned, p.source.ID)
		}
	}
	b.plans = make(map[string]brokerPlan)
	b.mu.Unlock()
	// Shutdown must still release sessions when its caller has already been
	// canceled; bound the cleanup so an unresponsive gate cannot block forever.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.shutdownTimeout)
	defer cancel()
	if lazyCancel != nil {
		lazyCancel()
	}
	if lazyDone != nil {
		select {
		case <-lazyDone:
		case <-cleanupCtx.Done():
			if b.deps.OnEventError != nil {
				b.deps.OnEventError(fmt.Errorf("sessions: lazy cleanup worker did not stop during shutdown"))
			}
		}
	}
	for _, id := range ids {
		if !b.StopPlayback(cleanupCtx, id) && b.deps.OnEventError != nil {
			b.deps.OnEventError(fmt.Errorf("sessions: could not stop playback %s during shutdown", id))
		}
	}
	for _, id := range planned {
		b.deps.Playback.Stop(id)
	}
	if firstClose && b.cluster != nil {
		b.cluster.close()
	}
}
