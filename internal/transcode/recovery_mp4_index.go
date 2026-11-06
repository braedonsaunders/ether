package transcode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	recoveryMoovLimit        = 16 << 20
	recoveryIndexReadBudget  = recoveryMoovLimit + (64 << 10) + (16 << 10)
	recoveryIndexBoxLimit    = 4096
	recoveryIndexSampleLimit = 1000000
	recoveryIndexTimeout     = 5 * time.Second
	recoveryAUMax            = 8191
)

var errRecoveryAACIndex = errors.New("transcode: recovery AAC source index unverified")

// recoveryAACIndexFacts is a source observation, NOT retained-output agreement,
// decoder continuity, placement authority, or permission to start/publish.
type recoveryAACIndexFacts struct {
	// Explicit source roll metadata is a dependency, never independent decoding.
	DecoderRollSignaled                                     bool
	DecoderRollDistance                                     int16
	PreviousOffset, NextOffset                              int64
	PreviousSize, NextSize                                  int
	NextSample                                              int64
	SourceNext                                              core.RecoveryPacketClock
	CodecConfigSHA256, PreviousRawAUSHA256, NextRawAUSHA256 [32]byte
}

type recoveryIndexReader struct {
	ctx              context.Context
	src              core.RecoverySourceReader
	revision         string
	size             int64
	remaining, calls int
	boxes            int
}

func (r *recoveryIndexReader) read(off int64, n int) ([]byte, error) {
	if r.ctx.Err() != nil {
		return nil, r.ctx.Err()
	}
	if off < 0 || n < 0 || off > r.size || int64(n) > r.size-off || n > r.remaining {
		return nil, errRecoveryAACIndex
	}
	r.remaining -= n
	b := make([]byte, n)
	for done := 0; done < n; {
		if r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}
		if r.calls >= 4608 {
			return nil, errRecoveryAACIndex
		}
		r.calls++
		count := min(64<<10, n-done)
		got, err := r.src.ReadRecoveryAt(r.ctx, r.revision, b[done:done+count], off+int64(done))
		// Synchronous call has actually returned. A deadline never substitutes for
		// joining an uncooperative reader or releasing its input responsibility.
		if r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}
		if err != nil || got != count {
			return nil, errRecoveryAACIndex
		}
		done += got
	}
	return b, nil
}

type recoveryMP4Parser struct {
	b     []byte
	boxes int
	ctx   context.Context
}

func (p *recoveryMP4Parser) children(start, end int) ([]mp4Box, error) {
	if start < 0 || end > len(p.b) || start > end {
		return nil, errRecoveryAACIndex
	}
	var out []mp4Box
	for start < end {
		if p.ctx.Err() != nil {
			return nil, p.ctx.Err()
		}
		if p.boxes == 0 || end-start < 8 {
			return nil, errRecoveryAACIndex
		}
		p.boxes--
		size := uint64(binary.BigEndian.Uint32(p.b[start:]))
		hdr := 8
		if size == 1 {
			if end-start < 16 {
				return nil, errRecoveryAACIndex
			}
			size = binary.BigEndian.Uint64(p.b[start+8:])
			hdr = 16
		}
		if size < uint64(hdr) || size > uint64(end-start) {
			return nil, errRecoveryAACIndex
		}
		out = append(out, mp4Box{typ: string(p.b[start+4 : start+8]), start: start, end: start + int(size), hdr: hdr})
		start += int(size)
	}
	return out, nil
}
func (p *recoveryMP4Parser) inside(box mp4Box, skip int) ([]mp4Box, error) {
	return p.children(box.start+box.hdr+skip, box.end)
}
func recoveryUnique(kids []mp4Box, name string) (mp4Box, error) {
	var found mp4Box
	count := 0
	for _, k := range kids {
		if k.typ == name {
			found = k
			count++
		}
	}
	if count != 1 {
		return mp4Box{}, errRecoveryAACIndex
	}
	return found, nil
}
func recoveryHas(kids []mp4Box, name string) bool {
	for _, k := range kids {
		if k.typ == name {
			return true
		}
	}
	return false
}
func recoveryTable(b []byte, width, maxCount int) ([]byte, int, error) {
	if len(b) < 8 || !bytes.Equal(b[:4], []byte{0, 0, 0, 0}) {
		return nil, 0, errRecoveryAACIndex
	}
	count := uint64(binary.BigEndian.Uint32(b[4:]))
	if count == 0 || count > uint64(maxCount) || uint64(len(b)-8) != count*uint64(width) {
		return nil, 0, errRecoveryAACIndex
	}
	return b[8:], int(count), nil
}

