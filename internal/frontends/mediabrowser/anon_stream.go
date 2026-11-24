package mediabrowser

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// anonSessionTTL bounds how long a credential-less stream may ride a session
// opened through PlaybackInfo. It is deliberately short: the index exists to
// carry the window between PlaybackInfo and the client's first ranged GET of
// the direct file, not to keep an anonymous grant alive for a whole movie.
const anonSessionTTL = 3 * time.Minute

// anonSessionCap bounds the in-memory index so a burst of PlaybackInfo calls
// cannot grow it without limit. Entries are evicted oldest-first.
const anonSessionCap = 1024

// anonEntry records a playback session an authenticated viewer opened, so a
// following credential-less stream request from the same peer for the same
// item can be served as that viewer. The viewer is stored by value: the
// session already belongs to it, and holding a pointer would share mutable
// state across requests.
type anonEntry struct {
	sessionID string
	peerAddr  netip.Addr
	item      frontend.RatingKey
	channel   string
	version   string
	detailSrc string
	// sourceID is the MediaSource Id PlaybackInfo returned (an Auto switch
	// returns a release handle the client then names as mediaSourceId).
	sourceID string
	viewer   frontend.Viewer
	device   frontend.Device
	// plan is the PlaybackInfo plan request the session was opened from. A
	// session superseded by a later PlaybackInfo on the same device is
	// reopened from it when the client streams this item again.
	plan *frontend.PlanRequest
	// superseded is set once the same viewer and device opened a different
	// item from this peer. Findroid posts PlaybackInfo for every queued
	// episode before it reads the one it plays; the broker keeps one session
	// per device, so those later opens retire the playing title's session.
	superseded bool
	// served is when a stream was last served from this entry.
	served  time.Time
	expires time.Time
	seq     uint64
}

// anonIndex is a bounded, per-instance map from (peer, item) to the most
// recent session opened for that item from that peer.
type anonIndex struct {
	mu      sync.Mutex
	entries map[anonKey][]anonEntry
	seq     uint64
	// reopen serialises reopening superseded sessions, so the parallel
	// ranged GETs of one player open one session, not one each.
	reopen sync.Mutex
}

type anonKey struct {
	peer netip.Addr
	item frontend.RatingKey
}

func newAnonIndex() *anonIndex {
	return &anonIndex{entries: map[anonKey][]anonEntry{}}
}

// remember stores one session, replacing any earlier entry for the same peer
// and item so a superseded session never wins. The index is pruned first.
func (x *anonIndex) remember(e anonEntry) {
	if !e.peerAddr.IsValid() || (e.item == 0 && e.channel == "") {
		return
	}
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.pruneLocked(now)
	x.seq++
	e.seq = x.seq
	e.expires = now.Add(anonSessionTTL)
	key := anonKey{peer: e.peerAddr, item: e.item}
	for other, list := range x.entries {
		// A deferred queue entry opened nothing, so it retired nothing.
		if e.sessionID == "" || other.peer != e.peerAddr || other.item == e.item {
			continue
		}
		for idx := range list {
			if sameAnonDevice(list[idx], e) {
				list[idx].superseded = true
			}
		}
	}
	kept := x.entries[key][:0]
	for _, existing := range x.entries[key] {
		if existing.channel == e.channel && existing.version == e.version && existing.detailSrc == e.detailSrc {
			continue
		}
		kept = append(kept, existing)
	}
	x.entries[key] = append(kept, e)
	x.trimLocked()
}

func sameAnonDevice(a, b anonEntry) bool {
	return a.viewer.ID == b.viewer.ID && a.device.ID != "" && a.device.ID == b.device.ID
}

