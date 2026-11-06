package playbackts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"testing"

	"github.com/braedonsaunders/ether/internal/playbackbuffer"
)

var testLimits = Limits{MaxBytes: 1 << 20, MaxPESBytes: 65536, MaxAccessUnits: 1024}

func table(b []byte) []byte {
	c := crc(b)
	return append(b, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}
func packetBytes(pid uint16, cc byte, start bool, payload []byte, pcr int64) []byte {
	b := bytes.Repeat([]byte{0xff}, 188)
	b[0] = 0x47
	b[1] = byte(pid >> 8)
	if start {
		b[1] |= 0x40
	}
	b[2] = byte(pid)
	b[3] = 0x30 | cc
	n := 183 - len(payload)
	b[4] = byte(n)
	if n > 0 {
		b[5] = 0
	}
	if pcr >= 0 {
		b[5] = 0x10
		base := uint64(pcr) / 300
		ext := uint64(pcr) % 300
		b[6] = byte(base >> 25)
		b[7] = byte(base >> 17)
		b[8] = byte(base >> 9)
		b[9] = byte(base >> 1)
		b[10] = byte(base<<7) | 0x7e | byte(ext>>8)
		b[11] = byte(ext)
	}
	if len(payload) == 0 {
		b[3] = 0x20 | cc
	}
	copy(b[188-len(payload):], payload)
	return b
}
func ptsBytes(n int64) []byte {
	return []byte{0x21 | byte(n>>29)&14, byte(n >> 22), byte(n>>14)&0xfe | 1, byte(n >> 7), byte(n<<1) | 1}
}
func pes(id byte, pts int64, payload []byte) []byte {
	b := []byte{0, 0, 1, id, 0, 0, 0x80, 0x80, 5}
	b = append(b, ptsBytes(pts)...)
	b = append(b, payload...)
	binary.BigEndian.PutUint16(b[4:6], uint16(len(b)-6))
	return b
}
func fixture(pts int64, start byte) []byte {
	pat := table([]byte{0, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe1, 0})
	pmt := table([]byte{2, 0xb0, 23, 0, 1, 0xc1, 0, 0, 0xe1, 1, 0xf0, 0, 0x1b, 0xe1, 1, 0xf0, 0, 0x0f, 0xe1, 2, 0xf0, 0})
	b := packetBytes(0, start, true, append([]byte{0}, pat...), -1)
	b = append(b, packetBytes(256, start, true, append([]byte{0}, pmt...), -1)...)
	video := []byte{0, 0, 1, 0x67, 0x42, 0, 0x1e, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, 0, 0, 1, 0x65, 0x88, 0x80}
	b = append(b, packetBytes(257, start, true, pes(0xe0, pts, video), pts*300)...)
	adts := []byte{0xff, 0xf1, 0x4c, 0x80, 1, 0x3f, 0xfc, 0xaa, 0xbb}
	b = append(b, packetBytes(258, start, true, pes(0xc0, pts, adts), -1)...)
	return b
}
func TestCounterWrapAdaptationAndInvariance(t *testing.T) {
	oldBytes := fixture(90000, 15)
	old, err := parse(oldBytes, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	candidate := fixture(180000, 7)
	// Adaptation-only packets do not advance counters, including at wrap.
	candidate = append(candidate, packetBytes(257, 7, false, nil, 180000*300+1)...)
	for i := 0; i < 18; i++ {
		candidate = append(candidate, packetBytes(0, byte(8+i)&15, true, append([]byte{0}, old.PAT...), -1)...)
	}
	before := append([]byte(nil), candidate...)
	out, final, err := normalizeCounters(old, candidate, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(candidate, before) {
		t.Fatal("input mutated")
	}
	for i := range out {
		if i%188 == 3 {
			if out[i]&0xf0 != before[i]&0xf0 {
				t.Fatal("header changed")
			}
		} else if out[i] != before[i] {
			t.Fatalf("payload changed %d", i)
		}
	}
	if out[3]&15 != 0 || out[4*188+3]&15 != 0 || final.PIDs[0].LastPayloadCC != 2 {
		t.Fatal("counter wrap/adaptation")
	}
	if final.Tracks[258].PES[0].Units[0].Hash != sha256.Sum256([]byte{0xaa, 0xbb}) || !bytes.Equal(final.Tracks[258].Config, []byte{0x11, 0x90}) {
		t.Fatal("AAC observations")
	}
}
func TestStrictRejections(t *testing.T) {
	tests := map[string]func([]byte) []byte{
		"tei":            func(b []byte) []byte { b[1] |= 0x80; return b },
		"scrambled":      func(b []byte) []byte { b[3] |= 0x80; return b },
		"unknown":        func(b []byte) []byte { b = append(b, packetBytes(8191, 0, false, nil, -1)...); return b },
		"partial packet": func(b []byte) []byte { return b[:len(b)-1] },
		"duplicate":      func(b []byte) []byte { return append(b, b[376:564]...) },
		"discontinuity":  func(b []byte) []byte { b[5] |= 0x80; return b },
		"psi crc":        func(b []byte) []byte { b[187] ^= 1; return b },
		"partial pes":    func(b []byte) []byte { b[3*188+4]--; return b },
		"pcr reserved":   func(b []byte) []byte { b[376+10] &= 0x81; return b },
		"pcr regression": func(b []byte) []byte { return append(b, packetBytes(257, 0, false, nil, 1)...) },
		"adaptation cc":  func(b []byte) []byte { return append(b, packetBytes(257, 1, false, nil, -1)...) },
		"adts length":    func(b []byte) []byte { b[len(b)-5] = 0; return b },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, e := parse(mutate(fixture(90000, 0)), testLimits); e == nil {
				t.Fatal("accepted")
			}
		})
	}
}
func TestMismatchAndBounds(t *testing.T) {
	old, _ := parse(fixture(90000, 0), testLimits)
	if _, _, e := normalizeCounters(old, fixture(89999, 0), testLimits); e == nil {
		t.Fatal("regression")
	}
	wrong, _ := parse(fixture(180000, 0), testLimits)
	wrong.PAT = append([]byte(nil), wrong.PAT...)
	wrong.PAT[5] ^= 2
	if _, _, e := normalizeCounters(wrong, fixture(270000, 0), testLimits); e == nil {
		t.Fatal("program mismatch")
	}
	old.Tracks[258].Config = []byte{0}
	if _, _, e := normalizeCounters(old, fixture(180000, 0), testLimits); e == nil {
		t.Fatal("config mismatch")
	}
	k := playbackbuffer.Key{Session: "s", Generation: 2, SeekEpoch: 1, Sequence: 1}
	data := fixture(180000, 0)
	c, e := NewCandidate(k, int64(len(data)), bytes.NewReader(data), testLimits)
	if e != nil {
		t.Fatal(e)
	}
	data[0] = 0
	read := make([]byte, c.Size())
	if _, e = c.ReadAt(read, 0); e != nil || read[0] != 0x47 {
		t.Fatal("candidate aliases input")
	}
	if _, e = c.ReadAt(read, -1); e == nil {
		t.Fatal("negative offset")
	}
	if _, e = NewCandidate(k, int64(len(data)-1), bytes.NewReader(data), testLimits); e == nil {
		t.Fatal("trailing bytes")
	}
	if _, e = NewCandidate(k, 2<<20, bytes.NewReader(nil), testLimits); e == nil {
		t.Fatal("oversize")
	}
}
func TestPublishedReceiptIdentity(t *testing.T) {
	budget := playbackbuffer.Budget{Bytes: 1 << 20, Segments: 8, Inits: 1}
	s, e := playbackbuffer.New(playbackbuffer.Limits{Global: budget, PerSession: budget, Sessions: 1})
	if e != nil {
		t.Fatal(e)
	}
	state := playbackbuffer.State{Generation: 1, SeekEpoch: 3, PublishedFloor: -1}
	if e = s.OpenSession("actual", state); e != nil {
		t.Fatal(e)
	}
	k := playbackbuffer.Key{Session: "actual", Generation: 1, SeekEpoch: 3, Sequence: 0}
	b := fixture(90000, 0)
	if e = s.Retain(context.Background(), k, playbackbuffer.MPEGTS, int64(len(b)), bytes.NewReader(b)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Acquire(k); e == nil {
		t.Fatal("staged readable")
	}
	if e = s.Publish("actual", state, []playbackbuffer.Key{k}); e != nil {
		t.Fatal(e)
	}
	l, e := s.Acquire(k)
	if e != nil {
		t.Fatal(e)
	}
	r, e := Observe(l, testLimits)
	if e != nil {
		t.Fatal(e)
	}
	if r.Identity().Key != k || r.SHA256() != sha256.Sum256(b) {
		t.Fatal("identity/hash")
	}
	if e = s.CloseSession("actual"); e == nil {
		t.Fatal("lost pin")
	}
	// A real transition invalidates fresh observations on an old pinned lease.
	current := state
	current.PublishedFloor = 0
	next := playbackbuffer.State{Generation: 2, SeekEpoch: 3, PublishedFloor: 1}
	token, e := s.FreezeGeneration("actual", current, next)
	if e != nil {
		t.Fatal(e)
	}
	nextKey := playbackbuffer.Key{Session: "actual", Generation: 2, SeekEpoch: 3, Sequence: 1}
	nextBytes := fixture(180000, 0)
	if e = s.Retain(context.Background(), nextKey, playbackbuffer.MPEGTS, int64(len(nextBytes)), bytes.NewReader(nextBytes)); e != nil {
		t.Fatal(e)
	}
	if e = s.PublishTransition(token, []playbackbuffer.Key{nextKey}); e != nil {
		t.Fatal(e)
	}
	if _, e = Observe(l, testLimits); e == nil {
		t.Fatal("stale generation observed")
	}
	if r.Identity().Key != k {
		t.Fatal("historical receipt changed")
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = Observe(l, testLimits); e == nil {
		t.Fatal("closed receipt")
	}
	var zero playbackbuffer.Lease
	if _, e = zero.Identity(); e == nil {
		t.Fatal("zero lease")
	}
	if _, e = (*playbackbuffer.Lease)(nil).Identity(); e == nil {
		t.Fatal("nil lease")
	}
	if _, e = candidateEOF(); e != io.EOF {
		t.Fatal(e)
	}
	if e = s.CloseSession("actual"); e != nil {
		t.Fatal(e)
	}
}
func candidateEOF() (int, error) { return (&Candidate{}).ReadAt(make([]byte, 1), 0) }

type noProgressReader struct{}

func (noProgressReader) Read([]byte) (int, error) { return 0, nil }
func TestReaderAndParserBounds(t *testing.T) {
	k := playbackbuffer.Key{Session: "s", Generation: 1, Sequence: 0}
	if _, e := NewCandidate(k, 188, noProgressReader{}, testLimits); e != io.ErrNoProgress {
		t.Fatal(e)
	}
	data := fixture(90000, 0)
	for _, l := range []Limits{{MaxBytes: 188, MaxPESBytes: 65536, MaxAccessUnits: 1024}, {MaxBytes: 1 << 20, MaxPESBytes: 16, MaxAccessUnits: 1024}, {MaxBytes: 1 << 20, MaxPESBytes: 65536, MaxAccessUnits: 1}, {}} {
		if _, e := parse(data, l); e == nil {
			t.Fatal("ignored bounds", l)
		}
	}
	// A complete bounded PES with broken timestamp marker must reject.
	b := append([]byte(nil), data...)
	payloadOffset := 3*188 + 5 + int(b[3*188+4])
	b[payloadOffset+9] &= 0xfe
	if _, e := parse(b, testLimits); e == nil {
		t.Fatal("PES marker")
	}
	// Valid CRC does not excuse a PMT program mismatch.
	b = append([]byte(nil), data...)
	pmtStart := 188 + 5 + int(b[188+4]) + 1
	b[pmtStart+4] = 2
	copy(b[pmtStart:pmtStart+26], table(b[pmtStart:pmtStart+22]))
	if _, e := parse(b, testLimits); e == nil {
		t.Fatal("PMT program")
	}
}
