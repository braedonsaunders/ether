package livetv

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/faillog"
)

type reelClipCache interface {
	PeekCachedReelClip(reelID, clipID string) (string, time.Duration, bool)
	CachedReelClip(reelID, clipID string) (string, bool)
}

type reelSubEntry struct {
	end   time.Time
	items []Programme
}

func (s *Service) peekReelClipPath(c Channel, reelID, clipID string) string {
	if clipID == "" {
		return ""
	}
	if reelID == "" && c.Look != nil {
		reelID = c.Look.ReelID
	}
	if cache, ok := s.o.Graphics.(reelClipCache); ok && reelID != "" {
		if path, _, ok := cache.PeekCachedReelClip(reelID, clipID); ok && path != "" {
			return path
		}
	}
	if reel := c.reelForItem(Item{ReelID: reelID, ReelClipID: clipID}); reel != nil {
		for _, clip := range reel.Clips {
			if clip.ID == clipID && clip.Path != "" {
				return clip.Path
			}
		}
	}
	return ""
}

func (s *Service) openReelClipPath(c Channel, reelID, clipID string) string {
	if clipID == "" {
		return ""
	}
	if reelID == "" && c.Look != nil {
		reelID = c.Look.ReelID
	}
	if cache, ok := s.o.Graphics.(reelClipCache); ok && reelID != "" {
		if path, ok := cache.CachedReelClip(reelID, clipID); ok && path != "" {
			return path
		}
	}
	if reel := c.reelForItem(Item{ReelID: reelID, ReelClipID: clipID}); reel != nil {
		for _, clip := range reel.Clips {
			if clip.ID == clipID && clip.Path != "" {
				return clip.Path
			}
		}
	}
	return ""
}

// A missing master changes only the airing's input. It never changes schedule
// spans, programme identities, or the guide. When graphics supports live
// real-time rendering (>= 1.0x), cold moments render animated directly into the
// stream and save reusable masters while airing instead of falling back to a still.
// An unresolvable reel link clip skips silently for its slot, filling with a
// cached clip from the same reel, channel/block filler, or an animated break bumper.
func (s *Service) broadcastSchedule(ctx context.Context, c Channel, from, until time.Time, check bool) []Programme {
	c = s.graphicsChannel(c)
	ps := s.readyProgrammes(c, Schedule(c, from, until))
	if s.o.Graphics == nil {
		return ps
	}
	out := make([]Programme, 0, len(ps))
	usedPodClips := map[string]bool{}
	masterRequested := false
	for i := range ps {
		orig := ps[i]
		orig.graphicsID = orig.ID
		if !interstitial(orig.Item) {
			clear(usedPodClips)
		}
		s.mu.Lock()
		orig.graphicsFallback = !s.graphicsSkips[c.ID+"/"+orig.ID].IsZero()
		sub, hasSub := s.reelSubstitutions[c.ID+"/"+orig.ID]
		s.mu.Unlock()
		if !check || !interstitial(orig.Item) {
			out = append(out, orig)
			continue
		}
		if hasSub && len(sub.items) > 0 {
			for _, sp := range sub.items {
				if sp.graphicsID == "" {
					sp.graphicsID = sp.ID
				}
				s.mu.Lock()
				if !s.graphicsSkips[c.ID+"/"+sp.graphicsID].IsZero() {
					sp.graphicsFallback = true
				}
				s.mu.Unlock()
				if interstitial(sp.Item) {
					req := s.clipRequest(c, sp, ps)
					sp.graphicsRequest = &req
					if sp.Item.ReelClipID != "" {
						usedPodClips[sp.Item.ReelClipID] = true
					}
				}
				out = append(out, sp)
			}
			continue
		}
		request := s.clipRequest(c, orig, ps)
		orig.graphicsRequest = &request
		if orig.graphicsFallback {
			out = append(out, orig)
			continue
		}
		if orig.Item.ReelClipID != "" {
			if s.peekReelClipPath(c, request.ReelID, orig.Item.ReelClipID) != "" {
				usedPodClips[orig.Item.ReelClipID] = true
				out = append(out, orig)
				continue
			}
			req := request
			req.Boundary = orig.Start
			s.o.Graphics.Ensure(ctx, req)
			if orig.Start.After(from.Add(broadcastPrewarm)) {
				out = append(out, orig)
				continue
			}
			subs := s.substituteReelSlot(ctx, c, orig, request.ReelID, ps, usedPodClips)
			s.latchReelSubstitution(c.ID+"/"+orig.ID, orig.End, subs)
			out = append(out, subs...)
			continue
		}
		ready := false
		if live, ok := s.o.Graphics.(liveStaticSource); ok {
			ready = live.LiveStatic(ctx, request)
		}
		if !ready {
			_, ready = s.o.Graphics.Clip(ctx, request)
		}
		if !ready {
			if lm, ok := s.o.Graphics.(liveMomentSource); ok {
				allowed, speedX, reason := lm.LiveRenderAllowed(request)
				if allowed {
					// Pre-render distant moments via the bounded queue; imminent or
					// active moments render live and save directly in OpenMoment.
					if !masterRequested && orig.Start.After(from.Add(broadcastPrewarm)) {
						masterRequested = true
						req := request
						req.Boundary = orig.Start
						s.o.Graphics.Ensure(ctx, req)
					}
				} else if !orig.Start.After(from) {
					if reason == "" {
						reason = "live render unavailable"
					}
					s.noteMomentPlayout(c.ID, orig.Item.Slate, "fallback", speedX, reason)
					s.recordGraphicsSkip(c, orig, fmt.Errorf("%s", reason))
					orig.graphicsFallback = true
				}
			} else {
				s.o.Graphics.Ensure(ctx, request)
				if !orig.Start.After(from) {
					s.recordGraphicsSkip(c, orig, fmt.Errorf("master not cached at airing boundary"))
					orig.graphicsFallback = true
				}
			}
		}
		out = append(out, orig)
	}
	return out
}