// touchDevice extends every window the viewer's device holds on this peer.
// An authenticated progress report proves the app is still playing from
// here, so a seek, a reconnect after a pause, or the next queued episode
// keeps streaming. Without reports the windows still lapse.
func (x *anonIndex) touchDevice(peer netip.Addr, viewerID int64, deviceID string) {
	if !peer.IsValid() || deviceID == "" {
		return
	}
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.pruneLocked(now)
	for key, list := range x.entries {
		if key.peer != peer {
			continue
		}
		for idx := range list {
			if list[idx].viewer.ID == viewerID && list[idx].device.ID == deviceID {
				list[idx].expires = now.Add(anonSessionTTL)
			}
		}
	}
}

// touch extends the window of the entry a stream was just served from.
func (x *anonIndex) touch(peer netip.Addr, item frontend.RatingKey, seq uint64) {
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	for idx, e := range x.entries[anonKey{peer: peer, item: item}] {
		if e.seq == seq && e.expires.After(now) {
			x.entries[anonKey{peer: peer, item: item}][idx].expires = now.Add(anonSessionTTL)
			x.entries[anonKey{peer: peer, item: item}][idx].served = now
		}
	}
}

// anonStreamingWindow is how recently a credential-less stream must have been
// served for its device to count as playing it.
const anonStreamingWindow = 30 * time.Second

// streamingOther reports whether the viewer's device on this peer is playing
// another item through a credential-less stream right now.
func (x *anonIndex) streamingOther(peer netip.Addr, viewerID int64, deviceID string, item frontend.RatingKey) bool {
	if !peer.IsValid() || deviceID == "" {
		return false
	}
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	for key, list := range x.entries {
		if key.peer != peer || key.item == item {
			continue
		}
		for _, e := range list {
			if e.viewer.ID == viewerID && e.device.ID == deviceID && !e.superseded && e.sessionID != "" && now.Sub(e.served) < anonStreamingWindow {
				return true
			}
		}
	}
	return false
}

