package resolver

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// RememberedReleaseAllowed rechecks identity at use time, without searching or
// probing. A previous successful open is not evidence that a namesake belongs
// to this title. Pins remain the owner's explicit identity decision.
func (s *Service) RememberedReleaseAllowed(ctx context.Context, t *core.Title, rel core.Release) bool {
	if t == nil {
		return false
	}
	pin, pinned := s.PinFor(t.Key)
	if pinned && pin.Release.ID == rel.ID {
		return true
	}
	if s.db != nil {
		s.loadRejectionsFor(ctx, t.Key)
	}
	if s.rejectedFor(t.Key, rel) {
		return false
	}
	req := s.searchRequest(ctx, t)
	if req.Type == core.TypeEpisode {
		if rel.Target != nil && (rel.Target.Season != req.Season || rel.Target.Episode != req.Episode) {
			return false
		}
		// Cached torrent descriptors must carry today's file-selection target.
		if rel.Protocol == core.ProtocolTorrent && t.ShowYear > 0 && (rel.Target == nil || rel.Target.ShowYear != t.ShowYear || rel.Target.EpisodeTitle != t.Title) {
			return false
		}
	}
	if rel.Info.Title == "" {
		rel.Info = releaseparse.Parse(rel.Name)
	}
	identity := s.localEpisodeIdentity(ctx, t, req, []core.Release{rel})
	if identity.reject(rel, t, req) != "" {
		return false
	}
	s.prepareSeriesYears(ctx, t, req, []core.Release{rel})
	if rejectIdentityYears(rel, t, req, s.catalogLanguage(), func(y int) bool { return s.seriesYearAccepted(t.ShowKey, y) }) != "" {
		return false
	}
	if req.Type == core.TypeEpisode && (identity.shared(req) || s.hasSeriesNamesake(ctx, t, req)) {
		// Old indexer IDs may have been copied from the query. Contradictory
		// regional evidence needs a year or episode subtitle before reuse.
		if episodeNamesakeConflict(rel, t, req) {
			return false
		}
		return episodeIdentityEvidence(rel, req)
	}
	return true
}

func episodeNamesakeConflict(rel core.Release, t *core.Title, req core.SearchRequest) bool {
	return episodeRegionConflict(rel, t) && releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year) == 0 && !releaseparse.EpisodeTitleEvidence(rel.Name, req.EpisodeTitle)
}
