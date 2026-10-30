package transcode

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Native ISO-BMFF (MP4, MOV, M4V) probing. Everything a decision needs
// lives in the moov box, which is read whole: at the head of web-optimised
// files, or after mdat, in which case the box header tells where.

const nativeMoovLimit = 48 << 20

func isISOBMFF(head []byte) bool {
	if len(head) < 8 {
		return false
	}
	switch string(head[4:8]) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide", "pnot":
		return true
	}
	return false
}

func (s *nativeSource) mp4() (*core.MediaInfo, error) {
	var moov []byte
	for off, n := int64(0), 0; off < s.size && n < 64; n++ {
		h, err := s.read(off, int(min(16, s.size-off)))
		if err != nil {
			return nil, err
		}
		if len(h) < 8 {
			return nil, incomplete("truncated box header")
		}
		size, hdr := int64(binary.BigEndian.Uint32(h)), int64(8)
		switch size {
		case 1:
			if len(h) < 16 {
				return nil, incomplete("truncated box header")
			}
			size, hdr = int64(binary.BigEndian.Uint64(h[8:])), 16
		case 0:
			size = s.size - off
		}
		if size < hdr || size > s.size-off {
			return nil, incomplete("bad top-level box size")
		}
		if string(h[4:8]) == "moov" {
			if size > nativeMoovLimit {
				return nil, incomplete("moov too large")
			}
			b, err := s.read(off, int(size))
			if err != nil {
				return nil, err
			}
			moov = b
			break
		}
		off += size
	}
	if moov == nil {
		return nil, incomplete("no moov")
	}
	return mp4Info(moov, s.size)
}

// mp4Info maps a whole moov box (header included) to MediaInfo the way
// libavformat's mov demuxer reports it.
func mp4Info(b []byte, size int64) (*core.MediaInfo, error) {
	top, err := readBoxes(b, 0, len(b))
	if err != nil || len(top) != 1 || top[0].typ != "moov" {
		return nil, incomplete("bad moov")
	}
	moov := top[0]
	if _, ok := child(b, moov, "mvex"); ok {
		return nil, incomplete("fragmented mp4") // sample tables live in moof boxes
	}
	mvhd, ok := child(b, moov, "mvhd")
	if !ok {
		return nil, incomplete("no mvhd")
	}
	scale, dur := fullBoxTime(mvhd.payload(b))
	if scale == 0 || dur == 0 {
		return nil, incomplete("no movie duration")
	}
	m := &core.MediaInfo{Container: "mp4", Size: size, Duration: ticksToDuration(int64(dur), scale)}
	if m.Duration <= 0 {
		return nil, incomplete("bad movie duration")
	}
	m.BitrateKbps = int(float64(size*8) / m.Duration.Seconds() / 1000)
	kids, err := readBoxes(b, moov.start+moov.hdr, moov.end)
	if err != nil {
		return nil, incomplete("bad moov children")
	}
	hasVideo := false
	index := 0
	for _, k := range kids {
		if k.typ != "trak" {
			continue
		}
		if _, ok := path(b, k, "tref", "chap"); ok {
			return nil, incomplete("quicktime chapter track")
		}
		si, ok, err := mp4Track(b, k, scale)
		if err != nil {
			return nil, err
		}
		// libavformat makes a stream of every trak, data tracks included.
		si.Index = index
		index++
		if !ok {
			continue
		}
		if si.Type == "video" {
			hasVideo = true
		}
		m.Streams = append(m.Streams, si)
	}
	if !hasVideo {
		return nil, incomplete("no video stream")
	}
	if udta, ok := child(b, moov, "udta"); ok {
		if chpl, ok := child(b, udta, "chpl"); ok {
			m.Chapters = neroChapters(chpl.payload(b), m.Duration)
		}
	}
	return m, nil
}

// fullBoxTime reads timescale and duration from an mvhd/mdhd payload.
func fullBoxTime(p []byte) (scale uint32, dur uint64) {
	if len(p) < 4 {
		return 0, 0
	}
	if p[0] == 1 {
		if len(p) < 32 {
			return 0, 0
		}
		return binary.BigEndian.Uint32(p[20:]), binary.BigEndian.Uint64(p[24:])
	}
	if len(p) < 20 {
		return 0, 0
	}
	return binary.BigEndian.Uint32(p[12:]), uint64(binary.BigEndian.Uint32(p[16:]))
}

