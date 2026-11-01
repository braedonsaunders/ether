package subtitles

import (
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var errMP4 = errors.New("mp4 timed-text index unreadable")

type mp4Box struct {
	kind string
	data []byte
}

func mp4Boxes(b []byte) ([]mp4Box, error) {
	var out []mp4Box
	for len(b) > 0 {
		if len(b) < 8 {
			return nil, errMP4
		}
		n := uint64(binary.BigEndian.Uint32(b))
		h := 8
		if n == 1 {
			if len(b) < 16 {
				return nil, errMP4
			}
			n = binary.BigEndian.Uint64(b[8:])
			h = 16
		}
		if n == 0 {
			n = uint64(len(b))
		}
		if n < uint64(h) || n > uint64(len(b)) {
			return nil, errMP4
		}
		out = append(out, mp4Box{string(b[4:8]), b[h:int(n)]})
		b = b[int(n):]
		if len(out) > 200000 {
			return nil, errMP4
		}
	}
	return out, nil
}

func mp4Child(b []byte, kind string) []byte {
	as, err := mp4Boxes(b)
	if err != nil {
		return nil
	}
	for _, a := range as {
		if a.kind == kind {
			return a.data
		}
	}
	return nil
}

func mp4U32(b []byte, off int) uint32 {
	if off+4 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint32(b[off:])
}

func (r *rangeReader) mp4TextCues(size int64) ([]Cue, error) {
	for off, n := int64(0), 0; off < size && n < 256; n++ {
		h, err := r.read(off, 8)
		if err != nil || len(h) < 8 {
			return nil, errMP4
		}
		sz := int64(binary.BigEndian.Uint32(h))
		head := int64(8)
		if sz == 1 {
			x, err := r.read(off+8, 8)
			if err != nil || len(x) < 8 {
				return nil, errMP4
			}
			u := binary.BigEndian.Uint64(x)
			if u > uint64(size) {
				return nil, errMP4
			}
			sz = int64(u)
			head = 16
		}
		if sz == 0 {
			sz = size - off
		}
		if sz < head || sz > size-off {
			return nil, errMP4
		}
		if string(h[4:8]) == "moov" {
			body, err := r.read(off+head, int(sz-head))
			if err != nil {
				return nil, err
			}
			return mp4Tracks(r, body, size)
		}
		off += sz
	}
	return nil, errMP4
}

func mp4Tracks(r *rangeReader, moov []byte, fileSize int64) ([]Cue, error) {
	boxes, err := mp4Boxes(moov)
	if err != nil {
		return nil, err
	}
	for _, a := range boxes {
		if a.kind != "trak" {
			continue
		}
		cues, err := mp4TextTrack(r, a.data, fileSize)
		if err == nil && len(cues) > 0 {
			return cues, nil
		}
	}
	return nil, errMP4
}

func mp4TextTrack(r *rangeReader, trak []byte, fileSize int64) ([]Cue, error) {
	mdia := mp4Child(trak, "mdia")
	hdlr := mp4Child(mdia, "hdlr")
	if len(hdlr) < 12 {
		return nil, errMP4
	}
	handler := string(hdlr[8:12])
	if handler != "text" && handler != "sbtl" && handler != "subt" {
		return nil, errMP4
	}
	stsd := mp4Child(mp4Child(mp4Child(mdia, "minf"), "stbl"), "stsd")
	if !mp4HasTX3G(stsd) {
		return nil, errMP4
	}
	mdhd := mp4Child(mdia, "mdhd")
	if len(mdhd) < 16 {
		return nil, errMP4
	}
	tsOff := 12
	if mdhd[0] == 1 {
		tsOff = 20
	}
	scale := mp4U32(mdhd, tsOff)
	if scale == 0 {
		return nil, errMP4
	}
	stbl := mp4Child(mp4Child(mdia, "minf"), "stbl")
	sizes, err := mp4SampleSizes(mp4Child(stbl, "stsz"))
	if err != nil {
		return nil, err
	}
	times, err := mp4SampleTimes(mp4Child(stbl, "stts"), len(sizes))
	if err != nil {
		return nil, err
	}
	offs, err := mp4SampleOffsets(stbl, len(sizes), fileSize)
	if err != nil {
		return nil, err
	}
	var out []Cue
	for i := range sizes {
		start := time.Duration(times[i]) * time.Second / time.Duration(scale)
		end := start + 2*time.Second
		if i+1 < len(times) {
			end = time.Duration(times[i+1]) * time.Second / time.Duration(scale)
		}
		text := ""
		if sizes[i] > 0 && sizes[i] <= 1<<16 && offs[i] >= 0 {
			raw, err := r.read(offs[i], int(sizes[i]))
			if err == nil {
				text = parseTX3G(raw)
			}
		}
		if strings.TrimSpace(text) == "" || end <= start {
			continue
		}
		out = append(out, Cue{Start: start, End: end, Text: text})
	}
	if len(out) == 0 {
		return nil, errMP4
	}
	return out, nil
}

func mp4HasTX3G(stsd []byte) bool {
	if len(stsd) < 16 {
		return false
	}
	return bytesContains(stsd, []byte("tx3g")) || bytesContains(stsd, []byte("text"))
}

func bytesContains(b, sub []byte) bool {
	return strings.Contains(string(b), string(sub))
}

func mp4SampleSizes(stsz []byte) ([]uint32, error) {
	if len(stsz) < 12 {
		return nil, errMP4
	}
	fixed, count := mp4U32(stsz, 4), mp4U32(stsz, 8)
	if count == 0 || count > 200000 {
		return nil, errMP4
	}
	out := make([]uint32, count)
	for i := range out {
		out[i] = fixed
		if fixed == 0 {
			if 12+i*4+4 > len(stsz) {
				return nil, errMP4
			}
			out[i] = mp4U32(stsz, 12+i*4)
		}
	}
	return out, nil
}

func mp4SampleTimes(stts []byte, count int) ([]uint64, error) {
	if len(stts) < 8 {
		return nil, errMP4
	}
	n := int(mp4U32(stts, 4))
	if n <= 0 || 8+n*8 > len(stts) {
		return nil, errMP4
	}
	out := make([]uint64, 0, count)
	var clock uint64
	for i := 0; i < n; i++ {
		run, delta := mp4U32(stts, 8+i*8), mp4U32(stts, 8+i*8+4)
		for j := uint32(0); j < run; j++ {
			out = append(out, clock)
			clock += uint64(delta)
			if len(out) > count {
				return nil, errMP4
			}
		}
	}
	if len(out) != count {
		return nil, errMP4
	}
	return out, nil
}

func mp4SampleOffsets(stbl []byte, count int, fileSize int64) ([]int64, error) {
	stsc := mp4Child(stbl, "stsc")
	if len(stsc) < 8 {
		return nil, errMP4
	}
	ns := int(mp4U32(stsc, 4))
	if ns <= 0 || 8+ns*12 > len(stsc) {
		return nil, errMP4
	}
	chunks, wide := mp4Child(stbl, "stco"), false
	if chunks == nil {
		chunks = mp4Child(stbl, "co64")
		wide = true
	}
	w := 4
	if wide {
		w = 8
	}
	if len(chunks) < 8 {
		return nil, errMP4
	}
	nc := int(mp4U32(chunks, 4))
	if nc <= 0 || 8+nc*w > len(chunks) {
		return nil, errMP4
	}
	stsz := mp4Child(stbl, "stsz")
	sizes, err := mp4SampleSizes(stsz)
	if err != nil {
		return nil, err
	}
	fixed := make([]int64, count)
	sample, scRow := 0, 0
	for chunk := 1; chunk <= nc; chunk++ {
		for scRow+1 < ns && int(mp4U32(stsc, 8+(scRow+1)*12)) <= chunk {
			scRow++
		}
		n := int(mp4U32(stsc, 8+scRow*12+4))
		var off uint64
		if wide {
			off = binary.BigEndian.Uint64(chunks[8+(chunk-1)*8:])
		} else {
			off = uint64(mp4U32(chunks, 8+(chunk-1)*4))
		}
		for j := 0; j < n && sample < count; j++ {
			if off > uint64(fileSize) {
				return nil, errMP4
			}
			fixed[sample] = int64(off)
			off += uint64(sizes[sample])
			sample++
		}
	}
	if sample != count {
		return nil, errMP4
	}
	return fixed, nil
}

func parseTX3G(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	n := int(binary.BigEndian.Uint16(b))
	if n <= 0 || 2+n > len(b) {
		// Some writers store raw UTF-8 without the length prefix.
		s := strings.TrimSpace(string(b))
		if utf8.ValidString(s) {
			return s
		}
		return ""
	}
	s := strings.TrimSpace(string(b[2 : 2+n]))
	if !utf8.ValidString(s) {
		return ""
	}
	return s
}
