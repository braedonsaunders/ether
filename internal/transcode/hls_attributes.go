package transcode

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"strings"
)

// HLSVariantAttributes describes the output selected by the media decision.
// Apple HLS requires CODECS; HDR and Dolby Vision also need explicit signaling.
// https://developer.apple.com/streaming/hls-authoring-specification-for-apple-devices.html
func HLSVariantAttributes(info core.MediaInfo, d core.Decision) string {
	var codecs []string
	videoRange, supplemental := "", ""
	if v := info.Video(); v != nil {
		codec, profile, depth, height := v.Codec, v.Profile, v.BitDepth, v.Height
		videoRange = "SDR"
		if !d.VideoCopy {
			codec, profile, depth = d.VideoCodec, "", 8
			if codec == "" {
				codec = "h264"
			}
			// The level follows the encoded size, not the source's.
			_, height = decisionOutputSize(v, d)
			// What planVideo encodes: HDR kept only in HEVC, from the
			// decoded picture (a Dolby Vision stream's base layer).
			if codec == "hevc" && keepsHDR(v) && !d.ToneMap {
				depth = 10
				videoRange = "PQ"
				if encodedRange(v) == rangeHLG {
					videoRange = "HLG"
				}
			}
		} else if !d.ToneMap {
			switch encodedRange(v) {
			case rangeHDR10:
				videoRange = "PQ"
			case rangeHLG:
				videoRange = "HLG"
			}
		}
		c := estimateCodecs(codec, profile, height, depth)
		if !d.VideoCopy && codec == "h264" && d.H264Level > 0 {
			c = fmt.Sprintf("avc1.6400%02x", d.H264Level)
		}
		if d.VideoCopy && v.CodecString != "" {
			c = v.CodecString
		}
		if d.VideoCopy && v.DolbyVision && !d.StripDolbyVision {
			if v.DolbyVisionProfile == 5 && v.DolbyVisionLevel > 0 {
				c = fmt.Sprintf("dvh1.05.%02d", v.DolbyVisionLevel)
				videoRange = "PQ"
			}
			if v.DolbyVisionProfile == 8 && v.DolbyVisionLevel > 0 {
				brand := ""
				switch v.DolbyVisionCompatibilityID {
				case 1:
					brand = "db1p"
					videoRange = "PQ"
				case 4:
					brand = "db4h"
					videoRange = "HLG"
				}
				if brand != "" {
					supplemental = fmt.Sprintf("dvh1.08.%02d/%s", v.DolbyVisionLevel, brand)
				}
			}
		}
		codecs = append(codecs, c)
	}
	if a := findStream(info, d.AudioStream, "audio"); a != nil {
		codec, profile := a.Codec, a.Profile
		if !d.AudioCopy {
			codec, profile = d.AudioCodec, ""
			if codec == "" {
				codec = "aac"
			}
		}
		codecs = append(codecs, estimateCodecs(codec, profile, 0, 0))
	}
	attrs := `,CODECS="` + strings.Join(codecs, ",") + `"`
	if videoRange != "" {
		attrs += `,VIDEO-RANGE=` + videoRange
	}
	if supplemental != "" {
		attrs += `,SUPPLEMENTAL-CODECS="` + supplemental + `"`
	}
	return attrs
}