// mp4Track maps one trak. ok is false for tracks ffprobe reports as data.
func mp4Track(b []byte, trak mp4Box, movieScale uint32) (core.StreamInfo, bool, error) {
	var si core.StreamInfo
	hdlr, ok := path(b, trak, "mdia", "hdlr")
	if !ok || len(hdlr.payload(b)) < 12 {
		return si, false, incomplete("track without handler")
	}
	handler := string(hdlr.payload(b)[8:12])
	mdhd, ok := path(b, trak, "mdia", "mdhd")
	if !ok {
		return si, false, incomplete("track without mdhd")
	}
	md := mdhd.payload(b)
	scale, dur := fullBoxTime(md)
	langAt := 20
	if len(md) > 0 && md[0] == 1 {
		langAt = 32
	}
	if len(md) >= langAt+2 {
		si.Language = normalizeLanguage(movLanguage(binary.BigEndian.Uint16(md[langAt:])))
	}
	if tkhd, ok := child(b, trak, "tkhd"); ok && len(tkhd.payload(b)) >= 4 {
		si.Default = tkhd.payload(b)[3]&1 != 0
	}
	if udta, ok := child(b, trak, "udta"); ok {
		if name, ok := child(b, udta, "name"); ok {
			si.Title = strings.TrimRight(string(name.payload(b)), "\x00")
		}
	}
	stbl, ok := path(b, trak, "mdia", "minf", "stbl")
	if !ok {
		return si, false, incomplete("track without sample table")
	}
	stsd, ok := child(b, stbl, "stsd")
	if !ok || stsd.end-stsd.start < stsd.hdr+8 {
		return si, false, incomplete("track without sample description")
	}
	entries, err := readBoxes(b, stsd.start+stsd.hdr+8, stsd.end)
	if err != nil || len(entries) == 0 {
		return si, false, incomplete("bad sample description")
	}
	if len(entries) > 1 {
		return si, false, incomplete("multiple sample descriptions")
	}
	entry := entries[0]
	// The sample-entry fourcc is the codec tag ffprobe reports. A title is
	// only the udta name already copied above, not the handler name.
	si.CodecTag = entry.typ
	if scale > 0 {
		si.TimeBase = "1/" + strconv.FormatUint(uint64(scale), 10)
	}
	frames, bytes, deltas := sampleStats(b, stbl)
	// libavformat measures the stream over its edit list when it has one.
	played := 0.0
	if scale > 0 {
		played = float64(dur) / float64(scale)
	}
	if elst, ok := path(b, trak, "edts", "elst"); ok && movieScale > 0 {
		if d := editDuration(elst.payload(b)); d > 0 {
			played = float64(d) / float64(movieScale)
		}
	}
	if played > 0 && bytes > 0 {
		si.BitrateKbps = int(float64(bytes) * 8 / played / 1000)
	}
	switch handler {
	case "vide":
		si.Type = "video"
		if err := mp4Video(b, entry, &si); err != nil {
			return si, false, err
		}
		switch {
		case len(deltas) == 1 || (len(deltas) == 2 && deltas[1][0] == 1):
			if deltas[0][1] > 0 && scale > 0 {
				rate := float64(scale) / float64(deltas[0][1])
				si.AverageFrameRate = &rate
				si.ReferenceFrameRate = &rate
				si.FrameRate = math.Round(rate*1000) / 1000
			}
		case frames > 0 && dur > 0:
			si.FrameRate = math.Round(float64(frames)*float64(scale)/float64(dur)*1000) / 1000
		}
		if si.FrameRate == 0 {
			return si, false, incomplete("no frame rate")
		}
		return si, true, nil
	case "soun":
		si.Type = "audio"
		if err := mp4Audio(b, entry, &si); err != nil {
			return si, false, err
		}
		return si, true, nil
	case "sbtl", "subt", "text":
		si.Type = "subtitle"
		si.BitrateKbps = 0 // ffprobe reports none for subtitles
		switch entry.typ {
		case "tx3g", "text":
			si.Codec = "mov_text"
		case "wvtt":
			si.Codec = "webvtt"
		case "stpp":
			si.Codec = "ttml"
		default:
			return si, false, incomplete("subtitle entry %s", entry.typ)
		}
		return si, true, nil
	}
	return si, false, nil
}

