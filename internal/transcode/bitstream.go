package transcode

import "errors"

// Bitstream readers for the parameter sets native probing needs: H.264 and
// HEVC SPS video usability information (colour transfer, bit depth). Only
// header syntax is parsed; no slice data is ever decoded.

var errBits = errors.New("transcode: truncated bitstream")
var errHEVCExtension = errors.New("transcode: unsupported HEVC SPS extension")

type bitReader struct {
	b   []byte
	pos int // bit offset
	err error
}

// rbsp removes emulation-prevention bytes (00 00 03 -> 00 00).
func rbsp(nal []byte) []byte {
	out := make([]byte, 0, len(nal))
	zeros := 0
	for _, c := range nal {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

func (r *bitReader) u(n int) uint64 {
	if r.err != nil || n > 64 {
		r.err = errBits
		return 0
	}
	if r.pos+n > len(r.b)*8 {
		r.err = errBits
		return 0
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<1 | uint64(r.b[r.pos>>3]>>(7-uint(r.pos&7))&1)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.u(1) == 1 }

func (r *bitReader) skip(n int) {
	if r.err == nil && r.pos+n > len(r.b)*8 {
		r.err = errBits
		return
	}
	r.pos += n
}

// ue reads an unsigned Exp-Golomb code. Values above 2^31 are rejected as
// malformed: no parameter set field legitimately needs them.
func (r *bitReader) ue() uint64 {
	zeros := 0
	for r.err == nil && !r.flag() {
		zeros++
		if zeros > 31 {
			r.err = errBits
			return 0
		}
	}
	if r.err != nil {
		return 0
	}
	return (uint64(1)<<uint(zeros) - 1) + r.u(zeros)
}

func (r *bitReader) se() int64 {
	k := r.ue()
	if k&1 == 1 {
		return int64((k + 1) / 2)
	}
	return -int64(k / 2)
}

// spsFacts are the SPS fields Ether's decisions depend on. transfer is the
// H.273 transfer_characteristics code; 0 when the VUI does not carry one.
type spsFacts struct {
	bitDepth int
	transfer int
	// chroma is chroma_format_idc. H.264 omits it for the 8-bit 4:2:0 profiles,
	// where the specification default is 1. Zero means monochrome.

	chroma int
	// VUI picture format; zero when not signalled (fullRange -1).
	sarNum, sarDen    int
	fullRange         int
	primaries, matrix int
	// refs is max_num_ref_frames (H.264) or the highest temporal layer's
	// max_dec_pic_buffering (H.265). hasRefs is false when the SPS did not
	// reach that field.
	refs       int
	hasRefs    bool
	interlaced *bool
}

// avcSPS parses an H.264 sequence parameter set NAL (header byte included).
func avcSPS(nal []byte) (spsFacts, error) {
	if len(nal) < 4 || nal[0]&0x1f != 7 {
		return spsFacts{}, errBits
	}
	r := &bitReader{b: rbsp(nal[1:])}
	profile := r.u(8)
	r.skip(8) // constraint flags
	r.skip(8) // level
	r.ue()    // seq_parameter_set_id
	f := spsFacts{bitDepth: 8, fullRange: -1, chroma: 1}
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ue()
		f.chroma = int(chroma)
		if chroma == 3 {
			r.skip(1) // separate_colour_plane_flag
		}
		f.bitDepth = int(r.ue()) + 8
		r.ue()    // bit_depth_chroma_minus8
		r.skip(1) // qpprime_y_zero_transform_bypass_flag
		if r.flag() {
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists && r.err == nil; i++ {
				if r.flag() {
					size := 16
					if i >= 6 {
						size = 64
					}
					avcScalingList(r, size)
				}
			}
		}
	}
	r.ue() // log2_max_frame_num_minus4
	switch r.ue() {
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.skip(1)
		r.se()
		r.se()
		n := r.ue()
		if n > 255 {
			return spsFacts{}, errBits
		}
		for i := uint64(0); i < n && r.err == nil; i++ {
			r.se()
		}
	}
	f.refs = int(r.ue()) // max_num_ref_frames
	f.hasRefs = r.err == nil
	r.skip(1)             // gaps_in_frame_num_value_allowed_flag
	r.ue()                // pic_width_in_mbs_minus1
	r.ue()                // pic_height_in_map_units_minus1
	frameOnly := r.flag() // frame_mbs_only_flag
	interlaced := !frameOnly
	f.interlaced = &interlaced
	if !frameOnly {
		r.skip(1) // mb_adaptive_frame_field_flag
	}
	r.skip(1) // direct_8x8_inference_flag
	if r.flag() {
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	if r.err != nil {
		return spsFacts{}, r.err
	}
	if r.flag() {
		vuiFacts(r, &f, false)
	}
	if r.err != nil {
		return spsFacts{}, r.err
	}
	return f, nil
}

func avcScalingList(r *bitReader, size int) {
	last, next := int64(8), int64(8)
	for j := 0; j < size && r.err == nil; j++ {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

// vuiSAR is Table E-1 of H.264/H.265: aspect_ratio_idc 1..16.
var vuiSAR = [...][2]int{{1, 1}, {12, 11}, {10, 11}, {16, 11}, {40, 33}, {24, 11}, {20, 11}, {32, 11},
	{80, 33}, {18, 11}, {15, 11}, {64, 33}, {160, 99}, {4, 3}, {3, 2}, {2, 1}}

// vuiFacts reads the start of vui_parameters, which H.264 and H.265 share,
// through the colour description.
func vuiFacts(r *bitReader, f *spsFacts, strict bool) {
	if r.flag() { // aspect_ratio_info_present_flag
		idc := int(r.u(8))
		switch {
		case idc == 255:
			f.sarNum, f.sarDen = int(r.u(16)), int(r.u(16))
		case idc >= 1 && idc <= len(vuiSAR):
			f.sarNum, f.sarDen = vuiSAR[idc-1][0], vuiSAR[idc-1][1]
		}
	}
	if r.flag() { // overscan_info_present_flag
		r.skip(1)
	}
	if !r.flag() { // video_signal_type_present_flag
		return
	}
	format := r.u(3) // video_format
	if strict && format > 5 {
		r.err = errBits
	}
	f.fullRange = int(r.u(1))
	if !r.flag() {
		return
	}
	f.primaries = int(r.u(8))
	f.transfer = int(r.u(8))
	f.matrix = int(r.u(8))
}

// primariesName and matrixName map H.273 codes to ffprobe's names.
func primariesName(code int) string {
	switch code {
	case 1:
		return "bt709"
	case 4:
		return "bt470m"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 9:
		return "bt2020"
	}
	return ""
}

func matrixName(code int) string {
	switch code {
	case 1:
		return "bt709"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 9:
		return "bt2020nc"
	case 10:
		return "bt2020c"
	}
	return ""
}

// hevcSPS parses an HEVC sequence parameter set NAL (2-byte header
// included), following ITU-T H.265 7.3.2.2 through the VUI colour
// description.
func hevcSPS(nal []byte) (spsFacts, error) {
	return parseHEVCSPS(nal, false)
}

// parseHEVCSPS can additionally validate the complete VUI and SPS tail.
// Unsupported extension syntax is distinguished from a malformed header.
func parseHEVCSPS(nal []byte, strict bool) (spsFacts, error) {
	if len(nal) < 3 || (nal[0]>>1)&0x3f != 33 {
		return spsFacts{}, errBits
	}
	if strict && (nal[0]&0x80 != 0 || nal[1]&7 == 0) {
		return spsFacts{}, errBits
	}
	if strict && (nal[0]&1 != 0 || nal[1]>>3 != 0) && (nal[2]>>1)&7 == 7 {
		return spsFacts{}, errHEVCExtension // multilayer SPS needs its VPS
	}
	if strict {
		zeros := 0
		for i, b := range nal[2:] {
			if zeros >= 2 {
				if b <= 2 || (b == 3 && (i+1 == len(nal)-2 || nal[i+3] > 3)) {
					return spsFacts{}, errBits
				}
				if b == 3 {
					zeros = 0
					continue
				}
			}
			if b == 0 {
				zeros++
			} else {
				zeros = 0
			}
		}
	}
	r := &bitReader{b: rbsp(nal[2:])}
	r.skip(4) // sps_video_parameter_set_id
	maxSub := int(r.u(3))
	if strict && maxSub > 6 {
		return spsFacts{}, errBits
	}
	r.skip(1) // sps_temporal_id_nesting_flag
	// profile_tier_level(1, maxSub): source scan constraints precede
	// the remaining constraint flags and level.
	r.skip(40)
	progressiveSource, interlacedSource := r.flag(), r.flag()
	r.skip(54)
	subProfile := make([]bool, maxSub)
	subLevel := make([]bool, maxSub)
	for i := 0; i < maxSub; i++ {
		subProfile[i] = r.flag()
		subLevel[i] = r.flag()
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			r.skip(2)
		}
	}
	for i := 0; i < maxSub; i++ {
		if subProfile[i] {
			r.skip(88)
		}
		if subLevel[i] {
			r.skip(8)
		}
	}
	id := r.ue() // sps_seq_parameter_set_id
	chroma := int(r.ue())
	if strict && (id > 15 || chroma > 3) {
		return spsFacts{}, errBits
	}
	separateColour := false
	if chroma == 3 {
		separateColour = r.flag()
	}
	width := r.ue()  // pic_width_in_luma_samples
	height := r.ue() // pic_height_in_luma_samples
	if strict && (width == 0 || height == 0) {
		return spsFacts{}, errBits
	}
	var crop [4]uint64
	if r.flag() {
		left, right, top, bottom := r.ue(), r.ue(), r.ue(), r.ue()
		crop = [4]uint64{left, right, top, bottom}
		if strict && (left > width || right > width || top > height || bottom > height) {
			return spsFacts{}, errBits
		}
	}
	subWidth, subHeight := uint64(1), uint64(1)
	if !separateColour && (chroma == 1 || chroma == 2) {
		subWidth = 2
	}
	if !separateColour && chroma == 1 {
		subHeight = 2
	}
	if strict && ((crop[0]+crop[1])*subWidth >= width || (crop[2]+crop[3])*subHeight >= height) {
		return spsFacts{}, errBits
	}
	f := spsFacts{bitDepth: int(r.ue()) + 8, fullRange: -1, chroma: chroma}
	if progressiveSource != interlacedSource {
		f.interlaced = &interlacedSource
	}
	chromaDepth := int(r.ue()) + 8 // bit_depth_chroma_minus8
	pocBits := int(r.ue()) + 4
	if strict && (f.bitDepth > 16 || chromaDepth > 16 || pocBits > 16) {
		return spsFacts{}, errBits
	}
	ordering := r.flag()
	first := maxSub
	if ordering {
		first = 0
	}
	for i := first; i <= maxSub; i++ {
		// Highest layer wins: ffprobe's refs is that layer's DPB size.
		f.refs = int(r.ue()) + 1
		f.hasRefs = r.err == nil
		reorder := r.ue()
		if strict && (f.refs > 16 || reorder >= uint64(f.refs)) {
			return spsFacts{}, errBits
		}
		r.ue()
	}
	minCB, diffCB := r.ue(), r.ue()
	minTB, diffTB := r.ue(), r.ue()
	interDepth, intraDepth := r.ue(), r.ue()
	if strict && (minCB > 3 || diffCB > 3 || minTB > minCB || minTB+diffTB > min(5, minCB+diffCB+3)-2 || interDepth > minCB+diffCB+1-minTB || intraDepth > minCB+diffCB+1-minTB) {
		return spsFacts{}, errBits
	}
	if r.flag() { // scaling_list_enabled_flag
		if r.flag() { // sps_scaling_list_data_present_flag
			hevcScalingListData(r, strict)
		}
	}
	r.skip(2)     // amp_enabled_flag, sample_adaptive_offset_enabled_flag
	if r.flag() { // pcm_enabled_flag
		r.skip(8)
		r.ue()
		r.ue()
		r.skip(1)
	}
	sets := int(r.ue())
	if sets > 64 || r.err != nil {
		return spsFacts{}, errBits
	}
	deltas := make([]int, sets)
	for i := 0; i < sets && r.err == nil; i++ {
		inter := false
		if i != 0 {
			inter = r.flag()
		}
		if inter {
			r.skip(1) // delta_rps_sign
			r.ue()    // abs_delta_rps_minus1
			n := 0
			for j := 0; j <= deltas[i-1] && r.err == nil; j++ {
				used := r.flag()
				use := true
				if !used {
					use = r.flag()
				}
				if used || use {
					n++
				}
			}
			deltas[i] = n
			continue
		}
		neg, pos := r.ue(), r.ue()
		if neg > 16 || pos > 16 || (strict && neg+pos > 15) {
			return spsFacts{}, errBits
		}
		for j := uint64(0); j < neg+pos && r.err == nil; j++ {
			delta := r.ue()
			if strict && delta > 32767 {
				return spsFacts{}, errBits
			}
			r.skip(1)
		}
		deltas[i] = int(neg + pos)
	}
	if r.flag() { // long_term_ref_pics_present_flag
		n := r.ue()
		if n > 32 {
			return spsFacts{}, errBits
		}
		for i := uint64(0); i < n && r.err == nil; i++ {
			r.skip(pocBits + 1)
		}
	}
	r.skip(2) // sps_temporal_mvp_enabled_flag, strong_intra_smoothing_enabled_flag
	if r.err != nil {
		return spsFacts{}, r.err
	}
	if r.flag() {
		vuiFacts(r, &f, strict)
		if r.flag() { // chroma_loc_info_present_flag
			r.ueMax(5)
			r.ueMax(5)
		}
		r.skip(1)            // neutral_chroma_indication_flag
		fieldSeq := r.flag() // field_seq_flag
		// Frame pictures may still contain interlaced content. Only an
		// affirmative field sequence proves interlacing on its own.
		if fieldSeq {
			f.interlaced = &fieldSeq
		}
		r.skip(1) // frame_field_info_present_flag
		if strict {
			hevcVUITail(r, maxSub, width, height, subWidth, subHeight, crop)
		}
	}
	if strict && r.err == nil {
		hevcSPSTail(r, chroma, f.bitDepth, chromaDepth, minCB, diffCB)
	}
	if r.err != nil {
		return spsFacts{}, r.err
	}
	return f, nil
}

func hevcScalingListData(r *bitReader, strict bool) {
	for size := 0; size < 4; size++ {
		step := 1
		if size == 3 {
			step = 3
		}
		for matrix := 0; matrix < 6; matrix += step {
			if !r.flag() { // scaling_list_pred_mode_flag
				delta := r.ue()
				limit := matrix
				if size == 3 {
					limit /= 3
				}
				if strict && delta > uint64(limit) {
					r.err = errBits
				}
				continue
			}
			coefs := min(64, 1<<(4+(size<<1)))
			if size > 1 {
				dc := r.se()
				if strict && (dc < -7 || dc > 247) {
					r.err = errBits
				}
			}
			for i := 0; i < coefs && r.err == nil; i++ {
				coeff := r.se()
				if strict && (coeff < -128 || coeff > 127) {
					r.err = errBits
				}
			}
		}
	}
}

// transferName maps an H.273 transfer_characteristics code to ffmpeg's
// color_transfer name, as ffprobe reports it.
func transferName(code int) string {
	switch code {
	case 1:
		return "bt709"
	case 4:
		return "bt470m"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 8:
		return "linear"
	case 9:
		return "log100"
	case 10:
		return "log316"
	case 11:
		return "iec61966-2-4"
	case 12:
		return "bt1361e"
	case 13:
		return "iec61966-2-1"
	case 14:
		return "bt2020-10"
	case 15:
		return "bt2020-12"
	case 16:
		return "smpte2084"
	case 17:
		return "smpte428"
	case 18:
		return "arib-std-b67"
	}
	return ""
}
