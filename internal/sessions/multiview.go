package sessions

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	multiviewGroupTTL  = 30 * time.Minute
	maxMultiviewGroups = 4096
)

type multiviewGroup struct {
	id        string
	viewerID  int64
	protocol  frontend.Protocol
	deviceID  string
	clientID  string
	createdAt time.Time
	expiresAt time.Time
}

var _ frontend.MultiviewSessions = (*Broker)(nil)

// MultiviewAvailable reports whether this broker has a live backend and can
// host per-device multiview groups.
func (b *Broker) MultiviewAvailable() bool {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	return !closed && b.deps.Live != nil
}

// CreateMultiviewGroup creates a TTL-bounded multiview group scoped to one
// viewer, protocol (app) and device.
func (b *Broker) CreateMultiviewGroup(ctx context.Context, a frontend.SessionAccess) (frontend.MultiviewGroup, error) {
	if b.deps.Live == nil {
		return frontend.MultiviewGroup{}, core.ErrNotAvailable
	}
	if a.Protocol == "" || len(a.Protocol) > 128 || a.Device.ID == "" || len(a.Device.ID) > 1024 {
		return frontend.MultiviewGroup{}, frontend.ErrInvalidMultiview
	}
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return frontend.MultiviewGroup{}, err
	}
	now := b.deps.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return frontend.MultiviewGroup{}, core.ErrNotAvailable
	}
	b.pruneExpiredMultiviewLocked(now)
	if len(b.multiviewGroups) >= maxMultiviewGroups {
		return frontend.MultiviewGroup{}, core.ErrStreamLimit
	}
	if b.multiviewGroups == nil {
		b.multiviewGroups = make(map[string]*multiviewGroup)
	}
	id := "mv_" + rand.Text()
	g := &multiviewGroup{
		id:        id,
		viewerID:  v.ID,
		protocol:  a.Protocol,
		deviceID:  a.Device.ID,
		clientID:  clientKey(a.Protocol, a.Device),
		createdAt: now,
		expiresAt: now.Add(multiviewGroupTTL),
	}
	b.multiviewGroups[id] = g
	return frontend.MultiviewGroup{
		GroupID:   id,
		MaxTiles:  frontend.MaxMultiviewTiles,
		ExpiresAt: g.expiresAt,
	}, nil
}

// DeleteMultiviewGroup removes the group and ends every active tile in it.
func (b *Broker) DeleteMultiviewGroup(ctx context.Context, a frontend.SessionAccess, groupID string) error {
	if groupID == "" || a.Protocol == "" {
		return frontend.ErrInvalidMultiview
	}
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if a.Owner && !owner {
		return core.ErrUnauthorized
	}
	now := b.deps.Now()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return core.ErrNotAvailable
	}
	b.pruneExpiredMultiviewLocked(now)
	g := b.multiviewGroups[groupID]
	if g == nil {
		b.mu.Unlock()
		return core.ErrNotFound
	}
	if !owner && (g.viewerID != v.ID || g.protocol != a.Protocol || a.Device.ID == "" || g.deviceID != a.Device.ID) {
		b.mu.Unlock()
		return core.ErrUnauthorized
	}
	ownerID, clientID := g.viewerID, g.clientID
	b.mu.Unlock()

	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", ownerID, clientID))
	if err != nil {
		return err
	}
	defer unlock()

	b.mu.Lock()
	g = b.multiviewGroups[groupID]
	if g == nil {
		b.mu.Unlock()
		return core.ErrNotFound
	}
	delete(b.multiviewGroups, groupID)
	b.pruneNonGroupEpochsLocked()
	for planID, p := range b.plans {
		if p.request.Multiview != nil && p.request.Multiview.GroupID == groupID {
			delete(b.plans, planID)
		}
	}
	var removed []*brokerSession
	for id, s := range b.sessions {
		if s.multiviewGroup == groupID {
			delete(b.sessions, id)
			b.rememberRetiredLocked(s)
			removed = append(removed, s)
		}
	}
	b.mu.Unlock()

	b.finishSuperseded(ctx, removed, "")
	if b.cluster != nil && len(removed) > 0 {
		b.cluster.ownershipChanged(ctx)
	}
	return nil
}

// Caller holds b.mu.
func (b *Broker) pruneExpiredMultiviewLocked(now time.Time) {
	pruned := false
	for id, g := range b.multiviewGroups {
		if !g.expiresAt.After(now) {
			delete(b.multiviewGroups, id)
			pruned = true
		}
	}
	if pruned {
		b.pruneNonGroupEpochsLocked()
	}
}

