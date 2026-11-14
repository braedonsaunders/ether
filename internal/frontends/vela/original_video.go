package vela

import "github.com/braedonsaunders/ether/internal/frontend"

// originalVideo exposes observed source facts without leaking the probe DTO.
// Missing colour and Dolby Vision evidence stays missing, including layer flags.
func originalVideo(media frontend.MediaInfo) object {
	if media.Estimated {
		return nil
	}
	for _, s := range media.Streams {
		if s.Type != "video" {
			continue
		}
		out := object{}
		for key, value := range map[string]string{"codec": s.Codec, "profile": s.Profile, "colorPrimaries": s.ColorPrimaries, "colorTransfer": s.ColorTransfer, "colorSpace": s.ColorSpace, "colorRange": s.ColorRange} {
			if value != "" && value != "unknown" {
				out[key] = value
			}
		}
		for key, value := range map[string]int{"width": s.Width, "height": s.Height, "bitDepth": s.BitDepth, "level": s.Level, "dvProfile": s.DolbyVisionProfile, "dvLevel": s.DolbyVisionLevel} {
			if value > 0 {
				out[key] = value
			}
		}
		if s.FrameRate > 0 {
			out["frameRate"] = s.FrameRate
		}
		if s.ProbedDolbyVisionProfile != nil {
			out["dvProfile"] = *s.ProbedDolbyVisionProfile
		}
		if s.ProbedDolbyVisionLevel != nil {
			out["dvLevel"] = *s.ProbedDolbyVisionLevel
		}
		if (s.DolbyVisionBLCompatibilityIDPresent != nil && *s.DolbyVisionBLCompatibilityIDPresent) || s.DolbyVisionCompatibilityID > 0 {
			out["dvBlSignalCompatibilityId"] = s.DolbyVisionCompatibilityID
		}
		for key, value := range map[string]*bool{"rpuPresent": s.DolbyVisionRPUPresent, "elPresent": s.DolbyVisionELPresent, "blPresent": s.DolbyVisionBLPresent} {
			if value != nil {
				out[key] = *value
			}
		}
		return out
	}
	return nil
}
