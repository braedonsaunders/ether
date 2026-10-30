package silo

import "github.com/braedonsaunders/ether/internal/frontend"

func siloObservedRange(media frontend.MediaInfo, stream frontend.StreamInfo) string {
	if media.Estimated {
		return ""
	}
	return siloDynamicRange(stream)
}

// siloDynamicRange returns an empty string when the probe cannot establish a
// range. Positive enhancement evidence takes precedence over base transfer.
func siloDynamicRange(s frontend.StreamInfo) string {
	positive := func(v *bool) bool { return v != nil && *v }
	if s.DolbyVision || positive(s.DolbyVisionConfigurationPresent) || positive(s.DolbyVisionBLCompatibilityIDPresent) || positive(s.DolbyVisionRPUPresent) || positive(s.DolbyVisionELPresent) || positive(s.DolbyVisionBLPresent) || s.DolbyVisionProfile > 0 || (s.ProbedDolbyVisionProfile != nil && *s.ProbedDolbyVisionProfile > 0) {
		return "dolby_vision"
	}
	if positive(s.HDR10Plus) {
		return "hdr10_plus"
	}
	switch s.ColorTransfer {
	case "smpte2084":
		return "hdr10"
	case "arib-std-b67":
		return "hlg"
	case "bt709", "smpte170m", "bt470bg":
		if s.HDR10Plus != nil && !*s.HDR10Plus && s.DolbyVisionConfigurationPresent != nil && !*s.DolbyVisionConfigurationPresent && s.DolbyVisionRPUPresent != nil && !*s.DolbyVisionRPUPresent {
			return "sdr"
		}
	}
	return ""
}
