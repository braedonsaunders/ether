package transcode

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// A missing configuration record or an unsupported extension leaves the
// verdict unknown; it does not establish that the stream is malformed.
func malformedHEVCConfig(data []byte) bool {
	check := func(nal []byte) bool {
		_, err := parseHEVCSPS(nal, true)
		return err != nil && !errors.Is(err, errHEVCExtension)
	}
	if len(data) >= 23 && data[0] == 1 { // HEVCDecoderConfigurationRecord
		arrays, rest := int(data[22]), data[23:]
		for i := 0; i < arrays; i++ {
			if len(rest) < 3 {
				return false
			}
			kind, count := rest[0]&63, int(binary.BigEndian.Uint16(rest[1:3]))
			rest = rest[3:]
			for j := 0; j < count; j++ {
				if len(rest) < 2 {
					return false
				}
				n := int(binary.BigEndian.Uint16(rest[:2]))
				rest = rest[2:]
				if len(rest) < n {
					return false
				}
				if kind == 33 && check(rest[:n]) {
					return true
				}
				rest = rest[n:]
			}
		}
		return false
	}
	// MPEG-TS and raw HEVC extradata use Annex B start codes.
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
		if end > 1 && (data[0]>>1)&63 == 33 && check(data[:end]) {
			return true
		}
		if next < 0 {
			break
		}
		data = data[next:]
	}
	return false
}

// The VUI prefix is shared with AVC in vuiFacts. These routines follow the
// remaining HEVC syntax in H.265 Annex E and sections 7.3.2.2 and 7.3.2.2.3.
func (r *bitReader) ueMax(max uint64) uint64 {
	v := r.ue()
	if v > max {
		r.err = errBits
	}
	return v
}

func hevcVUITail(r *bitReader, maxSub int, width, height, subWidth, subHeight uint64, crop [4]uint64) {
	if r.flag() {
		for i := 0; i < 4; i++ {
			crop[i] += r.ue()
		}
		if (crop[0]+crop[1])*subWidth >= width || (crop[2]+crop[3])*subHeight >= height {
			r.err = errBits
		}
	}
	if r.flag() { // timing information
		numUnitsInTick := r.u(32)
		if numUnitsInTick == 0 || r.u(32) == 0 { // num_units_in_tick, time_scale
			r.err = errBits
		}
		if r.flag() {
			r.ue()
		}
		if r.flag() {
			hevcHRD(r, maxSub)
		}
	}
	if r.flag() { // bitstream restrictions
		r.skip(3)
		r.ueMax(4095)
		for i := 0; i < 4; i++ {
			r.ueMax(16)
		}
	}
}

func hevcHRD(r *bitReader, maxSub int) {
	nal, vcl := r.flag(), r.flag()
	subPic := false
	if nal || vcl {
		subPic = r.flag()
		if subPic {
			r.skip(19)
		}
		r.skip(8)
		if subPic {
			r.skip(4)
		}
		r.skip(15)
	}
	for i := 0; i <= maxSub && r.err == nil; i++ {
		fixed := r.flag()
		if !fixed {
			fixed = r.flag()
		}
		lowDelay := false
		if fixed {
			r.ueMax(2047)
		} else {
			lowDelay = r.flag()
		}
		count := uint64(0)
		if !lowDelay {
			count = r.ueMax(31)
		}
		for _, present := range []bool{nal, vcl} {
			if !present {
				continue
			}
			for j := uint64(0); j <= count && r.err == nil; j++ {
				r.ue()
				r.ue()
				if subPic {
					r.ue()
					r.ue()
				}
				r.skip(1)
			}
		}
	}
}

func hevcSPSTail(r *bitReader, chroma, lumaDepth, chromaDepth int, minCB, diffCB uint64) {
	if r.flag() {
		rangeExt, multiExt, ext3D, scc := r.flag(), r.flag(), r.flag(), r.flag()
		extBits := r.u(4)
		if rangeExt {
			r.skip(9)
		}
		if multiExt {
			r.skip(1)
		}
		if ext3D {
			r.skip(2)
			if v := r.ueMax(minCB + diffCB); v < minCB {
				r.err = errBits
			}
			r.skip(4)
			r.skip(3)
			if v := r.ueMax(minCB + diffCB); v < minCB {
				r.err = errBits
			}
			r.skip(5)
		}
		if scc {
			r.skip(1)
			if r.flag() {
				r.ueMax(64)
				r.ueMax(128)
				if r.flag() {
					n := r.ueMax(127) + 1
					components := 3
					if chroma == 0 {
						components = 1
					}
					for c := 0; c < components; c++ {
						depth := chromaDepth
						if c == 0 {
							depth = lumaDepth
						}
						r.skip(int(n) * depth)
					}
				}
			}
			if r.u(2) > 2 {
				r.err = errBits
			}
			r.skip(1)
		}
		if extBits != 0 {
			for r.err == nil && r.moreRBSPData() {
				r.skip(1)
			}
		}
	}
	if !r.flag() { // rbsp_stop_one_bit
		r.err = errBits
	}
	for r.pos%8 != 0 && r.err == nil {
		if r.flag() {
			r.err = errBits
		}
	}
	if r.err == nil && r.pos != len(r.b)*8 {
		r.err = errBits
	}
}

func (r *bitReader) moreRBSPData() bool {
	remaining := len(r.b)*8 - r.pos
	if remaining > 8 || remaining <= 0 {
		return remaining > 0
	}
	copy := *r
	return copy.u(remaining) != uint64(1)<<uint(remaining-1)
}
