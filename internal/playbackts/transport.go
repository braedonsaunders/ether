package playbackts

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
)

type pidState struct {
	CC, LastPayloadCC byte
	HasPayload        bool
	Adaptation        byte
}
type accessUnit struct {
	Hash          [32]byte
	Size          int
	Samples, Rate int
}
type pesObservation struct {
	PTS, DTS    int64
	PayloadHash [32]byte
	Units       []accessUnit
}
type track struct {
	PID     uint16
	Type    byte
	Config  []byte
	PES     []pesObservation
	pending []byte
}
type transport struct {
	PAT, PMT                []byte
	Program, PMTPID, PCRPID uint16
	PATVersion, PMTVersion  byte
	PIDs                    map[uint16]pidState
	Tracks                  map[uint16]*track
	FirstPCR, LastPCR       uint64
	HasPCR                  bool
}

type packet struct {
	pid            uint16
	cc, afc, flags byte
	start          bool
	payload        []byte
	pcr            uint64
	hasPCR         bool
}

func decodePacket(p []byte) (packet, error) {
	var v packet
	if len(p) != 188 || p[0] != 0x47 || p[1]&0x80 != 0 || p[3]&0xc0 != 0 {
		return v, ErrUnsupported
	}
	v.pid = uint16(p[1]&31)<<8 | uint16(p[2])
	v.cc = p[3] & 15
	v.afc = p[3] >> 4 & 3
	v.start = p[1]&0x40 != 0
	if v.afc == 0 {
		return v, ErrUnsupported
	}
	off := 4
	if v.afc&2 != 0 {
		n := int(p[4])
		off = 5 + n
		if off > 188 || (v.afc == 2 && off != 188) || (v.afc == 3 && off >= 188) {
			return v, ErrUnsupported
		}
		if n > 0 {
			v.flags = p[5]
			// Discontinuity, OPCR, splice countdown, private data and extensions need
			// additional proof and are outside this finite profile.
			if v.flags & ^byte(0x50) != 0 {
				return v, ErrUnsupported
			}
			used := 6
			if v.flags&0x10 != 0 {
				if n < 7 || p[10]&0x7e != 0x7e {
					return v, ErrUnsupported
				}
				base := uint64(p[6])<<25 | uint64(p[7])<<17 | uint64(p[8])<<9 | uint64(p[9])<<1 | uint64(p[10]>>7)
				ext := uint64(p[10]&1)<<8 | uint64(p[11])
				if ext >= 300 {
					return v, ErrUnsupported
				}
				v.pcr = base*300 + ext
				v.hasPCR = true
				used = 12
			}
			for _, b := range p[used:off] {
				if b != 0xff {
					return v, ErrUnsupported
				}
			}
		}
	}
	if v.afc&1 != 0 {
		v.payload = p[off:]
	} else if v.start {
		return v, ErrUnsupported
	}
	return v, nil
}
func crc(b []byte) uint32 {
	c := uint32(0xffffffff)
	for _, v := range b {
		c ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
	}
	return c
}
func psi(p packet, table byte) ([]byte, error) {
	b := p.payload
	if !p.start || len(b) < 4 || b[0] != 0 {
		return nil, ErrUnsupported
	}
	b = b[1:]
	n := 3 + int(binary.BigEndian.Uint16(b[1:3])&0xfff)
	if n < 12 || n > len(b) || b[0] != table || b[1]&0xf0 != 0xb0 || b[5]&0xc1 != 0xc1 || b[6] != 0 || b[7] != 0 || crc(b[:n]) != 0 {
		return nil, ErrUnsupported
	}
	for _, v := range b[n:] {
		if v != 0xff {
			return nil, ErrUnsupported
		}
	}
	return b[:n], nil
}
func parse(b []byte, limits Limits) (transport, error) {
	t := transport{PIDs: make(map[uint16]pidState), Tracks: make(map[uint16]*track)}
	fail := func() (transport, error) { return transport{}, ErrUnsupported }
	if !limits.valid() || len(b) == 0 || int64(len(b)) > limits.MaxBytes || len(b)%188 != 0 {
		return fail()
	}
	units := 0
	for off := 0; off < len(b); off += 188 {
		p, err := decodePacket(b[off : off+188])
		if err != nil {
			return fail()
		}
		if p.pid != 0 && (t.PAT == nil || p.pid != t.PMTPID) && t.Tracks[p.pid] == nil {
			return fail()
		}
		old, seen := t.PIDs[p.pid]
		if seen {
			want := old.CC
			if len(p.payload) > 0 {
				want = (want + 1) & 15
			}
			if p.cc != want {
				return fail()
			}
		}
		old.CC = p.cc
		old.Adaptation = p.flags
		if len(p.payload) > 0 {
			old.HasPayload = true
			old.LastPayloadCC = p.cc
		}
		t.PIDs[p.pid] = old
		if p.hasPCR {
			if t.PMT == nil || p.pid != t.PCRPID || (t.HasPCR && p.pcr < t.LastPCR) {
				return fail()
			}
			if !t.HasPCR {
				t.FirstPCR = p.pcr
			}
			t.LastPCR = p.pcr
			t.HasPCR = true
		}
		if len(p.payload) == 0 {
			continue
		}
		if p.pid == 0 {
			s, e := psi(p, 0)
			if e != nil || len(s) != 16 || s[10]&0xe0 != 0xe0 {
				return fail()
			}
			program := binary.BigEndian.Uint16(s[8:10])
			pid := binary.BigEndian.Uint16(s[10:12]) & 8191
			if program == 0 || pid == 0 || pid == 8191 || (t.PAT != nil && !bytes.Equal(t.PAT, s)) {
				return fail()
			}
			t.PAT = append([]byte(nil), s...)
			t.Program = program
			t.PMTPID = pid
			t.PATVersion = s[5] >> 1 & 31
			continue
		}
		if p.pid == t.PMTPID {
			s, e := psi(p, 2)
			if e != nil || len(s) != 26 || binary.BigEndian.Uint16(s[3:5]) != t.Program || s[8]&0xe0 != 0xe0 || s[10] != 0xf0 || s[11] != 0 {
				return fail()
			}
			if t.PMT != nil {
				if !bytes.Equal(t.PMT, s) {
					return fail()
				}
				continue
			}
			t.PCRPID = binary.BigEndian.Uint16(s[8:10]) & 8191
			types := map[byte]bool{}
			for i := 12; i < 22; i += 5 {
				typ := s[i]
				pid := binary.BigEndian.Uint16(s[i+1:i+3]) & 8191
				if (typ != 0x1b && typ != 0x0f) || types[typ] || pid == 0 || pid == 8191 || pid == t.PMTPID || t.Tracks[pid] != nil || s[i+1]&0xe0 != 0xe0 || s[i+3] != 0xf0 || s[i+4] != 0 {
					return fail()
				}
				types[typ] = true
				t.Tracks[pid] = &track{PID: pid, Type: typ}
			}
			if t.Tracks[t.PCRPID] == nil {
				return fail()
			}
			t.PMT = append([]byte(nil), s...)
			t.PMTVersion = s[5] >> 1 & 31
			continue
		}
		tr := t.Tracks[p.pid]
		if tr == nil {
			return fail()
		}
		if p.start {
			if len(tr.pending) > 0 {
				if err := finishPES(tr, limits, &units); err != nil {
					return fail()
				}
			}
			tr.pending = nil
		} else if len(tr.pending) == 0 {
			return fail()
		}
		if len(p.payload) > limits.MaxPESBytes-len(tr.pending) {
			return fail()
		}
		tr.pending = append(tr.pending, p.payload...)
	}
	if t.PMT == nil || !t.HasPCR {
		return fail()
	}
	for _, tr := range t.Tracks {
		if finishPES(tr, limits, &units) != nil || len(tr.PES) == 0 {
			return fail()
		}
		tr.pending = nil
	}
	return t, nil
}
func timestamp(b []byte, prefix byte) (int64, bool) {
	if len(b) < 5 || b[0]>>4 != prefix || b[0]&1 == 0 || b[2]&1 == 0 || b[4]&1 == 0 {
		return 0, false
	}
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1), true
}
func finishPES(t *track, limits Limits, count *int) error {
	b := t.pending
	if len(b) < 14 || !bytes.Equal(b[:3], []byte{0, 0, 1}) || b[6] != 0x80 || (b[7] != 0x80 && b[7] != 0xc0) {
		return ErrUnsupported
	}
	n := int(binary.BigEndian.Uint16(b[4:6]))
	if n == 0 || n+6 != len(b) {
		return ErrUnsupported
	}
	h := 5
	if b[7] == 0xc0 {
		h = 10
	}
	if int(b[8]) != h || 9+h >= len(b) {
		return ErrUnsupported
	}
	pts, ok := timestamp(b[9:], b[7]>>6)
	if !ok {
		return ErrUnsupported
	}
	dts := pts
	if h == 10 {
		dts, ok = timestamp(b[14:], 1)
		if !ok {
			return ErrUnsupported
		}
	}
	if pts < dts {
		return ErrUnsupported
	}
	if len(t.PES) > 0 {
		last := t.PES[len(t.PES)-1]
		if pts <= last.PTS || dts <= last.DTS {
			return ErrUnsupported
		}
	}
	payload := b[9+h:]
	obs := pesObservation{PTS: pts, DTS: dts, PayloadHash: sha256.Sum256(payload)}
	if t.Type == 0x0f {
		if b[3] < 0xc0 || b[3] > 0xdf || pts != dts {
			return ErrUnsupported
		}
		for len(payload) > 0 {
			// MPEG-4 AAC-LC, one raw_data_block, no CRC; other profiles unsupported.
			if len(payload) < 7 || payload[0] != 0xff || payload[1] != 0xf1 || payload[2]>>6 != 1 || payload[6]&3 != 0 {
				return ErrUnsupported
			}
			rateIndex := payload[2] >> 2 & 15
			channels := (payload[2]&1)<<2 | payload[3]>>6
			rates := [13]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
			if rateIndex != 3 || channels == 0 || channels > 2 {
				return ErrUnsupported
			}
			size := int(payload[3]&3)<<11 | int(payload[4])<<3 | int(payload[5]>>5)
			if size <= 7 || size > len(payload) {
				return ErrUnsupported
			}
			config := []byte{2<<3 | rateIndex>>1, (rateIndex&1)<<7 | channels<<3}
			if t.Config == nil {
				t.Config = config
			} else if !bytes.Equal(config, t.Config) {
				return ErrUnsupported
			}
			if *count >= limits.MaxAccessUnits {
				return ErrUnsupported
			}
			(*count)++
			obs.Units = append(obs.Units, accessUnit{Hash: sha256.Sum256(payload[7:size]), Size: size - 7, Samples: 1024, Rate: rates[rateIndex]})
			payload = payload[size:]
		}
	} else {
		if b[3] < 0xe0 || b[3] > 0xef {
			return ErrUnsupported
		}
		// Annex-B NAL observations do not prove decoded video AU ends. That
		// requires the mux's concrete sample/VUI evidence before splice admission.
		config, err := videoConfig(payload)
		if err != nil {
			return err
		}
		if len(config) > 0 {
			if t.Config == nil {
				t.Config = config
			} else if !bytes.Equal(t.Config, config) {
				return ErrUnsupported
			}
		}
		if t.Config == nil || *count >= limits.MaxAccessUnits {
			return ErrUnsupported
		}
		(*count)++
	}
	t.PES = append(t.PES, obs)
	return nil
}
func videoConfig(b []byte) ([]byte, error) {
	prefix := func(b []byte) int {
		if len(b) >= 4 && bytes.Equal(b[:4], []byte{0, 0, 0, 1}) {
			return 4
		}
		if len(b) >= 3 && bytes.Equal(b[:3], []byte{0, 0, 1}) {
			return 3
		}
		return 0
	}
	var config []byte
	vcl := false
	sps, pps := false, false
	for len(b) > 0 {
		n := prefix(b)
		if n == 0 {
			return nil, ErrUnsupported
		}
		b = b[n:]
		end := 0
		for end < len(b) && prefix(b[end:]) == 0 {
			end++
		}
		nal := b[:end]
		if len(nal) == 0 || nal[0]&0x80 != 0 {
			return nil, ErrUnsupported
		}
		typ := nal[0] & 31
		switch typ {
		case 7, 8:
			if len(nal) < 4 || len(nal) > 65535 || vcl || (typ == 7 && sps) || (typ == 8 && pps) {
				return nil, ErrUnsupported
			}
			if typ == 7 {
				sps = true
			} else {
				pps = true
			}
			config = append(config, byte(len(nal)>>8), byte(len(nal)))
			config = append(config, nal...)
		case 1, 5:
			if len(nal) < 2 {
				return nil, ErrUnsupported
			}
			vcl = true
		case 6, 9, 10, 11, 12:
		default:
			return nil, ErrUnsupported
		}
		b = b[end:]
	}
	if !vcl || sps != pps {
		return nil, ErrUnsupported
	}
	return config, nil
}

