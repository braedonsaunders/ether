package livetv

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Schedule is deterministic across restarts and query windows. A shuffle is a
// stable hash ordering, not process-global randomness.
//
// Channels saved before weekly blocks and playout modes take legacySchedule,
// which is the original day-parting loop: blocks are UTC minutes every day,
// a shuffle is one stable permutation, and the last item in a block is clipped.
// That path is what keeps an existing channel's guide identical.
func Schedule(c Channel, from, until time.Time) []Programme {
	if !until.After(from) || c.Kind != "virtual" {
		return nil
	}
	if c.Epoch.IsZero() {
		c.Epoch = time.Unix(0, 0).UTC()
	}
	if legacyCalendar(c) {
		return legacySchedule(c, from, until)
	}
	return weeklySchedule(c, from, until)
}

// legacyCalendar is true when every scheduling field is the zero value the
// original engine understood. Weekdays, playout mode, alignment, filler and
// repeat gaps opt into weeklySchedule. A timezone names the clock for painted
// blocks; with no blocks a 24/7 channel stays on the original epoch loop.
func legacyCalendar(c Channel) bool {
	if c.Mode != "" || c.Align != "" || c.Fit != "" || c.RepeatDays > 0 || len(c.Filler) > 0 || c.MarathonKey != 0 {
		return false
	}
	// Fallback and off-air only change a channel that has nothing else to play.
	// A channel that already has items keeps the original loop.
	if len(c.Items) == 0 && (c.OffAir || len(c.Fallback) > 0) {
		return false
	}
	if c.Timezone != "" && len(c.Blocks) > 0 {
		return false
	}
	for _, b := range c.Blocks {
		if len(b.Days) > 0 || b.Mode != "" || b.Align != "" || b.Fit != "" || b.RepeatDays > 0 || len(b.Filler) > 0 || b.MarathonKey != 0 {
			return false
		}
	}
	return true
}

func legacySchedule(c Channel, from, until time.Time) []Programme {
	var out []Programme
	emit := func(items []Item, shuffle bool, anchor, end time.Time, salt int64) {
		emitCycle(&out, c, items, nil, shuffle, anchor, end, from, until, salt, "")
	}
	if len(c.Blocks) == 0 {
		emit(c.Items, c.Shuffle, c.Epoch, until.Add(24*time.Hour), 0)
	} else {
		day := time.Date(from.UTC().Year(), from.UTC().Month(), from.UTC().Day(), 0, 0, 0, 0, time.UTC)
		for ; day.Before(until); day = day.AddDate(0, 0, 1) {
			blocks := append([]Block(nil), c.Blocks...)
			sort.Slice(blocks, func(i, j int) bool { return blocks[i].Start < blocks[j].Start })
			prev := 0
			for _, b := range blocks {
				if b.Start > prev {
					emit(c.Items, c.Shuffle, day.Add(time.Duration(prev)*time.Minute), day.Add(time.Duration(b.Start)*time.Minute), day.Unix())
				}
				emit(b.Items, b.Shuffle, day.Add(time.Duration(b.Start)*time.Minute), day.Add(time.Duration(b.End)*time.Minute), day.Unix()+int64(b.Start))
				prev = b.End
			}
			if prev < 1440 {
				emit(c.Items, c.Shuffle, day.Add(time.Duration(prev)*time.Minute), day.Add(24*time.Hour), day.Unix())
			}
		}
	}
	return out
}

// emitCycle plays items back to back from anchor, optionally in a stable
// shuffle, clipping the item that would run past end. from/until only decide
// which programmes are returned; the cursor jumps whole cycles so a narrow
// window sees the same programme as a wide one.
func emitCycle(out *[]Programme, c Channel, items, filler []Item, shuffle bool, anchor, end, from, until time.Time, salt int64, mode string) {
	if !end.After(from) || !until.After(anchor) {
		return
	}
	ordered := arrange(items, filler, mode, shuffle, c.Seed, salt, 0)
	if len(ordered) == 0 {
		emitSlate(out, c, anchor, end, from, until)
		return
	}
	var cycle time.Duration
	for _, item := range ordered {
		if item.DurationMS <= 0 || item.Slate == offAirSlate {
			return
		}
		cycle += time.Duration(item.DurationMS) * time.Millisecond
	}
	if cycle <= 0 {
		return
	}
	cursor := anchor
	if from.After(cursor) {
		cursor = cursor.Add((from.Sub(cursor) / cycle) * cycle)
	}
	for cursor.Before(until) && cursor.Before(end) && len(*out) < 20000 {
		for _, item := range ordered {
			finish := cursor.Add(time.Duration(item.DurationMS) * time.Millisecond)
			if finish.After(end) {
				finish = end
			}
			appendProgramme(out, c, item, cursor, finish, from, until)
			cursor = finish
			if !cursor.Before(end) || !cursor.Before(until) {
				break
			}
		}
	}
}

