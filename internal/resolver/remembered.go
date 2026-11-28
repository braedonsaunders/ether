package resolver

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// RememberedReleaseAllowed rechecks identity at use time, without searching or
// probing. A previous successful open is not evidence that a namesake belongs
// to this title. Strict pins remain the owner's explicit identity decision; a
// preferred pin is rechecked like any remembered source, since normal ranking
// can replace it.
func (s *Service) RememberedReleaseAllowed(ctx context.Context, t *core.Title, rel core.Release) bool {
	if t == nil {
		return false
	}
	pin, pinned := s.PinFor(t.Key)
	if pinned && !pin.Fallback && pin.Release.ID == rel.ID {
		return true
	}
	if s.rejectedFor(t.Key, rel) {
		return false
	}
	req := s.numberedRequest(ctx, s.searchRequest(ctx, t), true)
	if rel.Info.Title == "" {
		rel.Info = releaseparse.Parse(rel.Name)
	}
	if req.Type == core.TypeEpisode {
		season, episode := releaseNumbers(rel.Info, req)
		if rel.Target != nil && (rel.Target.Season != season || rel.Target.Episode != episode) {
			return false
		}
		// Cached torrent descriptors must carry today's file-selection target.
		if rel.Protocol == core.ProtocolTorrent && t.ShowYear > 0 && (rel.Target == nil || rel.Target.ShowYear != t.ShowYear || rel.Target.EpisodeTitle != t.Title) {
			return false
		}
	}
	identity := s.localEpisodeIdentity(ctx, t, req, []core.Release{rel})
	if identity.reject(rel, t, req) != "" {
		return false
	}
	s.prepareSeriesYears(ctx, t, req, []core.Release{rel})
	s.prepareTitleIDVerifications(ctx, t, req, s.catalogLanguage(), []core.Release{rel})
	rel = s.applyVerifiedTitleIDs(rel, t, req)
	if rejectIdentityMetadata(rel, t, req, s.catalogLanguage(), func(y int) bool { return s.seriesYearAccepted(t.ShowKey, y) }, s.metadataTitleMatches(rel, req)) != "" {
		return false
	}
	if req.Type == core.TypeEpisode && (identity.shared(req) || s.hasSeriesNamesake(ctx, t, req)) {
		evidence := identity.evidence(rel, t, req)
		// Old indexer IDs may have been copied from the query. Contradictory
		// regional evidence needs positive identity evidence before reuse.
		if evidence == "" && episodeNamesakeConflict(rel, t, req) {
			return false
		}
		return evidence != ""
	}
	return true
}

func episodeNamesakeConflict(rel core.Release, t *core.Title, req core.SearchRequest) bool {
	return episodeRegionConflict(rel, t) && releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year) == 0 && !releaseparse.EpisodeTitleEvidence(rel.Name, req.EpisodeTitle) && !releaseparse.EpisodeMatchesAirDate(rel.Info, req.Released)
}
