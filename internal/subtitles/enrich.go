package subtitles

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Enrich is playback.Deps.AugmentMedia. It attaches sidecars already on
// disk for this release. It does not search or download: a file is fetched
// only when the viewer picks one. The source is exclusively owned by the
// preparing generation, so local hashing cannot race disposal during failover.
func (s *Service) Enrich(ctx context.Context, title *core.Title, user *core.User, res *core.Resolution, src core.MediaSource, info *core.MediaInfo, prefs ...Preferences) {
	if !s.Enabled() || info == nil || res == nil {
		return
	}
	release := res.Release.Name
	hash := ""
	if src != nil && strings.HasPrefix(res.Release.ID, "local:") {
		var err error
		hash, err = MovieHash(src, src.Size())
		if err != nil {
			s.log.Debug("local subtitle hash unavailable", "error", err)
		}
	}
	sidecars, err := s.SidecarsWithHash(ctx, title, user, info.Streams, release, hash, prefs...)
	if err != nil {
		s.log.Debug("subtitle enrichment unavailable", "error", err)
	}
	info.Streams = append(info.Streams, sidecars...)
}