func mp4Video(b []byte, e mp4Box, si *core.StreamInfo) error {
	p := e.payload(b)
	if len(p) < 78 {
		return incomplete("short video sample entry")
	}
	si.Width = int(binary.BigEndian.Uint16(p[24:]))
	si.Height = int(binary.BigEndian.Uint16(p[26:]))
	if si.Width == 0 || si.Height == 0 {
		return incomplete("video geometry missing")
	}
	kids, err := readBoxes(b, e.start+e.hdr+78, e.end)
	if err != nil {
		return incomplete("invalid video sample entry")
	}
	find := func(typ string) []byte {
		for _, k := range kids {
			if k.typ == typ {
				return k.payload(b)
			}
		}
		return nil
	}
	transfer, primaries, matrix, fullRange := 0, 0, 0, -1
	if c := find("colr"); len(c) >= 10 && (string(c[:4]) == "nclx" || string(c[:4]) == "nclc") {
		primaries = int(binary.BigEndian.Uint16(c[4:]))
		transfer = int(binary.BigEndian.Uint16(c[6:]))
		matrix = int(binary.BigEndian.Uint16(c[8:]))
		if string(c[:4]) == "nclx" && len(c) >= 11 {
			fullRange = int(c[10] >> 7)
		}
	}
	displayW, displayH := 0, 0
	if p := find("pasp"); len(p) >= 8 {
		if h, v := int(binary.BigEndian.Uint32(p)), int(binary.BigEndian.Uint32(p[4:])); h > 0 && v > 0 {
			displayW, displayH = si.Width*h, si.Height*v
		}
	}
	dovi := find("dvcC")
	if dovi == nil {
		dovi = find("dvvC")
	}
	if dovi == nil {
		dovi = find("dvwC")
	}
	err = nil
	switch e.typ {
	case "avc1", "avc3", "dva1", "dvav":
		err = videoConfig(si, "h264", find("avcC"), transfer, dovi)
	case "hvc1", "hev1", "dvh1", "dvhe":
		err = videoConfig(si, "hevc", find("hvcC"), transfer, dovi)
	case "av01", "dav1":
		err = videoConfig(si, "av1", find("av1C"), transfer, dovi)
	default:
		return incomplete("video sample entry %s", e.typ)
	}
	if err != nil {
		return err
	}
	containerPicture(si, primaries, matrix, fullRange, displayW, displayH)
	switch e.typ {
	case "dva1", "dvav", "dvh1", "dvhe", "dav1":
		si.DolbyVision = true // ffprobe also infers it from the sample entry
		if dovi == nil {
			si.DolbyVisionConfigurationPresent = nil // conflicting/incomplete signalling
		}
	}
	return nil
}