// Caller holds b.mu.
func (b *Broker) pruneNonGroupEpochsLocked() {
	if len(b.nonGroupEpoch) == 0 {
		return
	}
	if len(b.multiviewGroups) == 0 {
		b.nonGroupEpoch = nil
		return
	}
	active := make(map[string]struct{}, len(b.multiviewGroups))
	for _, g := range b.multiviewGroups {
		active[ownerGateKey(g.viewerID, g.clientID)] = struct{}{}
	}
	for k := range b.nonGroupEpoch {
		if _, ok := active[k]; !ok {
			delete(b.nonGroupEpoch, k)
		}
	}
}

// Caller holds b.mu.
func (b *Broker) touchMultiviewLocked(groupID string, now time.Time) {
	if groupID == "" {
		return
	}
	if g := b.multiviewGroups[groupID]; g != nil && g.expiresAt.After(now) {
		g.expiresAt = now.Add(multiviewGroupTTL)
	}
}

// Caller holds b.mu. Validates group ownership, tile bounds, and TTL,
// refreshes the group TTL, and returns the resulting active tile count.
func (b *Broker) validateMultiviewLocked(viewerID int64, protocol frontend.Protocol, device core.Device, mv *frontend.MultiviewTarget, now time.Time) (*multiviewGroup, int, error) {
	if mv == nil || mv.GroupID == "" || mv.Tile < 0 || mv.Tile >= frontend.MaxMultiviewTiles || mv.TileCount < 0 || mv.TileCount > frontend.MaxMultiviewTiles || mv.MaxHeight < 0 || mv.MaxHeight > 16384 {
		return nil, 0, frontend.ErrInvalidMultiview
	}
	g := b.multiviewGroups[mv.GroupID]
	if g == nil {
		return nil, 0, &frontend.MultiviewError{Cause: core.ErrNotFound}
	}
	if !g.expiresAt.After(now) {
		delete(b.multiviewGroups, mv.GroupID)
		b.pruneNonGroupEpochsLocked()
		return nil, 0, &frontend.MultiviewError{Cause: core.ErrNotFound}
	}
	if g.viewerID != viewerID || g.protocol != protocol || device.ID == "" || g.deviceID != device.ID {
		return nil, 0, &frontend.MultiviewError{Cause: core.ErrUnauthorized}
	}
	g.expiresAt = now.Add(multiviewGroupTTL)
	tiles := 1
	for _, existing := range b.sessions {
		if existing.multiviewGroup == g.id && existing.multiviewTile != mv.Tile {
			tiles++
		}
	}
	if mv.TileCount > tiles {
		tiles = mv.TileCount
	}
	if tiles > frontend.MaxMultiviewTiles {
		tiles = frontend.MaxMultiviewTiles
	}
	return g, tiles, nil
}

// roundMultiviewHeight rounds a positive on-screen tile pixel height up to
// the nearest standard rung (480, 720, 1080, 2160).
func roundMultiviewHeight(h int) int {
	switch {
	case h <= 0:
		return 0
	case h <= 480:
		return 480
	case h <= 720:
		return 720
	case h <= 1080:
		return 1080
	default:
		return 2160
	}
}

// multiviewTileMaxHeight returns the height hint for a multiview tile:
// when declaredTileHeight > 0, it rounds up to 480/720/1080/2160; when absent,
// 2 tiles (or fewer) get 1080p and 3-4 tiles get 720p. Never exceeds the
// device's own declared MaxHeight.
func multiviewTileMaxHeight(deviceMaxHeight, tileCount, declaredTileHeight int) int {
	var hint int
	switch {
	case declaredTileHeight > 0:
		hint = roundMultiviewHeight(declaredTileHeight)
	case tileCount >= 3:
		hint = 720
	default:
		hint = 1080
	}
	if deviceMaxHeight > 0 && (hint == 0 || deviceMaxHeight < hint) {
		return deviceMaxHeight
	}
	return hint
}