func weeklySchedule(c Channel, from, until time.Time) []Programme {
	loc := zoneOf(c.Timezone)
	var out []Programme
	// Repeat gaps remember what aired before the query window. Simulating
	// that lookback (and no further) keeps two overlapping queries equal.
	origin := from.Add(-lookback(c))
	day := dayStart(origin, loc)
	if day.After(origin) {
		day = day.AddDate(0, 0, -1)
	}
	recent := map[core.RatingKey]time.Time{}
	for ; day.Before(until); day = day.AddDate(0, 0, 1) {
		weekday := int(day.Weekday())
		blocks := make([]Block, 0, len(c.Blocks))
		for _, b := range c.Blocks {
			if blockOnDay(b, weekday) {
				blocks = append(blocks, b)
			}
		}
		sort.Slice(blocks, func(i, j int) bool { return blocks[i].Start < blocks[j].Start })
		prev := 0
		for _, b := range blocks {
			if b.Start > prev {
				emitSegment(&out, c, segment{items: c.Items, filler: c.Filler, shuffle: c.Shuffle, mode: c.Mode, align: c.Align, fit: c.Fit, repeat: c.RepeatDays, marathon: c.MarathonKey, fallback: c.Fallback, offAir: c.OffAir}, day, prev, b.Start, from, until, loc, day.Unix(), recent)
			}
			emitSegment(&out, c, segment{items: b.Items, filler: orFiller(b.Filler, c.Filler), shuffle: b.Shuffle, mode: orMode(b.Mode, c.Mode), align: orText(b.Align, c.Align), fit: orText(b.Fit, c.Fit), repeat: orInt(b.RepeatDays, c.RepeatDays), marathon: orKey(b.MarathonKey, c.MarathonKey), fallback: c.Fallback, offAir: c.OffAir}, day, b.Start, b.End, from, until, loc, day.Unix()+int64(b.Start), recent)
			prev = b.End
		}
		if prev < 1440 {
			emitSegment(&out, c, segment{items: c.Items, filler: c.Filler, shuffle: c.Shuffle, mode: c.Mode, align: c.Align, fit: c.Fit, repeat: c.RepeatDays, marathon: c.MarathonKey, fallback: c.Fallback, offAir: c.OffAir}, day, prev, 1440, from, until, loc, day.Unix(), recent)
		}
	}
	return out
}

func lookback(c Channel) time.Duration {
	d := c.RepeatDays
	for _, b := range c.Blocks {
		if b.RepeatDays > d {
			d = b.RepeatDays
		}
	}
	if d < 0 {
		d = 0
	}
	if d > 30 {
		d = 30
	}
	return time.Duration(d) * 24 * time.Hour
}

type segment struct {
	items, filler, fallback []Item
	shuffle                 bool
	mode, align, fit        string
	repeat                  int
	marathon                core.RatingKey
	offAir                  bool
}

func emitSegment(out *[]Programme, c Channel, seg segment, day time.Time, startMin, endMin int, from, until time.Time, loc *time.Location, salt int64, recent map[core.RatingKey]time.Time) {
	anchor := atMinute(day, startMin, loc)
	end := atMinute(day, endMin, loc)
	items := seg.items
	if len(items) == 0 {
		items = seg.fallback
	}
	if len(items) == 0 {
		if seg.offAir || seg.mode != "" || seg.align != "" || seg.repeat > 0 {
			emitSlate(out, c, anchor, end, from, until)
		}
		return
	}
	if needsWalk(seg) {
		emitWalk(out, c, items, seg, anchor, end, from, until, loc, salt, recent)
		return
	}
	emitCycle(out, c, items, seg.filler, seg.shuffle || seg.mode == "shuffle", anchor, end, from, until, salt, seg.mode)
}

