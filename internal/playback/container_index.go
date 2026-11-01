package playback

// Container metadata is untrusted. Indexing reads only bounded headers/tables;
// media payloads are skipped by offset and malformed/unsupported files retain
// the duration-based TimeMap fallback.
import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"io"
	"sort"
	"time"
)

const indexBudget = 8 << 20
const maxIndexEntries = 1000000

var errIndex = errors.New("playback: unsupported or invalid container index")

type indexReader struct {
	ctx  context.Context
	src  io.ReaderAt
	size int64
	left int
}

func (r *indexReader) read(off int64, n int) ([]byte, error) {
	if r.ctx.Err() != nil {
		return nil, r.ctx.Err()
	}
	if off < 0 || n < 0 || off > r.size-int64(n) || n > r.left {
		return nil, errIndex
	}
	r.left -= n
	b := make([]byte, n)
	var err error
	if cr, ok := r.src.(core.MetadataReaderAt); ok {
		_, err = cr.ReadMetadataAtContext(r.ctx, b, off)
	} else if cr, ok := r.src.(core.ContextReaderAt); ok {
		_, err = cr.ReadAtContext(r.ctx, b, off)
	} else {
		_, err = r.src.ReadAt(b, off)
	}
	return b, err
}
func containerIndex(ctx context.Context, src io.ReaderAt, size int64) []IndexPoint {
	r := &indexReader{ctx: ctx, src: src, size: size, left: indexBudget}
	b, err := r.read(0, 4)
	if err != nil {
		return nil
	}
	var pts []IndexPoint
	if binary.BigEndian.Uint32(b) == 0x1a45dfa3 {
		pts, _ = r.matroska()
	} else {
		pts, _ = r.mp4()
	}
	sort.Slice(pts, func(i, j int) bool {
		if pts[i].Offset == pts[j].Offset {
			return pts[i].Time < pts[j].Time
		}
		return pts[i].Offset < pts[j].Offset
	})
	out := pts[:0]
	for _, p := range pts {
		if p.Offset < 0 || p.Offset >= size || p.Time < 0 {
			continue
		}
		if len(out) > 0 && (p.Offset <= out[len(out)-1].Offset || p.Time < out[len(out)-1].Time) {
			continue
		}
		out = append(out, p)
	}
	return out
}

type atom struct {
	kind string
	data []byte
}

func atoms(b []byte) ([]atom, error) {
	var out []atom
	for len(b) > 0 {
		if len(b) < 8 {
			return nil, errIndex
		}
		n := uint64(binary.BigEndian.Uint32(b))
		h := 8
		if n == 1 {
			if len(b) < 16 {
				return nil, errIndex
			}
			n = binary.BigEndian.Uint64(b[8:])
			h = 16
		}
		if n == 0 {
			n = uint64(len(b))
		}
		if n < uint64(h) || n > uint64(len(b)) {
			return nil, errIndex
		}
		out = append(out, atom{string(b[4:8]), b[h:int(n)]})
		b = b[int(n):]
		if len(out) > maxIndexEntries {
			return nil, errIndex
		}
	}
	return out, nil
}
func child(b []byte, k string) []byte {
	as, e := atoms(b)
	if e != nil {
		return nil
	}
	for _, a := range as {
		if a.kind == k {
			return a.data
		}
	}
	return nil
}
func (r *indexReader) mp4() ([]IndexPoint, error) {
	var sidxPts []IndexPoint // fragmented MP4 fallback (sidx)
	for off, n := int64(0), 0; off < r.size && n < 1024; n++ {
		h, e := r.read(off, 8)
		if e != nil {
			return nil, e
		}
		size := int64(binary.BigEndian.Uint32(h))
		head := int64(8)
		if size == 1 {
			x, e := r.read(off+8, 8)
			if e != nil {
				return nil, e
			}
			u := binary.BigEndian.Uint64(x)
			if u > uint64(r.size) {
				return nil, errIndex
			}
			size = int64(u)
			head = 16
		}
		if size == 0 {
			size = r.size - off
		}
		if size < head || size > r.size-off {
			return nil, errIndex
		}
		if string(h[4:8]) == "moov" {
			b, e := r.read(off+head, int(size-head))
			if e != nil {
				return nil, e
			}
			as, e := atoms(b)
			if e != nil {
				return nil, e
			}
			for _, a := range as {
				if a.kind == "trak" {
					if p, e := mp4Track(a.data, r.size, mp4Scale(child(b, "mvhd"))); e == nil && len(p) > 0 {
						return p, nil
					}
				}
			}
			// No usable sync-sample table (fragmented MP4 keeps empty
			// sample tables): keep scanning for an sidx.
		}
		if string(h[4:8]) == "sidx" && sidxPts == nil && size-head <= 1<<20 {
			b, e := r.read(off+head, int(size-head))
			if e != nil {
				return nil, e
			}
			sidxPts, _ = parseSidx(b, off+size, r.size)
		}
		off += size
	}
	if len(sidxPts) > 0 {
		return sidxPts, nil
	}
	return nil, errIndex
}