// forget drops a peer's windows for an item its viewer stopped, so a stopped
// title is never reopened by a credential-less request.
func (x *anonIndex) forget(peer netip.Addr, item frontend.RatingKey, viewerID int64, deviceID string) {
	if !peer.IsValid() || item == 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	key := anonKey{peer: peer, item: item}
	kept := x.entries[key][:0]
	for _, e := range x.entries[key] {
		if e.viewer.ID == viewerID && e.device.ID == deviceID {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		delete(x.entries, key)
	} else {
		x.entries[key] = kept
	}
}

// lookup returns the newest unexpired entry for a peer and item.
func (x *anonIndex) lookup(peer netip.Addr, item frontend.RatingKey) (anonEntry, bool) {
	if !peer.IsValid() || item == 0 {
		return anonEntry{}, false
	}
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.pruneLocked(now)
	live := x.entries[anonKey{peer: peer, item: item}]
	var best anonEntry
	found := false
	for _, e := range live {
		if !e.expires.After(now) {
			continue
		}
		if !found || e.seq > best.seq {
			best, found = e, true
		}
	}
	return best, found
}

func (x *anonIndex) pruneLocked(now time.Time) {
	for key, list := range x.entries {
		kept := list[:0]
		for _, e := range list {
			if e.expires.After(now) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(x.entries, key)
			continue
		}
		x.entries[key] = kept
	}
}

// trimLocked enforces the entry cap oldest-first.
func (x *anonIndex) trimLocked() {
	total := 0
	for _, list := range x.entries {
		total += len(list)
	}
	for total > anonSessionCap {
		var oldestKey anonKey
		oldestIdx, oldestSeq := -1, ^uint64(0)
		for key, list := range x.entries {
			for idx, e := range list {
				if e.seq < oldestSeq {
					oldestSeq, oldestKey, oldestIdx = e.seq, key, idx
				}
			}
		}
		if oldestIdx < 0 {
			return
		}
		list := x.entries[oldestKey]
		list = append(list[:oldestIdx], list[oldestIdx+1:]...)
		if len(list) == 0 {
			delete(x.entries, oldestKey)
		} else {
			x.entries[oldestKey] = list
		}
		total--
	}
}

// peerAddr returns the verified transport peer for a request, or an invalid
// address when the host cannot resolve one. Without a peer the anonymous
// stream path is disabled rather than binding an entry to every caller.
func (i *instance) peerAddr(r *http.Request) netip.Addr {
	if i.host.ClientIP == nil {
		return netip.Addr{}
	}
	if ip, ok := i.host.ClientIP(r); ok {
		return ip
	}
	return netip.Addr{}
}

// rememberAnonymousSession records a session an authenticated viewer just
// opened so a following credential-less stream from the same peer and item
// can be served as that viewer. Only item streams are indexed: a live
// channel is opened and read through the LiveStreams endpoints, and a
// credential-less channel request has no session to match against.
func (i *instance) rememberAnonymousSession(r *http.Request, viewer *frontend.Viewer, sessionID string, item frontend.RatingKey, channel string, device frontend.Device, version, detailSrc, sourceID string, plan frontend.PlanRequest) {
	if i.anon == nil || viewer == nil || sessionID == "" || item == 0 {
		return
	}
	peer := i.peerAddr(r)
	if !peer.IsValid() {
		return
	}
	plan.Viewer = nil
	i.anon.remember(anonEntry{
		sessionID: sessionID, peerAddr: peer, item: item, channel: channel,
		version: version, detailSrc: detailSrc, sourceID: sourceID, viewer: *viewer,
		device: device, plan: &plan,
	})
}

// deferQueuedPlayback reports whether a PlaybackInfo is a queue prefetch from
// a device that is playing another item through a credential-less stream
// (Findroid asks for the previous and next episode as it starts one). Opening
// it would retire the session that device is reading; it is answered from the
// plan alone, and its stream opens a session when the player reaches it.
func (i *instance) deferQueuedPlayback(r *http.Request, viewer *frontend.Viewer, device frontend.Device, item frontend.RatingKey) bool {
	if i.anon == nil || viewer == nil || item == 0 {
		return false
	}
	return i.anon.streamingOther(i.peerAddr(r), viewer.ID, device.ID, item)
}

// rememberQueuedPlayback records a deferred queue entry: no session yet, and
// the plan request its stream opens one from.
func (i *instance) rememberQueuedPlayback(r *http.Request, viewer *frontend.Viewer, item frontend.RatingKey, device frontend.Device, version, detailSrc, sourceID string, plan frontend.PlanRequest) {
	if i.anon == nil || viewer == nil || item == 0 {
		return
	}
	peer := i.peerAddr(r)
	if !peer.IsValid() {
		return
	}
	plan.Viewer = nil
	plan.Intent.Estimate = false
	i.anon.remember(anonEntry{
		peerAddr: peer, item: item, version: version, detailSrc: detailSrc, sourceID: sourceID,
		viewer: *viewer, device: device, plan: &plan, superseded: true,
	})
}

// noteAnonymousTimeline keeps a device's credential-less windows in step
// with its authenticated progress reports: reports extend them, and a stop
// ends the stopped item's window.
func (i *instance) noteAnonymousTimeline(r *http.Request, viewer *frontend.Viewer, device frontend.Device, item frontend.RatingKey, stopped bool) {
	if i.anon == nil || viewer == nil || device.ID == "" {
		return
	}
	peer := i.peerAddr(r)
	if stopped {
		i.anon.forget(peer, item, viewer.ID, device.ID)
		return
	}
	i.anon.touchDevice(peer, viewer.ID, device.ID)
}

// liveAnonymousSession reports the broker sessions visible to an entry and
// whether the entry's own session is among them.
func (i *instance) liveAnonymousSession(r *http.Request, entry anonEntry) (frontend.SessionAccess, []frontend.Session, bool) {
	viewer := entry.viewer
	access := frontend.SessionAccess{Viewer: &viewer, Device: entry.device, Protocol: i.protocol}
	if access.Device.ID == "" {
		return access, nil, false
	}
	sessions, err := i.host.Sessions.Sessions(r.Context(), access)
	if err != nil {
		return access, nil, false
	}
	for _, session := range sessions {
		if session.ID == entry.sessionID && session.Item == entry.item && session.Channel == entry.channel {
			return access, sessions, true
		}
	}
	return access, sessions, false
}

// reopenAnonymous replans and reopens a session that a later PlaybackInfo on
// the same device superseded, as the viewer that opened it. A stopped or
// expired session is never reopened: its entry is gone or not superseded.
func (i *instance) reopenAnonymous(r *http.Request, peer netip.Addr, key frontend.RatingKey) (anonEntry, frontend.SessionAccess, []frontend.Session, bool) {
	i.anon.reopen.Lock()
	defer i.anon.reopen.Unlock()
	entry, ok := i.anon.lookup(peer, key)
	if !ok {
		return anonEntry{}, frontend.SessionAccess{}, nil, false
	}
	// Another request of the same player may have reopened it meanwhile.
	if access, sessions, live := i.liveAnonymousSession(r, entry); live {
		return entry, access, sessions, true
	}
	if !entry.superseded || entry.plan == nil {
		return anonEntry{}, frontend.SessionAccess{}, nil, false
	}
	viewer := entry.viewer
	request := *entry.plan
	request.Viewer = &viewer
	plan, err := i.host.Sessions.Plan(r.Context(), request)
	if err != nil || plan.Estimated {
		return anonEntry{}, frontend.SessionAccess{}, nil, false
	}
	session, err := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: i.protocol, PlanID: plan.ID, Viewer: &viewer, Device: entry.device})
	if err != nil {
		return anonEntry{}, frontend.SessionAccess{}, nil, false
	}
	// As PlaybackInfo does: the direct file is served, and the session
	// listed, only once its stream has started.
	access := frontend.SessionAccess{Viewer: &viewer, Device: entry.device, Protocol: i.protocol}
	if _, err := i.host.Sessions.StartStream(r.Context(), access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect, Position: request.Intent.Position}); err != nil {
		_ = i.host.Sessions.Stop(r.Context(), access, session.ID)
		return anonEntry{}, frontend.SessionAccess{}, nil, false
	}
	if i.host.Log != nil {
		i.host.Log.Debug("reopened superseded credential-less stream", "item", key, "old", entry.sessionID, "session", session.ID)
	}
	i.rememberAnonymousSession(r, &viewer, session.ID, key, entry.channel, entry.device, entry.version, entry.detailSrc, entry.sourceID, request)
	entry, ok = i.anon.lookup(peer, key)
	if !ok || entry.sessionID != session.ID {
		return anonEntry{}, frontend.SessionAccess{}, nil, false
	}
	access, sessions, live := i.liveAnonymousSession(r, entry)
	return entry, access, sessions, live
}

