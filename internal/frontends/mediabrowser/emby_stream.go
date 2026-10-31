package mediabrowser

import (
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) embyMediaStreamDTO(dto map[string]any, stream frontend.StreamInfo) {
	streamMetadataDTO(dto, stream)
	delete(dto, "IsOriginal") // This field is not part of Emby's stream DTO.
	dto["AttachmentSize"] = 0
	dto["Protocol"] = "File"
	if stream.Type == "video" {
		// Use only positive probe evidence. PQ alone does not prove HDR10.
		// An empty transfer is SDR only for 8-bit 4:2:0, which is what Emby
		// 4.10.2 reports for that picture.
		switch {
		case stream.DolbyVisionConfigurationPresent != nil && *stream.DolbyVisionConfigurationPresent:
			dto["ExtendedVideoType"] = "DolbyVision"
		case stream.HDR10Plus != nil && *stream.HDR10Plus:
			dto["ExtendedVideoType"] = "Hdr10Plus"
		case stream.ColorTransfer == "arib-std-b67":
			dto["ExtendedVideoType"] = "HyperLogGamma"
		}
		// A completed probe that found neither Dolby Vision nor HDR10+ has no
		// extended type. PQ remains unclassified: it can still be HDR10.
		if _, set := dto["ExtendedVideoType"]; !set && stream.DolbyVisionConfigurationPresent != nil && !*stream.DolbyVisionConfigurationPresent && (stream.HDR10Plus == nil || !*stream.HDR10Plus) && stream.ColorTransfer != "smpte2084" {
			dto["ExtendedVideoType"] = "None"
			dto["ExtendedVideoSubType"] = "None"
			dto["ExtendedVideoSubTypeDescription"] = "None"
			if embySDR(stream) {
				dto["VideoRange"] = "SDR"
			}
		}
	} else if _, set := dto["ExtendedVideoType"]; !set {
		// Audio and subtitle streams have no extended video type. Emby still
		// sends None rather than omitting the fields.
		dto["ExtendedVideoType"] = "None"
		dto["ExtendedVideoSubType"] = "None"
		dto["ExtendedVideoSubTypeDescription"] = "None"
	}
	if stream.CodecTag != "" {
		dto["CodecTag"] = stream.CodecTag
	}
	if stream.TimeBase != "" {
		dto["TimeBase"] = stream.TimeBase
	}
	if stream.PixelFormat != "" {
		dto["PixelFormat"] = stream.PixelFormat
	}
	for key, value := range map[string]string{"ColorTransfer": stream.ColorTransfer, "ColorPrimaries": stream.ColorPrimaries, "ColorSpace": stream.ColorSpace} {
		if value != "" {
			dto[key] = value
		}
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
	} else if stream.Type == "video" && !stream.Interlaced {
		delete(dto, "IsInterlaced")
	}
	parts := strings.Split(stream.SampleAspectRatio, ":")
	var n, d int
	if stream.Type == "video" && len(parts) == 2 {
		var errN, errD error
		n, errN = strconv.Atoi(parts[0])
		d, errD = strconv.Atoi(parts[1])
		if errN == nil && errD == nil && n > 0 && d > 0 && n <= 100000 && d <= 100000 {
			dto["IsAnamorphic"] = n != d
		}
	}
}

// embySDR matches Emby 4.10.2. An explicit SDR transfer curve is enough.
// With no transfer at all, that server still reports SDR for 8-bit 4:2:0
// once Dolby Vision is known absent. Other pixel formats and depths stay
// unlabelled: an unrecognised curve is not rewritten as SDR.
func embySDR(stream frontend.StreamInfo) bool {
	switch stream.ColorTransfer {
	case "bt709", "bt470m", "bt470bg", "smpte170m", "smpte240m", "iec61966-2-1", "bt2020-10", "bt2020-12":
		return true
	case "":
	default:
		return false
	}
	depth := stream.BitDepth
	if stream.BitsPerRawSample != nil && *stream.BitsPerRawSample > 0 {
		depth = *stream.BitsPerRawSample
	}
	if depth != 8 {
		return false
	}
	switch stream.PixelFormat {
	case "yuv420p", "yuvj420p":
		return true
	default:
		return false
	}
}