// parseSidx turns a Segment Index Box into points at each referenced
// sub-segment: first_offset counts from the first byte after the sidx
// box (base), times accumulate sub-segment durations over the timescale.
func parseSidx(b []byte, base, fileSize int64) ([]IndexPoint, error) {
	if len(b) < 12 {
		return nil, errIndex
	}
	version := b[0]
	timescale := u32(b, 8)
	if timescale == 0 {
		return nil, errIndex
	}
	var ept, first uint64
	pos := 12
	if version == 0 {
		if len(b) < pos+12 {
			return nil, errIndex
		}
		ept = uint64(u32(b, pos))
		first = uint64(u32(b, pos+4))
		pos += 8
	} else if version == 1 {
		if len(b) < pos+20 {
			return nil, errIndex
		}
		ept = binary.BigEndian.Uint64(b[pos:])
		first = binary.BigEndian.Uint64(b[pos+8:])
		pos += 16
	} else {
		return nil, errIndex
	}
	count := int(binary.BigEndian.Uint16(b[pos+2:]))
	pos += 4
	if count == 0 || count > maxIndexEntries || pos+count*12 > len(b) {
		return nil, errIndex
	}
	off := base + int64(first)
	stamp := ept
	var pts []IndexPoint
	for i := 0; i < count; i++ {
		ref := b[pos+i*12:]
		size := u32(ref, 0) & 0x7fffffff
		dur := u32(ref, 4)
		if off < 0 || off >= fileSize {
			return nil, errIndex
		}
		seconds := float64(stamp) / float64(timescale)
		if seconds < float64((time.Duration(1<<63-1))/time.Second) {
			pts = append(pts, IndexPoint{off, time.Duration(seconds * float64(time.Second))})
		}
		off += int64(size)
		stamp += uint64(dur)
	}
	return pts, nil
}
func u32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off : off+4]) }
func table(b []byte, width int) (int, error) {
	if len(b) < 8 {
		return 0, errIndex
	}
	n := uint64(u32(b, 4))
	if n > maxIndexEntries || n*uint64(width) > uint64(len(b)-8) {
		return 0, errIndex
	}
	return int(n), nil
}

// Edit lists shift composition time into the presentation timeline. Complex
// cuts/rate changes are left to the fallback rather than inventing offsets.
func mp4Scale(b []byte) uint32 {
	if len(b) < 16 {
		return 0
	}
	off := 12
	if b[0] == 1 {
		off = 20
	}
	if len(b) < off+4 {
		return 0
	}
	return u32(b, off)
}
func mp4EditOffset(trak []byte, scale, movieScale uint32) (float64, error) {
	b := child(child(trak, "edts"), "elst")
	if b == nil {
		return 0, nil
	}
	if len(b) < 8 || b[0] > 1 {
		return 0, errIndex
	}
	width := 12
	if b[0] == 1 {
		width = 20
	}
	n, e := table(b, width)
	if e != nil || n > 8 {
		return 0, errIndex
	}
	var delay float64
	found := false
	var offset float64
	for i := 0; i < n; i++ {
		v := b[8+i*width:]
		var duration uint64
		var media int64
		rate := 8
		if b[0] == 1 {
			duration = binary.BigEndian.Uint64(v)
			media = int64(binary.BigEndian.Uint64(v[8:]))
			rate = 16
		} else {
			duration = uint64(u32(v, 0))
			media = int64(int32(u32(v, 4)))
		}
		if u32(v, rate) != 0x10000 {
			return 0, errIndex
		}
		if media == -1 && !found {
			if movieScale == 0 {
				return 0, errIndex
			}
			delay += float64(duration) * float64(scale) / float64(movieScale)
		} else if media >= 0 && !found {
			offset = delay - float64(media)
			found = true
		} else {
			return 0, errIndex
		}
	}
	return offset, nil
}