type recoveryMDAT struct{ start, end int64 }

func (r *recoveryIndexReader) moov() ([]byte, []recoveryMDAT, error) {
	var moov []byte
	var media []recoveryMDAT
	for off, n := int64(0), 0; off < r.size; n++ {
		if n >= recoveryIndexBoxLimit || r.boxes == 0 || r.size-off < 8 {
			return nil, nil, errRecoveryAACIndex
		}
		r.boxes--
		h, err := r.read(off, int(min(16, r.size-off)))
		if err != nil {
			return nil, nil, err
		}
		size := uint64(binary.BigEndian.Uint32(h))
		hdr := uint64(8)
		if size == 1 {
			if len(h) < 16 {
				return nil, nil, errRecoveryAACIndex
			}
			size = binary.BigEndian.Uint64(h[8:])
			hdr = 16
		}
		if size == 0 {
			size = uint64(r.size - off)
		}
		if size < hdr || size > uint64(r.size-off) {
			return nil, nil, errRecoveryAACIndex
		}
		switch string(h[4:8]) {
		case "moof":
			return nil, nil, errRecoveryAACIndex
		case "moov":
			if moov != nil || size > recoveryMoovLimit {
				return nil, nil, errRecoveryAACIndex
			}
			moov, err = r.read(off, int(size))
			if err != nil {
				return nil, nil, err
			}
		case "mdat":
			media = append(media, recoveryMDAT{off + int64(hdr), off + int64(size)})
		}
		off += int64(size)
	}
	if moov == nil || len(media) == 0 {
		return nil, nil, errRecoveryAACIndex
	}
	return moov, media, nil
}