func needsWalk(seg segment) bool {
	return seg.mode == "random" || seg.align != "" || seg.repeat > 0
}

const (
	offAirSlate = "offair"
	offAirTitle = "Off air"
	maxWalk     = 8_000_000
)

func emitWalk(out *[]Programme, c Channel, items []Item, seg segment, anchor, end, from, until time.Time, loc *time.Location, salt int64, recent map[core.RatingKey]time.Time) {
	// Lookback days end before `from`. They still have to run so a repeat
	// gap remembers them; appendProgramme drops them from the result.
	if !end.After(anchor) || !until.After(anchor) {
		return
	}
	random := seg.mode == "random"
	seq := arrange(items, nil, seg.mode, seg.shuffle || seg.mode == "shuffle", c.Seed, salt, seg.marathon)
	var units [][]Item
	var pos []int
	if random {
		units = unitsOf(arrange(items, nil, "order", false, c.Seed, salt, seg.marathon))
		pos = make([]int, len(units))
		if len(units) == 0 {
			fillGap(out, c, seg.filler, anchor, end, from, until)
			return
		}
	} else if len(seq) == 0 {
		fillGap(out, c, seg.filler, anchor, end, from, until)
		return
	}
	gap := time.Duration(seg.repeat) * 24 * time.Hour
	cursor := anchor
	step := int64(0)
	seqAt := 0
	var last core.RatingKey
	alignStep := alignDuration(seg.align)
	for cursor.Before(end) && len(*out) < 20000 && step < maxWalk {
		if alignStep > 0 {
			start := snapForward(cursor, alignStep, loc)
			if start.After(cursor) {
				if !start.Before(end) {
					fillGap(out, c, seg.filler, cursor, end, from, until)
					return
				}
				fillGap(out, c, seg.filler, cursor, start, from, until)
				cursor = start
			}
		}
		var item Item
		var ok bool
		if random {
			item, ok = takeRandom(units, pos, c.Seed, salt, step, cursor, recent, gap, last)
		} else {
			item, seqAt, ok = takeSequence(seq, seqAt, cursor, recent, gap)
		}
		if !ok {
			ready := earliestReady(playedPool(random, units, seq), recent, gap, cursor)
			if !ready.After(cursor) || !ready.Before(end) {
				fillGap(out, c, seg.filler, cursor, end, from, until)
				return
			}
			fillGap(out, c, seg.filler, cursor, ready, from, until)
			cursor = ready
			continue
		}
		dur := time.Duration(item.DurationMS) * time.Millisecond
		if dur <= 0 {
			step++
			continue
		}
		finish := cursor.Add(dur)
		if seg.fit != "pad" && alignStep > 0 {
			if boundary := snapForward(cursor.Add(time.Nanosecond), alignStep, loc); finish.After(boundary) {
				finish = boundary
			}
		}
		if finish.After(end) {
			finish = end
		}
		if !finish.After(cursor) {
			return
		}
		appendProgramme(out, c, item, cursor, finish, from, until)
		recent[item.Key] = cursor
		last = item.Key
		cursor = finish
		step++
		if alignStep == 0 && len(seg.filler) > 0 && cursor.Before(end) {
			bump := seg.filler[int(step)%len(seg.filler)]
			bd := time.Duration(bump.DurationMS) * time.Millisecond
			if bd > 0 {
				bf := cursor.Add(bd)
				if bf.After(end) {
					bf = end
				}
				appendProgramme(out, c, bump, cursor, bf, from, until)
				cursor = bf
			}
		}
	}
}

func playedPool(random bool, units [][]Item, seq []Item) [][]Item {
	if random {
		return units
	}
	return [][]Item{seq}
}

func takeSequence(seq []Item, at int, cursor time.Time, recent map[core.RatingKey]time.Time, gap time.Duration) (Item, int, bool) {
	if len(seq) == 0 {
		return Item{}, at, false
	}
	for k := 0; k < len(seq); k++ {
		j := (at + k) % len(seq)
		it := seq[j]
		if gap > 0 {
			if played, ok := recent[it.Key]; ok && cursor.Sub(played) < gap {
				continue
			}
		}
		return it, (j + 1) % len(seq), true
	}
	return Item{}, at, false
}

