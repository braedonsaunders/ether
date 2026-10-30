package transcode

import "errors"

// Bitstream readers for the parameter sets native probing needs: H.264 and
// HEVC SPS video usability information (colour transfer, bit depth). Only
// header syntax is parsed; no slice data is ever decoded.

var errBits = errors.New("transcode: truncated bitstream")

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
	// chroma is chroma_format_idc. Baseline and Main are 4:2:0.
	chroma int
	// VUI picture format; zero when not signalled (fullRange -1).
	sarNum, sarDen    int
	fullRange         int
	primaries, matrix int
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
	r.ue()    // max_num_ref_frames
	r.skip(1) // gaps_in_frame_num_value_allowed_flag
	r.ue()    // pic_width_in_mbs_minus1
	r.ue()    // pic_height_in_map_units_minus1
	if !r.flag() {
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
		vuiFacts(r, &f)
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
func vuiFacts(r *bitReader, f *spsFacts) {
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
	r.skip(3) // video_format
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
	if len(nal) < 3 || (nal[0]>>1)&0x3f != 33 {
		return spsFacts{}, errBits
	}
	r := &bitReader{b: rbsp(nal[2:])}
	r.skip(4) // sps_video_parameter_set_id
	maxSub := int(r.u(3))
	r.skip(1) // sps_temporal_id_nesting_flag
	// profile_tier_level(1, maxSub)
	r.skip(96)
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
	r.ue() // sps_seq_parameter_set_id
	if r.ue() == 3 {
		r.skip(1) // separate_colour_plane_flag
	}
	r.ue() // pic_width_in_luma_samples
	r.ue() // pic_height_in_luma_samples
	if r.flag() {
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	f := spsFacts{bitDepth: int(r.ue()) + 8, fullRange: -1}
	r.ue() // bit_depth_chroma_minus8
	pocBits := int(r.ue()) + 4
	ordering := r.flag()
	first := maxSub
	if ordering {
		first = 0
	}
	for i := first; i <= maxSub; i++ {
		r.ue()
		r.ue()
		r.ue()
	}
	for i := 0; i < 6; i++ {
		r.ue() // coding/transform block sizes and hierarchy depths
	}
	if r.flag() && r.flag() { // scaling_list_enabled_flag, sps_scaling_list_data_present_flag
		hevcScalingListData(r)
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
		if neg > 16 || pos > 16 {
			return spsFacts{}, errBits
		}
		for j := uint64(0); j < neg+pos && r.err == nil; j++ {
			r.ue()
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
		vuiFacts(r, &f)
	}
	if r.err != nil {
		return spsFacts{}, r.err
	}
	return f, nil
}

func hevcScalingListData(r *bitReader) {
	for size := 0; size < 4; size++ {
		step := 1
		if size == 3 {
			step = 3
		}
		for matrix := 0; matrix < 6; matrix += step {
			if !r.flag() { // scaling_list_pred_mode_flag
				r.ue()
				continue
			}
			coefs := min(64, 1<<(4+(size<<1)))
			if size > 1 {
				r.se()
			}
			for i := 0; i < coefs && r.err == nil; i++ {
				r.se()
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
