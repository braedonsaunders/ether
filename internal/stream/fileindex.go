package stream

import (
	"sort"
	"sync"

	"github.com/braedonsaunders/ether/internal/nzb"
)

// postedFile is one file of the NZB with its byte-offset index: which
// segment (article) holds which decoded byte range.
//
// yEnc parts of a post have a uniform size except the last, so before any
// article is fetched the index assumes part i covers [i*P, (i+1)*P). Every
// fetched article carries its exact =ypart range, which pins that slot
// ("anchor"); slots between anchors are re-estimated from the nearest anchor
// before them, so a non-uniform post is corrected as more parts are seen.
type postedFile struct {
	idx     int // index in nzb.Files
	file    *nzb.File
	groups  []string
	segBase int // global segment number of slot 0 (cache key)
	defPart func() int64

	mu       sync.RWMutex
	slots    []slot
	anchors  []int // sorted indices of known slots
	partSize int64 // learned uniform part size, 0 = unknown
	size     int64 // exact decoded size, valid if sizeKnown
	known    bool  // size learned from a yEnc header
	assumed  int64 // size assumed by a volume layout until known
	name     string
}

// maxTailGrowth is how many slots beyond the NZB listing a yEnc part total
// may always add; beyond that a total may at most double the listed count
// (repeated headers cannot compound the growth).
const maxTailGrowth = 1024

type slot struct {
	msgID      string   // "" when the NZB lacks this part
	alts       []string // repost message-IDs of this part, tried in order when msgID is missing
	begin, end int64    // exact [begin,end) when known
	known      bool
}

func newPostedFile(idx int, f *nzb.File, segBase int, defPart func() int64) *postedFile {
	n := f.SegmentCount()
	pf := &postedFile{idx: idx, file: f, groups: f.Groups, segBase: segBase, defPart: defPart, name: f.Name}
	pf.slots = make([]slot, n)
	for _, s := range f.Segments {
		if s.Number >= 1 && s.Number <= n {
			pf.slots[s.Number-1].msgID = s.MessageID
			pf.slots[s.Number-1].alts = s.Alternates
		}
	}
	return pf
}

// numSlots is the number of parts the file has.
func (pf *postedFile) numSlots() int {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return len(pf.slots)
}

// Size returns the decoded size: exact if learned, else the assumed size,
// else an estimate from the part size.
func (pf *postedFile) Size() int64 {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return pf.sizeLocked()
}

func (pf *postedFile) sizeLocked() int64 {
	if pf.known {
		return pf.size
	}
	if pf.assumed > 0 {
		return pf.assumed
	}
	return int64(len(pf.slots)) * pf.partLocked()
}

// SizeKnown reports whether the exact size has been learned.
func (pf *postedFile) SizeKnown() bool {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return pf.known
}

func (pf *postedFile) setAssumed(n int64) {
	pf.mu.Lock()
	pf.assumed = n
	pf.mu.Unlock()
}

// partLocked is the uniform part size to estimate with.
func (pf *postedFile) partLocked() int64 {
	if pf.partSize > 0 {
		return pf.partSize
	}
	if pf.defPart != nil {
		if p := pf.defPart(); p > 0 {
			return p
		}
	}
	// Typical Usenet article payload; only used before anything is known.
	return 716800
}

// rangeOf returns the (estimated or exact) decoded range of slot i.
func (pf *postedFile) rangeOf(i int) (begin, end int64, exact bool) {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return pf.rangeLocked(i)
}

func (pf *postedFile) rangeLocked(i int) (int64, int64, bool) {
	s := pf.slots[i]
	if s.known {
		return s.begin, s.end, true
	}
	p := pf.partLocked()
	size := pf.sizeLocked()
	// Nearest anchor before i.
	k := sort.SearchInts(pf.anchors, i) // first anchor >= i (i itself is not an anchor)
	var begin int64
	if k > 0 {
		a := pf.anchors[k-1]
		begin = pf.slots[a].end + int64(i-a-1)*p
	} else {
		begin = int64(i) * p
	}
	// Keep estimates monotonic and below the next anchor.
	if k < len(pf.anchors) {
		next := pf.slots[pf.anchors[k]].begin
		if begin > next {
			begin = next
		}
	}
	if begin > size {
		begin = size
	}
	end := begin + p
	if i == len(pf.slots)-1 || end > size {
		end = size
	}
	if k < len(pf.anchors) {
		if next := pf.slots[pf.anchors[k]].begin; end > next {
			end = next
		}
	}
	if end < begin {
		end = begin
	}
	return begin, end, false
}

