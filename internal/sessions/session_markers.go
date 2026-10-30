package sessions

import (
	"context"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Markers never borrows title-only or duration-only marker caches. It requires
// a complete provider result for the session's exact current source revision.
func (b *Broker) Markers(ctx context.Context, access frontend.SessionAccess, id string) (frontend.SessionMarkers, error) {
	s, viewer, err := b.access(ctx, access, id)
	if err != nil {
		return frontend.SessionMarkers{}, err
	}
	if b.cluster != nil && s.public.Protocol != "plex" {
		active, err := b.cluster.store.active(ctx, s.public.ID)
		if err != nil {
			return frontend.SessionMarkers{}, err
		}
		if !active {
			return frontend.SessionMarkers{}, core.ErrNotFound
		}
	}
	if s.live || b.deps.SourceMarkers == nil {
		return frontend.SessionMarkers{}, nil
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil || ps.UserID != s.public.ViewerID || ps.Key != s.public.Item || ps.Info.Estimated {
		return frontend.SessionMarkers{}, nil
	}
	revision := sourceRevision(ps)
	if revision == "" {
		return frontend.SessionMarkers{}, nil
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
		if err != nil || viewer == nil {
			return frontend.SessionMarkers{}, core.ErrUnauthorized
		}
	}
	ranges, known, err := b.deps.SourceMarkers(ctx, viewer, ps.Key, revision, publicMedia(ps.Info))
	if err != nil {
		return frontend.SessionMarkers{}, err
	}
	if !known {
		return frontend.SessionMarkers{}, nil
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return frontend.SessionMarkers{}, err
	}
	defer unlock()
	// Failover during lookup must not attach old markers to the new source.
	current, ok := b.deps.Playback.Session(s.playID)
	if !ok || sourceRevision(current) != revision {
		return frontend.SessionMarkers{}, nil
	}
	if _, _, err := b.access(ctx, access, id); err != nil {
		return frontend.SessionMarkers{}, err
	}
	if b.cluster != nil && s.public.Protocol != "plex" {
		active, err := b.cluster.store.active(ctx, s.public.ID)
		if err != nil {
			return frontend.SessionMarkers{}, err
		}
		if !active {
			return frontend.SessionMarkers{}, core.ErrNotFound
		}
	}
	if len(ranges) > 0 && ps.Info.Duration <= 0 {
		return frontend.SessionMarkers{}, core.ErrNotAvailable
	}
	out := frontend.SessionMarkers{Known: true, SourceETag: revision, Markers: make([]frontend.Marker, 0, len(ranges))}
	for _, m := range ranges {
		if m.Type == "" || m.Start < 0 || m.End <= m.Start || (ps.Info.Duration > 0 && m.End > ps.Info.Duration) {
			return frontend.SessionMarkers{}, core.ErrNotAvailable
		}
		out.Markers = append(out.Markers, m)
	}
	return out, nil
}
