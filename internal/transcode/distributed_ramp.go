package transcode

import (
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// ValidDistributedRamp bounds the versioned, deterministic encoded-HLS plan.
// Copy, live, offset, subtitles and native recovery retain their existing paths.
func ValidDistributedRamp(q core.HLSRequest) bool {
	r := q.Ramp
	if r.Version == 0 {
		return r == (core.HLSRamp{})
	}
	if r.Version != 1 {
		return false
	}
	if r.Segments == 0 {
		return r.Duration == 0
	}
	steady := q.SegmentDuration
	if steady == 0 {
		steady = DefaultSegmentDuration
	}
	return r.Segments > 0 && r.Segments <= 16 && r.Duration >= 100*time.Millisecond && r.Duration < steady && steady <= time.Minute && q.Info.Duration > 0 && !q.Info.Estimated && q.Info.Video() != nil && q.Decision.Kind == core.DecisionTranscode && !q.Decision.VideoCopy && (q.Decision.VideoCodec == "" || q.Decision.VideoCodec == "h264") && (q.Decision.Container == ContainerMPEGTS || q.Decision.Container == ContainerFMP4) && (q.Protocol == "" || q.Protocol == core.ProtocolHLS) && !q.LiveInput && q.StartOffset == 0 && q.SoftSubtitleStream == nil && len(q.TextSubtitleStreams) == 0 && q.RecoveryStart == nil && !enhancementSelected(q.Decision)
}

// AuthorDistributedRamp snapshots shared configuration once, before durable
// admission. Unsupported requests remain on their legacy steady private path.
// Reencoded audio stays steady until combined ramp/audio recovery is qualified.
func AuthorDistributedRamp(q core.HLSRequest, c config.Transcode) core.HLSRequest {
	if q.Ramp != (core.HLSRamp{}) || q.Resume != nil || !c.FastStart || !q.Decision.AudioCopy {
		return q
	}
	original := q
	a := adaptiveFromConfig(c)
	q.Ramp = core.HLSRamp{Version: 1, Segments: a.RampSegs, Duration: a.RampDur}
	if q.SegmentDuration == 0 {
		q.SegmentDuration = DefaultSegmentDuration
	}
	if q.Ramp.Duration >= q.SegmentDuration {
		q.Ramp.Duration = q.SegmentDuration / 2
	}
	if !ValidDistributedRamp(q) {
		return original
	}
	return q
}
