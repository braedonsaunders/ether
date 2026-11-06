package livetv

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/faillog"
	"time"
)

// A missing master changes only the airing's input. It never changes schedule
// spans, programme identities, or the guide. The clock latches the live fallback
// for that whole moment; a master that arrives late is available next time.
func (s *Service) broadcastSchedule(ctx context.Context, c Channel, from, until time.Time, check bool) []Programme {
	c = s.graphicsChannel(c)
	ps := Schedule(c, from, until)
	if s.o.Graphics == nil {
		return ps
	}
	for i := range ps {
		p := &ps[i]
		p.graphicsID = p.ID
		s.mu.Lock()
		p.graphicsFallback = !s.graphicsSkips[c.ID+"/"+p.ID].IsZero()
		s.mu.Unlock()
		if !check || !interstitial(p.Item) {
			continue
		}
		request := s.clipRequest(c, *p, ps)
		p.graphicsRequest = &request
		if p.graphicsFallback {
			continue
		}
		ready := false
		if p.Item.ReelClipID != "" {
			if reel := c.reelForItem(p.Item); reel != nil {
				for _, clip := range reel.Clips {
					if clip.ID == p.Item.ReelClipID && clip.Path != "" {
						ready = true
						break
					}
				}
			}
		} else {
			if live, ok := s.o.Graphics.(liveStaticSource); ok {
				ready = live.LiveStatic(ctx, request)
			}
			if !ready {
				_, ready = s.o.Graphics.Clip(ctx, request)
			}
		}
		if !ready {
			if p.Item.ReelClipID == "" {
				s.o.Graphics.Ensure(ctx, request)
			}
			if !p.Start.After(from) {
				s.recordGraphicsSkip(c, *p, fmt.Errorf("master not cached at airing boundary"))
				p.graphicsFallback = true
			}
		}
	}
	return ps
}

func (s *Service) broadcastGuide(c Channel, from, until time.Time) []Programme {
	return guideSchedule(c, from, until)
}

// recordGraphicsSkip latches a fallback decision for the original airing. A
// cache miss or decoder failure never invalidates or advances the guide.
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
		faillog.New(s.o.Logger, "livetv: interstitial using live fallback").Fail(err, "programme", id, "moment", p.Item.Slate)
	}
}
