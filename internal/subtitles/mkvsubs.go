package subtitles

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Published Matroska element IDs (https://www.matroska.org/technical/elements.html).
const (
	mkvEBMLID          = 0x1a45dfa3
	mkvSegmentID       = 0x18538067
	mkvSeekHeadID      = 0x114d9b74
	mkvSeekID          = 0x4dbb
	mkvSeekIDID        = 0x53ab
	mkvSeekPositionID  = 0x53ac
	mkvInfoID          = 0x1549a966
	mkvTimecodeScaleID = 0x2ad7b1
	mkvTracksID        = 0x1654ae6b
	mkvTrackEntryID    = 0xae
	mkvTrackNumberID   = 0xd7
	mkvTrackTypeID     = 0x83
	mkvCodecIDID       = 0x86
	mkvCuesID          = 0x1c53bb6b
	mkvCuePointID      = 0xbb
	mkvCueTimeID       = 0xb3
	mkvCueTrackPosID   = 0xb7
	mkvCueTrackID      = 0xf7
	mkvCueClusterPosID = 0xf1
	mkvCueRelPosID     = 0xf0
	mkvCueDurationID   = 0xb2
	mkvClusterID       = 0x1f43b675
	mkvTimestampID     = 0xe7
	mkvSimpleBlockID   = 0xa3
	mkvBlockGroupID    = 0xa0
	mkvBlockID         = 0xa1
	mkvBlockDurationID = 0x9b
)

var errMKV = errors.New("matroska subtitle index unreadable")

type mkvElem struct {
	id         uint64
	start, end int64
	hdr        int
}

func mkvVint(b []byte, id bool) (uint64, int, error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, errMKV
	}
	n, mask := 1, byte(0x80)
	for b[0]&mask == 0 {
		n++
		mask >>= 1
	}
	if n > 8 || n > len(b) || (id && n > 4) {
		return 0, 0, errMKV
	}
	v := uint64(b[0])
	if !id {
		v &= uint64(mask - 1)
	}
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, nil
}

func (r *rangeReader) mkvElement(off, end int64) (mkvElem, error) {
	n := int(min(int64(12), end-off))
	if n < 2 {
		return mkvElem{}, errMKV
	}
	b, err := r.read(off, n)
	if err != nil {
		return mkvElem{}, err
	}
	id, ni, err := mkvVint(b, true)
	if err != nil {
		return mkvElem{}, err
	}
	size, ns, err := mkvVint(b[ni:], false)
	if err != nil {
		return mkvElem{}, err
	}
	start := off + int64(ni+ns)
	if size == (uint64(1)<<uint(7*ns))-1 {
		size = uint64(end - start)
	}
	if size > uint64(end-start) {
		return mkvElem{}, errMKV
	}
	return mkvElem{id: id, start: start, end: start + int64(size), hdr: ni + ns}, nil
}

func mkvChildren(b []byte) ([]struct {
	id uint64
	b  []byte
}, error) {
	var out []struct {
		id uint64
		b  []byte
	}
	for len(b) > 0 {
		id, ni, err := mkvVint(b, true)
		if err != nil {
			return nil, err
		}
		size, ns, err := mkvVint(b[ni:], false)
		if err != nil || size > uint64(len(b)-ni-ns) {
			return nil, errMKV
		}
		out = append(out, struct {
			id uint64
			b  []byte
		}{id, b[ni+ns : ni+ns+int(size)]})
		b = b[ni+ns+int(size):]
		if len(out) > 200000 {
			return nil, errMKV
		}
	}
	return out, nil
}

