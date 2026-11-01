package subtitles

import (
	"encoding/binary"
	"time"
)

func vintBytes(v uint64, id bool) []byte {
	if id {
		// Matroska IDs are already encoded VINTs; write the significant bytes.
		var b []byte
		for x := v; x > 0; x >>= 8 {
			b = append([]byte{byte(x)}, b...)
		}
		if len(b) == 0 {
			b = []byte{0x80}
		}
		return b
	}
	n, mask := 1, uint64(0x80)
	for v >= mask {
		n++
		mask <<= 7
	}
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	out[0] |= byte(0x80 >> (n - 1))
	return out
}

func mkvEl(id uint64, body []byte) []byte {
	out := append(vintBytes(id, true), vintBytes(uint64(len(body)), false)...)
	return append(out, body...)
}

func mkvU(id, v uint64) []byte {
	var b []byte
	if v == 0 {
		b = []byte{0}
	} else {
		for x := v; x > 0; x >>= 8 {
			b = append([]byte{byte(x)}, b...)
		}
	}
	return mkvEl(id, b)
}

func mkvS(id uint64, s string) []byte { return mkvEl(id, []byte(s)) }

func mkvSeek(id, pos uint64) []byte {
	idb := vintBytes(id, true)
	posb := make([]byte, 8)
	binary.BigEndian.PutUint64(posb, pos)
	return mkvEl(mkvSeekID, append(mkvEl(mkvSeekIDID, idb), mkvEl(mkvSeekPositionID, posb)...))
}

func buildTestMKV(cues []Cue) []byte {
	track := mkvEl(mkvTrackEntryID, append(append(
		mkvU(mkvTrackNumberID, 1),
		mkvU(mkvTrackTypeID, 0x11)...),
		mkvS(mkvCodecIDID, "S_TEXT/UTF8")...))
	tracks := mkvEl(mkvTracksID, track)
	info := mkvEl(mkvInfoID, mkvU(mkvTimecodeScaleID, 1000000))

	var clusters []byte
	type cl struct {
		pos, rel, time, dur uint64
	}
	var meta []cl
	for _, c := range cues {
		ms := uint64(c.Start / time.Millisecond)
		dur := uint64((c.End - c.Start) / time.Millisecond)
		if dur == 0 {
			dur = 800
		}
		block := append(vintBytes(1, false), 0, 0, 0x80)
		block = append(block, []byte(c.Text)...)
		simple := mkvEl(mkvSimpleBlockID, block)
		ts := mkvU(mkvTimestampID, ms)
		cluster := mkvEl(mkvClusterID, append(ts, simple...))
		meta = append(meta, cl{pos: uint64(len(clusters)), rel: uint64(len(ts)), time: ms, dur: dur})
		clusters = append(clusters, cluster...)
	}
	// Cues after clusters. CueClusterPosition is from Segment payload start.
	var cuePoints []byte
	for _, m := range meta {
		cueTrack := append(append(append(
			mkvU(mkvCueTrackID, 1),
			mkvU(mkvCueClusterPosID, uint64(len(info)+len(tracks))+m.pos)...),
			mkvU(mkvCueRelPosID, m.rel)...),
			mkvU(mkvCueDurationID, m.dur)...)
		cuePoints = append(cuePoints, mkvEl(mkvCuePointID, append(
			mkvU(mkvCueTimeID, m.time),
			mkvEl(mkvCueTrackPosID, cueTrack)...))...)
	}
	cuesEl := mkvEl(mkvCuesID, cuePoints)
	var dummies []byte
	dummy := mkvEl(mkvClusterID, mkvU(mkvTimestampID, 0))
	for i := 0; i < 200; i++ {
		dummies = append(dummies, dummy...)
	}
	seekBody := append(append(
		mkvSeek(mkvInfoID, 0),
		mkvSeek(mkvTracksID, 0)...),
		mkvSeek(mkvCuesID, 0)...)
	seekHead := mkvEl(mkvSeekHeadID, seekBody)
	base := uint64(len(seekHead))
	seekBody = append(append(
		mkvSeek(mkvInfoID, base),
		mkvSeek(mkvTracksID, base+uint64(len(info)))...),
		mkvSeek(mkvCuesID, base+uint64(len(info)+len(tracks)+len(dummies)+len(clusters)))...)
	seekHead = mkvEl(mkvSeekHeadID, seekBody)
	body := append(append(append(append(seekHead, info...), tracks...), dummies...), append(clusters, cuesEl...)...)
	ebml := mkvEl(mkvEBMLID, mkvS(0x4282, "matroska"))
	return append(ebml, mkvEl(mkvSegmentID, body)...)
}

func mp4Make(kind string, payload []byte) []byte {
	n := uint32(8 + len(payload))
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, n)
	copy(out[4:8], kind)
	copy(out[8:], payload)
	return out
}

func mp4Full(kind string, version byte, flags uint32, payload []byte) []byte {
	head := make([]byte, 4+len(payload))
	head[0] = version
	head[1] = byte(flags >> 16)
	head[2] = byte(flags >> 8)
	head[3] = byte(flags)
	copy(head[4:], payload)
	return mp4Make(kind, head)
}