func takeRandom(units [][]Item, pos []int, seed, salt, step int64, cursor time.Time, recent map[core.RatingKey]time.Time, gap time.Duration, last core.RatingKey) (Item, bool) {
	open := openUnits(units, pos)
	if len(open) == 0 {
		for i := range pos {
			pos[i] = 0
		}
		open = openUnits(units, pos)
	}
	if len(open) == 0 {
		return Item{}, false
	}
	start := int(stepHash(seed, salt, step) % uint64(len(open)))
	for k := 0; k < len(open); k++ {
		u := open[(start+k)%len(open)]
		it := units[u][pos[u]]
		if len(open) > 1 && it.Key == last && k == 0 {
			continue
		}
		if gap > 0 {
			if played, ok := recent[it.Key]; ok && cursor.Sub(played) < gap {
				continue
			}
		}
		pos[u]++
		return it, true
	}
	return Item{}, false
}

func openUnits(units [][]Item, pos []int) []int {
	var open []int
	for u := range units {
		if pos[u] < len(units[u]) {
			open = append(open, u)
		}
	}
	return open
}

func earliestReady(units [][]Item, recent map[core.RatingKey]time.Time, gap time.Duration, cursor time.Time) time.Time {
	var ready time.Time
	for _, u := range units {
		for _, it := range u {
			at, ok := recent[it.Key]
			if !ok {
				return cursor
			}
			next := at.Add(gap)
			if next.After(cursor) && (ready.IsZero() || next.Before(ready)) {
				ready = next
			}
		}
	}
	return ready
}

func fillGap(out *[]Programme, c Channel, filler []Item, fromCursor, end, winFrom, winUntil time.Time) {
	cursor := fromCursor
	if len(filler) == 0 {
		emitSlate(out, c, cursor, end, winFrom, winUntil)
		return
	}
	i := 0
	for cursor.Before(end) && len(*out) < 20000 {
		item := filler[i%len(filler)]
		i++
		dur := time.Duration(item.DurationMS) * time.Millisecond
		if dur <= 0 {
			emitSlate(out, c, cursor, end, winFrom, winUntil)
			return
		}
		finish := cursor.Add(dur)
		if finish.After(end) {
			finish = end
		}
		appendProgramme(out, c, item, cursor, finish, winFrom, winUntil)
		cursor = finish
	}
}

func emitSlate(out *[]Programme, c Channel, anchor, end, from, until time.Time) {
	if !end.After(anchor) {
		return
	}
	appendProgramme(out, c, Item{Title: offAirTitle, Slate: offAirSlate, DurationMS: end.Sub(anchor).Milliseconds()}, anchor, end, from, until)
}

func appendProgramme(out *[]Programme, c Channel, item Item, cursor, finish, from, until time.Time) {
	if finish.After(from) && cursor.Before(until) && finish.After(cursor) {
		*out = append(*out, Programme{ID: fmt.Sprintf("%s-%d", c.ID, cursor.UnixMilli()), ChannelID: c.ID, Start: cursor, End: finish, Item: item})
	}
}

func arrange(items, filler []Item, mode string, shuffle bool, seed, salt int64, marathon core.RatingKey) []Item {
	src := append([]Item(nil), items...)
	if marathon != 0 || mode == "marathon" {
		src = marathonItems(src, marathon)
	}
	var ordered []Item
	switch mode {
	case "shuffle":
		ordered = flatten(shuffleUnits(unitsOf(src), seed, salt))
	case "newest":
		ordered = flatten(newestUnits(unitsOf(src)))
	case "marathon", "order":
		ordered = flatten(unitsOf(src))
	case "random":
		ordered = src
	default:
		if shuffle {
			ordered = shuffleItems(src, seed, salt)
		} else {
			ordered = src
		}
	}
	if len(filler) == 0 || len(ordered) == 0 || mode == "random" {
		return ordered
	}
	var out []Item
	for i, it := range ordered {
		out = append(out, it)
		out = append(out, filler[i%len(filler)])
	}
	return out
}

