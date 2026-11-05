package livetv

import (
	"fmt"
	"sort"
	"time"
)

// calendarBlock is a disjoint interval in the local broadcast day. Specials
// outrank recurring pins; pins outrank dayparts. Saved order breaks ties.
type calendarBlock struct {
	Block
	index    int
	original Block
	pinned   bool
	reel     *ReelSnapshot
}

func baseSegment(c Channel, day time.Time) segment {
	if len(c.Sources) > 0 {
		c.Items = programmingItems(c.Sources, c.Items, c, day, Block{ID: "base", Start: 0, End: 1440})
		c.Mode = "sources"
		c.Shuffle = false
		c.MarathonKey = 0
		if len(c.Sources) == 1 && c.Sources[0].Strip {
			c.Align = "half"
			c.Fit = "pad"
		}
	}
	return segment{items: c.Items, filler: c.Filler, shuffle: c.Shuffle, mode: c.Mode, align: c.Align, fit: c.Fit, repeat: c.RepeatDays, marathon: c.MarathonKey, fallback: c.Fallback, offAir: c.OffAir}
}

func calendarBlocks(c Channel, day time.Time) []calendarBlock {
	var choices []calendarBlock
	for _, dated := range []bool{true, false} {
		for _, p := range c.Pins {
			if (p.Date != "") != dated || (dated && p.Date != day.Format("2006-01-02")) || (!dated && !blockOnDay(Block{Days: p.Days}, int(day.Weekday()))) {
				continue
			}
			label := p.Name + " · pinned slot"
			if dated {
				label = p.Name + " · dated special"
			}
			choices = append(choices, calendarBlock{Block: Block{ID: p.ID, Name: label, Start: p.Start, End: p.End, Days: p.Days, Items: p.Items, Sources: p.Sources, Mode: p.Mode, RepeatDays: p.RepeatDays, Breaks: p.Breaks}, pinned: true})
		}
	}
	for _, b := range c.Blocks {
		if blockOnDay(b, int(day.Weekday())) {
			choices = append(choices, calendarBlock{Block: b})
		}
	}
	boundaries := []int{0, 1440}
	for _, b := range choices {
		boundaries = append(boundaries, b.Start, b.End)
	}
	sort.Ints(boundaries)
	var out []calendarBlock
	for i := 1; i < len(boundaries); i++ {
		start, end := boundaries[i-1], boundaries[i]
		if start == end || start < 0 || end > 1440 {
			continue
		}
		for index, b := range choices {
			b.index = index
			b.original = b.Block
			if b.Start <= start && b.End >= end {
				if len(out) > 0 && out[len(out)-1].index == b.index && out[len(out)-1].End == start {
					out[len(out)-1].End = end
					break
				}
				if b.Breaks != nil {
					b.reel = c.programmingReels[b.Breaks.ReelID]
				}
				b.Start = start
				b.End = end
				out = append(out, b)
				break
			}
		}
	}
	return out
}

// programmingItems chooses from saved snapshots only. The daily seed changes
// the weighted source order without disturbing an ordered show's episodes.
func programmingItems(sources []ProgramSource, fallback []Item, c Channel, day time.Time, b Block) []Item {
	if len(sources) == 0 {
		return fallback
	}
	var parts [][]Item
	var weights []int
	for _, s := range sources {
		items := append([]Item(nil), s.Items...)
		if len(s.Sources) > 0 {
			items = programmingItems(s.Sources, items, c, day, b)
		}
		if s.Order == "shuffle" {
			items = shuffleItems(items, c.Seed, day.Unix()+int64Hash(s.ID))
		}
		if s.Strip && len(items) > 0 {
			// A strip consumes the same number of slots on every eligible day,
			// counting only broadcast weekdays since the channel's epoch.
			epoch := dayStart(c.Epoch, day.Location())
			if s.StartDate != "" {
				if at, err := time.ParseInLocation("2006-01-02", s.StartDate, day.Location()); err == nil {
					epoch = at
				}
			}
			elapsed := broadcastDays(epoch, day, b.Days)
			stripBlock := b
			if len(sources) == 1 {
				stripBlock.Align = "half"
				stripBlock.Fit = "pad"
			}
			at := stripPosition(items, elapsed, stripBlock)

			items = append(items[at:], items[:at]...)
		}
		for i := range items {
			if len(s.Sources) == 0 || items[i].SourceID == "" {
				items[i].SourceID = s.ID
			}
		}
		parts = append(parts, items)
		w := s.Weight
		if w < 1 {
			w = 1
		}
		if w > 100 {
			w = 100
		}
		weights = append(weights, w)
	}
	// Weighted fair selection gives every non-empty source its configured share
	// even when its pool is smaller. Pool sizes never secretly become weights.
	var out []Item
	positions := make([]int, len(parts))
	total := 0
	for i, p := range parts {
		if len(p) > 0 {
			total += weights[i]
		}
	}
	if total == 0 {
		return nil
	}
	count := 0
	for _, p := range parts {
		count += len(p)
	}
	if count < total {
		count = total
	}
	if count > 10000 {
		count = 10000
	}
	scores := make([]int, len(parts))
	rotation := int(stepHash(c.Seed, day.Unix(), int64Hash(b.ID)) % uint64(len(parts)))
	for n := 0; n < count; n++ {
		chosen := -1
		for offset := 0; offset < len(parts); offset++ {
			i := (offset + rotation) % len(parts)
			if len(parts[i]) == 0 {
				continue
			}
			scores[i] += weights[i]
			if chosen < 0 || scores[i] > scores[chosen] {
				chosen = i
			}
		}
		scores[chosen] -= total
		out = append(out, parts[chosen][positions[chosen]%len(parts[chosen])])
		positions[chosen]++
	}
	return out
}