func mp4Audio(b []byte, e mp4Box, si *core.StreamInfo) error {
	p := e.payload(b)
	if len(p) < 28 {
		return incomplete("short audio sample entry")
	}
	version := binary.BigEndian.Uint16(p[8:])
	channels := int(binary.BigEndian.Uint16(p[16:]))
	rate := int(binary.BigEndian.Uint32(p[24:]) >> 16)
	childrenAt := 28
	switch version {
	case 1:
		childrenAt += 16
	case 2:
		if len(p) < 64 {
			return incomplete("short v2 audio sample entry")
		}
		rate = int(math.Float64frombits(binary.BigEndian.Uint64(p[32:])))
		channels = int(binary.BigEndian.Uint32(p[40:]))
		childrenAt += 36
	}
	var kids []mp4Box
	if e.start+e.hdr+childrenAt <= e.end {
		kids, _ = readBoxes(b, e.start+e.hdr+childrenAt, e.end)
	}
	find := func(typ string) []byte {
		for _, k := range kids {
			if k.typ == typ {
				return k.payload(b)
			}
			if k.typ == "wave" { // QuickTime wraps esds in wave
				if c, ok := child(b, k, typ); ok {
					return c.payload(b)
				}
			}
		}
		return nil
	}
	switch e.typ {
	case "mp4a":
		oti, dsi := esdsConfig(find("esds"))
		switch oti {
		case 0x40, 0x66, 0x67, 0x68:
			si.Codec = "aac"
			profile, sbrRate, ch, ok := aacConfig(dsi)
			if !ok {
				return incomplete("bad AudioSpecificConfig")
			}
			si.Profile, channels = profile, ch
			if sbrRate > 0 {
				rate = sbrRate
			} else if r := ascRate(dsi); r > 0 {
				rate = r
			}
		case 0x69, 0x6b:
			si.Codec = "mp3"
		default:
			return incomplete("mp4a object type %#x", oti)
		}
	case ".mp3":
		si.Codec = "mp3"
	case "ac-3":
		d := find("dac3")
		if len(d) < 3 {
			return incomplete("ac-3 without dac3")
		}
		si.Codec = "ac3"
		bits := uint32(d[0])<<16 | uint32(d[1])<<8 | uint32(d[2])
		fscod, acmod, lfe := bits>>22, (bits>>11)&7, (bits>>10)&1
		channels = acmodChannels[acmod] + int(lfe)
		rate = ac3Rate(fscod, rate)
		if code := (bits >> 5) & 0x1f; code < uint32(len(ac3Bitrates)) {
			si.BitrateKbps = ac3Bitrates[code]
		}
	case "ec-3":
		d := find("dec3")
		if len(d) < 5 {
			return incomplete("ec-3 without dec3")
		}
		si.Codec = "eac3"
		si.BitrateKbps = int(binary.BigEndian.Uint16(d) >> 3)
		sub := uint32(d[2])<<16 | uint32(d[3])<<8 | uint32(d[4])
		fscod, acmod, lfe, deps := sub>>22, (sub>>9)&7, (sub>>8)&1, (sub>>1)&0xf
		if deps != 0 {
			return incomplete("e-ac-3 dependent substreams")
		}
		channels = acmodChannels[acmod] + int(lfe)
		rate = ac3Rate(fscod, rate)
	case "Opus":
		si.Codec = "opus"
		if d := find("dOps"); len(d) >= 2 && d[1] > 0 {
			channels = int(d[1])
		}
		rate = 48000
	case "fLaC":
		si.Codec = "flac"
		// dfLa: full box, then a STREAMINFO metadata block.
		if d := find("dfLa"); len(d) >= 4+4+18 {
			info := d[8:]
			v := binary.BigEndian.Uint64(info[10:])
			rate = int(v >> 44)
			channels = int(v>>41&7) + 1
		}
	case "dtsc", "dtsh", "dtsl", "dtse":
		si.Codec = "dts"
	case "mlpa":
		si.Codec = "truehd"
	case "ipcm", "lpcm", "sowt", "twos", "in24", "in32", "fl32", "fl64":
		si.Codec = "pcm"
	default:
		return incomplete("audio sample entry %s", e.typ)
	}
	if channels <= 0 || rate <= 0 {
		return incomplete("audio channels or rate missing")
	}
	si.Channels, si.SampleRate = channels, rate
	si.ChannelLayout = defaultChannelLayout(si.Codec, channels)
	return nil
}

var acmodChannels = [8]int{2, 1, 2, 3, 3, 4, 4, 5}

var ac3Bitrates = []int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}

func ac3Rate(fscod uint32, fallback int) int {
	switch fscod {
	case 0:
		return 48000
	case 1:
		return 44100
	case 2:
		return 32000
	}
	return fallback
}

// ascRate is the core sampling frequency of an AudioSpecificConfig.
func ascRate(b []byte) int {
	r := &bitReader{b: b}
	if r.u(5) == 31 {
		r.skip(6)
	}
	rates := []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	i := int(r.u(4))
	if i == 15 {
		return int(r.u(24))
	}
	if r.err != nil || i >= len(rates) {
		return 0
	}
	return rates[i]
}

// esdsConfig returns the object type indication and decoder-specific info
// of an esds box payload.
func esdsConfig(esds []byte) (byte, []byte) {
	if len(esds) < 4 {
		return 0, nil
	}
	descriptor := func(d []byte) (tag byte, body, rest []byte, ok bool) {
		if len(d) < 2 {
			return 0, nil, nil, false
		}
		tag = d[0]
		n, i := 0, 1
		for ; i < len(d) && i <= 4; i++ {
			n = n<<7 | int(d[i]&0x7f)
			if d[i]&0x80 == 0 {
				i++
				break
			}
		}
		if i+n > len(d) {
			return 0, nil, nil, false
		}
		return tag, d[i : i+n], d[i+n:], true
	}
	tag, es, _, ok := descriptor(esds[4:])
	if !ok || tag != 0x03 || len(es) < 3 {
		return 0, nil
	}
	flags := es[2]
	es = es[3:]
	if flags&0x80 != 0 && len(es) >= 2 {
		es = es[2:]
	}
	if flags&0x40 != 0 && len(es) >= 1 && len(es) >= 1+int(es[0]) {
		es = es[1+int(es[0]):]
	}
	if flags&0x20 != 0 && len(es) >= 2 {
		es = es[2:]
	}
	tag, dc, _, ok := descriptor(es)
	if !ok || tag != 0x04 || len(dc) < 13 {
		return 0, nil
	}
	tag, dsi, _, ok := descriptor(dc[13:])
	if !ok || tag != 0x05 {
		return dc[0], nil
	}
	return dc[0], dsi
}

