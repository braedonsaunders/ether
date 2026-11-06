package livetv

import "time"

func fillerMoment(kind string) bool {
	switch kind {
	case "ident", "upnext", "tonight", "break", "back":
		return true
	}
	return false
}

// fillProgrammingGap uses immutable source snapshots and the schedule's own
// history. It never opens a source or renders. Classic calendars retain fillGap.
func fillProgrammingGap(out *[]Programme, c Channel, filler []Item, start, end, from, until time.Time) {
	cursor := start
	emit := func(it Item, limit time.Time, trim bool) bool {
		d := time.Duration(it.DurationMS) * time.Millisecond
		if d <= 0 || !cursor.Before(limit) {
			return false
		}
		finish := cursor.Add(d)
		if finish.After(limit) {
			if !trim {
				return false
			}
			finish = limit
		}
		appendProgramme(out, c, it, cursor, finish, from, until)
		cursor = finish
		return true
	}
	moment := func(kind string) (Item, bool) {
		d, ok := c.momentDuration(kind)
		return Item{Slate: kind, Title: kind, DurationMS: d.Milliseconds()}, ok && d > 0
	}
	bumpers := c.Look != nil && ((c.Look.BreakPattern != nil && c.Look.BreakPattern.BumperStyle == "break-back") || (c.Look.BreakPattern == nil && c.Look.Breaks == "between"))
	back, hasBack := moment("back")
	hasBack = hasBack && bumpers
	breakItem, hasBreak := moment("break")
	hasBreak = hasBreak && bumpers
	var promos []Item
	seen := map[string]bool{}
	addPromo := func(it Item) {
		if it.DurationMS > 0 && !seen[it.Slate] {
			promos = append(promos, it)
			seen[it.Slate] = true
		}
	}
	// Explicit moment fillers remain intentional even if their automatic toggle is off.
	var clips []Item
	for _, it := range filler {
		if fillerMoment(it.Slate) && it.AssetDigest == "" {
			addPromo(it)
		} else {
			clips = append(clips, it)
		}
	}
	if c.Look != nil {
		plain := c
		look := *c.Look
		look.BreakPattern = nil
		look.Breaks = ""
		plain.Look = &look
		for _, it := range plain.momentsWithin(start, 0) {
			addPromo(it)
		}
	}
	limit := end
	if hasBack {
		d := time.Duration(back.DurationMS) * time.Millisecond
		if d < end.Sub(start) {
			limit = end.Add(-d)
		} else {
			limit = start
		}
	}
	podChannel := c
	if c.Look != nil {
		look := *c.Look
		podChannel.Look = &look
		if look.BreakPattern != nil {
			p := *look.BreakPattern
			p.BumperStyle = "none"
			look.BreakPattern = &p
		}
	}
	var pod []Item
	opening := time.Duration(0)
	if hasBreak {
		opening = time.Duration(breakItem.DurationMS) * time.Millisecond
	}
	available := limit.Sub(cursor) - opening
	if available > 0 {
		if c.Look != nil && c.Look.BreakPattern != nil {
			pod = podChannel.podItems(cursor.Add(opening), available)
		} else if c.Look != nil {
			pod = podChannel.reelItems(cursor.Add(opening), available)
		}
	}
	if hasBreak && cursor.Add(opening).Before(limit) && (len(pod) > 0 || len(promos) > 0 || len(clips) > 0) {
		emit(breakItem, limit, false)
	}
	for _, it := range pod {
		emit(it, limit, false)
	}
	for _, it := range promos {
		emit(it, limit, false)
	}
	// A selected channel reel is also a short-form filler pool after its pod.
	// It shares the pod's no-repeat history rather than restarting the same spots.
	if c.reel != nil {
		for _, clip := range c.reel.Clips {
			clips = append(clips, Item{Slate: "reel", ReelID: c.reel.ID, ReelClipID: clip.ID, Title: clip.Name, DurationMS: clip.Duration.Milliseconds()})
		}
	}
	eligible := func(it Item) bool {
		if it.DurationMS <= 0 {
			return false
		}
		if it.ReelClipID != "" && c.momentHistory != nil {
			reel := c.reelForItem(it)
			repeat := time.Duration(0)
			if reel != nil {
				repeat = reel.Repeat
			}
			if c.Look != nil && c.Look.BreakPattern != nil {
				repeat = max(repeat, time.Duration(c.Look.BreakPattern.NoRepeatMinutes)*time.Minute)
			}
			if last, ok := c.momentHistory.last[it.ReelClipID]; ok && cursor.Sub(last) < repeat {
				return false
			}
		}
		return true
	}
	note := func(it Item) {
		if it.ReelClipID != "" && c.momentHistory != nil {
			c.momentHistory.last[it.ReelClipID] = cursor
		}
	}
	for cursor.Before(limit) && len(*out) < 20000 {
		progressed := false
		for _, it := range clips {
			if !eligible(it) || cursor.Add(time.Duration(it.DurationMS)*time.Millisecond).After(limit) {
				continue
			}
			note(it)
			if emit(it, limit, false) {
				progressed = true
			}
			if !cursor.Before(limit) {
				break
			}
		}
		if !progressed {
			break
		}
	}
	// Exhausted commercial repetition windows must not turn a scheduled strip
	// into off-air. Cycle available promos, then the enabled break/back creative.
	fallback := append([]Item(nil), promos...)
	if hasBreak {
		fallback = append(fallback, breakItem)
	}
	if hasBack {
		fallback = append(fallback, back)
	}
	if len(fallback) == 0 {
		for _, it := range clips {
			if eligible(it) {
				fallback = append(fallback, it)
			}
		}
	}
	for cursor.Before(limit) && len(*out) < 20000 {
		progressed := false
		for _, it := range fallback {
			if emit(it, limit, true) {
				progressed = true
			}
			if !cursor.Before(limit) {
				break
			}
		}
		if !progressed {
			break
		}
	}
	if hasBack {
		emit(back, end, true)
	}
	if cursor.Before(end) {
		emitSlate(out, c, cursor, end, from, until)
	}
}
