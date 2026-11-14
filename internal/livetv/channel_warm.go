package livetv

import (
	"context"
	"time"
)

const defaultProgrammeWarmLead = 5 * time.Minute

func channelWarmRefresh(p Programme, now time.Time) time.Duration {
	if !p.Start.After(now) && p.End.After(now) {
		return 15 * time.Second
	}
	return 10 * time.Minute
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// selectChannelWarmTargets picks at most the currently airing programme (on
// cold tune) plus ONE lookahead programme item for an actively watched channel:
//   - If now is inside a break/interstitial pod, immediately warm the post-break
//     programme (the interrupted show's exact return offset, or the next show).
//   - Otherwise, warm the single nearest upcoming programme or post-break return
//     starting within lead (T-minus).
func selectChannelWarmTargets(ps []Programme, now time.Time, lead time.Duration) (targets []Programme, breakReturn bool) {
	inBreak := false
	for _, p := range ps {
		if !now.Before(p.Start) && now.Before(p.End) {
			if p.Item.Key != 0 {
				targets = append(targets, p)
			} else if p.Item.Slate != offAirSlate {
				inBreak = true
			}
			break
		}
	}
	for _, p := range ps {
		if p.Item.Key == 0 || !p.Start.After(now) {
			continue
		}
		if inBreak {
			targets = append(targets, p)
			return targets, p.sourceOffset > 0
		}
		if !p.Start.After(now.Add(lead)) {
			targets = append(targets, p)
			return targets, p.sourceOffset > 0
		}
		break
	}
	return targets, false
}

// triggerBreakWarm immediately warms the post-break return point (or next show)
// in the background when an actively watched channel enters a break pod.
func (s *Service) triggerBreakWarm(channelID string, window []Programme, now time.Time) {
	if s == nil || s.o.Warm == nil || !s.isActivelyWatched(channelID) {
		return
	}
	var target *Programme
	inBreak := false
	for i := range window {
		p := &window[i]
		if !now.Before(p.Start) && now.Before(p.End) {
			inBreak = p.Item.Key == 0 && p.Item.Slate != offAirSlate
			break
		}
	}
	if !inBreak {
		return
	}
	for i := range window {
		p := window[i]
		if p.Item.Key != 0 && p.Start.After(now) {
			target = &p
			break
		}
	}
	if target == nil {
		return
	}
	p := *target
	offset := p.Offset(p.Start)
	wk := warmTarget{channelID: channelID, key: p.Item.Key, offsetSec: int64(offset / time.Second)}
	s.warmMu.Lock()
	if s.warmed == nil {
		s.warmed = map[warmTarget]time.Time{}
	}
	if now.Sub(s.warmed[wk]) < channelWarmRefresh(p, now) {
		s.warmMu.Unlock()
		return
	}
	s.warmed[wk] = now
	s.warmMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if !s.isActivelyWatched(channelID) {
			return
		}
		ctx, cancel := context.WithTimeout(s.serviceContext(), 15*time.Second)
		err := s.o.Warm(ctx, p.Item.Key, offset)
		cancel()
		if err != nil {
			s.warmMu.Lock()
			delete(s.warmed, wk)
			s.warmMu.Unlock()
			return
		}
		s.noteWarmExecuted(p.sourceOffset > 0)
	}()
}