// replanLive re-tunes an active live session (such as upgrading a focused
// multiview tile) with a revised MaxHeight cap while preserving the broker
// session identity and sibling multiview tiles. The old output continues
// playing until the replacement is ready, and if the source cannot go higher
// the existing output and URL are kept unchanged. Caller holds the tile or
// owner gate.
func (b *Broker) replanLive(ctx context.Context, viewer *core.Viewer, snap *brokerSession, requestedMaxHeight int) (string, error) {
	if b.deps.Live == nil || snap == nil || !snap.live || snap.directLive != nil || snap.public.Channel == "" {
		return "", core.ErrNotAvailable
	}
	if requestedMaxHeight < 0 || requestedMaxHeight > 16384 {
		return "", frontend.ErrInvalidMultiview
	}
	nextHeight := requestedMaxHeight
	if nextHeight == 0 {
		nextHeight = snap.baseMaxHeight
	} else {
		if snap.multiviewGroup != "" {
			nextHeight = roundMultiviewHeight(nextHeight)
		}
		if snap.baseMaxHeight > 0 && snap.baseMaxHeight < nextHeight {
			nextHeight = snap.baseMaxHeight
		}
	}
	oldPath := "/sessions/media/" + snap.public.ID + "/" + snap.grant + "/live/index.m3u8"
	upgrading := requestedMaxHeight == 0 || (snap.caps.MaxHeight > 0 && (nextHeight == 0 || nextHeight > snap.caps.MaxHeight))
	if nextHeight == snap.caps.MaxHeight || (upgrading && snap.knownVideoHeight > 0 && snap.caps.MaxHeight > 0 && snap.knownVideoHeight < snap.caps.MaxHeight) {
		b.mu.Lock()
		b.touchMultiviewLocked(snap.multiviewGroup, b.deps.Now())
		b.mu.Unlock()
		return oldPath, nil
	}

	nextCaps := snap.caps
	nextCaps.MaxHeight = nextHeight
	nextCaps.ExplicitMaxHeight = nextHeight > 0

	liveCtx := core.WithLiveHandover(core.WithLiveApp(ctx, string(snap.public.Protocol)))
	if err := b.deps.Live.Allowed(liveCtx, viewer, snap.public.Channel); err != nil {
		return "", err
	}
	var newLiveID string
	var tunedMedia *core.MediaInfo
	var err error
	if mb, ok := b.deps.Live.(LiveMediaBackend); ok {
		newLiveID, tunedMedia, err = mb.TuneMedia(liveCtx, viewer, snap.public.Channel, nextCaps, snap.public.ClientSessionID)
	} else {
		newLiveID, err = b.deps.Live.Tune(liveCtx, viewer, snap.public.Channel, nextCaps, snap.public.ClientSessionID)
	}
	if err != nil {
		return "", capacityError(err)
	}

	newHeight := liveVideoHeight(tunedMedia)
	if snap.knownVideoHeight > 0 && newHeight > 0 {
		noUpgrade := upgrading && newHeight <= snap.knownVideoHeight
		noDowngrade := !upgrading && nextHeight > 0 && (snap.caps.MaxHeight == 0 || nextHeight < snap.caps.MaxHeight) && newHeight >= snap.knownVideoHeight
		if noUpgrade || noDowngrade {
			_ = b.deps.Live.Stop(liveCtx, viewer, newLiveID)
			b.mu.Lock()
			b.touchMultiviewLocked(snap.multiviewGroup, b.deps.Now())
			b.mu.Unlock()
			return oldPath, nil
		}
	}

	liveMedia := normalizeLiveMedia(tunedMedia, livePlanContainer(nextCaps))
	newGrant := rand.Text()
	newMediaPath := "/sessions/media/" + snap.public.ID + "/" + newGrant + "/live/index.m3u8"
	now := b.deps.Now()

	b.mu.Lock()
	live := b.sessions[snap.public.ID]
	if b.closed || live == nil || live.playID != snap.playID {
		b.mu.Unlock()
		_ = b.deps.Live.Stop(liveCtx, viewer, newLiveID)
		return "", core.ErrNotFound
	}
	oldPlayID := live.playID
	live.playID = newLiveID
	live.grant = newGrant
	live.caps = nextCaps
	live.opts.MaxHeight = nextHeight
	live.knownVideoHeight = newHeight
	live.public.MediaPath = newMediaPath
	live.public.Media = &liveMedia
	if live.public.Multiview != nil {
		mv := *live.public.Multiview
		mv.MaxHeight = nextHeight
		live.public.Multiview = &mv
	}
	b.touchMultiviewLocked(live.multiviewGroup, now)
	updatedPublic := copySession(live.public)
	b.mu.Unlock()

	if oldPlayID != "" && oldPlayID != newLiveID {
		_ = b.deps.Live.Stop(liveCtx, viewer, oldPlayID)
	}
	if b.cluster != nil && !b.policy(updatedPublic.Protocol).ExternalAdmission {
		if err := b.cluster.store.updateSource(ctx, updatedPublic, newLiveID, *viewer, now); err != nil && b.deps.OnEventError != nil {
			b.deps.OnEventError(err)
		}
	}
	return newMediaPath, nil
}
