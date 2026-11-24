package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// ---- bans, retiring and moving viewers between Plex servers ----

// plexServerHealth is what plex.tv's answers say about one Plex server.
type plexServerHealth struct {
	Blocked bool
	Reason  string
	// Problem is a plex.tv failure that does not show a ban by itself
	// (publishing fails, the server is not listed): shown, not acted on.
	Problem string
}

// health reads a server's registrar status ("" is the main server). Only a
// token plex.tv refuses (401/403) counts as blocked: a banned account or a
// server removed from it. Other failures are reported as problems.
func (ps *plexExtras) health(id string) plexServerHealth {
	var reg *plextv.Registrar
	if id == "" {
		reg = ps.app.registrar
	} else if rt := ps.runtime(id); rt != nil {
		reg = rt.registrar
	}
	if reg == nil {
		return plexServerHealth{}
	}
	st := reg.Status()
	switch {
	case !st.Claimed:
		return plexServerHealth{}
	case st.TokenRejected:
		return plexServerHealth{Blocked: true, Reason: "plex.tv refuses this server's token: the Plex account may be banned or the server removed from it"}
	case !st.Listed && st.LastPublishError != "":
		return plexServerHealth{Problem: "plex.tv does not list this server and publishing fails: " + st.LastPublishError}
	}
	return plexServerHealth{}
}

// retired reports whether the owner stopped new invites on a server.
func retired(cfg config.Config, id string) bool {
	if id == "" {
		return cfg.Plex.MainRetired
	}
	e, ok := cfg.PlexExtraServer(id)
	return ok && e.Retired
}

// takesInvites reports whether new viewers may go to a server: claimed,
// running, not retired and not blocked.
func (ps *plexExtras) takesInvites(cfg config.Config, id string) bool {
	if retired(cfg, id) {
		return false
	}
	if id == "" {
		if !cfg.Plex.Claimed {
			return false
		}
	} else if !cfg.Plex.ExtraServersEnabled {
		return false
	} else if e, ok := cfg.PlexExtraServer(id); !ok || !e.Active() || ps.runtime(id) == nil {
		return false
	}
	return !ps.health(id).Blocked
}

// setRetired stops (or resumes) new invites on a server.
func (ps *plexExtras) setRetired(id string, on bool) error {
	err := ps.app.cfg.Update(func(c *config.Config) error {
		if id == "" {
			c.Plex.MainRetired = on
			return nil
		}
		for i := range c.PlexExtraServers {
			if c.PlexExtraServers[i].ID == id {
				c.PlexExtraServers[i].Retired = on
				return nil
			}
		}
		return fmt.Errorf("plex server %s not found", id)
	})
	return err
}

