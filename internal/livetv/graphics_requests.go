package livetv

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// GraphicsRequests is a demand-driven planning snapshot. Empty lookID limits it
// strictly to channels with an active viewer: an enabled channel nobody watches
// — clock, blocks, showcase or plain lineup — costs zero renders and zero reel
// link resolutions. Near boundaries sort first across watched channels so a
// distant element never queues ahead of an imminent boundary.
// A non-empty lookID answers explicit console look queries.
func (s *Service) GraphicsRequests(ctx context.Context, lookID string) []core.ClipRequest {
	var active map[string]bool
	if lookID == "" {
		active = s.activeWatchedChannels()
		if len(active) == 0 {
			return nil
		}
	}
	cs, e := s.virtualChannels(ctx)
	if e != nil {
		return nil
	}
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
		} else if !active[c.ID] {
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
		if lookID != "" {
			// Explicit console look queries may enumerate reusable moments.
			for _, m := range []string{"standby", "ident", "break", "back", "offair"} {
				if c.Look != nil {
					pending = append(pending, scheduled{time.Time{}, s.clipRequest(c, Programme{Item: Item{Slate: m}, Start: now, End: now})})
				}
			}
		}
		if lookID == "" {
			// Scan far enough ahead to cover the watched channel's current show
			// segment so the next commercial pod's link clips resolve ahead of time.
			podHorizon := max(horizon, 60*time.Minute)
			snapshot := Schedule(c, now, now.Add(podHorizon+24*time.Hour))

			// Identify reel clips belonging to any active pod and the single next
			// upcoming commercial pod during the current segment.
			nextPodClips := map[string]bool{}
			seenFuturePod := false
			inFuturePod := false
			for _, p := range snapshot {
				if p.End.Before(now) || p.End.Equal(now) {
					continue
				}
				if p.Start.After(now.Add(podHorizon)) {
					break
				}
				isPod := interstitial(p.Item)
				if !p.Start.After(now) && p.End.After(now) {
					if p.Item.ReelClipID != "" {
						nextPodClips[p.ID] = true
					}
					continue
				}
				if isPod {
					if !seenFuturePod || inFuturePod {
						seenFuturePod = true
						inFuturePod = true
						if p.Item.ReelClipID != "" {
							nextPodClips[p.ID] = true
						}
					}
				} else if inFuturePod {
					// First non-pod item after the upcoming pod ends that pod.
					inFuturePod = false
				}
			}

			for _, p := range snapshot {
				if p.End.Before(now) || p.End.Equal(now) {
					continue
				}
				if p.Item.ReelClipID != "" && nextPodClips[p.ID] {
					pending = append(pending, scheduled{p.Start, s.clipRequest(c, p, snapshot)})
					continue
				}
				momentLead := 3 * time.Minute
				if renderer != nil {
					momentLead = max(momentLead, renderer.RenderLead(channelLook, p.Item.Slate))
				}
				if strings.HasPrefix(p.Item.Slate, "plugin:") || p.Item.ReelClipID != "" {
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
	// Nearest non-zero boundary sorts first; zero-time reusable masters sort after
	// imminent boundaries so an active channel's upcoming cut always wins first.
	sort.SliceStable(pending, func(i, j int) bool {
		iz, jz := pending[i].at.IsZero(), pending[j].at.IsZero()
		if iz != jz {
			return !iz
		}
		return pending[i].at.Before(pending[j].at)
	})
	out := make([]core.ClipRequest, 0, len(pending))
	requestedMaster := map[string]bool{}
	for _, p := range pending {
		request := p.request
		if request.ReelClipID == "" && lookID == "" {
			// The current moment queues its own cold master. Prepare only the
			// nearest missing future master for each watched channel.
			if !p.at.After(now) || requestedMaster[request.ChannelID] {
				continue
			}
			if _, cached := s.o.Graphics.Clip(ctx, request); cached {
				continue
			}
			requestedMaster[request.ChannelID] = true
		}
		request.Boundary = p.at
		out = append(out, request)
	}
	return out
}
