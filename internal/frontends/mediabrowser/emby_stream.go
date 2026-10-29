package mediabrowser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) embyMediaStreamDTO(dto map[string]any, stream frontend.StreamInfo) {
	delete(dto, "IsOriginal") // This field is not part of Emby's stream DTO.
	if stream.CodecTag != "" {
		dto["CodecTag"] = stream.CodecTag
	}
	if stream.TimeBase != "" {
		dto["TimeBase"] = stream.TimeBase
	}
	if stream.PixelFormat != "" {
		dto["PixelFormat"] = stream.PixelFormat
	}
	if stream.AverageFrameRate != nil {
		dto["AverageFrameRate"] = *stream.AverageFrameRate
	}
	if stream.ReferenceFrameRate != nil {
		dto["RealFrameRate"] = *stream.ReferenceFrameRate
	}
	if stream.NALLengthSize != nil {
		dto["NalLengthSize"] = strconv.Itoa(*stream.NALLengthSize)
	}
	if stream.ReferenceFrames != nil {
		dto["RefFrames"] = *stream.ReferenceFrames
	}
	if stream.ProbedLevel != nil {
		dto["Level"] = *stream.ProbedLevel
	}
	if stream.BitsPerRawSample != nil {
		dto["BitDepth"] = *stream.BitsPerRawSample
	}
	if stream.ProbedInterlaced != nil {
		dto["IsInterlaced"] = *stream.ProbedInterlaced
	} else if !stream.Interlaced {
		delete(dto, "IsInterlaced")
	}
	if stream.DisplayAspectRatio != "" {
		dto["AspectRatio"] = stream.DisplayAspectRatio
		return
	}
	if stream.Type != "video" || stream.Width <= 0 || stream.Height <= 0 || stream.Width > 100000 || stream.Height > 100000 {
		return
	}
	parts := strings.Split(stream.SampleAspectRatio, ":")
	if len(parts) != 2 {
		return
	}
	n, errN := strconv.Atoi(parts[0])
	d, errD := strconv.Atoi(parts[1])
	if errN != nil || errD != nil || n <= 0 || d <= 0 || n > 100000 || d > 100000 {
		return
	}
	w, h := int64(stream.Width)*int64(n), int64(stream.Height)*int64(d)
	a, b := w, h
	for b != 0 {
		a, b = b, a%b
	}
	dto["AspectRatio"] = fmt.Sprintf("%d:%d", w/a, h/a)
}