// locate returns the slot whose (estimated) range contains off.
func (pf *postedFile) locate(off int64) int {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	n := len(pf.slots)
	// Largest i with begin(i) <= off. Estimates are non-decreasing in i.
	i := sort.Search(n, func(i int) bool {
		b, _, _ := pf.rangeLocked(i)
		return b > off
	}) - 1
	if i < 0 {
		i = 0
	}
	// Skip empty estimated slots that end at or before off.
	for i < n-1 {
		_, e, _ := pf.rangeLocked(i)
		if e > off {
			break
		}
		i++
	}
	return i
}

// slotsFor lists the slots that (by current estimates) cover [off, off+n).
func (pf *postedFile) slotsFor(off, n int64) []int {
	if n <= 0 {
		return nil
	}
	first := pf.locate(off)
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	var out []int
	for i := first; i < len(pf.slots); i++ {
		b, _, _ := pf.rangeLocked(i)
		if b >= off+n && len(out) > 0 {
			break
		}
		out = append(out, i)
	}
	return out
}

// learn records the exact range of slot i from its yEnc header. begin/end
// are 0-based [begin,end); fileSize and total may be 0 when unknown. It
// reports whether the slot's range differed from the estimate.
func (pf *postedFile) learn(i int, begin, end, fileSize int64, total int) (changed bool) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if listed := pf.file.SegmentCount(); total > len(pf.slots) && total-listed <= max(listed, maxTailGrowth) {
		// The post has more parts than the NZB lists: the tail is missing.
		// The total comes from an untrusted yEnc header; an absurd one must
		// not allocate an enormous slot table.
		pf.slots = append(pf.slots, make([]slot, total-len(pf.slots))...)
	}
	if fileSize > 0 && (!pf.known || pf.size != fileSize) {
		pf.size, pf.known = fileSize, true
		changed = true
	}
	if i < 0 || i >= len(pf.slots) {
		return changed
	}
	s := &pf.slots[i]
	if s.known {
		if s.begin != begin || s.end != end {
			s.begin, s.end = begin, end
			changed = true
		}
		return changed
	}
	eb, ee, _ := pf.rangeLocked(i)
	if eb != begin || ee != end {
		changed = true
	}
	s.begin, s.end, s.known = begin, end, true
	k := sort.SearchInts(pf.anchors, i)
	pf.anchors = append(pf.anchors, 0)
	copy(pf.anchors[k+1:], pf.anchors[k:])
	pf.anchors[k] = i
	// The first full part fixes the uniform part size.
	isLast := i == len(pf.slots)-1 || (pf.known && end >= pf.size)
	if pf.partSize == 0 && !isLast && end > begin {
		pf.partSize = end - begin
	}
	return changed
}

// slotKnown reports whether slot i's exact range has been learned.
func (pf *postedFile) slotKnown(i int) bool {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return i >= 0 && i < len(pf.slots) && pf.slots[i].known
}

func (pf *postedFile) msgID(i int) string {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	if i < 0 || i >= len(pf.slots) {
		return ""
	}
	return pf.slots[i].msgID
}

// alternates returns the repost message-IDs of slot i (shared; do not
// modify).
func (pf *postedFile) alternates(i int) []string {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	if i < 0 || i >= len(pf.slots) {
		return nil
	}
	return pf.slots[i].alts
}

// fitsSlot reports whether a decoded repost article can stand in for slot
// i: its yEnc header must name this part and agree with every range and
// size the index already knows, so its bytes are cached and served exactly
// as the original's would be. An article without a part header is only
// accepted for a single-part file of matching size.
func (pf *postedFile) fitsSlot(i int, seg *segData) bool {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	if i < 0 || i >= len(pf.slots) {
		return false
	}
	if !seg.rangeExact {
		return len(pf.slots) == 1 && (!pf.known || int64(len(seg.data)) == pf.size)
	}
	if seg.total != len(pf.slots) || (pf.known && seg.fileSize != pf.size) {
		return false
	}
	if s := pf.slots[i]; s.known && (s.begin != seg.begin || s.end != seg.end()) {
		return false
	}
	if i > 0 && pf.slots[i-1].known && pf.slots[i-1].end != seg.begin {
		return false
	}
	if i+1 < len(pf.slots) && pf.slots[i+1].known && pf.slots[i+1].begin != seg.end() {
		return false
	}
	return true
}

func (pf *postedFile) learnedPartSize() int64 {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return pf.partSize
}

func (pf *postedFile) setName(n string) {
	pf.mu.Lock()
	pf.name = n
	pf.mu.Unlock()
}

func (pf *postedFile) Name() string {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return pf.name
}