func mp4Track(trak []byte, fileSize int64, movieScale uint32) ([]IndexPoint, error) {
	mdia := child(trak, "mdia")
	hdlr := child(mdia, "hdlr")
	if len(hdlr) < 12 || string(hdlr[8:12]) != "vide" {
		return nil, errIndex
	}
	mdhd := child(mdia, "mdhd")
	if len(mdhd) < 16 {
		return nil, errIndex
	}
	tsOff := 12
	if mdhd[0] == 1 {
		tsOff = 20
	}
	if len(mdhd) < tsOff+4 {
		return nil, errIndex
	}
	scale := u32(mdhd, tsOff)
	if scale == 0 {
		return nil, errIndex
	}
	edit, e := mp4EditOffset(trak, scale, movieScale)
	if e != nil {
		return nil, e
	}
	stbl := child(child(mdia, "minf"), "stbl")
	sz := child(stbl, "stsz")
	if len(sz) < 12 {
		return nil, errIndex
	}
	fixed, count := u32(sz, 4), u32(sz, 8)
	if count == 0 || count > maxIndexEntries {
		return nil, errIndex
	}
	if fixed == 0 && uint64(count)*4 > uint64(len(sz)-12) {
		return nil, errIndex
	}
	sizes := make([]uint32, count)
	for i := range sizes {
		sizes[i] = fixed
		if fixed == 0 {
			sizes[i] = u32(sz, 12+i*4)
		}
	}
	chunks := child(stbl, "stco")
	wide := false
	if chunks == nil {
		chunks = child(stbl, "co64")
		wide = true
	}
	w := 4
	if wide {
		w = 8
	}
	nc, e := table(chunks, w)
	if e != nil {
		return nil, e
	}
	sc := child(stbl, "stsc")
	ns, e := table(sc, 12)
	if e != nil || ns == 0 {
		return nil, errIndex
	}
	if u32(sc, 8) != 1 {
		return nil, errIndex
	}
	for i := 0; i < ns; i++ {
		at := 8 + i*12
		if u32(sc, at+4) == 0 || u32(sc, at) > uint32(nc) || (i > 0 && u32(sc, at) <= u32(sc, at-12)) {
			return nil, errIndex
		}
	}
	timing := child(stbl, "stts")
	nt, e := table(timing, 8)
	if e != nil {
		return nil, e
	}
	times := make([]uint64, count)
	sample := 0
	var clock uint64
	for i := 0; i < nt; i++ {
		at := 8 + i*8
		n, delta := u32(timing, at), u32(timing, at+4)
		if n > count-uint32(sample) {
			return nil, errIndex
		}
		for j := uint32(0); j < n; j++ {
			times[sample] = clock
			clock += uint64(delta)
			sample++
		}
	}
	if sample != int(count) {
		return nil, errIndex
	}
	composition := child(stbl, "ctts")
	cto := make([]int64, count)
	if composition != nil {
		n, e := table(composition, 8)
		if e != nil || composition[0] > 1 {
			return nil, errIndex
		}
		sample = 0
		for i := 0; i < n; i++ {
			at := 8 + i*8
			run := u32(composition, at)
			v := int64(u32(composition, at+4))
			if composition[0] == 1 {
				v = int64(int32(v))
			}
			if run > count-uint32(sample) {
				return nil, errIndex
			}
			for j := uint32(0); j < run; j++ {
				cto[sample] = v
				sample++
			}
		}
		if sample != int(count) {
			return nil, errIndex
		}
	}
	sync := child(stbl, "stss")
	keys := map[uint32]bool{}
	if sync != nil {
		n, e := table(sync, 4)
		if e != nil {
			return nil, e
		}
		for i := 0; i < n; i++ {
			k := u32(sync, 8+i*4)
			if k == 0 || k > count {
				return nil, errIndex
			}
			keys[k] = true
		}
	}
	var pts []IndexPoint
	sample = 0
	scRow := 0
	for chunk := 1; chunk <= nc; chunk++ {
		for scRow+1 < ns && int(u32(sc, 8+(scRow+1)*12)) <= chunk {
			scRow++
		}
		n := u32(sc, 8+scRow*12+4)
		if n > count-uint32(sample) {
			return nil, errIndex
		}
		var off uint64
		if wide {
			off = binary.BigEndian.Uint64(chunks[8+(chunk-1)*8:])
		} else {
			off = uint64(u32(chunks, 8+(chunk-1)*4))
		}
		for j := uint32(0); j < n; j++ {
			if off > uint64(fileSize) || uint64(sizes[sample]) > uint64(fileSize)-off {
				return nil, errIndex
			}
			stamp := float64(times[sample]) + float64(cto[sample]) + edit
			if (sync == nil || keys[uint32(sample+1)]) && stamp >= 0 {
				seconds := stamp / float64(scale)
				if seconds < float64((time.Duration(1<<63-1))/time.Second) {
					pts = append(pts, IndexPoint{int64(off), time.Duration(seconds * float64(time.Second))})
				}
			}
			off += uint64(sizes[sample])
			sample++
		}
	}
	if sample != int(count) {
		return nil, errIndex
	}
	return pts, nil
}

