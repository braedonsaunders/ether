package core

import "slices"

// CloneMediaInfo detaches every mutable fact while preserving nil and empty
// values. The caller must own the input or hold its writer's lock during this
// copy; cloning does not synchronize concurrent mutation of the input itself.
func CloneMediaInfo(info MediaInfo) MediaInfo {
	info.Streams = slices.Clone(info.Streams)
	info.Chapters = slices.Clone(info.Chapters)
	info.Keyframes = slices.Clone(info.Keyframes)
	for i := range info.Streams {
		s := &info.Streams[i]
		for _, field := range []**bool{&s.DefaultDisposition, &s.ForcedDisposition, &s.HearingImpaired, &s.Original, &s.VisualImpaired, &s.Commentary, &s.Dub, &s.HDR10Plus, &s.DolbyVisionBLPresent, &s.DolbyVisionELPresent, &s.DolbyVisionRPUPresent, &s.ProbedInterlaced, &s.DolbyVisionConfigurationPresent, &s.DolbyVisionBLCompatibilityIDPresent} {
			if *field != nil {
				value := **field
				*field = &value
			}
		}
		for _, field := range []**int{&s.NALLengthSize, &s.ReferenceFrames, &s.BitsPerRawSample, &s.ProbedLevel, &s.ProbedDolbyVisionProfile, &s.ProbedDolbyVisionLevel} {
			if *field != nil {
				value := **field
				*field = &value
			}
		}
		for _, field := range []**float64{&s.AverageFrameRate, &s.ReferenceFrameRate} {
			if *field != nil {
				value := **field
				*field = &value
			}
		}
	}
	return info
}
