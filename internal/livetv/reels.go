package livetv

import (
	"context"
	"sort"
	"time"
)

// ReelSnapshot is copied from saved On Air metadata outside request/tick paths.
// Anchor is the persisted saved-revision timestamp, never the query window.
type ReelSnapshot struct {
	ID     string
	Anchor time.Time
	Order  string
	Repeat time.Duration
	Clips  []ReelClip
}
type ReelClip struct {
	ID, Name, Path string
	Duration       time.Duration
}
type momentHistory struct {
	last      map[string]time.Time
	next      int
	lastBreak time.Time
}

func (s *Service) refreshReels() {
	if s.o.Reels == nil {
		return
	}
	cs, err := s.virtualChannels(s.ctx)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, c := range cs {
		looks := []*ChannelLook{c.Look}
		for _, b := range c.Blocks {
			looks = append(looks, b.Breaks)
		}
		for _, p := range c.Pins {
			looks = append(looks, p.Breaks)
		}
		for _, look := range looks {
			if look == nil || look.ReelID == "" || seen[look.ReelID] {
				continue
			}
			id := look.ReelID
			seen[id] = true
			snapshot, err := s.o.Reels(s.ctx, id)
			if err != nil {
				continue
			}
			snapshot.Clips = append([]ReelClip(nil), snapshot.Clips...)
			s.mu.Lock()
			if s.reelSnapshots == nil {
				s.reelSnapshots = map[string]*ReelSnapshot{}
			}
			s.reelSnapshots[id] = &snapshot
			s.mu.Unlock()
		}
	}
}
func (c Channel) reelItems(at time.Time, budget time.Duration) []Item {
	if c.reel == nil || c.momentHistory == nil || at.Before(c.reel.Anchor) || len(c.reel.Clips) == 0 {
		return nil
	}
	target := time.Duration(c.Look.BreakSeconds) * time.Second
	if target <= 0 {
		target = 60 * time.Second
	}
	pool := append([]ReelClip(nil), c.reel.Clips...)
	if c.reel.Order == "shuffle" {
		sort.SliceStable(pool, func(i, j int) bool {
			return stepHash(c.Seed, int64(len(pool)), at.UnixNano()+int64Hash(pool[i].ID)) < stepHash(c.Seed, int64(len(pool)), at.UnixNano()+int64Hash(pool[j].ID))
		})
	}
	var out []Item
	var played time.Duration
	for attempts := 0; played < target && attempts < 1000; attempts++ {
		found := false
		for n := 0; n < len(pool); n++ {
			index := n
			if c.reel.Order != "shuffle" {
				index = (c.momentHistory.next + n) % len(pool)
			}
			clip := pool[index]
			if clip.Duration <= 0 {
				continue
			}
			start := at.Add(played)
			if last, ok := c.momentHistory.last[clip.ID]; ok && c.reel.Repeat > 0 && start.Sub(last) < c.reel.Repeat {
				continue
			}
			if budget > 0 && played+clip.Duration > budget {
				continue
			}
			out = append(out, Item{Slate: "reel", Title: clip.Name, DurationMS: clip.Duration.Milliseconds(), ReelID: c.reel.ID, ReelClipID: clip.ID})
			c.momentHistory.last[clip.ID] = start
			c.momentHistory.next = (index + 1) % len(pool)
			played += clip.Duration
			found = true
			break
		}
		if !found {
			break
		}
	}
	return out
}
func int64Hash(id string) int64 {
	var h uint64 = 1469598103934665603
	for i := range id {
		h = (h ^ uint64(id[i])) * 1099511628211
	}
	return int64(h)
}

// RefreshGraphics refreshes saved reel metadata after a graphics/reel revision.
// App wiring may invoke it in its existing background invalidation flow.
func (s *Service) RefreshGraphics(ctx context.Context) {
	if ctx.Err() == nil {
		select {
		case s.graphicsWake <- struct{}{}:
		default:
		}
	}
}
