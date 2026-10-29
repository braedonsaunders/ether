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
	DB                 *sql.DB
	Cluster            clusterbus.Node
	Live               LiveBackend
	Lookup             func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error)
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
	request  frontend.PlanRequest
	source   *core.PlaySession
	decision core.Decision
	created  time.Time
	live     bool
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
}

// Broker coordinates neutral sessions with the shared playback admission manager.
// Plex's legacy timeline tracker is not yet included in Sessions; owner
// Activity continues to use the shared playback manager until that move lands.
type Broker struct {
	deps         BrokerDeps
	mu           sync.Mutex
	plans        map[string]brokerPlan
	sessions     map[string]*brokerSession
	owners       *PlaybackOwners
	gates        *Lifecycle
	bindings     *BindingTable[frontend.PlayIntent]
	closed       bool
	pendingPlans int
	cluster      *brokerCluster
}

func NewBroker(d BrokerDeps) (*Broker, error) {
	if d.Lookup == nil || d.Playback == nil || d.ActiveViewer == nil || d.Events == nil || d.CanPlay == nil || d.BitrateLimit == nil || d.Progress == nil || d.OnTimeline == nil {
		return nil, errors.New("sessions: missing required dependency")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	b := &Broker{deps: d, plans: make(map[string]brokerPlan), sessions: make(map[string]*brokerSession), owners: NewPlaybackOwners(), gates: NewLifecycle(), bindings: NewBindingTable[frontend.PlayIntent]()}
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
			s := *p.SubtitleStream
			p.SubtitleStream = &s
		}
		r.Intent.Plex = &p
	}
	return r
}
func publicMedia(info core.MediaInfo) core.MediaInfo {
	info.Streams = slices.Clone(info.Streams)
	for i := range info.Streams {
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
	if req.Intent.Estimate {
		info := b.deps.Playback.Estimate(ctx, title, req.Caps)
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
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("owner/%d/%s", v.ID, req.Caps.ClientID))
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	defer unlock()
	_, existed := b.deps.Playback.SessionFor(v.ID, req.Caps.ClientID, req.Item)
	ps, err := b.deps.Playback.Prepare(ctx, v, req.Item, req.Caps)
	if err != nil {
		return frontend.PlayPlan{}, err
	}
	if ps == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
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
	b.plans[id] = brokerPlan{request: req, source: ps, decision: dec, created: b.deps.Now()}
	b.mu.Unlock()
	return frontend.PlayPlan{ID: id, Media: publicMedia(ps.Info), Decision: dec}, nil
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
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("owner/%d/%s", v.ID, p.request.Caps.ClientID))
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
	id, grant := rand.Text(), rand.Text()
	playbackID := p.request.Intent.PlaybackID
	if playbackID == "" && req.Protocol != "plex" {
		playbackID = id
	}
	session := frontend.Session{ID: id, MediaPath: "/sessions/media/" + id + "/" + grant + "/file", Protocol: req.Protocol, ViewerID: v.ID, Device: p.request.Device, Item: p.request.Item, StartedAt: b.deps.Now()}
	s := &brokerSession{public: session, grant: grant, playID: ps.ID, clientID: p.request.Caps.ClientID, playbackID: playbackID, caps: p.request.Caps, opts: p.request.Intent.Options, decision: p.decision, allowTranscoding: p.request.Intent.AllowTranscoding}
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
	b.owners.Bind(v.ID, s.clientID, session.Item, s.playbackID, ps.ID, true, b.deps.Now())
	b.sessions[id] = s
	b.mu.Unlock()
	b.publish(ctx, frontend.Event{Type: "SessionStarted", ViewerID: v.ID, Item: session.Item, SessionID: id})
	return session, nil
}
func (b *Broker) access(ctx context.Context, a frontend.SessionAccess, id string) (*brokerSession, *core.Viewer, error) {
	viewer, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
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
	if !b.permitted(viewer, s.caps) && !owner {
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
	return out, nil
}
func (b *Broker) Report(ctx context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	s, v, err := b.access(ctx, a, id)
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	s, v, err = b.access(ctx, a, id)
	if err != nil {
		return err
	}
	if v.ID != s.public.ViewerID {
		v, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
		if err != nil || v == nil {
			return core.ErrUnauthorized
		}
	}
	if s.live {
		b.publish(ctx, frontend.Event{Type: "SessionUpdated", ViewerID: s.public.ViewerID, SessionID: id})
		if t.State == "stopped" {
			return b.stopLocked(ctx, s)
		}
		return nil
	}
	reportedID := t.PlaybackID
	if reportedID == s.public.ID {
		reportedID = s.playbackID
	}
	if b.owners.Stale(s.public.ViewerID, s.clientID, s.public.Item, reportedID) {
		if b.owners.Stale(s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
			return b.stopLocked(ctx, s)
		}
		return nil
	}
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.public.Protocol, SessionID: s.public.ID, ClientID: s.public.Device.ID})
	if err := RecordProgress(ctx, b.deps.Progress, s.public.ViewerID, s.public.Item, t.Position, t.Duration, t.State); err != nil {
		return err
	}
	if toucher, ok := b.deps.Playback.(interface{ Touch(string) }); ok && (t.State == "playing" || t.State == "paused") {
		toucher.Touch(s.playID)
	}
	if progress, ok := b.deps.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(s.public.ViewerID, s.clientID, s.public.Item, t.Position, t.State)
	}
	if b.deps.OnTimeline != nil {
		b.deps.OnTimeline(v, s.public.Item, t.Position, t.Duration, t.State)
	}
	b.publish(ctx, frontend.Event{Type: "SessionUpdated", ViewerID: s.public.ViewerID, Item: s.public.Item, SessionID: id})
	if t.State == "stopped" {
		return b.stopLocked(ctx, s)
	}
	return nil
}
func (b *Broker) Stop(ctx context.Context, a frontend.SessionAccess, id string) error {
	s, _, err := b.access(ctx, a, id)
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
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	s, _, err = b.access(ctx, a, id)
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
	}
	b.mu.Unlock()
	if !ok {
		return nil
	}
	if b.cluster != nil {
		if err := b.cluster.store.remove(ctx, s.public.ID); err != nil && b.deps.OnEventError != nil {
			b.deps.OnEventError(err)
		}
	}
	b.bindings.Remove(s.public.ID)
	if s.live {
		err := b.deps.Live.Stop(ctx, &core.Viewer{ID: s.public.ViewerID}, s.playID)
		b.publish(ctx, frontend.Event{Type: "SessionStopped", ViewerID: s.public.ViewerID, SessionID: s.public.ID})
		return err
	}
	if b.deps.Transcoder != nil {
		b.deps.Transcoder.Stop(s.public.ID)
	}
	if !b.owners.Stale(s.public.ViewerID, s.clientID, s.public.Item, s.playbackID) {
		b.deps.Playback.Stop(s.playID)
	}
	b.publish(ctx, frontend.Event{Type: "SessionStopped", ViewerID: s.public.ViewerID, Item: s.public.Item, SessionID: s.public.ID})
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
	for i := range entries {
		s := &entries[i]
		unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("owner/%d/%s", s.public.ViewerID, s.clientID))
		if err == nil {
			_ = b.stopLocked(ctx, s)
			unlock()
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
	return len(entries) > 0
}

// Close stops local broker sessions and prevents new plans or opens.
func (b *Broker) Close(ctx context.Context) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	var ids, planned []string
	for _, s := range b.sessions {
		ids = append(ids, s.playID)
	}
	for _, p := range b.plans {
		if p.source != nil {
			planned = append(planned, p.source.ID)
		}
	}
	b.plans = make(map[string]brokerPlan)
	b.mu.Unlock()
	for _, id := range ids {
		b.StopPlayback(ctx, id)
	}
	for _, id := range planned {
		b.deps.Playback.Stop(id)
	}
	if b.cluster != nil {
		b.cluster.close()
	}
}
