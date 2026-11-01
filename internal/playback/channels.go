package playback

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// PrepareChannel is an internal playout operation. One viewing session may
// overlap at most three programme sources for continuity. The server and user
// limits count the group once, and ordinary Prepare cannot create groups.
func (m *Manager) PrepareChannel(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, group string) (*core.PlaySession, error) {
	if u == nil {
		return nil, core.ErrUnauthorized
	}
	if len(group) < 16 || len(group) > 128 || strings.ContainsAny(group, "/\\\x00\r\n") {
		return nil, fmt.Errorf("invalid channel session")
	}
	id := identity{userID: u.ID, clientID: caps.ClientID, key: key, surface: playbackSurface(string(caps.Protocol))}
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
	ps, err := m.Prepare(ctx, u, key, caps)
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
	units := map[string]int64{}
	for g, slot := range m.externalGroups {
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
	return
}

type externalGroup struct {
	user core.User
	caps core.ClientCaps
}

// BeginChannel reserves a viewing unit in the same transaction as ordinary
// Prepare. This also counts passthrough IPTV, which has no on-demand source.
func (m *Manager) BeginChannel(u *core.User, caps core.ClientCaps, group string) error {
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
	userN, total, _ := m.reservationCountsLocked(id)
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
	m.externalGroups[group] = externalGroup{user: *u, caps: caps}
	m.mu.Unlock()
	if m.reg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	claim := unitRow{id: groupID(group), user: u.ID, client: caps.ClientID, grp: group, kind: unitGroup,
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
