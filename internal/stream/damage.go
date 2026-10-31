package stream

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Damage recovery is deliberately narrower than arbitrary hole padding: one
// confirmed absent article, bounded by real yEnc ranges on BOTH sides, entirely
// inside an unlaced Matroska frame. Container structure is never synthesized.
// Stored archives, unknown mappings, lacing, CRC-protected masters and missing
// structural bytes retain the normal source-failure path.
const maxConcealedArticle = 1 << 20
const maxConcealedSpans = 64

type damageSpan struct{ begin, end int64 }
type damageRecovery struct {
	mu       sync.Mutex
	handler  func(offset, length int64) bool
	observed map[int64]bool
	denied   map[int]bool
	spans    []damageSpan // sorted, immutable decisions for this source's lifetime
}

// SetDamageHandler opts playback into bounded payload concealment. The handler
// receives a zero-length early notice once per missing slot, then an exact
// positive length once per safe gap; false escalates through the normal missing-article
// error. It must return promptly. Repeated/concurrent range reads reuse the
// original decision and exact zero bytes, even if the article later returns.
// Background prefetch never conceals bytes or populates caches with padding.
func (s *source) SetDamageHandler(handler func(offset, length int64) bool) {
	s.damage.mu.Lock()
	s.damage.handler = handler
	s.damage.mu.Unlock()
}

func (s *source) readRaw(ctx context.Context, p []byte, off int64, prio core.Priority) (int, error) {
	return readPlan(ctx, s.e, s.rel, s.plan, p, off, prio, s.fo)
}

func (s *source) readWithDamageRecovery(ctx context.Context, p []byte, off int64, prio core.Priority) (int, error) {
	if prio != core.PriorityLive {
		return s.readRaw(ctx, p, off, prio)
	}
	n := 0
	for n < len(p) {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		pos := off + int64(n)
		end := off + int64(len(p))
		s.damage.mu.Lock()
		enabled := s.damage.handler != nil
		if !enabled && len(s.damage.spans) == 0 {
			s.damage.mu.Unlock()
			return s.readRaw(ctx, p, off, prio)
		}
		var fill int64
		for _, span := range s.damage.spans {
			if span.end <= pos {
				continue
			}
			if span.begin <= pos {
				fill = min(end, span.end) - pos
			} else {
				end = min(end, span.begin)
			}
			break
		}
		s.damage.mu.Unlock()
		if fill > 0 {
			clear(p[n : n+int(fill)])
			n += int(fill)
			continue
		}
		m, err := s.readRaw(ctx, p[n:n+int(end-pos)], pos, prio)
		// A parallel reader may have proved a gap while this read fetched.
		s.damage.mu.Lock()
		for _, span := range s.damage.spans {
			lo, hi := max(pos, span.begin), min(pos+int64(m), span.end)
			if hi > lo {
				clear(p[n+int(lo-pos) : n+int(hi-pos)])
			}
		}
		s.damage.mu.Unlock()
		n += m
		if err != nil {
			if enabled && ctx.Err() == nil && (errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrArticleCorrupt) || errors.Is(err, ErrCorrupt)) {
				s.noticeMissing(off + int64(n))
				if errors.Is(err, core.ErrArticleMissing) && s.concealMissing(ctx, off+int64(n), prio) {
					continue
				}
			}
			return n, err
		}
		if m == 0 {
			return n, ErrCorrupt
		}
	}
	return n, nil
}

func (s *source) noticeMissing(pos int64) {
	d := &s.damage
	d.mu.Lock()
	defer d.mu.Unlock()
	// Quantize the event to its NZB slot for plain sources. Other layouts only
	// use this early signal; recovery never grants them padding permission.
	key := pos
	if s.plan.kind == kindPlain && len(s.plan.vols) == 1 {
		key = int64(s.plan.vols[0].locate(pos))
	}
	if d.observed == nil {
		d.observed = make(map[int64]bool)
	}
	if d.handler != nil && !d.observed[key] && len(d.observed) < maxConcealedSpans {
		d.observed[key] = true
		d.handler(pos, 0)
	}
}

func (s *source) concealMissing(ctx context.Context, pos int64, prio core.Priority) bool {
	d := &s.damage
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, span := range d.spans {
		if pos >= span.begin && pos < span.end {
			return true
		}
	}
	if d.handler == nil || len(d.spans) >= maxConcealedSpans || s.plan.kind != kindPlain || len(s.plan.vols) != 1 {
		return false
	}
	ext := strings.ToLower(filepath.Ext(s.plan.name))
	if ext != ".mkv" && ext != ".webm" {
		return false
	}
	// Metadata discovery is cancellable and strictly bounded. Failure to prove
	// a safe frame costs at most this budget and never grants permission to pad.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pf := s.plan.vols[0]
	i := pf.locate(pos)
	if i <= 0 || i >= pf.numSlots()-1 {
		return false
	}
	if d.denied[i] {
		return false
	}
	// Retrying an already declined gap must not consume policy budget or
	// repeat a potentially expensive structural proof for every range reader.
	accepted := false
	defer func() {
		if !accepted && ctx.Err() == nil {
			if d.denied == nil {
				d.denied = make(map[int]bool)
			}
			if len(d.denied) < maxConcealedSpans {
				d.denied[i] = true
			}
		}
	}()
	left, err := s.e.segment(ctx, s.rel, pf, i-1, prio, s.fo)
	if err != nil || !left.rangeExact {
		return false
	}
	right, err := s.e.segment(ctx, s.rel, pf, i+1, prio, s.fo)
	if err != nil || !right.rangeExact {
		return false
	}
	begin, end := left.end(), right.begin
	if begin > pos || pos >= end || end-begin > maxConcealedArticle || end <= begin {
		return false
	}
	// Reconfirm the exact slot after adjacent anchors corrected the index.
	if _, err := s.e.segment(ctx, s.rel, pf, i, prio, s.fo); !errors.Is(err, core.ErrArticleMissing) {
		return false
	}
	if !s.matroskaPayloadContains(ctx, begin, end, prio) || ctx.Err() != nil {
		return false
	}
	if !d.handler(begin, end-begin) {
		return false
	}
	accepted = true
	d.spans = append(d.spans, damageSpan{begin, end})
	sort.Slice(d.spans, func(i, j int) bool { return d.spans[i].begin < d.spans[j].begin })
	return true
}

