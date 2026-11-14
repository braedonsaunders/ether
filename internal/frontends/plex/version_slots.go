package plex

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func versionIdentity(v frontend.ReleaseVariant) string {
	if v.ID != "" {
		return v.ID
	}
	return v.Release.ID
}

// slotMedia is the single projection for configured menus, including hubs.
// Settings choose the identities; cache/probe state can change their labels
// and stream facts but never the shape. No release search occurs here.
func (s *Server) slotMedia(ctx context.Context, pr *Request, t *frontend.Title, versions []frontend.ReleaseVariant, info frontend.MediaInfo, ps *frontend.PlaySession, stamp int64, streams bool) []model.Media {
	ik := s.playIdentityFor(pr, t.Key)
	now := s.now()
	pin := s.playIDs.pinned(ik, ps != nil, now)
	selected := pin.selected
	if selected == 0 {
		selected = int64(t.Key)
	}
	out := make([]model.Media, 0, len(versions))
	caps := s.clientCapsForKey(pr, t.Key)
	stored := selection{}
	if streams {
		stored = s.selections.get(pr.User.ID, int64(t.Key))
	}
	for _, v := range versions {
		id := s.variantMediaID(t, v)
		if ps != nil && id == selected {
			claimed := v.Info
			v.Info = info
			if ps.Resolution != nil && (ps.Resolution.Release.Name != "" || ps.Resolution.Release.Info.Height > 0) {
				v.Release = ps.Resolution.Release
				if v.Auto {
					v.Label = probedAutoTitle(v.Release, claimed, info)
				} else {
					v.Label = strings.TrimPrefix(probedAutoTitle(v.Release, claimed, info), "Auto · ")
				}
			}
		}
		v.Info = variantInfoForClient(v.Info, caps)
		m := buildMediaVariant(t, v, stamp, s.variantIDs)
		if streams {
			m.Part[0].Key = s.playIDs.partKey(ik, m.ID, m.Part[0].Key, now)
			facts := s.withCachedSidecars(ctx, pr.User, t, v.Info)
			sel := s.preferredSelection(ctx, pr.User, t.Key, facts, t.Language, resolveSelection(stored, facts))
			m.Part[0].Stream = s.issuedStreams(t.Key, ik, m.ID, facts, sel, pr.User, pr)
		}
		if id == selected {
			m.Selected = model.Ptr(true)
			m.Part[0].Selected = model.Ptr(true)
		}
		out = append(out, m)
	}
	// A settings list is issued only by detail/queue. Hubs merely project the
	// same configured slots and do not create per-client flow rows.
	if streams {
		s.playIDs.issueConfiguredSlots(ik, versions, now)
	}
	return out
}

// Configured slots are not discovery snapshots. Their stable identities come
// from settings, while labels and current source facts may be refreshed.
func (p *playIdentities) issueConfiguredSlots(k playIdentityKey, vs []frontend.ReleaseVariant, now time.Time) {
	if !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil {
		return
	}
	e.variants = cloneVariants(vs)
	e.resolved = true
	e.at = now
}
