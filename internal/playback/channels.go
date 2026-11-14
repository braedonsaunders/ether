package playback

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// PrepareChannel is an internal playout operation. One viewing session may
// overlap at most three programme sources for continuity. The server and user
// limits count the group once, and ordinary Prepare cannot create groups.
func (m *Manager) PrepareChannel(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, group string) (*core.PlaySession, error) {
	return m.prepareChannelRelease(ctx, u, key, caps, group, "")
}

// PrepareChannelRelease binds a planned act-break timeline to its analyzed
// release while retaining the ordinary channel group admission accounting.
func (m *Manager) PrepareChannelRelease(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, group, release string) (*core.PlaySession, error) {
	return m.prepareChannelRelease(ctx, u, key, caps, group, release)
}
func (m *Manager) prepareChannelRelease(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, group, release string) (*core.PlaySession, error) {
	if u == nil {
		return nil, core.ErrUnauthorized
	}
	if len(group) < 16 || len(group) > 128 || strings.ContainsAny(group, "/\\\x00\r\n") {
		return nil, fmt.Errorf("invalid channel session")
	}
	id := identity{userID: u.ID, clientID: caps.ClientID, key: key, surface: m.playbackSurface(string(caps.Protocol))}
	m.mu.Lock()
	if m.channelGroups == nil {
		m.channelGroups = map[identity]string{}
	}
	if current := m.channelGroups[id]; current != "" && current != group {
		m.mu.Unlock()
		return nil, fmt.Errorf("programme source belongs to another channel session")
	}
	m.channelGroups[id] = group
	m.mu.Unlock()
	var ps *core.PlaySession
	var err error
	if release != "" {
		ps, err = m.PrepareSurface(ctx, u, key, caps, release, true)
	} else {
		ps, err = m.Prepare(ctx, u, key, caps)
	}
	if err != nil {
		m.mu.Lock()
		if m.byIdent[id] == nil && !m.hasFlightLocked(id) {
			delete(m.channelGroups, id)
		}
		m.mu.Unlock()
	}
	return ps, err
}

// reservationCountsLocked counts distinct viewing groups and ordinary sources.
func (m *Manager) reservationCountsLocked(id identity) (user, total, children int) {
	group := m.channelGroups[id]
	joining := strings.HasPrefix(id.clientID, "join/")
	units := map[string]int64{}
	broadcastUnits := map[string]bool{}
	viewerUnits := map[string]bool{}
	for g, slot := range m.externalGroups {
		if slot.broadcast != "" {
			if slot.broadcast != group {
				broadcastUnits[slot.broadcast] = true
			}
			if slot.user.ID == id.userID && (joining || group == "") {
				key := slot.caps.ClientID
				if key == "" {
					key = g
				}
				if !joining || key != strings.TrimPrefix(id.clientID, "join/") {
					viewerUnits[key] = true
				}
			}
			continue
		}
		if g == group && slot.user.ID == id.userID {
			continue
		}
		units[fmt.Sprintf("group/%d/%s", slot.user.ID, g)] = slot.user.ID
	}
	add := func(other identity, unit string) {
		g := m.channelGroups[other]
		if group != "" && g == group && other.userID == id.userID {
			children++
			return
		}
		if strings.HasPrefix(g, "broadcast-") {
			broadcastUnits[g] = true
			return
		}
		if g != "" {
			unit = fmt.Sprintf("group/%d/%s", other.userID, g)
		} else if other.clientID != "" {
			unit = fmt.Sprintf("client/%d/%s/%s", other.userID, other.surface, other.clientID)
		}
		units[unit] = other.userID
	}
	now := m.now()
	for sid, s := range m.sessions {
		if !m.sessionCountsForLimit(s, now) {
			continue
		}
		if s.ident.userID == id.userID && s.ident.clientID == id.clientID && s.ident.surface == id.surface {
			continue
		}
		add(s.ident, "session/"+sid)
	}
	for other := range m.reservations {
		add(other, pendingID("", other))
	}
	for _, uid := range units {
		total++
		if uid == id.userID {
			user++
		}
	}
	total += len(broadcastUnits)
	user += len(viewerUnits)
	return
}

type externalGroup struct {
	broadcast string
	user      core.User
	caps      core.ClientCaps
}

// BeginChannel reserves a viewing unit in the same transaction as ordinary
// Prepare. This also counts passthrough IPTV, which has no on-demand source.
func (m *Manager) BeginChannel(u *core.User, caps core.ClientCaps, group string) error {
	return m.beginChannel(u, caps, group, "")
}