// verifyRecoveryAACIndex reads actual sample tables and the two neighboring AUs
// through the same claimed, authenticated lease. It is intentionally NOT wired
// to Manager admission. No timestamps/ordinals are inferred from a playlist.
func verifyRecoveryAACIndex(parent context.Context, src core.RecoverySourceReader, want *core.RecoveryAACCursor) (recoveryAACIndexFacts, error) {
	var zero recoveryAACIndexFacts
	if parent == nil || src == nil || want == nil {
		return zero, errRecoveryAACIndex
	}
	expected := *want
	if expected.SourceRevision == "" || len(expected.SourceRevision) > 512 || expected.StreamIndex < 0 || expected.StreamIndex >= 64 || expected.SampleRate != 48000 || (expected.Channels != 1 && expected.Channels != 2) || expected.SamplesPerAU != 1024 || expected.NextSample < 0 || expected.NextSample%1024 != 0 {
		return zero, errRecoveryAACIndex
	}
	ctx, cancel := context.WithTimeout(parent, recoveryIndexTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	revision, size, err := src.RecoverySourceIdentity()
	if err != nil || revision != expected.SourceRevision || size <= 0 {
		return zero, errRecoveryAACIndex
	}
	reader := &recoveryIndexReader{ctx: ctx, src: src, revision: revision, size: size, remaining: recoveryIndexReadBudget, boxes: recoveryIndexBoxLimit}
	data, media, err := reader.moov()
	if err != nil {
		return zero, err
	}
	parser := &recoveryMP4Parser{b: data, boxes: reader.boxes, ctx: ctx}
	facts, err := parser.audioIndex(expected, media)
	if err != nil {
		return zero, err
	}
	previous, err := reader.read(facts.PreviousOffset, facts.PreviousSize)
	if err != nil {
		return zero, err
	}
	next, err := reader.read(facts.NextOffset, facts.NextSize)
	if err != nil {
		return zero, err
	}
	facts.PreviousRawAUSHA256 = sha256.Sum256(previous)
	facts.NextRawAUSHA256 = sha256.Sum256(next)
	if facts.PreviousRawAUSHA256 != expected.PreviousRawAUSHA256 || facts.NextRawAUSHA256 != expected.NextRawAUSHA256 || facts.CodecConfigSHA256 != expected.CodecConfigSHA256 {
		return zero, errRecoveryAACIndex
	}
	revision, size, err = src.RecoverySourceIdentity()
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if err != nil || revision != expected.SourceRevision || size != reader.size {
		return zero, errRecoveryAACIndex
	}
	return facts, nil
}

func (p *recoveryMP4Parser) audioIndex(want core.RecoveryAACCursor, media []recoveryMDAT) (recoveryAACIndexFacts, error) {
	var zero recoveryAACIndexFacts
	top, err := p.children(0, len(p.b))
	if err != nil || len(top) != 1 || top[0].typ != "moov" {
		return zero, errRecoveryAACIndex
	}
	kids, err := p.inside(top[0], 0)
	if err != nil || recoveryHas(kids, "mvex") {
		return zero, errRecoveryAACIndex
	}
	var tracks []mp4Box
	for _, k := range kids {
		if k.typ == "trak" {
			tracks = append(tracks, k)
		}
	}
	if len(tracks) > 64 || want.StreamIndex >= len(tracks) {
		return zero, errRecoveryAACIndex
	}
	track, err := p.inside(tracks[want.StreamIndex], 0)
	if err != nil {
		return zero, err
	}
	mdia, err := recoveryUnique(track, "mdia")
	if err != nil {
		return zero, err
	}
	md, err := p.inside(mdia, 0)
	if err != nil {
		return zero, err
	}
	handler, err := recoveryUnique(md, "hdlr")
	if err != nil {
		return zero, err
	}
	h := handler.payload(p.b)
	if len(h) < 12 || !bytes.Equal(h[:4], []byte{0, 0, 0, 0}) || string(h[8:12]) != "soun" {
		return zero, errRecoveryAACIndex
	}
	mdhd, err := recoveryUnique(md, "mdhd")
	if err != nil {
		return zero, err
	}
	m := mdhd.payload(p.b)
	if len(m) != 24 || !bytes.Equal(m[:4], []byte{0, 0, 0, 0}) || binary.BigEndian.Uint32(m[12:]) != 48000 {
		return zero, errRecoveryAACIndex
	}
	shift, editSamples, err := p.editShift(kids, track)
	if err != nil {
		return zero, err
	}
	minf, err := recoveryUnique(md, "minf")
	if err != nil {
		return zero, err
	}
	mk, err := p.inside(minf, 0)
	if err != nil {
		return zero, err
	}
	if err = p.localDataReference(mk); err != nil {
		return zero, err
	}
	stbl, err := recoveryUnique(mk, "stbl")
	if err != nil {
		return zero, err
	}
	tables, err := p.inside(stbl, 0)
	if err != nil {
		return zero, err
	}
	for _, k := range tables {
		switch k.typ {
		case "stsd", "stts", "stsc", "stsz", "stco", "co64", "sgpd", "sbgp":
		default:
			return zero, errRecoveryAACIndex
		}
	}
	config, err := p.audioConfig(tables, want.Channels)
	if err != nil {
		return zero, err
	}
	stts, err := recoveryUnique(tables, "stts")
	if err != nil {
		return zero, err
	}
	runs, count, err := recoveryTable(stts.payload(p.b), 8, recoveryIndexSampleLimit)
	if err != nil {
		return zero, err
	}
	// Strict initial subset: every AAC sample, including the tail, is exactly
	// 1024 samples. Short tails/variable deltas need a separate reviewed policy.
	var samples uint64
	for i := 0; i < count; i++ {
		if p.ctx.Err() != nil {
			return zero, p.ctx.Err()
		}
		n := uint64(binary.BigEndian.Uint32(runs[i*8:]))
		if n == 0 || binary.BigEndian.Uint32(runs[i*8+4:]) != 1024 || n > recoveryIndexSampleLimit-samples {
			return zero, errRecoveryAACIndex
		}
		samples += n
	}
	if uint64(binary.BigEndian.Uint32(m[16:])) != samples*1024 || want.NextSample > math.MaxInt64-shift {
		return zero, errRecoveryAACIndex
	}
	raw := want.NextSample + shift
	if raw%1024 != 0 || raw/1024 < 1 || uint64(raw/1024) >= samples {
		return zero, errRecoveryAACIndex
	}
	if editSamples >= 0 && (want.NextSample > editSamples || editSamples-want.NextSample < 1024) {
		return zero, errRecoveryAACIndex
	}
	if err = p.rollGroups(tables, samples); err != nil {
		return zero, err
	}
	if shift > int64(samples*1024) || (editSamples >= 0 && editSamples > int64(samples*1024)-shift) {
		return zero, errRecoveryAACIndex
	}
	ordinal := uint64(raw / 1024)
	if !recoveryClockEquals(want.SourceNext, want.NextSample, 48000) {
		return zero, errRecoveryAACIndex
	}
	offsets, sizes, err := p.sampleLocations(tables, samples, ordinal, media)
	if err != nil {
		return zero, err
	}
	rollSignaled := recoveryHas(tables, "sgpd")
	rollDistance := int16(0)
	if rollSignaled {
		rollDistance = -1
	}
	return recoveryAACIndexFacts{DecoderRollSignaled: rollSignaled, DecoderRollDistance: rollDistance, PreviousOffset: offsets[0], NextOffset: offsets[1], PreviousSize: sizes[0], NextSize: sizes[1], NextSample: want.NextSample, SourceNext: core.RecoveryPacketClock{PTS: want.NextSample, DTS: want.NextSample, TimeBaseNum: 1, TimeBaseDen: 48000}, CodecConfigSHA256: config}, nil
}

// Only one rate-one, nonempty edit is understood. Its media start and duration
// are evaluated in their actual media/movie timescales with exact arithmetic.
func (p *recoveryMP4Parser) editShift(movie, track []mp4Box) (int64, int64, error) {
	if !recoveryHas(track, "edts") {
		return 0, -1, nil
	}
	mvhd, err := recoveryUnique(movie, "mvhd")
	if err != nil {
		return 0, 0, err
	}
	m := mvhd.payload(p.b)
	if len(m) != 100 || !bytes.Equal(m[:4], []byte{0, 0, 0, 0}) {
		return 0, 0, errRecoveryAACIndex
	}
	scale := uint64(binary.BigEndian.Uint32(m[12:]))
	if scale == 0 || scale > 1000000000 {
		return 0, 0, errRecoveryAACIndex
	}
	edts, err := recoveryUnique(track, "edts")
	if err != nil {
		return 0, 0, err
	}
	children, err := p.inside(edts, 0)
	if err != nil || len(children) != 1 || children[0].typ != "elst" {
		return 0, 0, errRecoveryAACIndex
	}
	e := children[0].payload(p.b)
	if len(e) != 20 || !bytes.Equal(e[:8], []byte{0, 0, 0, 0, 0, 0, 0, 1}) || binary.BigEndian.Uint32(e[16:]) != 0x00010000 {
		return 0, 0, errRecoveryAACIndex
	}
	duration := uint64(binary.BigEndian.Uint32(e[8:]))
	shift := int64(int32(binary.BigEndian.Uint32(e[12:])))
	if duration == 0 || shift < 0 || shift%1024 != 0 || duration*48000%scale != 0 {
		return 0, 0, errRecoveryAACIndex
	}
	return shift, int64(duration * 48000 / scale), nil
}

func (p *recoveryMP4Parser) localDataReference(minf []mp4Box) error {
	dinf, err := recoveryUnique(minf, "dinf")
	if err != nil {
		return err
	}
	children, err := p.inside(dinf, 0)
	if err != nil || len(children) != 1 || children[0].typ != "dref" {
		return errRecoveryAACIndex
	}
	dref := children[0]
	b := dref.payload(p.b)
	if len(b) < 8 || !bytes.Equal(b[:8], []byte{0, 0, 0, 0, 0, 0, 0, 1}) {
		return errRecoveryAACIndex
	}
	refs, err := p.inside(dref, 8)
	if err != nil || len(refs) != 1 || refs[0].typ != "url " || !bytes.Equal(refs[0].payload(p.b), []byte{0, 0, 0, 1}) {
		return errRecoveryAACIndex
	}
	return nil
}

func (p *recoveryMP4Parser) audioConfig(tables []mp4Box, channels int) ([32]byte, error) {
	var zero [32]byte
	stsd, err := recoveryUnique(tables, "stsd")
	if err != nil {
		return zero, err
	}
	b := stsd.payload(p.b)
	if len(b) < 8 || !bytes.Equal(b[:8], []byte{0, 0, 0, 0, 0, 0, 0, 1}) {
		return zero, errRecoveryAACIndex
	}
	entries, err := p.inside(stsd, 8)
	if err != nil || len(entries) != 1 || entries[0].typ != "mp4a" {
		return zero, errRecoveryAACIndex
	}
	e := entries[0].payload(p.b)
	if len(e) < 28 || !bytes.Equal(e[:8], []byte{0, 0, 0, 0, 0, 0, 0, 1}) || !bytes.Equal(e[8:16], make([]byte, 8)) || int(binary.BigEndian.Uint16(e[16:])) != channels || binary.BigEndian.Uint32(e[24:]) != 48000<<16 {
		return zero, errRecoveryAACIndex
	}
	children, err := p.inside(entries[0], 28)
	if err != nil {
		return zero, err
	}
	for _, k := range children {
		if k.typ != "esds" && k.typ != "btrt" {
			return zero, errRecoveryAACIndex
		}
	}
	esds, err := recoveryUnique(children, "esds")
	if err != nil {
		return zero, err
	}
	raw := esds.payload(p.b)
	if len(raw) < 4 || !bytes.Equal(raw[:4], []byte{0, 0, 0, 0}) {
		return zero, errRecoveryAACIndex
	}
	asc, err := recoveryESDSConfig(raw[4:])
	if err != nil {
		return zero, err
	}
	canonical := []byte{0x11, byte(0x80 | channels<<3)} // LC, 48 kHz, 1024 samples, no extensions/PCE.
	// Accept only an absent-SBR sync extension in addition to the canonical ASC.
	if (len(asc) != 2 && len(asc) != 5) || !bytes.Equal(asc[:2], canonical) || (len(asc) == 5 && !bytes.Equal(asc[2:], []byte{0x56, 0xe5, 0x00})) {
		return zero, errRecoveryAACIndex
	}
	return sha256.Sum256(canonical), nil
}

// recoveryDescriptor requires a terminating length byte within the four-byte
// limit, and bounds the body against its immediate parent descriptor.
func recoveryDescriptor(b []byte, tag byte) (body, rest []byte, err error) {
	if len(b) < 2 || b[0] != tag {
		return nil, nil, errRecoveryAACIndex
	}
	n := 0
	for i := 1; i <= 4 && i < len(b); i++ {
		n = n<<7 | int(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			start := i + 1
			if n > len(b)-start {
				return nil, nil, errRecoveryAACIndex
			}
			return b[start : start+n], b[start+n:], nil
		}
	}
	return nil, nil, errRecoveryAACIndex
}

// recoveryESDSConfig accepts only a local MPEG-4 audio ES, its sole decoder
// config/ASC, and the predefined MP4 SL config. Every parent is consumed fully;
// optional dependencies, URLs, extensions and unrecognized trailing data reject.
// Keep this strict recovery policy separate from ordinary native probing.
func recoveryESDSConfig(b []byte) ([]byte, error) {
	es, rest, err := recoveryDescriptor(b, 0x03)
	if err != nil || len(rest) != 0 || len(es) < 3 || es[2] != 0 {
		return nil, errRecoveryAACIndex
	}
	dc, slBytes, err := recoveryDescriptor(es[3:], 0x04)
	if err != nil || len(dc) < 13 || dc[0] != 0x40 || dc[1] != 0x15 {
		return nil, errRecoveryAACIndex
	}
	asc, rest, err := recoveryDescriptor(dc[13:], 0x05)
	if err != nil || len(rest) != 0 {
		return nil, errRecoveryAACIndex
	}
	sl, rest, err := recoveryDescriptor(slBytes, 0x06)
	if err != nil || len(rest) != 0 || !bytes.Equal(sl, []byte{0x02}) {
		return nil, errRecoveryAACIndex
	}
	return asc, nil
}

// Explicit roll=-1 is understood as a decoder dependency, never independence.
// This verifies source indexing only; a continuing decoder/held-output seam must
// separately establish compatibility with the actual retained predecessor AU.
func (p *recoveryMP4Parser) rollGroups(tables []mp4Box, samples uint64) error {
	if !recoveryHas(tables, "sgpd") && !recoveryHas(tables, "sbgp") {
		return nil
	}
	group, err := recoveryUnique(tables, "sgpd")
	if err != nil {
		return err
	}
	mapping, err := recoveryUnique(tables, "sbgp")
	if err != nil {
		return err
	}
	g, m := group.payload(p.b), mapping.payload(p.b)
	if !bytes.Equal(g, []byte{1, 0, 0, 0, 'r', 'o', 'l', 'l', 0, 0, 0, 2, 0, 0, 0, 1, 0xff, 0xff}) || len(m) != 20 || !bytes.Equal(m[:12], []byte{0, 0, 0, 0, 'r', 'o', 'l', 'l', 0, 0, 0, 1}) || uint64(binary.BigEndian.Uint32(m[12:])) != samples || binary.BigEndian.Uint32(m[16:]) != 1 {
		return errRecoveryAACIndex
	}
	return nil
}

func (p *recoveryMP4Parser) sampleLocations(tables []mp4Box, samples, ordinal uint64, media []recoveryMDAT) ([2]int64, [2]int, error) {
	var offsets [2]int64
	var sizes [2]int
	fail := func() ([2]int64, [2]int, error) { return offsets, sizes, errRecoveryAACIndex }
	stsz, err := recoveryUnique(tables, "stsz")
	if err != nil {
		return fail()
	}
	z := stsz.payload(p.b)
	if len(z) < 12 || !bytes.Equal(z[:4], []byte{0, 0, 0, 0}) || uint64(binary.BigEndian.Uint32(z[8:])) != samples {
		return fail()
	}
	fixed := uint64(binary.BigEndian.Uint32(z[4:]))
	if fixed > recoveryAUMax {
		return fail()
	}
	if (fixed == 0 && uint64(len(z)-12) != samples*4) || (fixed != 0 && len(z) != 12) {
		return fail()
	}
	sizeAt := func(index uint64) uint64 {
		if fixed != 0 {
			return fixed
		}
		return uint64(binary.BigEndian.Uint32(z[12+index*4:]))
	}
	typ, width := "stco", 4
	if recoveryHas(tables, "co64") {
		if recoveryHas(tables, "stco") {
			return fail()
		}
		typ, width = "co64", 8
	}
	offsetsBox, err := recoveryUnique(tables, typ)
	if err != nil {
		return fail()
	}
	chunks, chunkCount, err := recoveryTable(offsetsBox.payload(p.b), width, recoveryIndexSampleLimit)
	if err != nil {
		return fail()
	}
	stsc, err := recoveryUnique(tables, "stsc")
	if err != nil {
		return fail()
	}
	runs, runCount, err := recoveryTable(stsc.payload(p.b), 12, recoveryIndexSampleLimit)
	if err != nil {
		return fail()
	}
	previous := uint32(0)
	for i := 0; i < runCount; i++ {
		if i%1024 == 0 && p.ctx.Err() != nil {
			return offsets, sizes, p.ctx.Err()
		}
		first, count, desc := binary.BigEndian.Uint32(runs[i*12:]), binary.BigEndian.Uint32(runs[i*12+4:]), binary.BigEndian.Uint32(runs[i*12+8:])
		if first <= previous || first > uint32(chunkCount) || (i == 0 && first != 1) || count == 0 || uint64(count) > samples || desc != 1 {
			return fail()
		}
		previous = first
	}
	var index uint64
	var previousEnd int64
	run := 0
	found := 0
	mediaIndex := 0
	for chunk := 0; chunk < chunkCount; chunk++ {
		if p.ctx.Err() != nil {
			return offsets, sizes, p.ctx.Err()
		}
		if run+1 < runCount && uint32(chunk+1) == binary.BigEndian.Uint32(runs[(run+1)*12:]) {
			run++
		}
		count := uint64(binary.BigEndian.Uint32(runs[run*12+4:]))
		if count > samples-index {
			return fail()
		}
		offset := uint64(binary.BigEndian.Uint32(chunks[chunk*width:]))
		if width == 8 {
			offset = binary.BigEndian.Uint64(chunks[chunk*width:])
		}
		if offset > math.MaxInt64 || int64(offset) < previousEnd {
			return fail()
		}
		at := int64(offset)
		start := at
		for j := uint64(0); j < count; j++ {
			if j%1024 == 0 && p.ctx.Err() != nil {
				return offsets, sizes, p.ctx.Err()
			}
			size := sizeAt(index)
			if size == 0 || size > recoveryAUMax || at > math.MaxInt64-int64(size) {
				return fail()
			}
			if index == ordinal-1 || index == ordinal {
				slot := int(index - (ordinal - 1))
				offsets[slot], sizes[slot] = at, int(size)
				found++
			}
			at += int64(size)
			index++
		}
		for mediaIndex < len(media) && start >= media[mediaIndex].end {
			mediaIndex++
		}
		if mediaIndex >= len(media) || start < media[mediaIndex].start || at > media[mediaIndex].end {
			return fail()
		}
		previousEnd = at
	}
	if index != samples || found != 2 {
		return fail()
	}
	return offsets, sizes, nil
}
