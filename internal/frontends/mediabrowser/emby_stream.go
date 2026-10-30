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
		// Use only positive probe evidence. PQ alone does not prove HDR10,
		// and missing metadata cannot establish SDR or absence of HDR.
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
			dto["VideoRange"] = "SDR"
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
