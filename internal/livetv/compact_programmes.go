package livetv

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Time rows retain no media metadata or GC-scanned pointers. The immutable
// channel item table and templates are shared by all rows and representations.
type compactProgrammeRow struct {
	start, end int64
	meta       uint32
}
type compactProgrammeMeta struct {
	item                                            uint32
	blockID, sourceID, rule, reason, lens           string
	id, channelID, graphicsID                       string
	pinned, deriveID, deriveChannel, deriveGraphics bool
	graphicsFallback                                bool
	hasStartException, hasEndException              bool
	sourceOffset                                    time.Duration
	policyTag                                       *broadcastPolicyTag
	graphicsRequest                                 *core.ClipRequest
	startLoc, endLoc                                *time.Location
	startException, endException                    time.Time
}
type compactItemIdentity struct {
	key                core.RatingKey
	slate, clip, title string
}
type compactItemTable struct {
	items []Item
	index map[compactItemIdentity][]uint32
}
type compactProgrammes struct {
	rows  []compactProgrammeRow
	meta  []compactProgrammeMeta
	items *compactItemTable
}

func (t *compactItemTable) intern(item Item) uint32 {
	if t.index == nil {
		t.index = map[compactItemIdentity][]uint32{}
	}
	key := compactItemIdentity{item.Key, item.Slate, item.ReelClipID, item.Title}
	for _, index := range t.index[key] {
		if reflect.DeepEqual(t.items[index], item) {
			return index
		}
	}
	index := uint32(len(t.items))
	t.items = append(t.items, item)
	t.index[key] = append(t.index[key], index)
	return index
}
func compactProgrammeSlice(channelID string, ps []Programme, items *compactItemTable) *compactProgrammes {
	out := &compactProgrammes{rows: make([]compactProgrammeRow, len(ps)), items: items}
	index := map[compactProgrammeMeta]uint32{}
	for i, p := range ps {
		expected := fmt.Sprintf("%s-%d", channelID, p.Start.UnixMilli())
		m := compactProgrammeMeta{item: items.intern(p.Item), blockID: p.BlockID, sourceID: p.SourceID, rule: p.Rule, reason: p.Reason, lens: p.Lens, id: p.ID, channelID: p.ChannelID, graphicsID: p.graphicsID, pinned: p.Pinned, sourceOffset: p.sourceOffset, policyTag: p.policyTag, graphicsRequest: p.graphicsRequest, graphicsFallback: p.graphicsFallback, startLoc: p.Start.Location(), endLoc: p.End.Location()}
		if p.ID == expected {
			m.id = ""
			m.deriveID = true
		}
		if p.ChannelID == channelID {
			m.channelID = ""
			m.deriveChannel = true
		}
		if p.graphicsID == expected {
			m.graphicsID = ""
			m.deriveGraphics = true
		}
		if p.Start != p.Start.Round(0) || !time.Unix(0, p.Start.UnixNano()).Equal(p.Start) {
			m.startException = p.Start
			m.hasStartException = true
		}
		if p.End != p.End.Round(0) || !time.Unix(0, p.End.UnixNano()).Equal(p.End) {
			m.endException = p.End
			m.hasEndException = true
		}
		at, ok := index[m]
		if !ok {
			at = uint32(len(out.meta))
			out.meta = append(out.meta, m)
			index[m] = at
		}
		out.rows[i] = compactProgrammeRow{p.Start.UnixNano(), p.End.UnixNano(), at}
	}
	return out
}
func (s *compactProgrammes) slice(channelID string, from, until time.Time) []Programme {
	if s == nil {
		return nil
	}
	first := sort.Search(len(s.rows), func(i int) bool { return s.end(i).After(from) })
	last := first + sort.Search(len(s.rows)-first, func(i int) bool { return !s.start(first + i).Before(until) })
	if first == last {
		return nil
	}
	out := make([]Programme, last-first)
	for i := range out {
		row := s.rows[first+i]
		m := s.meta[row.meta]
		p := Programme{ID: m.id, ChannelID: m.channelID, Start: s.start(first + i), End: s.end(first + i), Item: s.items.items[m.item], BlockID: m.blockID, SourceID: m.sourceID, Rule: m.rule, Reason: m.reason, Lens: m.lens, Pinned: m.pinned, sourceOffset: m.sourceOffset, policyTag: m.policyTag, graphicsRequest: m.graphicsRequest, graphicsID: m.graphicsID, graphicsFallback: m.graphicsFallback}
		if m.deriveChannel {
			p.ChannelID = channelID
		}
		if m.deriveID {
			p.ID = fmt.Sprintf("%s-%d", channelID, p.Start.UnixMilli())
		}
		if m.deriveGraphics {
			p.graphicsID = fmt.Sprintf("%s-%d", channelID, p.Start.UnixMilli())
		}
		out[i] = p
	}
	return out
}
func (s *compactProgrammes) start(i int) time.Time {
	m := s.meta[s.rows[i].meta]
	if m.hasStartException {
		return m.startException
	}
	return time.Unix(0, s.rows[i].start).In(m.startLoc)
}
func (s *compactProgrammes) end(i int) time.Time {
	m := s.meta[s.rows[i].meta]
	if m.hasEndException {
		return m.endException
	}
	return time.Unix(0, s.rows[i].end).In(m.endLoc)
}