// serveAnonymousStream answers a credential-less stream alias by matching a
// session an authenticated viewer recently opened from the same peer for the
// same item. It reports whether it wrote a response; the caller keeps the
// 401 when it did not.
func (i *instance) serveAnonymousStream(w http.ResponseWriter, r *http.Request, name string) bool {
	if i.host.Sessions == nil || i.anon == nil {
		return false
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		return false
	}
	format := frontend.StreamDirect
	switch {
	case name == "stream", strings.HasPrefix(name, "stream."), name == "universal":
	case name == "master.m3u8", name == "main.m3u8":
		format = frontend.StreamHLS
	default:
		return false
	}
	peer := i.peerAddr(r)
	entry, ok := i.anon.lookup(peer, key)
	if !ok {
		return false
	}
	// A mediaSourceId/version handle given on the anonymous URL must be the
	// one the session was opened for; nothing else is served.
	if sourceIDs := route.Query(r.URL.Query(), "MediaSourceId"); len(sourceIDs) > 1 {
		return false
	} else if len(sourceIDs) == 1 && sourceIDs[0] != "" && !sameItemID(sourceIDs[0], key) {
		if sourceIDs[0] != entry.sourceID && !sameVersionHandle(sourceIDs[0], entry.version) && !sameVersionHandle(sourceIDs[0], entry.detailSrc) {
			return false
		}
	}
	access, sessions, found := i.liveAnonymousSession(r, entry)
	if !found {
		if !entry.superseded {
			return false
		}
		if entry, access, sessions, found = i.reopenAnonymous(r, peer, key); !found {
			return false
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), verifiedDeviceKey{}, entry.device))
	i.anon.touch(peer, key, entry.seq)
	viewer := entry.viewer
	versionScope, err := i.versionScope(r, &viewer)
	if err != nil {
		return false
	}
	i.serveAliasSession(w, r, access, sessions, entry.sessionID, versionScope, entry.version, entry.detailSrc, format, false)
	return true
}

