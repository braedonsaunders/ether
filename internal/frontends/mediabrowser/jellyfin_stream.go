package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"strconv"
)

// jellyfinStreamDTO exposes only probed values, preserving unknowns as absent.
func (i *instance) jellyfinStreamDTO(dto map[string]any, s frontend.StreamInfo) {
	streamMetadataDTO(dto, s)
	if s.AverageFrameRate != nil {
		dto["AverageFrameRate"] = *s.AverageFrameRate
	}
	if s.ReferenceFrameRate != nil {
		dto["ReferenceFrameRate"] = *s.ReferenceFrameRate
	}
	for key, value := range map[string]string{"AspectRatio": s.DisplayAspectRatio, "PixelFormat": s.PixelFormat, "CodecTag": s.CodecTag, "TimeBase": s.TimeBase, "ColorRange": s.ColorRange, "ColorSpace": s.ColorSpace, "ColorTransfer": s.ColorTransfer, "ColorPrimaries": s.ColorPrimaries} {
		if value != "" {
			dto[key] = value
		}
	}
	if s.NALLengthSize != nil {
		dto["NalLengthSize"] = strconv.Itoa(*s.NALLengthSize)
	}
	if s.ReferenceFrames != nil {
		dto["RefFrames"] = *s.ReferenceFrames
	}
	if s.BitsPerRawSample != nil {
		dto["BitDepth"] = *s.BitsPerRawSample
	}
	if s.ProbedLevel != nil {
		dto["Level"] = *s.ProbedLevel
	}
	if s.ProbedInterlaced != nil {
		dto["IsInterlaced"] = *s.ProbedInterlaced
	}
}
