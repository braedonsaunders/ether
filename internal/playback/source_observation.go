package playback

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// CurrentSource copies the current generation's historical open observation.
// It performs no filesystem or source callback I/O. Fresh pin/serve validation
// remains separate; this is not proof that a local path is still unchanged.
func (m *Manager) CurrentSource(ctx context.Context, id string) (core.PlaybackSourceObservation, error) {
	if err := ctx.Err(); err != nil {
		return core.PlaybackSourceObservation{}, err
	}
	s := m.lookup(id)
	if s == nil {
		return core.PlaybackSourceObservation{}, core.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.cur == nil {
		return core.PlaybackSourceObservation{}, core.ErrNotFound
	}
	s.cur.mu.Lock()
	out := s.cur.observation
	s.cur.mu.Unlock()
	if out.HDR != nil {
		value := *out.HDR
		out.HDR = &value
	}
	return out, nil
}

// observedHDR keeps contradictory or incomplete probe evidence distinct from
// proven absence. A container configuration or dynamic metadata is positive
// evidence even when the base-layer transfer reports bt709.
func observedHDR(video core.StreamInfo) *bool {
	positive := video.DolbyVision || video.ColorTransfer == "smpte2084" || video.ColorTransfer == "arib-std-b67" ||
		(video.HDR10Plus != nil && *video.HDR10Plus) ||
		(video.DolbyVisionConfigurationPresent != nil && *video.DolbyVisionConfigurationPresent) ||
		(video.DolbyVisionBLCompatibilityIDPresent != nil && *video.DolbyVisionBLCompatibilityIDPresent) ||
		(video.DolbyVisionRPUPresent != nil && *video.DolbyVisionRPUPresent) ||
		(video.DolbyVisionELPresent != nil && *video.DolbyVisionELPresent) ||
		(video.DolbyVisionBLPresent != nil && *video.DolbyVisionBLPresent) ||
		(video.ProbedDolbyVisionProfile != nil && *video.ProbedDolbyVisionProfile > 0) || video.DolbyVisionProfile > 0
	if positive {
		value := true
		return &value
	}
	if video.ColorTransfer == "bt709" && video.HDR10Plus != nil && !*video.HDR10Plus && video.DolbyVisionConfigurationPresent != nil && !*video.DolbyVisionConfigurationPresent && video.DolbyVisionRPUPresent != nil && !*video.DolbyVisionRPUPresent {
		value := false
		return &value
	}
	return nil
}
