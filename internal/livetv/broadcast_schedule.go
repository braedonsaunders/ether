package livetv

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/faillog"
	"strings"
	"time"
)

// Missing clips are decided once per airing and shared by every viewer of the
// node-local broadcast. Guide reads apply recorded decisions without cache I/O.
func (s *Service) broadcastSchedule(ctx context.Context, c Channel, from, until time.Time, check bool) []Programme {
	c = s.graphicsChannel(c)
	if (c.Look == nil && len(c.Clock) == 0 && len(c.Blocks) == 0 && len(c.Pins) == 0) || s.o.Graphics == nil {
		return Schedule(c, from, until)
	}
	start, end := from, until
	if c.Look != nil && c.Look.Placement == "insert" {
		start = dayStart(from, zoneOf(c.Timezone))
		end = until.Add(24 * time.Hour)
	}
	ps := Schedule(c, start, end)
	var out []Programme
	var advance time.Duration
	for _, p := range ps {
		p.graphicsID = p.ID
		s.mu.Lock()
		skip := s.graphicsSkips[c.ID+"/"+p.ID]
		s.mu.Unlock()
		if skip.IsZero() && check && interstitial(p.Item) && p.End.Add(-advance).After(from) && p.Start.Add(-advance).Before(until) {
			request := s.clipRequest(c, p, ps)
			p.graphicsRequest = &request
			ready := false
			if p.Item.ReelClipID == "" {
				if live, ok := s.o.Graphics.(liveStaticSource); ok {
					ready = live.LiveStatic(ctx, request)
				}
				if !ready {
					_, ready = s.o.Graphics.Clip(ctx, request)
				}
			}
			if p.Item.ReelClipID != "" {
				ready = false
				if reel := c.reelForItem(p.Item); reel != nil {
					for _, clip := range reel.Clips {
						if clip.ID == p.Item.ReelClipID && clip.Path != "" {
							ready = true
							break
						}
					}
				}
			}
			if !ready {
				if p.Item.ReelClipID == "" {
					s.o.Graphics.Ensure(ctx, request)
				}
				// Upcoming elements are render jobs, not skipped airings.
				if !p.Start.Add(-advance).After(from) {
					s.recordGraphicsSkip(c, p, fmt.Errorf("clip not cached"))
					skip = p.End
				}
			}
		}
		if !skip.IsZero() {
			if c.Look != nil && c.Look.Placement == "insert" && !strings.HasPrefix(p.Item.Slate, "plugin:") {
				advance += p.End.Sub(p.Start)
				continue
			}
			plain := c
			plain.Look = nil
			plain.Clock = nil
			plain.clipDuration = nil
			plain.reel = nil
			plain.programmingReels = nil
			if strings.HasPrefix(p.Item.Slate, "plugin:") {
				fillGap(&out, plain, c.Filler, p.Start, p.End, from, until)
				continue
			}
			for _, fallback := range Schedule(plain, p.Start, p.End) {
				a, b := p.Start, p.End
				if fallback.Start.After(a) {
					a = fallback.Start
				}
				if fallback.End.Before(b) {
					b = fallback.End
				}
				appendProgramme(&out, plain, fallback.Item, a, b, from, until)
			}
			continue
		}
		p.Start = p.Start.Add(-advance)
		p.End = p.End.Add(-advance)
		if advance > 0 {
			p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
		}
		if p.End.After(from) && p.Start.Before(until) {
			out = append(out, p)
		}
	}
	return out
}
func (s *Service) broadcastGuide(c Channel, from, until time.Time) []Programme {
	if c.Look == nil || c.Look.Placement != "insert" || s.o.Graphics == nil {
		return guideSchedule(c, from, until)
	}
	ps := s.broadcastSchedule(context.Background(), c, dayStart(from, zoneOf(c.Timezone)), until.Add(24*time.Hour), false)
	var out []Programme
	for _, p := range ps {
		if interstitial(p.Item) {
			if len(out) > 0 {
				out[len(out)-1].End = p.End
			}
			continue
		}
		if len(out) > 0 && p.Item.Key != 0 && out[len(out)-1].Item.Key == p.Item.Key && p.sourceOffset > 0 {
			out[len(out)-1].End = p.End
			continue
		}
		out = append(out, p)
	}
	var visible []Programme
	for _, p := range out {
		if p.End.After(from) && p.Start.Before(until) {
			visible = append(visible, p)
		}
	}
	return visible
}

// recordGraphicsSkip is shared by cache preflight and decoder failures (a master
// can be evicted or disappear between preflight and prepare). The airing's
// original schedule ID survives inserted advances, so every viewer agrees.
func (s *Service) recordGraphicsSkip(c Channel, p Programme, err error) {
	id := p.graphicsID
	if id == "" {
		id = p.ID
	}
	s.mu.Lock()
	if s.graphicsSkips == nil {
		s.graphicsSkips = map[string]time.Time{}
	}
	key := c.ID + "/" + id
	first := s.graphicsSkips[key].IsZero()
	s.graphicsSkips[key] = p.End
	for k, end := range s.graphicsSkips {
		if end.Before(s.o.Now().Add(-48 * time.Hour)) {
			delete(s.graphicsSkips, k)
		}
	}
	s.mu.Unlock()
	if first {
		faillog.New(s.o.Logger, "livetv: interstitial skipped").Fail(err, "programme", id, "moment", p.Item.Slate)
		if c.Look != nil && c.Look.Placement == "insert" {
			s.bumpLineup()
		}
	}
}
