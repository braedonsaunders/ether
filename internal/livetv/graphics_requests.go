package livetv

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"sort"
	"strings"
	"time"
)

// GraphicsRequests is a background-only planning snapshot. Empty lookID limits
// it to watched/clock channels, with at least three minutes of looks and fifteen minutes
// of plugin segments. Near boundaries sort first across all channels so a long
// future rundown cannot fill the render queue ahead of an imminent element.
// A lookID requests static clips for every channel using that saved look.
func (s *Service) GraphicsRequests(ctx context.Context, lookID string) []core.ClipRequest {
	cs, e := s.virtualChannels(ctx)
	if e != nil {
		return nil
	}
	recent := s.recentlyWatched()
	now := s.o.Now()
	type scheduled struct {
		at      time.Time
		request core.ClipRequest
	}
	var pending []scheduled
	for _, c := range cs {
		if (c.Look == nil && len(c.Clock) == 0 && len(c.Blocks) == 0 && len(c.Pins) == 0) || !c.Enabled {
			continue
		}
		if lookID != "" {
			if c.Look == nil || c.Look.LookID != lookID {
				continue
			}
		} else if !recent[c.ID] && len(c.Clock) == 0 {
			continue
		}
		c = s.graphicsChannel(c)
		channelLook := ""
		if c.Look != nil {
			channelLook = c.Look.LookID
		}
		lead := 3 * time.Minute
		renderer, _ := s.o.Graphics.(interface {
			RenderLead(string, string) time.Duration
		})
		if renderer != nil {
			lead = max(lead, renderer.RenderLead(channelLook, ""))
		}
		horizon := max(lead, 15*time.Minute)
		for _, m := range []string{"ident", "offair", "break"} {
			if c.Look == nil {
				continue
			}
			pending = append(pending, scheduled{now, s.clipRequest(c, Programme{Item: Item{Slate: m}, Start: now, End: now})})
		}
		if lookID == "" {
			snapshot := Schedule(c, now, now.Add(horizon+24*time.Hour))
			for _, p := range snapshot {
				momentLead := 3 * time.Minute
				if renderer != nil {
					momentLead = max(momentLead, renderer.RenderLead(channelLook, p.Item.Slate))
				}
				if strings.HasPrefix(p.Item.Slate, "plugin:") {
					momentLead = max(momentLead, 15*time.Minute)
				}
				if p.Start.After(now.Add(momentLead)) {
					continue
				}
				if interstitial(p.Item) || p.Item.Slate == offAirSlate {
					pending = append(pending, scheduled{p.Start, s.clipRequest(c, p, snapshot)})
				}
			}
		}
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].at.Before(pending[j].at) })
	out := make([]core.ClipRequest, 0, len(pending))
	for _, p := range pending {
		request := p.request
		request.Boundary = p.at
		out = append(out, request)
	}
	return out
}
