package transcode

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// ErrIncompatibleSwap reports a replacement source whose video cannot be
// delivered in the session's fixed output contract.
var ErrIncompatibleSwap = errors.New("transcode: replacement cannot match the delivered video")

// picture is a stream's sample aspect ratio and colour as decoders
// interpret it.
type picture struct {
	sarNum, sarDen int
	prim, matrix   string // ffprobe names
	full           bool   // full (pc) rather than limited (tv) range
}

// pictureOf resolves v's signalled picture format. Unsignalled facts take
// the decoder defaults: square pixels and limited range (the H.264/H.265
// VUI defaults); untagged colour is BT.2020 for PQ/HLG, BT.601 for SD
// (BT.470BG at 576 lines, else SMPTE 170M) and BT.709 otherwise.
func pictureOf(v *core.StreamInfo) picture {
	p := picture{sarNum: 1, sarDen: 1, prim: v.ColorPrimaries, matrix: v.ColorSpace, full: v.ColorRange == "pc"}
	if n, d, ok := strings.Cut(v.SampleAspectRatio, ":"); ok {
		if a, b := atoi(n), atoi(d); a > 0 && b > 0 {
			p.sarNum, p.sarDen = reduce(a, b)
		}
	}
	defPrim, defMatrix := "bt709", "bt709"
	switch r := encodedRange(v); {
	case r == rangeHDR10 || r == rangeHLG:
		defPrim, defMatrix = "bt2020", "bt2020nc"
	case v.Height == 576:
		defPrim, defMatrix = "bt470bg", "bt470bg"
	case v.Height > 0 && v.Height < 576:
		defPrim, defMatrix = "smpte170m", "smpte170m"
	}
	if p.prim == "" {
		p.prim = defPrim
	}
	if p.matrix == "" {
		p.matrix = defMatrix
	}
	return p
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// sameColour reports whether two pictures share primaries, matrix and
// range. BT.601's 625- and 525-line matrices have the same coefficients.
func (p picture) sameColour(o picture) bool {
	m := func(s string) string {
		if s == "bt470bg" {
			return "smpte170m"
		}
		return s
	}
	return p.prim == o.prim && m(p.matrix) == m(o.matrix) && p.full == o.full
}

// videoContract is the video a static-header session delivered first.
// Plex's native player opens a segmented-Matroska EXT-X-MAP once and
// appends every later Cluster to that demuxer, so after a failover the
// pictures must keep the header's codec, bit depth, dynamic range, colour,
// size and sample aspect ratio. In-band parameter sets let the decoder
// accept a new encode, but they do not make a different picture format
// safe to present under the old track description.
type videoContract struct {
	copied bool // the opening decision copied the video
	codec  string
	depth  int    // 8 or 10
	rng    string // rangeSDR, rangeHDR10, rangeHLG; rangeDV is profile 5
	dv     bool   // Dolby Vision signalled in the header (copied RPU)
	// dvProfile and dvCompat are the signalled Dolby Vision configuration.
	dvProfile, dvCompat int
	width               int
	height              int
	pic                 picture
}

// contractOf derives the delivered video from the session's opening
// source and decision, matching what buildArgs produces for it.
func contractOf(info core.MediaInfo, d core.Decision) (*videoContract, bool) {
	v := info.Video()
	if v == nil || v.Width <= 0 || v.Height <= 0 {
		return nil, false
	}
	src := pictureOf(v)
	if d.VideoCopy {
		depth := v.BitDepth
		if depth <= 0 {
			depth = 8
		}
		c := &videoContract{
			copied: true, codec: v.Codec, depth: depth, rng: encodedRange(v),
			dv:    v.DolbyVision && !d.StripDolbyVision,
			width: v.Width, height: v.Height, pic: src,
		}
		if c.dv {
			c.dvProfile, c.dvCompat = v.DolbyVisionProfile, v.DolbyVisionCompatibilityID
		}
		return c, true
	}
	codec := d.VideoCodec
	if codec == "" {
		codec = "h264"
	}
	// planVideo's output tags; it keeps the source's range.
	c := &videoContract{codec: codec, depth: 8, rng: rangeSDR, pic: picture{prim: "bt709", matrix: "bt709", full: src.full}}
	if keepsHDR(v) && !d.ToneMap && codec == "hevc" {
		c.depth, c.rng = 10, encodedRange(v)
		c.pic.prim, c.pic.matrix = "bt2020", "bt2020nc"
	}
	c.width, c.height = decisionOutputSize(v, d)
	if c.width <= 0 || c.height <= 0 {
		return nil, false
	}
	// The scale filter keeps the display aspect, so rounding the width to
	// an even number leaves a slightly non-square sample aspect ratio.
	c.pic.sarNum, c.pic.sarDen = reduce(src.sarNum*v.Width*c.height, src.sarDen*v.Height*c.width)
	return c, true
}

func reduce(n, d int) (int, int) {
	if n <= 0 || d <= 0 {
		return 1, 1
	}
	a, b := n, d
	for b != 0 {
		a, b = b, a%b
	}
	return n / a, d / a
}

func (c *videoContract) String() string {
	r := "tv"
	if c.pic.full {
		r = "pc"
	}
	return fmt.Sprintf("%s %d-bit %s %s/%s/%s %dx%d sar %d:%d", c.codec, c.depth, c.rng,
		c.pic.prim, c.pic.matrix, r, c.width, c.height, c.pic.sarNum, c.pic.sarDen)
}

// copies reports whether v can be stream-copied under the contract.
func (c *videoContract) copies(v *core.StreamInfo) bool {
	depth := v.BitDepth
	if depth <= 0 {
		depth = 8
	}
	p := pictureOf(v)
	if v.Codec != c.codec || depth != c.depth || v.Width != c.width || v.Height != c.height ||
		p.sarNum != c.pic.sarNum || p.sarDen != c.pic.sarDen || !p.sameColour(c.pic) {
		return false
	}
	if c.dv {
		// A Dolby Vision header keeps its RPU: only the same single-layer
		// configuration continues it. Dropping or changing the signalling
		// mid-stream (an HDR10 copy or encode) is not proven safe for the
		// player that opened it.
		return v.DolbyVision && v.DolbyVisionProfile == c.dvProfile && v.DolbyVisionProfile != 7 &&
			v.DolbyVisionCompatibilityID == c.dvCompat && v.DolbyVisionRPUPresent != nil && *v.DolbyVisionRPUPresent &&
			encodedRange(v) == c.rng
	}
	if !v.DolbyVision {
		return dynamicRange(v) == c.rng
	}
	// A Dolby Vision replacement under a non-DV header is copied as its
	// compatible base layer with the RPU stripped (stripsDV).
	return encodedRange(v) == c.rng && c.rng != rangeDV
}

// stripsDV reports whether a copy of v under the contract must drop the
// Dolby Vision RPU and enhancement layer.
func (c *videoContract) stripsDV(v *core.StreamInfo) bool {
	return v.DolbyVision && !c.dv
}

// check reports why the replacement cannot be delivered under the
// contract.
func (c *videoContract) check(info core.MediaInfo, f features) error {
	v := info.Video()
	switch {
	case info.Estimated:
		// Copy or convert decisions need the real stream's format.
		return fmt.Errorf("%w: replacement was not probed", ErrIncompatibleSwap)
	case v == nil:
		return fmt.Errorf("%w: replacement has no video", ErrIncompatibleSwap)
	case c.copied && c.copies(v):
		return nil
	case v.Width <= 0 || v.Height <= 0:
		return fmt.Errorf("%w: replacement size unknown", ErrIncompatibleSwap)
	case c.codec != "h264" && c.codec != "hevc":
		return fmt.Errorf("%w: %s output cannot be re-encoded", ErrIncompatibleSwap, c.codec)
	case c.dv:
		return fmt.Errorf("%w: Dolby Vision profile %d output needs a profile %d copy with its RPU", ErrIncompatibleSwap, c.dvProfile, c.dvProfile)
	case c.rng == rangeDV:
		return fmt.Errorf("%w: Dolby Vision profile 5 output needs a profile 5 copy", ErrIncompatibleSwap)
	case c.depth != 8 && c.depth != 10, c.depth == 10 && c.codec != "hevc":
		return fmt.Errorf("%w: %d-bit %s cannot be encoded", ErrIncompatibleSwap, c.depth, c.codec)
	case c.rng != rangeSDR && c.depth != 10:
		return fmt.Errorf("%w: %s needs 10-bit output", ErrIncompatibleSwap, c.rng)
	case encodedRange(v) == rangeDV:
		return fmt.Errorf("%w: Dolby Vision profile 5 has no base layer to convert", ErrIncompatibleSwap)
	case c.tonemaps(v) && !c.pic.sameColour(picture{prim: "bt709", matrix: "bt709"}):
		// The tone mappers emit limited-range BT.709 only.
		return fmt.Errorf("%w: tone mapping cannot produce %s", ErrIncompatibleSwap, c)
	case c.tonemaps(v) && !canToneMap(f):
		return fmt.Errorf("%w: %s", ErrIncompatibleSwap, toneMapUnavailableMessage())
	case c.convertsColour(v) && !canConvertColour(v, c, f):
		return fmt.Errorf("%w: %s", ErrIncompatibleSwap, colourUnavailableMessage())
	}
	return nil
}

// tonemaps reports whether an encode of v maps HDR down to the SDR
// contract.
func (c *videoContract) tonemaps(v *core.StreamInfo) bool {
	return c.rng == rangeSDR && encodedRange(v) != rangeSDR
}

// convertsColour reports whether an encode of v needs a colour
// conversion: SDR into PQ/HLG, between PQ and HLG, or other primaries,
// matrix or range at the same dynamic range. zscale does this when the
// build has it; otherwise an SDR conversion uses colorspace or scale.
func (c *videoContract) convertsColour(v *core.StreamInfo) bool {
	if c.tonemaps(v) {
		return false
	}
	return encodedRange(v) != c.rng || !pictureOf(v).sameColour(c.pic)
}

// fit is the size v's picture scales to inside the contract's frame,
// keeping its display aspect ratio. The remainder is padded; nothing is
// cropped or stretched.
func (c *videoContract) fit(v *core.StreamInfo) (int, int) {
	if v.Width <= 0 || v.Height <= 0 {
		return c.width, c.height
	}
	src := pictureOf(v)
	// The source's display aspect in contract sample units.
	aspect := float64(v.Width*src.sarNum*c.pic.sarDen) / float64(v.Height*src.sarDen*c.pic.sarNum)
	w, h := float64(c.height)*aspect, float64(c.height)
	if w > float64(c.width) {
		w, h = float64(c.width), float64(c.width)/aspect
	}
	fw, fh := even(int(w+0.5)), even(int(h+0.5))
	// Rounding within two pixels of the frame is not worth a bar.
	if c.width-fw <= 2 {
		fw = c.width
	}
	if c.height-fh <= 2 {
		fh = c.height
	}
	return min(fw, c.width), min(fh, c.height)
}

// zscaleColour converts v's decoded picture, read with its signalled (or
// pictureOf default) primaries, matrix and range, to the contract's.
//
// SDR into PQ or HLG: zimg's display-referred path (FFmpeg never sets
// scene_referred) decodes BT.709/601 SDR through the BT.1886 EOTF,
// matrixes the primaries to BT.2020, and maps linear 1.0 to npl cd/m²
// before the PQ inverse EOTF (to_gamma_scale npl/10000), or relative to
// the 1000 cd/m² HLG reference display. npl=203 is Report ITU-R BT.2408-7
// §5.1.2's display-referred SDR mapping: 2.03× linear scaling, so 100% SDR
// lands on HDR reference white (203 cd/m², 58% PQ; 75% HLG). For HLG<->PQ
// the npl factors cancel and the 1000 cd/m² reference condition of §6.2
// applies. SDR to SDR keeps the BT.709 curve and converts only primaries,
// matrix and range.
func zscaleColour(v *core.StreamInfo, c *videoContract) string {
	src := pictureOf(v)
	trc := func(r string) string {
		switch r {
		case rangeHDR10:
			return "smpte2084"
		case rangeHLG:
			return "arib-std-b67"
		}
		return "bt709"
	}
	lim := func(full bool) string {
		if full {
			return "full"
		}
		return "limited"
	}
	return fmt.Sprintf("zscale=tin=%s:pin=%s:min=%s:rin=%s:t=%s:p=%s:m=%s:r=%s:npl=203",
		trc(encodedRange(v)), src.prim, src.matrix, lim(src.full), trc(c.rng), c.pic.prim, c.pic.matrix, lim(c.pic.full))
}

// adaptToContract adapts a failover decision (adaptDecision) so the video
// keeps the session's contract: copied only when the replacement already
// is the contract's stream and the opening decision was a copy, otherwise
// encoded to the contract (planVideo with runSpec.Target).
func adaptToContract(old core.Decision, oldInfo, newInfo core.MediaInfo, container string, c *videoContract) core.Decision {
	d := adaptDecision(old, oldInfo, newInfo, container)
	newV := newInfo.Video()
	if c == nil || newV == nil {
		return d
	}
	if c.copied && c.copies(newV) && !d.BurnSubtitles {
		d.Kind = core.DecisionRemux
		d.VideoCopy = true
		d.VideoCodec, d.MaxHeight = newV.Codec, newV.Height
		d.ToneMap = false
		d.StripDolbyVision = c.stripsDV(newV)
		return d
	}
	if d.VideoCopy {
		d.Reason += fmt.Sprintf("; failover: video %s %dx%d re-encoded to the delivered %s", newV.Codec, newV.Width, newV.Height, c)
	}
	d.Kind = core.DecisionTranscode
	d.VideoCopy = false
	d.VideoCodec, d.MaxHeight = c.codec, c.height
	if d.BitrateKbps <= 0 {
		d.BitrateKbps = bitrateCeiling(c.height, c.codec)
	}
	d.ToneMap = c.tonemaps(newV)
	d.StripDolbyVision = false
	return d
}
