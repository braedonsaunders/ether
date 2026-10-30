package playback

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// CurrentSource snapshots one generation without holding a lock across I/O.
func (m *Manager) CurrentSource(ctx context.Context, id string) (core.PlaybackSourceObservation, error) {
	if err := ctx.Err(); err != nil {
		return core.PlaybackSourceObservation{}, err
	}
	s := m.lookup(id)
	if s == nil {
		return core.PlaybackSourceObservation{}, core.ErrNotFound
	}
	s.mu.Lock()
	if s.closed || s.cur == nil || s.cur.src == nil {
		s.mu.Unlock()
		return core.PlaybackSourceObservation{}, core.ErrNotFound
	}
	g := s.cur
	info := g.infoSnapshot()
	source := g.src
	releaseID := ""
	if g.res != nil {
		releaseID = g.res.Release.ID
	}
	key := s.ident.key
	s.mu.Unlock()
	observation := core.PlaybackSourceObservation{}
	if revision := historySourceRevision(key, source); revision != "" {
		observation.Revision = revision
		observation.ReleaseID = releaseID
		if !info.Estimated {
			if video := info.Video(); video != nil {
				observation.VideoCodec, observation.Height = video.Codec, video.Height
				observation.HDR = observedHDR(*video)
			}
		}
	}
	return observation, nil
}

func historySourceRevision(key core.RatingKey, source core.MediaSource) string {
	// Local SourceRevision stats the filesystem. Do not do that under the
	// broker owner gate: a stalled mount could otherwise hold up Stop. Local
	// history stays unknown until a separately reviewed source-lease seam exists.
	if _, local := source.(core.LocalFileSource); local {
		return ""
	}
	return core.SourceRevision(&core.PlaySession{Key: key, Source: source})
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