// serveAliasSession serves one already-verified broker session: a direct file
// from its scoped media path, or an HLS start. The version/detail handles are
// re-checked against the session so a forged handle can never select it.
func (i *instance) serveAliasSession(w http.ResponseWriter, r *http.Request, access frontend.SessionAccess, sessions []frontend.Session, sessionID string, versionScope frontend.VersionScope, version, detailSource string, format frontend.StreamFormat, explicitSession bool) {
	if version != "" && i.host.VersionHandles != nil && i.host.VersionHandles.MatchSession(i.identityContext(r), versionScope, version, sessionID) != nil {
		http.NotFound(w, r)
		return
	}
	if detailSource != "" {
		planner, ok := i.host.Sessions.(frontend.DetailPlanner)
		if !ok || planner.MatchDetailSession(i.identityContext(r), versionScope, detailSource, sessionID) != nil {
			http.NotFound(w, r)
			return
		}
	}
	serveDirect := func(mediaPath string) {
		req := r.Clone(r.Context())
		u := *req.URL
		u.Path = mediaPath
		u.RawPath = ""
		req.URL = &u
		if i.protocol == "emby" {
			i.embyMediaHeaders(w, req)
		}
		i.host.Sessions.ServeMedia(w, req)
	}
	// PlaybackInfo already started this stream. Reuse its direct file or HLS
	// master instead of calling StartStream again at position 0 (which would
	// tear down and restart a resumed HLS stream from the beginning).
	if format == frontend.StreamDirect {
		for _, session := range sessions {
			if session.ID != sessionID || !directFilePath(session.MediaPath) {
				continue
			}
			serveDirect(session.MediaPath)
			return
		}
	} else if format == frontend.StreamHLS {
		for _, session := range sessions {
			if session.ID != sessionID || !strings.HasPrefix(session.MediaPath, "/sessions/media/") || !strings.HasSuffix(session.MediaPath, "/master.m3u8") || strings.ContainsAny(session.MediaPath, "\r\n") {
				continue
			}
			url := clientnet.RedirectPath(requestRedirectPrefix(r, i.host.Origin), "", session.MediaPath)
			http.Redirect(w, r, url, http.StatusTemporaryRedirect)
			return
		}
	}
	fullTimeline := i.protocol == "jellyfin" && format == frontend.StreamHLS
	url, err := i.host.Sessions.StartStream(r.Context(), access, sessionID, frontend.StreamRequest{Format: format, FullTimeline: fullTimeline})
	if err != nil {
		streamError(i.protocol, w, err)
		return
	}
	if !strings.HasPrefix(url, "/sessions/media/") || strings.ContainsAny(url, "\r\n") {
		http.Error(w, "Unavailable", 503)
		return
	}
	if !explicitSession && format == frontend.StreamDirect && directFilePath(url) {
		serveDirect(url)
		return
	}
	url = clientnet.RedirectPath(requestRedirectPrefix(r, i.host.Origin), "", url)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// sameItemID reports whether a mediaSourceId names the item itself, in
// either GUID form: the Jellyfin Kotlin SDK (Findroid) puts the dashed form
// in the path and the bare form in mediaSourceId.
func sameItemID(sourceID string, key frontend.RatingKey) bool {
	parsed, err := ids.ParseRatingKeyGUID(sourceID)
	return err == nil && parsed == key
}