func marathonItems(items []Item, key core.RatingKey) []Item {
	if key == 0 {
		for _, it := range items {
			if it.ShowKey != 0 {
				key = it.ShowKey
				break
			}
		}
	}
	if key == 0 {
		return items
	}
	var out []Item
	for _, it := range items {
		if it.ShowKey == key || it.Key == key {
			out = append(out, it)
		}
	}
	if len(out) == 0 {
		return items
	}
	return out
}

func unitsOf(items []Item) [][]Item {
	var out [][]Item
	index := map[core.RatingKey]int{}
	for _, it := range items {
		if it.ShowKey == 0 {
			out = append(out, []Item{it})
			continue
		}
		if i, ok := index[it.ShowKey]; ok {
			out[i] = append(out[i], it)
			continue
		}
		index[it.ShowKey] = len(out)
		out = append(out, []Item{it})
	}
	for i := range out {
		if len(out[i]) < 2 {
			continue
		}
		sort.SliceStable(out[i], func(a, b int) bool {
			ea, eb := out[i][a], out[i][b]
			if ea.Season != eb.Season {
				return ea.Season < eb.Season
			}
			if ea.Episode != eb.Episode {
				return ea.Episode < eb.Episode
			}
			return ea.Key < eb.Key
		})
	}
	return out
}

func shuffleUnits(units [][]Item, seed, salt int64) [][]Item {
	out := append([][]Item(nil), units...)
	sort.SliceStable(out, func(i, j int) bool {
		return orderHash(seed+salt, out[i][0]) < orderHash(seed+salt, out[j][0])
	})
	return out
}

func newestUnits(units [][]Item) [][]Item {
	out := append([][]Item(nil), units...)
	newest := func(u []Item) int64 {
		var m int64
		for _, it := range u {
			if it.ReleasedMS > m {
				m = it.ReleasedMS
			}
		}
		return m
	}
	sort.SliceStable(out, func(i, j int) bool {
		ni, nj := newest(out[i]), newest(out[j])
		if ni != nj {
			return ni > nj
		}
		return out[i][0].Title < out[j][0].Title
	})
	return out
}

func flatten(units [][]Item) []Item {
	var out []Item
	for _, u := range units {
		out = append(out, u...)
	}
	return out
}

func shuffleItems(items []Item, seed, salt int64) []Item {
	ordered := append([]Item(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return orderHash(seed+salt, ordered[i]) < orderHash(seed+salt, ordered[j])
	})
	return ordered
}

func orderHash(seed int64, item Item) uint64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%s", seed, item.Key, item.Title)))
	return binary.BigEndian.Uint64(h[:8])
}

func stepHash(seed, salt, step int64) uint64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("r/%d/%d/%d", seed, salt, step)))
	return binary.BigEndian.Uint64(h[:8])
}

func zoneOf(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

func dayStart(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
}

func atMinute(day time.Time, minute int, loc *time.Location) time.Time {
	if minute >= 1440 {
		if loc == time.UTC {
			return day.Add(24 * time.Hour)
		}
		return day.AddDate(0, 0, 1)
	}
	return day.Add(time.Duration(minute) * time.Minute)
}

func blockOnDay(b Block, weekday int) bool {
	if len(b.Days) == 0 {
		return true
	}
	for _, d := range b.Days {
		if d == weekday {
			return true
		}
	}
	return false
}

func alignDuration(align string) time.Duration {
	switch align {
	case "hour":
		return time.Hour
	case "half":
		return 30 * time.Minute
	default:
		return 0
	}
}

func snapForward(t time.Time, step time.Duration, loc *time.Location) time.Time {
	lt := t.In(loc)
	base := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), 0, 0, 0, loc)
	if step == 30*time.Minute && lt.Minute() >= 30 {
		base = base.Add(30 * time.Minute)
	}
	if !base.Before(t) && lt.Second() == 0 && lt.Nanosecond() == 0 && lt.Minute()%int(step/time.Minute) == 0 {
		return t
	}
	next := base
	if !next.After(t) {
		next = next.Add(step)
	}
	return next
}

func orText(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func orMode(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func orInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
func orKey(a, b core.RatingKey) core.RatingKey {
	if a != 0 {
		return a
	}
	return b
}
func orFiller(a, b []Item) []Item {
	if len(a) > 0 {
		return a
	}
	return b
}