func mkvUint(b []byte) uint64 {
	if len(b) > 8 {
		return 0
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

func isTextCodec(id string) bool {
	id = strings.ToUpper(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(id, "S_TEXT/"):
		return true
	case id == "S_VOBSUB" || id == "S_HDMV/PGS" || id == "S_HDMV/TEXTST":
		return false
	}
	return false
}

// ReadEmbeddedTextCues reads cue times from the first embedded TEXT
// subtitle track of a Matroska or MP4 source, using only the container
// index and bounded range reads.
func ReadEmbeddedTextCues(ctx context.Context, src io.ReaderAt, size int64) ([]Cue, error) {
	if src == nil || size < 16 {
		return nil, errMKV
	}
	r := newRangeReader(ctx, src, size)
	head, err := r.read(0, 8)
	if err != nil || len(head) < 4 {
		return nil, errMKV
	}
	if binary.BigEndian.Uint32(head) == mkvEBMLID {
		return r.mkvTextCues(size)
	}
	return r.mp4TextCues(size)
}

func (r *rangeReader) mkvTextCues(size int64) ([]Cue, error) {
	var seg mkvElem
	for off, n := int64(0), 0; off < size && n < 32; n++ {
		e, err := r.mkvElement(off, size)
		if err != nil {
			return nil, err
		}
		if e.id == mkvSegmentID {
			seg = e
			break
		}
		off = e.end
	}
	if seg.id == 0 {
		return nil, errMKV
	}
	scale := uint64(1000000)
	var (
		textTrack uint64
		cuesRaw   []byte
		seenInfo  bool
		seenTrk   bool
	)
	targets := map[uint64]int64{}
	visited := map[int64]bool{}
	inspect := func(e mkvElem) error {
		switch e.id {
		case mkvSeekHeadID, mkvInfoID, mkvTracksID, mkvCuesID:
		default:
			return nil
		}
		b, err := r.read(e.start, int(e.end-e.start))
		if err != nil {
			return err
		}
		children, err := mkvChildren(b)
		if err != nil {
			return err
		}
		switch e.id {
		case mkvCuesID:
			cuesRaw = b
		case mkvInfoID:
			seenInfo = true
			for _, c := range children {
				if c.id == mkvTimecodeScaleID {
					if v := mkvUint(c.b); v > 0 {
						scale = v
					}
				}
			}
		case mkvTracksID:
			seenTrk = true
			for _, c := range children {
				if c.id != mkvTrackEntryID {
					continue
				}
				fields, _ := mkvChildren(c.b)
				var num, typ uint64
				codec := ""
				for _, f := range fields {
					switch f.id {
					case mkvTrackNumberID:
						num = mkvUint(f.b)
					case mkvTrackTypeID:
						typ = mkvUint(f.b)
					case mkvCodecIDID:
						codec = string(f.b)
					}
				}
				if typ == 0x11 && isTextCodec(codec) && textTrack == 0 {
					textTrack = num
				}
			}
		case mkvSeekHeadID:
			for _, c := range children {
				if c.id != mkvSeekID {
					continue
				}
				fs, _ := mkvChildren(c.b)
				var id, pos uint64
				for _, f := range fs {
					if f.id == mkvSeekIDID {
						id = mkvUint(f.b)
					}
					if f.id == mkvSeekPositionID {
						pos = mkvUint(f.b)
					}
				}
				if pos < uint64(seg.end-seg.start) {
					targets[id] = seg.start + int64(pos)
				}
			}
		}
		return nil
	}
	for off, n := seg.start, 0; off < seg.end && n < 64; n++ {
		e, err := r.mkvElement(off, seg.end)
		if err != nil {
			return nil, err
		}
		if e.id == mkvClusterID {
			break
		}
		visited[off] = true
		if err = inspect(e); err != nil {
			return nil, err
		}
		if cuesRaw != nil && seenInfo && seenTrk {
			break
		}
		if e.end <= off {
			return nil, errMKV
		}
		off = e.end
	}
	for _, id := range []uint64{mkvInfoID, mkvTracksID, mkvCuesID} {
		off, ok := targets[id]
		if !ok || visited[off] {
			continue
		}
		e, err := r.mkvElement(off, seg.end)
		if err != nil || e.id != id {
			continue
		}
		if err = inspect(e); err != nil {
			return nil, err
		}
	}
	if textTrack == 0 || cuesRaw == nil || scale == 0 {
		return nil, errMKV
	}
	points, err := mkvChildren(cuesRaw)
	if err != nil {
		return nil, err
	}
	var out []Cue
	for _, p := range points {
		if p.id != mkvCuePointID {
			continue
		}
		fields, err := mkvChildren(p.b)
		if err != nil {
			continue
		}
		var stamp, dur, clusterPos, rel uint64
		hasTime, hasPos, hasRel := false, false, false
		for _, f := range fields {
			if f.id == mkvCueTimeID {
				stamp = mkvUint(f.b)
				hasTime = true
			}
		}
		if !hasTime {
			continue
		}
		for _, f := range fields {
			if f.id != mkvCueTrackPosID {
				continue
			}
			ps, err := mkvChildren(f.b)
			if err != nil {
				continue
			}
			var track uint64
			for _, q := range ps {
				switch q.id {
				case mkvCueTrackID:
					track = mkvUint(q.b)
				case mkvCueClusterPosID:
					clusterPos = mkvUint(q.b)
					hasPos = true
				case mkvCueRelPosID:
					rel = mkvUint(q.b)
					hasRel = true
				case mkvCueDurationID:
					dur = mkvUint(q.b)
				}
			}
			if track != textTrack || !hasPos {
				continue
			}
			start := time.Duration(stamp * scale)
			end := start + time.Duration(dur*scale)
			if dur == 0 {
				end = start + 2*time.Second
			}
			if end <= start {
				continue
			}
			_ = clusterPos
			_ = rel
			_ = hasRel
			out = append(out, Cue{Start: start, End: end})
		}
	}
	if len(out) == 0 {
		return nil, errMKV
	}
	return out, nil
}

func (r *rangeReader) mkvReadBlock(seg mkvElem, clusterPos, rel uint64, hasRel bool, track uint64) (string, bool) {
	clusterOff := seg.start + int64(clusterPos)
	if clusterOff < seg.start || clusterOff >= seg.end {
		return "", false
	}
	cluster, err := r.mkvElement(clusterOff, seg.end)
	if err != nil || cluster.id != mkvClusterID {
		// Try a few header lengths when the CueClusterPosition is
		// slightly off the parsed Cluster start.
		for hdr := 5; hdr <= 12; hdr++ {
			if text, ok := r.mkvBlockAt(clusterOff+int64(hdr)+int64(rel), seg.end, track); ok {
				return text, true
			}
		}
		return "", false
	}
	if hasRel {
		if text, ok := r.mkvBlockAt(cluster.start+int64(rel), cluster.end, track); ok {
			return text, true
		}
	}
	return r.mkvScanCluster(cluster, track)
}

func (r *rangeReader) mkvScanCluster(cluster mkvElem, track uint64) (string, bool) {
	for off, n := cluster.start, 0; off < cluster.end && n < 64; n++ {
		e, err := r.mkvElement(off, cluster.end)
		if err != nil {
			return "", false
		}
		if e.id == mkvSimpleBlockID || e.id == mkvBlockID {
			b, err := r.read(e.start, int(e.end-e.start))
			if err == nil {
				if text, ok := parseMKVBlock(b, track); ok {
					return text, true
				}
			}
		}
		if e.id == mkvBlockGroupID {
			body, err := r.read(e.start, int(e.end-e.start))
			if err == nil {
				if kids, kerr := mkvChildren(body); kerr == nil {
					for _, k := range kids {
						if k.id == mkvBlockID {
							if text, ok := parseMKVBlock(k.b, track); ok {
								return text, true
							}
						}
					}
				}
			}
		}
		if e.end <= off {
			return "", false
		}
		off = e.end
	}
	return "", false
}

func (r *rangeReader) mkvBlockAt(off, end int64, track uint64) (string, bool) {
	if off < 0 || off >= end {
		return "", false
	}
	e, err := r.mkvElement(off, end)
	if err != nil {
		return "", false
	}
	if e.id != mkvSimpleBlockID && e.id != mkvBlockID && e.id != mkvBlockGroupID {
		return "", false
	}
	body, err := r.read(e.start, int(e.end-e.start))
	if err != nil {
		return "", false
	}
	if e.id == mkvBlockGroupID {
		kids, err := mkvChildren(body)
		if err != nil {
			return "", false
		}
		for _, k := range kids {
			if k.id == mkvBlockID {
				return parseMKVBlock(k.b, track)
			}
		}
		return "", false
	}
	return parseMKVBlock(body, track)
}

func parseMKVBlock(b []byte, want uint64) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	track, n, err := mkvVint(b, false)
	if err != nil || track != want || n+3 > len(b) {
		return "", false
	}
	text := string(bytes.Trim(b[n+3:], "\x00"))
	text = strings.TrimSpace(text)
	if text == "" || !utf8.ValidString(text) {
		return "", false
	}
	return text, true
}
