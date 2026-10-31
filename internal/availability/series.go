package availability

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// A first-day series is a navigation container, not release evidence itself.
// Use the same episode gate as its children, without fetching their metadata.
func (s *Service) filterDiscovery(ctx context.Context, items []core.Title, input discoverySnapshot) []core.Title {
	var shows, releases []core.Title
	var keys []core.RatingKey
	for _, t := range items {
		if t.Type == core.TypeShow && EvidenceWindow(t, input.now) {
			shows = append(shows, t)
			keys = append(keys, t.Key)
		} else {
			releases = append(releases, t)
		}
	}
	allowed := map[core.RatingKey]bool{}
	for _, t := range s.filterReleaseDiscovery(ctx, releases, input) {
		allowed[t.Key] = true
	}
	if len(shows) > 0 {
		children := map[core.RatingKey][]core.Title{}
		var err error
		if c, ok := s.d.Catalog.(interface {
			CachedDiscoveryEpisodes(context.Context, []core.RatingKey) (map[core.RatingKey][]core.Title, error)
		}); ok {
			children, err = c.CachedDiscoveryEpisodes(ctx, keys)
		}
		var episodes []core.Title
		parents := map[core.RatingKey]core.RatingKey{}
		for _, show := range shows {
			if err != nil || show.Adult || !show.IsReleased(input.now) || show.Key.Kind() != core.KindShow {
				continue
			}
			eps := children[show.Key]
			if len(eps) == 0 {
				// Feed-first premiere evidence: this is only an S01E01 matching
				// descriptor. It creates no catalogue item or playable endpoint.
				// Once episodes are mirrored their own dates take precedence.
				eps = []core.Title{{Key: core.EpisodeKey(show.Key.TMDB(), 1, 1), Type: core.TypeEpisode,
					Season: 1, Episode: 1, Released: show.Released, Language: show.Language}}
			}
			for _, ep := range eps {
				if ep.Adult || !ep.IsReleased(input.now) {
					continue
				}
				ep.ShowKey, ep.ShowIDs = show.Key, show.IDs
				ep.ShowTitle, ep.ShowOriginalTitle, ep.ShowYear = show.Title, show.OriginalTitle, show.Year
				if ep.Language == "" {
					ep.Language = show.Language
				}
				episodes = append(episodes, ep)
				parents[ep.Key] = show.Key
			}
		}
		for _, ep := range s.filterReleaseDiscovery(ctx, episodes, input) {
			allowed[parents[ep.Key]] = true
		}
	}
	out := make([]core.Title, 0, len(items))
	for _, t := range items {
		if allowed[t.Key] {
			out = append(out, t)
		}
	}
	return out
}