func broadcastDays(from, to time.Time, days []int) int {
	// Calendar arithmetic, rather than 24-hour division, preserves DST days.
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	n := int(b.Sub(a) / (24 * time.Hour))
	if n <= 0 {
		return 0
	}
	valid := map[int]bool{}
	for _, d := range days {
		valid[d] = true
	}
	if len(days) == 0 {
		return n
	}
	count := (n / 7) * len(valid)
	for i := 0; i < n%7; i++ {
		if valid[(int(from.Weekday())+i)%7] {
			count++
		}
	}
	return count
}

// DaypartPreset supplies editable starting times; saved values are authoritative.
func DaypartPreset(name string) (int, int, error) {
	switch name {
	case "morning":
		return 360, 720, nil
	case "daytime":
		return 720, 1200, nil
	case "prime":
		return 1200, 1380, nil
	case "late":
		return 1380, 1440, nil
	case "overnight":
		return 0, 360, nil
	case "custom", "":
		return 0, 1440, nil
	}
	return 0, 0, fmt.Errorf("unknown daypart preset %q", name)
}

// stripPosition advances by the programmes actually consumed by each preceding
// slot. A repeated position skips whole cycles, keeping decades-old epochs cheap.
func stripPosition(items []Item, days int, b Block) int {
	at := 0
	seen := map[int]int{}
	for d := 0; d < days; d++ {
		if prior, ok := seen[at]; ok {
			period := d - prior
			if period > 0 {
				skip := (days - d) / period
				if skip > 0 {
					d += skip * period
					if d >= days {
						break
					}
				}
			}
		} else {
			seen[at] = d
		}
		remaining := int64(b.End-b.Start) * 60000
		for remaining > 0 {
			duration := items[at].DurationMS
			if duration <= 0 {
				return at
			}
			if b.Fit == "pad" && duration > remaining {
				break
			}
			remaining -= duration
			at = (at + 1) % len(items)
			if b.Align != "" && remaining > 0 {
				step := int64(30 * 60000)
				if b.Align == "hour" {
					step = 60 * 60000
				}
				used := int64(b.End-b.Start)*60000 - remaining
				remaining -= (step - used%step) % step
			}
		}
	}
	return at
}

func programmeRule(c Channel, item Item) string {
	if c.programmingBlockID == "" && item.SourceID == "" {
		return ""
	}
	label := orText(c.programmingRule, "Channel loop")
	var find func([]ProgramSource) *ProgramSource
	find = func(sources []ProgramSource) *ProgramSource {
		for i := range sources {
			s := &sources[i]
			if s.ID != "" && s.ID == item.SourceID {
				return s
			}
			if found := find(s.Sources); found != nil {
				return found
			}
		}
		return nil
	}
	var source *ProgramSource
	for _, b := range c.Blocks {
		if b.ID == c.programmingBlockID {
			source = find(b.Sources)
		}
	}
	for _, p := range c.Pins {
		if p.ID == c.programmingBlockID {
			source = find(p.Sources)
		}
	}
	if source == nil {
		source = find(c.Sources)
	}
	if source != nil {
		label += " · " + orText(source.Name, source.Kind)
		if source.Strip {
			label += " in order (strip)"
		} else if source.Order == "shuffle" {
			label += " shuffled"
		} else if source.Kind == "show" {
			label += " in order"
		}
	}
	return label
}

// A block owns its break policy, while the channel owns its shared creative
// identity. Empty style and asset overrides inherit the channel's choices.
func effectiveBlockLook(base, override *ChannelLook) *ChannelLook {
	if override == nil {
		return base
	}
	look := *override
	if base != nil {
		if look.LookID == "" {
			look.LookID = base.LookID
		}
		look.Ident = base.Ident
		look.UpNext = base.UpNext
		look.Tonight = base.Tonight
		look.OffAir = base.OffAir
		look.Bug = base.Bug
		assets := map[string]string{}
		for k, v := range base.MomentAssets {
			assets[k] = v
		}
		for k, v := range look.MomentAssets {
			assets[k] = v
		}
		look.MomentAssets = assets
	}
	return &look
}

func (c Channel) reelForItem(item Item) *ReelSnapshot {
	if reel := c.programmingReels[item.ReelID]; reel != nil {
		return reel
	}
	if c.reel != nil && (item.ReelID == "" || item.ReelID == c.reel.ID) {
		return c.reel
	}
	return nil
}