// JoinChannel keeps a viewer reservation under its session ID while sharing
// the physical output's server budget with other viewers and source decoders.
func (m *Manager) JoinChannel(u *core.User, caps core.ClientCaps, session, output string) error {
	if !strings.HasPrefix(output, "broadcast-") || len(output) > 128 {
		return fmt.Errorf("invalid broadcast group")
	}
	return m.beginChannel(u, caps, session, output)
}
func (m *Manager) beginChannel(u *core.User, caps core.ClientCaps, group, output string) error {
	if u == nil {
		return core.ErrUnauthorized
	}
	if len(group) < 16 || len(group) > 128 {
		return fmt.Errorf("invalid channel group")
	}
	cfg := m.config()
	userLimit := UserStreamLimit(cfg, u)
	serverLimit := ServerStreamLimit(cfg, m.opts.ConnectionsPerStream)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("playback is closing")
	}
	if _, ok := m.externalGroups[group]; !ok && m.draining {
		m.mu.Unlock()
		return errDraining
	}
	if slot, ok := m.externalGroups[group]; ok {
		m.mu.Unlock()
		if slot.user.ID == u.ID {
			return nil
		}
		return core.ErrUnauthorized
	}
	id := identity{userID: u.ID, clientID: "external/" + group}
	if output != "" {
		if m.channelGroups == nil {
			m.channelGroups = map[identity]string{}
		}
		id.clientID = "join/" + caps.ClientID
		m.channelGroups[id] = output
	}
	userN, total, _ := m.reservationCountsLocked(id)
	if output != "" {
		delete(m.channelGroups, id)
	}
	if n := userLimit; n > 0 && userN >= n {
		m.mu.Unlock()
		return core.ErrStreamLimit
	}
	if n := serverLimit; n > 0 && total >= n {
		m.mu.Unlock()
		return core.ErrStreamLimit
	}
	if m.externalGroups == nil {
		m.externalGroups = map[string]externalGroup{}
	}
	m.externalGroups[group] = externalGroup{user: *u, caps: caps, broadcast: output}
	m.mu.Unlock()
	if m.reg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	claimGroup := group
	if output != "" {
		claimGroup = output
	}
	claim := unitRow{id: groupID(group), user: u.ID, client: caps.ClientID, grp: claimGroup, kind: unitGroup,
		started: m.now().UnixMilli()}
	if err := m.reg.admit(ctx, u, id, claim, userN, total, userLimit, serverLimit); err != nil {
		m.mu.Lock()
		delete(m.externalGroups, group)
		m.mu.Unlock()
		m.reg.poke()
		return core.ErrStreamLimit
	}
	return nil
}
func (m *Manager) EndChannel(group string) {
	m.mu.Lock()
	delete(m.externalGroups, group)
	m.mu.Unlock()
	m.reg.poke()
}

func (m *Manager) channelSourceAllowed(s *session, r core.Release) bool {
	if s.caps.BroadcastBitrateKbps <= 0 {
		return true
	}
	if r.Info.FullSeason {
		r.Size = 0
	}
	if s.title == nil || !releasepolicy.ChannelSourceAllowed(r, s.title.Runtime, s.caps.BroadcastBitrateKbps) {
		return false
	}
	if policy, ok := m.deps.Resolver.(interface {
		ChannelSourceAllowed(core.Release, core.ClientCaps, *core.Title) bool
	}); ok {
		return policy.ChannelSourceAllowed(r, s.caps, s.title)
	}
	return true
}

// ChannelReadyRelease is a bounded, local evidence lookup for substitute races.
func (m *Manager) ChannelReadyRelease(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps) (string, bool) {
	ready, ok := m.deps.Resolver.(interface {
		ReadyChannelRelease(context.Context, core.ResolveRequest) (core.Release, bool)
	})
	if !ok {
		return "", false
	}
	stored, ok := m.deps.Catalog.(core.StoredTitleLookup)
	if !ok {
		return "", false
	}
	title, err := stored.StoredTitle(ctx, key)
	if err != nil || title == nil {
		return "", false
	}
	req := core.ResolveRequest{Title: title, Client: caps, User: u, Exclude: caps.BroadcastExclude}
	r, ok := ready.ReadyChannelRelease(ctx, req)
	if ok {
		return r.ID, true
	}
	// A recent successful play survives a restart, but is eligible only if
	// the resolver still has fresh verification and the current budget fits.
	if m.resume == nil || u == nil {
		return "", false
	}
	rec, err := m.resume.get(ctx, u.ID, key)
	if err != nil || rec == nil || rec.size <= 0 || rec.duration <= 0 || m.now().Before(rec.updatedAt) || m.now().Sub(rec.updatedAt) > 5*time.Minute || !sourceFitsBitrate(rec.size, rec.duration, 0, caps.BroadcastBitrateKbps) {
		return "", false
	}
	remembered, ok := m.deps.Resolver.(interface {
		ReadyRememberedChannelRelease(context.Context, core.ResolveRequest, core.Release) (core.Release, bool)
	})
	if !ok {
		return "", false
	}
	file := rec.release
	file.Size, file.Info.FullSeason = rec.size, false
	copyTitle := *title
	copyTitle.Runtime = rec.duration
	req.Title = &copyTitle
	r, ok = remembered.ReadyRememberedChannelRelease(ctx, req, file)
	return r.ID, ok
}