// matroskaPayloadContains walks only structurally proved EBML element headers;
// it never searches compressed frame bytes for magic values. Segment and Cluster
// boundaries must be intact. A frame is not replaced with Void: its header may
// already have been sent to a range client, so rewriting it would change an
// existing HTTP representation. Only codec payload bytes can be concealed.
func (s *source) matroskaPayloadContains(ctx context.Context, begin, end int64, prio core.Priority) bool {
	type master struct {
		end   int64
		start int64
		id    uint64
		crc   bool
	}
	stack := []master{{end: s.Size()}}
	var pos int64
	seenEBML, seenSegment := false, false
	// Bounded header work even for a malicious file containing tiny elements.
	for count := 0; count < 16384 && pos < end; count++ {
		for len(stack) > 1 && pos == stack[len(stack)-1].end {
			stack = stack[:len(stack)-1]
		}
		parent := &stack[len(stack)-1]
		if pos >= parent.end {
			return false
		}
		id, il, _, ok := s.ebmlVint(ctx, pos, true, prio)
		if !ok {
			return false
		}
		size, sl, unknown, ok := s.ebmlVint(ctx, pos+int64(il), false, prio)
		if !ok {
			return false
		}
		data := pos + int64(il+sl)
		limit := parent.end
		if !unknown {
			if size > uint64(limit-data) || data > limit {
				return false
			}
			limit = data + int64(size)
		}
		if pos == 0 {
			if id != 0x1a45dfa3 || unknown {
				return false
			}
			seenEBML = true
		}
		if unknown && id != 0x18538067 {
			return false
		}
		switch id {
		case 0x18538067: // Segment
			if !seenEBML || seenSegment || len(stack) != 1 {
				return false
			}
			seenSegment = true
			stack = append(stack, master{end: limit, start: data, id: id})
			pos = data
		case 0x1f43b675: // Cluster
			if !seenSegment || parent.id != 0x18538067 {
				return false
			}
			// Clusters before or after the gap cannot hold it. Descending into
			// every one walks each block header and exhausts the element budget
			// a few minutes into a real file.
			if end <= data || begin >= limit {
				pos = limit
				break
			}
			stack = append(stack, master{end: limit, start: data, id: id})
			pos = data
		case 0xa0: // BlockGroup
			if parent.id != 0x1f43b675 {
				return false
			}
			stack = append(stack, master{end: limit, start: data, id: id})
			pos = data
		case 0xbf: // CRC32 master checksum: payload changes would invalidate it.
			parent.crc = true
			pos = limit
		case 0xa3, 0xa1: // SimpleBlock, Block
			if (id == 0xa3 && parent.id != 0x1f43b675) || (id == 0xa1 && parent.id != 0xa0) {
				return false
			}
			if begin >= data && end <= limit {
				for _, p := range stack {
					if p.crc || (p.id != 0 && !s.noMasterCRC(ctx, p.start, p.end, prio)) {
						return false
					}
				}
				track, tl, unknownTrack, ok := s.ebmlVint(ctx, data, false, prio)
				if !ok || unknownTrack || track == 0 {
					return false
				}
				headerEnd := data + int64(tl) + 3 // track VINT, signed timecode, flags
				if headerEnd > begin {
					return false
				}
				var flags [1]byte
				if _, err := s.readRaw(ctx, flags[:], headerEnd-1, prio); err != nil || flags[0]&0x06 != 0 {
					return false
				}
				return true
			}
			pos = limit
		default:
			pos = limit
		}
	}
	return false
}

func (s *source) ebmlVint(ctx context.Context, off int64, id bool, prio core.Priority) (uint64, int, bool, bool) {
	var b [8]byte
	if _, err := s.readRaw(ctx, b[:1], off, prio); err != nil {
		return 0, 0, false, false
	}
	width := 1
	mask := byte(0x80)
	for width <= 8 && b[0]&mask == 0 {
		width++
		mask >>= 1
	}
	if width > 8 || (id && width > 4) {
		return 0, 0, false, false
	}
	if width > 1 {
		if _, err := s.readRaw(ctx, b[1:width], off+1, prio); err != nil {
			return 0, 0, false, false
		}
	}
	value := uint64(b[0])
	if !id {
		value &= uint64(mask - 1)
	}
	for _, v := range b[1:width] {
		value = value<<8 | uint64(v)
	}
	unknown := !id && value == (uint64(1)<<uint(7*width))-1
	return value, width, unknown, true
}

// CRC-32 is allowed anywhere among a master's children, not just first. Scan
// siblings using their lengths before altering any byte covered by that master.
func (s *source) noMasterCRC(ctx context.Context, pos, end int64, prio core.Priority) bool {
	for count := 0; pos < end && count < 16384; count++ {
		id, il, _, ok := s.ebmlVint(ctx, pos, true, prio)
		if !ok || id == 0xbf {
			return false
		}
		size, sl, unknown, ok := s.ebmlVint(ctx, pos+int64(il), false, prio)
		if !ok || unknown {
			return false
		}
		data := pos + int64(il+sl)
		if data > end || size > uint64(end-data) {
			return false
		}
		pos = data + int64(size)
	}
	return pos == end
}
