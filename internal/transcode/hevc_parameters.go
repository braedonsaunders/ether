package transcode

import (
	"bytes"
	"encoding/binary"
)

// hevcParameterNALs reads configuration arrays, Annex B, or length-prefixed
// packets. It retains only VPS/SPS/PPS; malformed lengths never escape the
// caller's bounded sample.
func hevcParameterNALs(data []byte, lengthSize int) map[byte][]byte {
	sets := make(map[byte][]byte)
	add := func(n []byte) {
		if len(n) >= 2 {
			typ := (n[0] >> 1) & 63
			if typ >= 32 && typ <= 34 {
				sets[typ] = n
			}
		}
	}
	if len(data) >= 23 && data[0] == 1 && lengthSize == 0 {
		rest := data[23:]
		for i := 0; i < int(data[22]); i++ {
			if len(rest) < 3 {
				return nil
			}
			kind := rest[0] & 63
			count := int(binary.BigEndian.Uint16(rest[1:3]))
			rest = rest[3:]
			for j := 0; j < count; j++ {
				if len(rest) < 2 {
					return nil
				}
				n := int(binary.BigEndian.Uint16(rest))
				rest = rest[2:]
				if n > len(rest) {
					return nil
				}
				if n < 2 || (rest[0]>>1)&63 != kind {
					return nil
				}
				add(rest[:n])
				rest = rest[n:]
			}
		}
		return sets
	}
	// Detect Annex B only at the packet start: length-prefixed slice payloads
	// may contain start-code-like bytes elsewhere.
	if bytes.HasPrefix(data, []byte{0, 0, 1}) || bytes.HasPrefix(data, []byte{0, 0, 0, 1}) {
		for {
			start := bytes.Index(data, []byte{0, 0, 1})
			if start < 0 {
				break
			}
			data = data[start+3:]
			next := bytes.Index(data, []byte{0, 0, 1})
			end := len(data)
			if next >= 0 {
				end = next
				if end > 0 && data[end-1] == 0 {
					end--
				}
			}
			add(data[:end])
			if next < 0 {
				break
			}
			data = data[next:]
		}
		return sets
	}
	if lengthSize < 1 || lengthSize > 4 {
		return sets
	}
	for len(data) >= lengthSize {
		var n uint32
		for _, b := range data[:lengthSize] {
			n = n<<8 | uint32(b)
		}
		data = data[lengthSize:]
		if uint64(n) > uint64(len(data)) || n == 0 {
			break
		}
		add(data[:int(n)])
		data = data[int(n):]
	}
	return sets
}

func completeHEVCConfig(data []byte) bool {
	if len(data) < 23 || data[0] != 1 {
		return false
	}
	sets := hevcParameterNALs(data, 0)
	return len(sets[32]) > 0 && len(sets[33]) > 0 && len(sets[34]) > 0
}

// hevcProbeConfig builds a decoder record for the native SPS parser only.
// It must not turn an empty source CodecPrivate into affirmative evidence
// that ffmpeg's copy filter can initialize from that source's extradata.
func hevcProbeConfig(original []byte, sets map[byte][]byte) []byte {
	sps := sets[33]
	if len(sps) < 3 {
		return original
	}
	f, err := hevcSPS(sps)
	if err != nil {
		return original
	}
	raw := rbsp(sps[2:])
	if len(raw) < 13 {
		return original
	}
	p := make([]byte, 23)
	p[0] = 1
	copy(p[1:13], raw[1:13]) // general_profile_tier_level
	p[13], p[15], p[16] = 0xf0, 0xfc, 0xfc|byte(f.chroma)
	p[17], p[18] = 0xf8|byte(max(0, f.bitDepth-8)), 0xf8|byte(max(0, f.bitDepth-8))
	p[21] = 3
	if len(original) >= 23 && original[0] == 1 {
		copy(p, original[:23])
	}
	p[22] = 0
	for _, typ := range []byte{32, 33, 34} {
		n := sets[typ]
		if len(n) == 0 || len(n) > 65535 {
			continue
		}
		p[22]++
		p = append(p, typ, 0, 1, byte(len(n)>>8), byte(len(n)))
		p = append(p, n...)
	}
	return p
}

// firstHEVCParameters scans at most 4 MiB in the first Cluster, on demand
// only when CodecPrivate cannot supply the SPS. No tail or library scan.
func (s *nativeSource) firstHEVCParameters(start, end int64, track uint64, private []byte) []byte {
	if start == 0 {
		return private
	}
	sets := hevcParameterNALs(private, 0)
	if sets == nil {
		sets = make(map[byte][]byte)
	}
	lengthSize := 4
	if len(private) >= 23 && private[0] == 1 {
		lengthSize = int(private[21]&3) + 1
	}
	for off, count := start, 0; off < end && off-start < nativeElementLimit && count < 4096; count++ {
		id, body, stop, err := s.header(off, end)
		if err != nil || stop <= off || stop-start > nativeElementLimit {
			break
		}
		if id == mkvSimpleBlock || id == mkvBlock || id == mkvBlockGroup {
			b, err := s.read(body, int(stop-body))
			if err != nil {
				break
			}
			if id == mkvBlockGroup {
				nodes, ok := mkvNodes(b)
				b = nil
				if ok {
					for _, n := range nodes {
						if n.id == mkvBlock {
							b = n.body
							break
						}
					}
				}
			}
			got, frame := mkvBlockFrame(b)
			if got == track {
				for typ, nal := range hevcParameterNALs(frame, lengthSize) {
					sets[typ] = nal
				}
				if len(sets[33]) > 0 {
					break
				}
			}
		}
		off = stop
	}
	return hevcProbeConfig(private, sets)
}