// normalizeCounters is deliberately private: transport continuity alone cannot
// authorize a splice. The proof-gated caller must still establish source/AU ends.
func normalizeCounters(previous transport, candidate []byte, limits Limits) ([]byte, transport, error) {
	next, err := parse(candidate, limits)
	if err != nil {
		return nil, transport{}, err
	}
	if !bytes.Equal(previous.PAT, next.PAT) || !bytes.Equal(previous.PMT, next.PMT) || !previous.HasPCR || next.FirstPCR < previous.LastPCR {
		return nil, transport{}, ErrUnsupported
	}
	for pid, tr := range previous.Tracks {
		nt := next.Tracks[pid]
		if nt == nil || !bytes.Equal(tr.Config, nt.Config) {
			return nil, transport{}, ErrUnsupported
		}
		last := tr.PES[len(tr.PES)-1]
		first := nt.PES[0]
		if first.PTS <= last.PTS || first.DTS <= last.DTS {
			return nil, transport{}, ErrUnsupported
		}
	}
	state := make(map[uint16]byte, len(previous.PIDs))
	for pid, s := range previous.PIDs {
		if !s.HasPayload {
			return nil, transport{}, ErrUnsupported
		}
		state[pid] = s.LastPayloadCC
	}
	out := append([]byte(nil), candidate...)
	for off := 0; off < len(out); off += 188 {
		p, err := decodePacket(out[off : off+188])
		if err != nil {
			return nil, transport{}, err
		}
		cc, ok := state[p.pid]
		if !ok {
			return nil, transport{}, ErrUnsupported
		}
		if len(p.payload) > 0 {
			cc = (cc + 1) & 15
		}
		state[p.pid] = cc
		out[off+3] = out[off+3]&0xf0 | cc
	}
	// Independently reparse all final bytes; assert exact invariance outside CC.
	final, err := parse(out, limits)
	if err != nil {
		return nil, transport{}, err
	}
	for i := range out {
		mask := byte(0xff)
		if i%188 == 3 {
			mask = 0xf0
		}
		if out[i]&mask != candidate[i]&mask {
			return nil, transport{}, ErrUnsupported
		}
	}
	if !bytes.Equal(final.PAT, next.PAT) || !bytes.Equal(final.PMT, next.PMT) {
		return nil, transport{}, ErrUnsupported
	}
	return out, final, nil
}
