package transcode

import (
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
)

// GraphError is a transcode this ffmpeg cannot build. The message is
// owner-visible (Activity and the transcoder health card). It is not an
// ffmpeg stderr tail, and it is not retried as the same filter graph.
type GraphError struct{ Message string }

func (e *GraphError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func toneMapUnavailableMessage() string {
	return "This FFmpeg cannot tone-map HDR video to SDR (no zscale, libplacebo, OpenCL, or hardware tone-mapping). Playback will try another release that does not need tone mapping."
}

func colourUnavailableMessage() string {
	return "This FFmpeg cannot convert this video's colours (no zscale or colorspace filter). Playback will try another release."
}

func filterUnavailableMessage(name string) string {
	return fmt.Sprintf("FFmpeg has no %q filter, and no other conversion graph can play this video.", name)
}

// canToneMap reports whether any probed filter can map HDR to SDR. Hardware
// filters count even when this session is not using that accelerator: the
// swap check does not know which encoder the next run will pick.
func canToneMap(f features) bool {
	if _, ok := cpuToneMap(f); ok {
		return true
	}
	return f.hasFilter("tonemap_vaapi") || f.hasFilter("tonemap_videotoolbox") || f.hasFilter("scale_vt") || f.hasFilter("tonemap_cuda")
}

// canConvertColour reports whether v can be encoded under c's colours.
func canConvertColour(v *core.StreamInfo, c *videoContract, f features) bool {
	_, ok := colourFilter(v, c, f)
	return ok
}

// colourFilter chooses the colour conversion for v into c.
// zscale is used when this ffmpeg has it: the npl=203 path is the accurate
// SDR↔HDR conversion. SDR matrix and range changes with the same primaries
// use scale, which is the fast correct path. Different SDR primaries (a
// BT.601 DVD into BT.709) use colorspace. Unknown filters are not treated
// as present.
func colourFilter(v *core.StreamInfo, c *videoContract, f features) (string, bool) {
	if c == nil || v == nil || !c.convertsColour(v) {
		return "", true
	}
	if f.hasFilter("zscale") {
		return zscaleColour(v, c), true
	}
	src := pictureOf(v)
	if encodedRange(v) != c.rng || c.rng != rangeSDR {
		return "", false
	}
	if src.prim == c.pic.prim && f.hasFilter("scale") {
		return scaleColour(src, c.pic), true
	}
	if f.hasFilter("colorspace") {
		return colorspaceColour(src, c.pic), true
	}
	return "", false
}

func colourRange(full bool) string {
	if full {
		return "pc"
	}
	return "tv"
}

func scaleMatrix(m string) string {
	switch m {
	case "bt2020nc", "bt2020ncl":
		return "bt2020"
	default:
		return m
	}
}

func colorspaceMatrix(m string) string {
	if m == "bt2020ncl" {
		return "bt2020nc"
	}
	return m
}

func scaleColour(src, dst picture) string {
	return fmt.Sprintf("scale=in_color_matrix=%s:out_color_matrix=%s:in_range=%s:out_range=%s",
		scaleMatrix(src.matrix), scaleMatrix(dst.matrix), colourRange(src.full), colourRange(dst.full))
}

func colorspaceColour(src, dst picture) string {
	return fmt.Sprintf("colorspace=ispace=%s:iprimaries=%s:itrc=bt709:irange=%s:space=%s:primaries=%s:trc=bt709:range=%s",
		colorspaceMatrix(src.matrix), src.prim, colourRange(src.full), colorspaceMatrix(dst.matrix), dst.prim, colourRange(dst.full))
}

// graphFailure is the owner-visible reason s cannot be started, or nil
// when the filter graph is one this ffmpeg can run.
func graphFailure(s runSpec) error {
	if s.Decision.VideoCopy {
		return nil
	}
	v := s.Info.Video()
	if v == nil {
		return nil
	}
	return planVideo(s, v).err
}

// toneMapPaths lists the HDR→SDR paths this ffmpeg and accelerator can run.
func toneMapPaths(f features, hw core.HWAccel) []string {
	var out []string
	if f.hasFilter("zscale") && f.hasFilter("tonemap") {
		out = append(out, "zscale")
	}
	if f.hasFilter("libplacebo") {
		out = append(out, "libplacebo")
	}
	if f.hasFilter("tonemap_opencl") {
		out = append(out, "opencl")
	}
	if hw.Name == AccelVAAPI && !hw.NoGPUTonemap && f.hasFilter("tonemap_vaapi") {
		out = append(out, "vaapi")
	}
	if hw.Name == AccelVideoToolbox && (f.hasFilter("tonemap_videotoolbox") || f.hasFilter("scale_vt")) {
		out = append(out, "videotoolbox")
	}
	if hw.Name == AccelNVENC && f.hasFilter("tonemap_cuda") {
		out = append(out, "cuda")
	}
	return out
}

func colourPaths(f features) []string {
	var out []string
	for _, name := range []string{"zscale", "colorspace", "scale"} {
		if f.hasFilter(name) {
			out = append(out, name)
		}
	}
	return out
}
