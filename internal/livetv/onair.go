package livetv

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func interstitial(it Item) bool {
	if it.Key != 0 {
		return false
	}
	if strings.HasPrefix(it.Slate, "plugin:") {
		return true
	}
	switch it.Slate {
	case "ident", "upnext", "break", "back", "tonight", "welcome", "reel":
		return true
	}
	return false
}

func (c Channel) moments(at time.Time) []Item { return c.momentsWithin(at, 0) }
func (c Channel) momentsWithin(at time.Time, budget time.Duration) []Item {
	if c.Look != nil && c.Look.BreakPattern != nil {
		// Era pods compose with the ordinary channel moments. Reserve their
		// time first so a full pod cannot silently consume ident/up-next.
		plain := c
		look := *c.Look
		look.BreakPattern = nil
		look.Breaks = ""
		plain.Look = &look
		regular := plain.momentsWithin(at, budget)
		remaining := budget
		if budget > 0 {
			for _, item := range regular {
				remaining -= time.Duration(item.DurationMS) * time.Millisecond
			}
			if remaining <= 0 {
				return regular
			}
		}
		pod := c.podItems(at, remaining)
		return append(pod, regular...)
	}
	if c.Look == nil || c.clipDuration == nil {
		return nil
	}
	var kinds []string
	breaks := c.Look.Breaks == "between"
	if breaks && c.Look.BreakEvery > 0 && c.momentHistory != nil && !c.momentHistory.lastBreak.IsZero() {
		breaks = at.Sub(c.momentHistory.lastBreak) >= time.Duration(c.Look.BreakEvery)*time.Minute
	}
	if breaks {
		if c.Look.BreakContent == "segments" {
			kinds = append(kinds, c.Look.SegmentIDs...)
		} else {
			kinds = append(kinds, "break")
		}
		kinds = append(kinds, "back")
	}

	if c.Look.Ident {
		kinds = append(kinds, "ident")
	}
	if c.Look.UpNext {
		kinds = append(kinds, "upnext")
	}
	tonight := c.Look.UpNext
	if c.Look.Tonight != nil {
		tonight = *c.Look.Tonight
	}
	if tonight && at.In(zoneOf(c.Timezone)).Hour() >= 18 {
		kinds = append(kinds, "tonight")
	}
	var base []Item
	var reserved time.Duration
	for _, kind := range kinds {
		if strings.HasPrefix(kind, "plugin:") {
			if item, ok := c.pluginItem(kind); ok {
				base = append(base, item)
				reserved += time.Duration(item.DurationMS) * time.Millisecond
			}
			continue
		}
		if d, ok := c.momentDuration(kind); ok && d > 0 && d%segmentDuration == 0 {
			base = append(base, Item{Slate: kind, Title: kind, DurationMS: d.Milliseconds()})
			reserved += d
		}
	}
	if breaks && c.momentHistory != nil && len(base) > 0 {
		c.momentHistory.lastBreak = at
	}
	if budget > 0 && reserved > budget {
		var fit []Item
		for _, item := range base {
			d := time.Duration(item.DurationMS) * time.Millisecond
			if d <= budget {
				fit = append(fit, item)
				budget -= d
			}
		}
		return fit
	}
	var out []Item
	cursor := at
	for _, item := range base {
		out = append(out, item)
		cursor = cursor.Add(time.Duration(item.DurationMS) * time.Millisecond)
		if item.Slate == "break" {
			if c.momentHistory != nil {
				c.momentHistory.lastBreak = at
			}
			available := time.Duration(0)
			if budget > 0 {
				available = budget - reserved
				if available <= 0 {
					continue
				}
			}
			reel := c.reelItems(cursor, available)
			out = append(out, reel...)
			for _, clip := range reel {
				cursor = cursor.Add(time.Duration(clip.DurationMS) * time.Millisecond)
			}
		}
	}
	return out
}

