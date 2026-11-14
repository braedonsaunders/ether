package livetv

import (
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
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
	case "ident", "upnext", "break", "back", "tonight", "welcome", "reel", "standby":
		return true
	}
	return false
}

// tonightEnabledAt reports whether the "tonight"/"coming up" 3–4 programme
// lineup graphic is active at this boundary. An explicit Tonight=true runs at
// any hour of the day; nil preserves the evening (>= 18:00) default when UpNext
// is enabled.
func tonightEnabledAt(c Channel, at time.Time) bool {
	if c.Look == nil {
		return false
	}
	if c.Look.Tonight != nil {
		return *c.Look.Tonight
	}
	return c.Look.UpNext && at.In(zoneOf(c.Timezone)).Hour() >= 18
}

func (c Channel) momentsWithin(at time.Time, budget time.Duration) []Item {
	if c.Look != nil && c.Look.BreakPattern != nil {
		// Compose the pod in broadcast order (before-break moments -> spots ->
		// after-break moments) instead of piling up bumpers after "back".
		if pod := c.podItemsAround(at, budget, true); len(pod) > 0 {
			return pod
		}
		plain := c
		look := *c.Look
		look.BreakPattern = nil
		look.Breaks = ""
		plain.Look = &look
		return plain.momentsWithin(at, budget)
	}
	if c.Look == nil || c.clipDuration == nil {
		return nil
	}
	var kinds []string
	breaks := c.Look.Breaks == "between"
	if breaks && c.Look.BreakEvery > 0 && c.momentHistory != nil && !c.momentHistory.lastBreak.IsZero() {
		breaks = at.Sub(c.momentHistory.lastBreak) >= time.Duration(c.Look.BreakEvery)*time.Minute
	}
	reelInsertionIndex := -1
	if breaks {
		var beforeKinds, afterKinds []string
		if cfgBefore, custom := normalizeRundown(c.Look.BeforeBreak); custom {
			beforeKinds = cfgBefore
		} else {
			if tonightEnabledAt(c, at) {
				beforeKinds = append(beforeKinds, "tonight")
			}
			if c.Look.UpNext {
				beforeKinds = append(beforeKinds, "upnext")
			}
			if c.Look.BreakContent == "segments" {
				beforeKinds = append(beforeKinds, c.Look.SegmentIDs...)
			} else {
				beforeKinds = append(beforeKinds, "break")
			}
		}
		if cfgAfter, custom := normalizeRundown(c.Look.AfterBreak); custom {
			afterKinds = cfgAfter
		} else {
			afterKinds = append(afterKinds, "back")
			if c.Look.Ident {
				afterKinds = append(afterKinds, "ident")
			}
		}
		kinds = append(kinds, beforeKinds...)
		reelInsertionIndex = len(beforeKinds) - 1
		kinds = append(kinds, afterKinds...)
	} else if cfgBetween, custom := normalizeRundown(c.Look.BetweenProgrammes); custom {
		kinds = append(kinds, cfgBetween...)
	} else {
		if c.Look.Ident {
			kinds = append(kinds, "ident")
		}
		if c.Look.UpNext {
			kinds = append(kinds, "upnext")
		}
		if tonightEnabledAt(c, at) {
			kinds = append(kinds, "tonight")
		}
	}
	var base []Item
	var reserved time.Duration
	reelAfterBaseIdx := -1
	for i, kind := range kinds {
		if item, ok := c.rundownMomentItem(kind); ok {
			base = append(base, item)
			reserved += time.Duration(item.DurationMS) * time.Millisecond
			if breaks && c.Look.BreakContent != "segments" && (kind == "break" || i == reelInsertionIndex) {
				reelAfterBaseIdx = len(base) - 1
			}
		} else if breaks && c.Look.BreakContent != "segments" && i == reelInsertionIndex && reelAfterBaseIdx < 0 {
			reelAfterBaseIdx = len(base) - 1
		}
	}
	if breaks && c.momentHistory != nil && (len(base) > 0 || c.Look.BreakContent == "reel") {
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
	emitReel := func() {
		if c.momentHistory != nil {
			c.momentHistory.lastBreak = at
		}
		available := time.Duration(0)
		if budget > 0 {
			available = budget - reserved
			if available <= 0 {
				return
			}
		}
		reel := c.reelItems(cursor, available)
		out = append(out, reel...)
		for _, clip := range reel {
			cursor = cursor.Add(time.Duration(clip.DurationMS) * time.Millisecond)
		}
	}
	if breaks && c.Look.BreakContent != "segments" && reelAfterBaseIdx < 0 {
		emitReel()
	}
	for i, item := range base {
		out = append(out, item)
		cursor = cursor.Add(time.Duration(item.DurationMS) * time.Millisecond)
		if breaks && c.Look.BreakContent != "segments" && i == reelAfterBaseIdx {
			emitReel()
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
			look.Tonight = nil
			look.BeforeBreak = nil
			look.AfterBreak = nil
			look.BetweenProgrammes = nil
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
	r := core.ClipRequest{
		LookID:     lookID,
		Moment:     p.Item.Slate,
		ChannelID:  c.ID,
		ReelID:     p.Item.ReelID,
		ReelClipID: p.Item.ReelClipID,
		Bind:       map[string]string{"channel.name": c.Name, "slate.start": fmt.Sprint(p.Start.Unix())},
		Art:        map[string]string{},
	}
	if p.Item.ReelClipID != "" && r.ReelID == "" && c.Look != nil {
		r.ReelID = c.Look.ReelID
	}
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
	if p.Item.Slate == "tonight" {
		if p.Start.In(zoneOf(c.Timezone)).Hour() < 18 {
			r.Bind["lineup.heading"] = "COMING UP"
		} else {
			r.Bind["lineup.heading"] = "TONIGHT"
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
	listed := 0
	for _, next := range upcoming {
		if next.Item.Key == 0 || next.Start.Before(p.End) {
			continue
		}
		at := next.Start.In(zoneOf(c.Timezone)).Format("15:04")
		if listed == 0 {
			r.Bind["next.title"] = next.Item.Title
			r.Bind["next.time"] = at
			if p.Item.Slate == "back" {
				r.Bind["now.title"] = next.Item.Title
				r.Bind["now.meta"] = ProgrammeMeta(next.Item)
			}
			if p.Item.Slate == "upnext" || p.Item.Slate == "tonight" {
				r.Bind["next.meta"] = ProgrammeMeta(next.Item)
				r.Bind["next.logline"] = Logline(next.Item.Summary)
			}
			if ref := ArtworkRef(next.Item.Key, next.Item.Thumb, "poster"); ref != "" {
				r.Art["next.poster"] = ref
			}
			if p.Item.Slate != "tonight" {
				break
			}
		}
		// The coming-up menu lists one row per programme, merging a series'
		// back-to-back episodes the way a printed listing would.
		if listed > 0 && r.Bind[fmt.Sprintf("list[%d].title", listed-1)] == next.Item.Title {
			continue
		}
		r.Bind[fmt.Sprintf("list[%d].title", listed)] = next.Item.Title
		r.Bind[fmt.Sprintf("list[%d].time", listed)] = at
		r.Bind[fmt.Sprintf("list[%d].meta", listed)] = ProgrammeMeta(next.Item)
		listed++
		if listed == 4 {
			break
		}
	}
	return r
}

// ProgrammeMeta is the one-line detail under a title: season, episode and
// episode name for a series; year and running time for a film.
func ProgrammeMeta(it Item) string {
	if it.Episode > 0 {
		meta := fmt.Sprintf("S%d E%d", it.Season, it.Episode)
		if it.EpisodeTitle != "" && it.EpisodeTitle != it.Title {
			meta += " · " + it.EpisodeTitle
		}
		return meta
	}
	var parts []string
	if it.ReleasedMS > 0 {
		parts = append(parts, time.UnixMilli(it.ReleasedMS).UTC().Format("2006"))
	}
	if it.DurationMS >= 60000 {
		m := it.DurationMS / 60000
		if m >= 60 {
			parts = append(parts, fmt.Sprintf("%dh %02dm", m/60, m%60))
		} else {
			parts = append(parts, fmt.Sprintf("%d min", m))
		}
	}
	return strings.Join(parts, " · ")
}

// Logline is a summary's first sentence, bounded for an eight-second card.
func Logline(summary string) string {
	summary = strings.TrimSpace(summary)
	for _, end := range []string{". ", "! ", "? "} {
		if i := strings.Index(summary, end); i > 0 {
			summary = summary[:i+1]
		}
	}
	if r := []rune(summary); len(r) > 140 {
		cut := string(r[:140])
		if i := strings.LastIndex(cut, " "); i > 80 {
			cut = cut[:i]
		}
		summary = strings.TrimRight(cut, " ,;:") + "…"
	}
	return summary
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
	for _, request := range s.GraphicsRequests(s.serviceContext(), "") {
		s.o.Graphics.Ensure(s.serviceContext(), request)
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