func buildTestMP4(cues []Cue) []byte {
	scale := uint32(1000)
	var mdat []byte
	var sizes []uint32
	var deltas []uint32
	prev := time.Duration(0)
	for _, c := range cues {
		text := []byte(c.Text)
		sample := make([]byte, 2+len(text))
		binary.BigEndian.PutUint16(sample, uint16(len(text)))
		copy(sample[2:], text)
		sizes = append(sizes, uint32(len(sample)))
		delta := uint32((c.Start - prev) / time.Millisecond)
		if delta == 0 && len(deltas) > 0 {
			delta = 1
		}
		if len(deltas) == 0 {
			delta = uint32(c.Start / time.Millisecond)
			if delta == 0 {
				delta = 0
			}
		}
		// stts stores duration of THIS sample, not the start gap.
		_ = delta
		dur := uint32((c.End - c.Start) / time.Millisecond)
		if dur == 0 {
			dur = 800
		}
		deltas = append(deltas, dur)
		prev = c.End
		mdat = append(mdat, sample...)
	}
	// stts: one run per sample so starts match cue times via cumulative deltas.
	// First sample starts at 0; we prepend a gap sample if needed? Simpler:
	// rebuild deltas as start[i+1]-start[i], last uses cue duration.
	deltas = deltas[:0]
	for i, c := range cues {
		if i+1 < len(cues) {
			d := uint32((cues[i+1].Start - c.Start) / time.Millisecond)
			if d == 0 {
				d = 1
			}
			deltas = append(deltas, d)
		} else {
			d := uint32((c.End - c.Start) / time.Millisecond)
			if d == 0 {
				d = 800
			}
			deltas = append(deltas, d)
		}
	}

	stsdPay := make([]byte, 8)
	binary.BigEndian.PutUint32(stsdPay[4:], 1)
	// tx3g sample entry: 16-byte VisualSampleEntry-like prefix + 4cc.
	entry := make([]byte, 8+32)
	binary.BigEndian.PutUint32(entry, uint32(len(entry)))
	copy(entry[4:8], "tx3g")
	stsdPay = append(stsdPay, entry...)
	stsd := mp4Full("stsd", 0, 0, stsdPay)

	sttsPay := make([]byte, 4+8*len(deltas))
	binary.BigEndian.PutUint32(sttsPay[0:], uint32(len(deltas)))
	for i, d := range deltas {
		binary.BigEndian.PutUint32(sttsPay[4+i*8:], 1)
		binary.BigEndian.PutUint32(sttsPay[4+i*8+4:], d)
	}
	stts := mp4Full("stts", 0, 0, sttsPay)

	stscPay := make([]byte, 4+12)
	binary.BigEndian.PutUint32(stscPay[0:], 1)
	binary.BigEndian.PutUint32(stscPay[4:], 1)
	binary.BigEndian.PutUint32(stscPay[8:], uint32(len(sizes)))
	binary.BigEndian.PutUint32(stscPay[12:], 1)
	stsc := mp4Full("stsc", 0, 0, stscPay)

	stszPay := make([]byte, 8+4*len(sizes))
	binary.BigEndian.PutUint32(stszPay[4:], uint32(len(sizes)))
	for i, sz := range sizes {
		binary.BigEndian.PutUint32(stszPay[8+i*4:], sz)
	}
	stsz := mp4Full("stsz", 0, 0, stszPay)

	// stco filled after we know mdat offset.
	stblWithoutCO := append(append(append(append([]byte{}, stsd...), stts...), stsc...), stsz...)
	stcoPlaceholder := mp4Full("stco", 0, 0, make([]byte, 8))
	stbl := mp4Make("stbl", append(stblWithoutCO, stcoPlaceholder...))
	minf := mp4Make("minf", append(mp4Make("nmhd", nil), append(mp4Make("dinf", nil), stbl...)...))
	hdlrPay := make([]byte, 20)
	copy(hdlrPay[4:8], "text")
	hdlr := mp4Full("hdlr", 0, 0, hdlrPay)
	mdhdPay := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdPay[8:], scale)
	mdhd := mp4Full("mdhd", 0, 0, mdhdPay)
	mdia := mp4Make("mdia", append(append(mdhd, hdlr...), minf...))
	tkhd := mp4Full("tkhd", 0, 0x1, make([]byte, 80))
	trak := mp4Make("trak", append(tkhd, mdia...))
	mvhdPay := make([]byte, 96)
	binary.BigEndian.PutUint32(mvhdPay[12:], scale)
	mvhd := mp4Full("mvhd", 0, 0, mvhdPay)
	moov := mp4Make("moov", append(mvhd, trak...))
	ftyp := mp4Make("ftyp", []byte("isom\x00\x00\x00\x00isommp41"))
	mdatBox := mp4Make("mdat", mdat)
	file := append(append(ftyp, moov...), mdatBox...)
	// Patch stco to the mdat payload offset.
	mdatOff := len(ftyp) + len(moov) + 8
	// Find stco in file and write the first chunk offset.
	stcoAt := indexOf(file, []byte("stco"))
	if stcoAt >= 4 {
		binary.BigEndian.PutUint32(file[stcoAt+8:], 1) // entry count (inside fullbox after ver/flags)
		binary.BigEndian.PutUint32(file[stcoAt+12:], uint32(mdatOff))
	}
	_ = binary.BigEndian.Uint32
	return file
}

func indexOf(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		ok := true
		for j := range sub {
			if b[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
