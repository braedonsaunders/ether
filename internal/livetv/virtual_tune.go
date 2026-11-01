package livetv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A virtual tune must not sit on one unplayable title until Plex gives up.
// The first attempt is long enough for a cached resolve; later attempts share
// whatever budget remains so a hung "not available yet" still reaches the
// next item or the channel's fallback.
var (
	virtualTuneBudget    = 12 * time.Second
	virtualAttemptBudget = 6 * time.Second
)

const virtualMaxAttempts = 4

func (s *Service) tuneVirtual(ctx context.Context, ss *session, current Programme) error {
	deadline := time.Now().Add(virtualTuneBudget)
	attempts := 0
	var last error
	for _, p := range s.virtualCandidates(ss.info.Channel, current) {
		if attempts >= virtualMaxAttempts || !time.Now().Before(deadline) {
			break
		}
		if s.knownUnavailable(ctx, p.Item.Key) {
			continue
		}
		slot := virtualAttemptBudget
		if remain := time.Until(deadline); remain < slot {
			slot = remain
		}
		if slot <= 0 {
			break
		}
		attempts++
		attempt, cancel := context.WithTimeout(ctx, slot)
		_, err := s.prepareTrack(attempt, ss, p)
		cancel()
		if err == nil {
			s.useProgramme(ss, p)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = err
		if !skippableTuneErr(err) {
			return err
		}
	}
	if last == nil {
		last = core.ErrNotAvailable
	}
	return last
}

func skippableTuneErr(err error) bool {
	return errors.Is(err, core.ErrNotAvailable) || errors.Is(err, core.ErrUnreleased) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Service) knownUnavailable(ctx context.Context, key core.RatingKey) bool {
	if key == 0 || s.o.Availability == nil {
		return false
	}
	known, ok := s.o.Availability.Available(ctx, key)
	return known && !ok
}

func (s *Service) useProgramme(ss *session, p Programme) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	chosen := p
	ss.info.Programme = &chosen
	ss.view.Key = p.Item.Key
	ss.view.TitleName = p.Item.Title
	if p.End.After(p.Start) {
		ss.view.Info.Duration = p.End.Sub(p.Start)
	}
}

// virtualCandidates starts with the guide's on-now title, then the rest of
// the running order, then fallback. Known-unplayable titles are skipped by
// the caller before Prepare.
func (s *Service) virtualCandidates(c Channel, current Programme) []Programme {
	now := s.o.Now()
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
	for _, p := range Schedule(c, now, now.Add(24*time.Hour)) {
		add(p)
	}
	for _, item := range c.Fallback {
		add(standIn(c, item, now))
	}
	for _, block := range c.Blocks {
		for _, item := range block.Items {
			add(standIn(c, item, now))
		}
	}
	for _, item := range c.Items {
		add(standIn(c, item, now))
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