// guideSchedule preserves the original gap guide, including filler and off-air
// rows. Insert placement folds clip spans into the preceding programme.
func guideSchedule(c Channel, from, until time.Time) []Programme {
	inserted := func(l *ChannelLook) bool { return l != nil && (l.Placement == "insert" || l.BreakPattern != nil) }
	hasInsert := inserted(c.Look)
	for _, b := range c.Blocks {
		hasInsert = hasInsert || inserted(b.Breaks)
	}
	for _, p := range c.Pins {
		hasInsert = hasInsert || inserted(p.Breaks)
	}
	if c.clipDuration == nil || (c.Look == nil && !hasInsert) {
		return Schedule(c, from, until)
	}
	if !hasInsert {
		if len(c.Clock) > 0 {
			// Suppress look interstitials while retaining the clock's channel policy.
			look := *c.Look
			look.Breaks = ""
			look.UpNext = false
			look.Ident = false
			c.Look = &look
		} else {
			c.Look = nil
		}
		return Schedule(c, from, until)
	}
	ps := Schedule(c, dayStart(from, zoneOf(c.Timezone)).AddDate(0, 0, -1), until.Add(24*time.Hour))
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

func (s *Service) clipRequest(c Channel, p Programme, snapshots ...[]Programme) core.ClipRequest {
	for _, b := range c.Blocks {
		if b.ID == p.BlockID && b.Breaks != nil {
			c.Look = effectiveBlockLook(c.Look, b.Breaks)
		}
	}
	for _, pin := range c.Pins {
		if pin.ID == p.BlockID && pin.Breaks != nil {
			c.Look = effectiveBlockLook(c.Look, pin.Breaks)
		}
	}
	lookID := ""
	if c.Look != nil {
		lookID = c.Look.LookID
	}
	r := core.ClipRequest{LookID: lookID, Moment: p.Item.Slate, ChannelID: c.ID, Bind: map[string]string{"channel.name": c.Name, "slate.start": fmt.Sprint(p.Start.Unix())}, Art: map[string]string{}}
	if c.Look != nil {
		r.Bug = c.Look.Bug
		r.AssetDigest = c.Look.MomentAssets[p.Item.Slate]
		r.AssetDuration = time.Duration(p.Item.DurationMS) * time.Millisecond
		r.Bind["channel.tagline"] = c.Look.Tagline
		r.Bind["channel.accent"] = c.Look.Accent
	}
	if p.Item.Slate == "bug" && c.Logo != "" {
		r.Art["channel.logo"] = c.Logo
	}
	if c.Personal != nil && c.personalViewer != nil {
		r.ViewerID = c.personalViewer.ID
		r.Bind["viewer.name"] = viewerName(c.personalViewer)
		r.Bind["viewer.greeting"] = "Welcome back"
		r.Bind["next.reason"] = p.Reason
		if p.Item.Slate == "welcome" {
			r.Bind["resume.title"] = p.Item.Title
			r.Bind["resume.minutes"] = p.Item.Summary
		}
	}
	if p.Item.Slate != "upnext" && p.Item.Slate != "tonight" && p.Item.Slate != "back" && p.Item.Slate != "lowerthird" {
		return r
	}
	c = s.graphicsChannel(c)
	var upcoming []Programme
	if len(snapshots) > 0 {
		upcoming = snapshots[0]
	} else {
		upcoming = Schedule(c, p.Start, p.End.Add(24*time.Hour))
	}
	for _, next := range upcoming {
		if next.Item.Key != 0 && !next.Start.Before(p.End) {
			r.Bind["next.title"] = next.Item.Title
			r.Bind["next.time"] = next.Start.In(zoneOf(c.Timezone)).Format("15:04")
			if ref := ArtworkRef(next.Item.Key, next.Item.Thumb, "poster"); ref != "" {
				r.Art["next.poster"] = ref
			}
			break
		}
	}
	return r
}
func (s *Service) graphicsChannel(c Channel) Channel {

	if s.o.Graphics != nil {
		c.clipDuration = s.o.Graphics.Duration
		if meta, ok := s.o.Graphics.(core.ChannelSegmentMetadata); ok {
			c.segmentInfo = meta.SegmentInfo
		}
		c.programmingReels = map[string]*ReelSnapshot{}
		s.mu.Lock()
		for _, b := range c.Blocks {
			if b.Breaks != nil {
				c.programmingReels[b.Breaks.ReelID] = s.reelSnapshots[b.Breaks.ReelID]
			}
		}
		for _, p := range c.Pins {
			if p.Breaks != nil {
				c.programmingReels[p.Breaks.ReelID] = s.reelSnapshots[p.Breaks.ReelID]
			}
		}
		s.mu.Unlock()
		if c.Look != nil && c.Look.BreakContent == "reel" {
			s.mu.Lock()
			c.reel = s.reelSnapshots[c.Look.ReelID]
			s.mu.Unlock()
		}
	}
	return c
}

func (s *Service) ensureGraphics() {
	if s.o.Graphics == nil {
		return
	}
	// Both maintenance paths share the measured lead and nearest-boundary order.
	// A second, three-hour scan must not refill the queue with distant masters.
	for _, request := range s.GraphicsRequests(s.ctx, "") {
		s.o.Graphics.Ensure(s.ctx, request)
	}
}

func (c Channel) pluginItem(id string) (Item, bool) {
	if c.clipDuration == nil {
		return Item{}, false
	}
	look := ""
	if c.Look != nil {
		look = c.Look.LookID
	}
	d, ok := c.clipDuration(look, id)
	if !ok || d <= 0 || d%segmentDuration != 0 {
		return Item{}, false
	}
	name, rating := id, ""
	if c.segmentInfo != nil {
		var exists bool
		name, rating, exists = c.segmentInfo(id)
		if !exists {
			return Item{}, false
		}
	}
	cap := c.Rating
	if c.Filter != nil && c.Filter.MaxContentRating != "" {
		cap = c.Filter.MaxContentRating
	}
	if cap != "" && rating == "" && (c.Look == nil || !c.Look.AllowUnratedSegments) {
		return Item{}, false
	}
	if rating != "" && (contentrating.Exceeds(rating, c.Rating) || (c.Filter != nil && contentrating.Exceeds(rating, c.Filter.MaxContentRating))) {
		return Item{}, false
	}
	return Item{Slate: id, Title: name, Rating: rating, DurationMS: d.Milliseconds()}, true
}

func (c Channel) momentDuration(kind string) (time.Duration, bool) {
	if c.Look == nil || c.clipDuration == nil {
		return 0, false
	}
	duration, ok := c.clipDuration(c.Look.LookID, kind)
	if c.Look.MomentAssets[kind] != "" && (!ok || duration <= 0) {
		return 8 * time.Second, true
	}
	return duration, ok
}