// sampleStats sums a track's sample count and bytes (stsz/stz2) and returns
// its stts runs as {count, delta} pairs.
func sampleStats(b []byte, stbl mp4Box) (frames, bytes uint64, deltas [][2]uint32) {
	if stsz, ok := child(b, stbl, "stsz"); ok {
		p := stsz.payload(b)
		if len(p) >= 12 {
			size, count := binary.BigEndian.Uint32(p[4:]), binary.BigEndian.Uint32(p[8:])
			frames = uint64(count)
			if size != 0 {
				bytes = uint64(size) * uint64(count)
			} else if uint64(len(p)) >= 12+4*uint64(count) {
				for i := uint32(0); i < count; i++ {
					bytes += uint64(binary.BigEndian.Uint32(p[12+4*i:]))
				}
			}
		}
	}
	if stts, ok := child(b, stbl, "stts"); ok {
		p := stts.payload(b)
		if len(p) >= 8 {
			n := binary.BigEndian.Uint32(p[4:])
			for i := uint32(0); i < n && uint64(len(p)) >= 8+8*uint64(i+1); i++ {
				deltas = append(deltas, [2]uint32{binary.BigEndian.Uint32(p[8+8*i:]), binary.BigEndian.Uint32(p[12+8*i:])})
			}
		}
	}
	return frames, bytes, deltas
}

// movLanguage decodes an mdhd language: packed ISO-639-2/T, or a
// QuickTime Macintosh language code (only English is mapped).
func movLanguage(code uint16) string {
	switch {
	case code == 0x7fff || code == 0x55c4:
		return ""
	case code >= 0x400:
		return string([]byte{byte(code>>10&0x1f) + 0x60, byte(code>>5&0x1f) + 0x60, byte(code&0x1f) + 0x60})
	}
	return ""
}

// neroChapters reads a udta/chpl box as libavformat does: ids are the
// chapter order and ends close at the next start or the duration.
func neroChapters(p []byte, duration time.Duration) []core.Chapter {
	if len(p) < 5 {
		return nil
	}
	i := 4
	if p[0] == 1 {
		i += 4
	}
	if i >= len(p) {
		return nil
	}
	n := int(p[i])
	i++
	var out []core.Chapter
	for k := 0; k < n && i+9 <= len(p); k++ {
		start := time.Duration(binary.BigEndian.Uint64(p[i:]) * 100)
		l := int(p[i+8])
		i += 9
		if i+l > len(p) {
			break
		}
		out = append(out, core.Chapter{ID: int64(k), Title: string(p[i : i+l]), Start: start})
		i += l
	}
	for k := range out {
		end := duration
		if k+1 < len(out) {
			end = out[k+1].Start
		}
		out[k].End = end
	}
	kept := out[:0]
	for _, c := range out {
		if c.End > c.Start {
			kept = append(kept, c)
		}
	}
	return kept
}

// editDuration sums the movie-timescale durations of a track's non-empty
// edits.
func editDuration(p []byte) uint64 {
	if len(p) < 8 {
		return 0
	}
	n := binary.BigEndian.Uint32(p[4:])
	var total uint64
	for i, at := uint32(0), 8; i < n; i++ {
		var seg uint64
		var media int64
		if p[0] == 1 {
			if len(p) < at+20 {
				return 0
			}
			seg, media = binary.BigEndian.Uint64(p[at:]), int64(binary.BigEndian.Uint64(p[at+8:]))
			at += 20
		} else {
			if len(p) < at+12 {
				return 0
			}
			seg, media = uint64(binary.BigEndian.Uint32(p[at:])), int64(int32(binary.BigEndian.Uint32(p[at+4:])))
			at += 12
		}
		if media >= 0 {
			total += seg
		}
	}
	return total
}