func (s *Service) substituteReelSlot(ctx context.Context, c Channel, orig Programme, reelID string, ps []Programme, usedPodClips map[string]bool) []Programme {
	if reelID == "" && c.Look != nil {
		reelID = c.Look.ReelID
	}
	if reel := c.reelForItem(Item{ReelID: reelID, ReelClipID: orig.Item.ReelClipID}); reel != nil && len(reel.Clips) > 0 {
		startIdx := 0
		for i, clip := range reel.Clips {
			if clip.ID == orig.Item.ReelClipID {
				startIdx = (i + 1) % len(reel.Clips)
				break
			}
		}
		var fresh, fallbackAlts []ReelClip
		for n := 0; n < len(reel.Clips); n++ {
			alt := reel.Clips[(startIdx+n)%len(reel.Clips)]
			if alt.ID == orig.Item.ReelClipID || alt.Duration <= 0 {
				continue
			}
			if s.peekReelClipPath(c, reel.ID, alt.ID) == "" {
				continue
			}
			if !usedPodClips[alt.ID] {
				fresh = append(fresh, alt)
			} else {
				fallbackAlts = append(fallbackAlts, alt)
			}
		}
		alts := append(fresh, fallbackAlts...)
		if len(alts) > 0 {
			var subs []Programme
			cursor := orig.Start
			for k := 0; cursor.Before(orig.End) && k < 16; k++ {
				alt := alts[k%len(alts)]
				end := cursor.Add(alt.Duration)
				if end.After(orig.End) || orig.End.Sub(end) < segmentDuration {
					end = orig.End
				}
				id := fmt.Sprintf("%s-sub-%d", orig.ID, k)
				sp := Programme{
					ID:         id,
					graphicsID: id,
					BlockID:    orig.BlockID,
					Start:      cursor,
					End:        end,
					Item: Item{
						Slate:      "reel",
						Title:      alt.Name,
						DurationMS: end.Sub(cursor).Milliseconds(),
						ReelID:     reel.ID,
						ReelClipID: alt.ID,
					},
				}
				req := s.clipRequest(c, sp, ps)
				sp.graphicsRequest = &req
				usedPodClips[alt.ID] = true
				subs = append(subs, sp)
				cursor = end
			}
			if len(subs) > 0 {
				return subs
			}
		}
	}

	if s.o.Playback != nil {
		if filler, ok := s.pickBreakFiller(ctx, c, orig); ok {
			filler.Slate = ""
			filler.DurationMS = orig.End.Sub(orig.Start).Milliseconds()
			id := orig.ID + "-filler"
			return []Programme{{
				ID:         id,
				graphicsID: id,
				BlockID:    orig.BlockID,
				Start:      orig.Start,
				End:        orig.End,
				Item:       filler,
			}}
		}
	}

	// When On Air live moment rendering is available and no reel clip or filler
	// is cached yet, loop the look's animated break bumper instead of a still.
	if lm, ok := s.o.Graphics.(liveMomentSource); ok && c.Look != nil && c.Look.LookID != "" {
		if dur, ok := c.momentDuration("break"); ok && dur > 0 {
			id := orig.ID + "-break-anim"
			bp := Programme{
				ID:         id,
				graphicsID: id,
				BlockID:    orig.BlockID,
				Start:      orig.Start,
				End:        orig.End,
				Item: Item{
					Slate:      "break",
					Title:      "Commercial break",
					DurationMS: orig.End.Sub(orig.Start).Milliseconds(),
				},
			}
			req := s.clipRequest(c, bp, ps)
			if allowed, _, _ := lm.LiveRenderAllowed(req); allowed {
				bp.graphicsRequest = &req
				return []Programme{bp}
			}
		}
	}

	fb := orig
	fb.graphicsFallback = true
	return []Programme{fb}
}

func (s *Service) pickBreakFiller(ctx context.Context, c Channel, orig Programme) (Item, bool) {
	var pool []Item
	if orig.BlockID != "" {
		for _, b := range c.Blocks {
			if b.ID == orig.BlockID && len(b.Filler) > 0 {
				pool = b.Filler
				break
			}
		}
	}
	if len(pool) == 0 {
		pool = c.Filler
	}
	if len(pool) == 0 {
		return Item{}, false
	}
	start := int(stepHash(c.Seed, int64(len(pool)), orig.Start.UnixNano()))
	for n := 0; n < len(pool); n++ {
		it := pool[(start+n)%len(pool)]
		if it.Key == 0 || s.knownUnavailable(ctx, it.Key) {
			continue
		}
		return it, true
	}
	return Item{}, false
}

func (s *Service) latchReelSubstitution(key string, end time.Time, subs []Programme) {
	copied := append([]Programme(nil), subs...)
	cutoff := s.o.Now().Add(-48 * time.Hour)
	s.mu.Lock()
	if s.reelSubstitutions == nil {
		s.reelSubstitutions = map[string]reelSubEntry{}
	}
	s.reelSubstitutions[key] = reelSubEntry{end: end, items: copied}
	for k, ent := range s.reelSubstitutions {
		if ent.end.Before(cutoff) {
			delete(s.reelSubstitutions, k)
		}
	}
	s.mu.Unlock()
}

func (s *Service) broadcastGuide(c Channel, from, until time.Time) []Programme {
	return s.readyProgrammes(c, guideSchedule(c, from, until))
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
