package sessions

import (
	"slices"

	"github.com/braedonsaunders/ether/internal/core"
)

// cloneClientProfiles preserves nil versus empty declarations while detaching
// every mutable member from the caller's capability declaration.
func cloneClientProfiles(in *core.ClientCapabilityProfiles) *core.ClientCapabilityProfiles {
	if in == nil {
		return nil
	}
	out := *in
	out.DirectPlay = slices.Clone(in.DirectPlay)
	for i := range out.DirectPlay {
		p := &out.DirectPlay[i]
		p.Containers = slices.Clone(p.Containers)
		p.VideoCodecs = slices.Clone(p.VideoCodecs)
		p.AudioCodecs = slices.Clone(p.AudioCodecs)
	}
	out.Transcoding = slices.Clone(in.Transcoding)
	for i := range out.Transcoding {
		p := &out.Transcoding[i]
		p.Containers = slices.Clone(p.Containers)
		p.VideoCodecs = slices.Clone(p.VideoCodecs)
		p.AudioCodecs = slices.Clone(p.AudioCodecs)
		p.Conditions = slices.Clone(p.Conditions)
		if p.BreakOnNonKeyFrames != nil {
			v := *p.BreakOnNonKeyFrames
			p.BreakOnNonKeyFrames = &v
		}
		if p.AudioVBR != nil {
			v := *p.AudioVBR
			p.AudioVBR = &v
		}
	}
	out.Subtitles = slices.Clone(in.Subtitles)
	out.Containers = slices.Clone(in.Containers)
	for i := range out.Containers {
		p := &out.Containers[i]
		p.Containers = slices.Clone(p.Containers)
		p.SubContainers = slices.Clone(p.SubContainers)
		p.Conditions = slices.Clone(p.Conditions)
	}
	out.Codecs = slices.Clone(in.Codecs)
	for i := range out.Codecs {
		p := &out.Codecs[i]
		p.Codecs = slices.Clone(p.Codecs)
		p.Containers = slices.Clone(p.Containers)
		p.SubContainers = slices.Clone(p.SubContainers)
		p.Conditions = slices.Clone(p.Conditions)
		p.ApplyConditions = slices.Clone(p.ApplyConditions)
	}
	return &out
}