type ebmlElement struct {
	id         uint64
	start, end int64
}

func vint(b []byte, id bool) (uint64, int, error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, errIndex
	}
	n := 1
	mask := byte(0x80)
	for b[0]&mask == 0 {
		n++
		mask >>= 1
	}
	if n > 8 || n > len(b) || (id && n > 4) {
		return 0, 0, errIndex
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
func (r *indexReader) element(off, end int64) (ebmlElement, error) {
	n := int(min(int64(12), end-off))
	b, e := r.read(off, n)
	if e != nil {
		return ebmlElement{}, e
	}
	id, ni, e := vint(b, true)
	if e != nil {
		return ebmlElement{}, e
	}
	size, ns, e := vint(b[ni:], false)
	if e != nil {
		return ebmlElement{}, e
	}
	start := off + int64(ni+ns)
	if size == (uint64(1)<<uint(7*ns))-1 {
		size = uint64(end - start)
	}
	if size > uint64(end-start) {
		return ebmlElement{}, errIndex
	}
	return ebmlElement{id, start, start + int64(size)}, nil
}
func ebmlChildren(b []byte) ([]struct {
	id uint64
	b  []byte
}, error) {
	var out []struct {
		id uint64
		b  []byte
	}
	for len(b) > 0 {
		id, ni, e := vint(b, true)
		if e != nil {
			return nil, e
		}
		size, ns, e := vint(b[ni:], false)
		if e != nil || size > uint64(len(b)-ni-ns) {
			return nil, errIndex
		}
		out = append(out, struct {
			id uint64
			b  []byte
		}{id, b[ni+ns : ni+ns+int(size)]})
		b = b[ni+ns+int(size):]
		if len(out) > maxIndexEntries {
			return nil, errIndex
		}
	}
	return out, nil
}
func ebmlUint(b []byte) uint64 {
	if len(b) > 8 {
		return 0
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// Matroska Cues reference cluster boundaries. Multiple keyframes in the same
// cluster share one byte anchor; keep its earliest cue, without fetching video
// clusters throughout a remote file. Between these anchors TimeMap interpolates.
// https://www.matroska.org/technical/cues.html
func (r *indexReader) matroska() ([]IndexPoint, error) {
	var seg ebmlElement
	for off, n := int64(0), 0; off < r.size && n < 32; n++ {
		e, err := r.element(off, r.size)
		if err != nil {
			return nil, err
		}
		if e.id == 0x18538067 {
			seg = e
			break
		}
		off = e.end
	}
	if seg.id == 0 {
		return nil, errIndex
	}
	scale := uint64(1000000)
	video := uint64(0)
	var cues []byte
	seenInfo, seenTracks := false, false
	targets := map[uint64]int64{}
	visited := map[int64]bool{}
	inspect := func(e ebmlElement) error {
		if e.id != 0x114d9b74 && e.id != 0x1549a966 && e.id != 0x1654ae6b && e.id != 0x1c53bb6b {
			return nil
		}
		b, err := r.read(e.start, int(e.end-e.start))
		if err != nil {
			return err
		}
		children, err := ebmlChildren(b)
		if err != nil {
			return err
		}
		switch e.id {
		case 0x1c53bb6b:
			cues = b
		case 0x1549a966:
			seenInfo = true
			for _, c := range children {
				if c.id == 0x2ad7b1 {
					scale = ebmlUint(c.b)
				}
			}
		case 0x1654ae6b:
			seenTracks = true
			for _, c := range children {
				if c.id != 0xae {
					continue
				}
				cs, _ := ebmlChildren(c.b)
				var num, typ uint64
				for _, v := range cs {
					if v.id == 0xd7 {
						num = ebmlUint(v.b)
					}
					if v.id == 0x83 {
						typ = ebmlUint(v.b)
					}
				}
				if typ == 1 && video == 0 {
					video = num
				}
			}
		case 0x114d9b74:
			for _, c := range children {
				if c.id != 0x4dbb {
					continue
				}
				cs, _ := ebmlChildren(c.b)
				var id, pos uint64
				for _, v := range cs {
					if v.id == 0x53ab {
						id = ebmlUint(v.b)
					}
					if v.id == 0x53ac {
						pos = ebmlUint(v.b)
					}
				}
				if pos < uint64(seg.end-seg.start) {
					targets[id] = seg.start + int64(pos)
				}
			}
		}
		return nil
	}
	// Walk segment-level elements, skipping Clusters by offset (their
	// payloads are never read), so Cues written at the end of the file
	// are found even without a SeekHead pointing at them. When SeekHead
	// already locates Cues, stop at the first Cluster instead of reading
	// cluster headers across a remote file; without SeekHead, bound cluster
	// skips so a large unindexed file does not trigger hundreds of range GETs.
	clusters := 0
	for off, n := seg.start, 0; off < seg.end && n < 8192; n++ {
		e, err := r.element(off, seg.end)
		if err != nil {
			return nil, err
		}
		visited[off] = true
		if e.id == 0x1f43b675 {
			clusters++
			if targets[0x1c53bb6b] > 0 || cues != nil || clusters > 64 {
				break
			}
		}
		if err = inspect(e); err != nil {
			return nil, err
		}
		if cues != nil && seenInfo && seenTracks {
			break
		}
		if e.end <= off {
			return nil, errIndex
		}
		off = e.end
	}
	// SeekHead points directly to tail Cues, avoiding a media-cluster scan.
	for _, id := range []uint64{0x1549a966, 0x1654ae6b, 0x1c53bb6b} {
		off, ok := targets[id]
		if !ok || visited[off] {
			continue
		}
		e, err := r.element(off, seg.end)
		if err != nil || e.id != id {
			return nil, errIndex
		}
		if err = inspect(e); err != nil {
			return nil, err
		}
	}
	if cues == nil || scale == 0 {
		return nil, errIndex
	}
	cs, err := ebmlChildren(cues)
	if err != nil {
		return nil, err
	}
	var pts []IndexPoint
	for _, c := range cs {
		if c.id != 0xbb {
			continue
		}
		fields, e := ebmlChildren(c.b)
		if e != nil {
			return nil, e
		}
		var stamp uint64
		hasTime := false
		for _, v := range fields {
			if v.id == 0xb3 {
				stamp = ebmlUint(v.b)
				hasTime = true
			}
		}
		if !hasTime {
			continue
		}
		for _, v := range fields {
			if v.id != 0xb7 {
				continue
			}
			ps, e := ebmlChildren(v.b)
			if e != nil {
				return nil, e
			}
			var track, pos uint64
			hasPos := false
			for _, p := range ps {
				if p.id == 0xf7 {
					track = ebmlUint(p.b)
				}
				if p.id == 0xf1 {
					pos = ebmlUint(p.b)
					hasPos = true
				}
			}
			if hasPos && (video == 0 || track == video) && pos < uint64(seg.end-seg.start) && stamp <= uint64(1<<63-1)/scale {
				pts = append(pts, IndexPoint{seg.start + int64(pos), time.Duration(stamp * scale)})
			}
		}
	}
	return pts, nil
}
