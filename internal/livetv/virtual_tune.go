package livetv

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func (s *Service) knownUnavailable(ctx context.Context, key core.RatingKey) bool {
	if key == 0 || s.o.Availability == nil {
		return false
	}
	known, ok := s.o.Availability.Available(ctx, key)
	return known && !ok
}

// virtualCandidates starts with the guide's on-now title, then the channel's
// fallback, then the running order: a stand-in never takes a later airing
// while fallback programming exists. Known-unplayable titles are skipped by
// the caller before Prepare.
func (s *Service) virtualCandidates(c Channel, current Programme) []Programme {
	now := s.o.Now()
	// A prewarm/retry for a future or still-prewarming slot calls this before
	// its own airtime. Scheduling the lookahead window from raw "now" would
	// then start it before current.Start, pulling in whatever programme is
	// still nominally airing at that moment - stale, already-aired content -
	// in as a fallback candidate. Anchor the window to the target slot's own
	// start so every fallback is strictly at or after the programme being
	// planned, never behind it.
	start := now
	if current.Start.After(start) {
		start = current.Start
	}
	var out []Programme
	seen := map[core.RatingKey]struct{}{}
	add := func(p Programme) {
		if len(out) >= 12 || p.Item.Key == 0 || p.Item.Slate != "" || p.Item.DurationMS <= 0 {
			return
		}
		if _, ok := seen[p.Item.Key]; ok {
			return
		}
		seen[p.Item.Key] = struct{}{}
		out = append(out, p)
	}
	add(current)
	if c.Personal == nil {
		for _, item := range c.Fallback {
			add(standIn(c, item, now))
		}
	}
	for _, p := range s.viewerSchedule(s.ctx, c, start, start.Add(24*time.Hour)) {
		add(p)
	}
	if c.Personal != nil {
		return out
	}
	for _, block := range c.Blocks {
		for _, item := range block.Items {
			add(standIn(c, item, now))
		}
	}
	for _, item := range c.Items {
		add(standIn(c, item, now))
	}
	if len(out) > 2 {
		sort.SliceStable(out[1:], func(i, j int) bool {
			a, b := out[i+1], out[j+1]
			return time.Duration(a.Item.DurationMS)*time.Millisecond >= current.End.Sub(current.Start) && time.Duration(b.Item.DurationMS)*time.Millisecond < current.End.Sub(current.Start)
		})
	}
	return out
}

func standIn(c Channel, item Item, now time.Time) Programme {
	dur := time.Duration(item.DurationMS) * time.Millisecond
	return Programme{
		ID:        fmt.Sprintf("%s-fill-%d", c.ID, item.Key),
		ChannelID: c.ID,
		Start:     now,
		End:       now.Add(dur),
		Item:      item,
	}
}