// plexMoveResult is one viewer's move.
type plexMoveResult struct {
	UserID int64  `json:"userId"`
	Name   string `json:"name"`
	To     string `json:"to"`
	ToName string `json:"toName,omitempty"`
	// InviteURL is plex.tv's invite link when its reply carried one;
	// AcceptURL is where the viewer accepts pending invites in any case.
	InviteURL string `json:"inviteUrl,omitempty"`
	AcceptURL string `json:"acceptUrl,omitempty"`
	// RemoveLater lists the servers that stop sharing with the viewer once
	// they accept the new invite (the old share keeps working until then).
	RemoveLater []string `json:"removeLater,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// serverClient is the plex.tv client and owner token of a server.
func (ps *plexExtras) serverClient(cfg config.Config, id string) (*plextv.Client, string, error) {
	if id == "" {
		if ps.app.plextv == nil || cfg.Plex.Token == "" {
			return nil, "", errors.New("the main Plex server is not signed in")
		}
		return ps.app.plextv, cfg.Plex.Token, nil
	}
	e, ok := cfg.PlexExtraServer(id)
	rt := ps.runtime(id)
	if !ok || rt == nil || e.Token == "" {
		return nil, "", fmt.Errorf("plex server %s is not running", id)
	}
	return rt.client, e.Token, nil
}

func serverName(cfg config.Config, id string) string {
	if id == "" {
		return cfg.Server.FriendlyName
	}
	if e, ok := cfg.PlexExtraServer(id); ok {
		return e.AdvertisedName(cfg)
	}
	return id
}

// pickExcept is pick without the servers in skip.
func (ps *plexExtras) pickExcept(ctx context.Context, skip map[string]bool) (string, error) {
	cfg := ps.app.cfg.Get()
	seats := cfg.PlexSeatsPerServer()
	used := ps.seatsUsed(ctx)
	if !skip[""] && used[""] < seats && ps.takesInvites(cfg, "") {
		return "", nil
	}
	for _, s := range activePlexOrder(cfg) {
		if !skip[s.ID] && used[s.ID] < seats && ps.takesInvites(cfg, s.ID) {
			return s.ID, nil
		}
	}
	// A server added but not started stays that way: only the owner starts it.
	return "", errPlexServersFull
}

// move shares each viewer on the server to ("auto": the first with room
// that is not one of theirs) and, when removeOld, takes them off the other
// servers that share with them. Watch history, settings and sign-ins
// belong to the Ether viewer, so nothing else moves.
func (ps *plexExtras) move(ctx context.Context, userIDs []int64, to string, removeOld bool) ([]plexMoveResult, error) {
	if ps.app.users == nil {
		return nil, errors.New("viewers are not available")
	}
	cfg := ps.app.cfg.Get()
	if to != "auto" {
		if to == "main" {
			to = ""
		}
		if to != "" {
			if _, ok := cfg.PlexExtraServer(to); !ok {
				return nil, fmt.Errorf("plex server %s not found", to)
			}
		}
	}
	members, err := ps.app.users.ServerMemberships(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]plexMoveResult, 0, len(userIDs))
	for _, uid := range userIDs {
		res := plexMoveResult{UserID: uid, AcceptURL: plextv.AcceptInvitesURL}
		u, err := ps.app.users.User(ctx, uid)
		if err != nil || u == nil {
			res.Error = "viewer not found"
			out = append(out, res)
			continue
		}
		res.Name = u.Title
		if res.Name == "" {
			res.Name = u.Username
		}
		if u.IsOwner || u.PlexID == 0 {
			res.Error = "not a Plex viewer"
			out = append(out, res)
			continue
		}
		from := members[uid]
		target := to
		if target == "auto" {
			skip := map[string]bool{}
			for _, id := range from {
				skip[id] = true
			}
			if target, err = ps.pickExcept(ctx, skip); err != nil {
				res.Error = err.Error()
				out = append(out, res)
				continue
			}
		} else if !ps.takesInvites(ps.app.cfg.Get(), target) {
			res.Error = serverName(cfg, target) + " is not taking new viewers (stopped, retired or blocked)"
			out = append(out, res)
			continue
		}
		cfg = ps.app.cfg.Get()
		res.To, res.ToName = target, serverName(cfg, target)
		if target == "" {
			res.To = "main"
		}
		client, token, err := ps.serverClient(cfg, target)
		if err == nil {
			handle := u.Username
			if handle == "" {
				handle = u.Email
			}
			res.InviteURL, err = client.ShareServerInvite(ctx, token, handle, u.PlexID, nil)
		}
		if err != nil {
			res.Error = "invite on " + res.ToName + ": " + err.Error()
			out = append(out, res)
			continue
		}
		ps.noteInvited(ctx, target)
		if removeOld {
			for _, id := range from {
				if id == target {
					continue
				}
				if err := ps.app.users.AddPendingMove(ctx, uid, id, target); err != nil {
					ps.log.Warn("plex move: record pending removal", "err", err)
					continue
				}
				res.RemoveLater = append(res.RemoveLater, serverName(cfg, id))
			}
		}
		// Memberships change only when plex.tv reports them: the new one when
		// the viewer accepts (friend sync), the old one when finishMoves has
		// removed that share. The sync below recounts seats, pending invites
		// included.
		out = append(out, res)
	}
	go func() {
		if err := ps.syncAll(context.Background()); err != nil {
			ps.log.Warn("plex move: friend sync", "err", err)
		}
	}()
	return out, nil
}

// memberNames maps each viewer to the names of the Plex servers sharing
// with them, for the console's tags.
func (ps *plexExtras) memberNames(ctx context.Context) map[int64][]plexMember {
	out := map[int64][]plexMember{}
	if ps.app.users == nil {
		return out
	}
	members, err := ps.app.users.ServerMemberships(ctx)
	if err != nil {
		return out
	}
	cfg := ps.app.cfg.Get()
	for uid, ids := range members {
		for _, id := range ids {
			key := id
			if key == "" {
				key = "main"
			}
			out[uid] = append(out[uid], plexMember{ID: key, Name: strings.TrimSpace(serverName(cfg, id))})
		}
	}
	return out
}

type plexMember struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// invite shares the server with a Plex user who is not on any server yet
// (a backup import), on the first server with room.
func (ps *plexExtras) invite(ctx context.Context, handle string, plexID int64) (plexMoveResult, error) {
	res := plexMoveResult{Name: handle, AcceptURL: plextv.AcceptInvitesURL}
	target, err := ps.pick(ctx)
	if err != nil {
		return res, err
	}
	cfg := ps.app.cfg.Get()
	res.To, res.ToName = target, serverName(cfg, target)
	if target == "" {
		res.To = "main"
	}
	client, token, err := ps.serverClient(cfg, target)
	if err != nil {
		return res, err
	}
	res.InviteURL, err = client.ShareServerInvite(ctx, token, handle, plexID, nil)
	if err == nil {
		ps.noteInvited(ctx, target)
	}
	return res, err
}

// finishMoves ends the old share of every moved viewer who accepted the new
// server's invite: plex.tv then lists them on the new server. A failed
// removal stays pending and is retried after the next friend sync.
func (ps *plexExtras) finishMoves(ctx context.Context) {
	if ps.app.users == nil {
		return
	}
	ps.mu.Lock()
	if ps.finishing {
		ps.mu.Unlock()
		return
	}
	ps.finishing = true
	ps.mu.Unlock()
	defer func() {
		ps.mu.Lock()
		ps.finishing = false
		ps.mu.Unlock()
	}()
	moves, err := ps.app.users.PendingMoves(ctx)
	if err != nil || len(moves) == 0 {
		return
	}
	members, err := ps.app.users.ServerMemberships(ctx)
	if err != nil {
		return
	}
	cfg := ps.app.cfg.Get()
	for _, m := range moves {
		accepted := false
		for _, id := range members[m.UserID] {
			if id == m.To {
				accepted = true
				break
			}
		}
		if !accepted {
			continue
		}
		u, err := ps.app.users.User(ctx, m.UserID)
		if err != nil || u == nil {
			continue
		}
		client, token, err := ps.serverClient(cfg, m.From)
		if err != nil {
			// The old server is stopped or gone: its share no longer matters,
			// but its membership stays until its own sync drops it.
			continue
		}
		if err := client.UnshareServer(ctx, token, u.PlexID, u.Username); err != nil && !errors.Is(err, plextv.ErrShareNotFound) {
			ps.log.Warn("plex move: remove old share", "server", m.From, "err", err)
			continue
		}
		if err := ps.app.users.DonePendingMove(ctx, m.UserID, m.From); err != nil {
			ps.log.Warn("plex move: record removal", "err", err)
			continue
		}
		ps.log.Info("plex move finished", "user", m.UserID, "from", serverName(cfg, m.From), "to", serverName(cfg, m.To))
	}
}

// setLibrarySections publishes the libraries on every extra server too (the
// main registrar gets them from the frontend runtime); a server started
// later picks them up in build.
func (ps *plexExtras) setLibrarySections(secs []plextv.LibrarySection) {
	ps.mu.Lock()
	ps.sections = append([]plextv.LibrarySection(nil), secs...)
	rts := make([]*plexServerRuntime, 0, len(ps.running))
	for _, rt := range ps.running {
		rts = append(rts, rt)
	}
	ps.mu.Unlock()
	for _, rt := range rts {
		rt.registrar.SetLibrarySections(secs)
	}
}
