package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// controlLegacyExperienceDetailRows assembles the same ordered sources used by post-play,
// filtering each candidate before deduplication and row limits.
func controlLegacyExperienceDetailRows(ctx context.Context, viewer *core.User, key core.RatingKey, limit int, visibility experience.Visibility,
	next func(context.Context, core.RatingKey) (*core.Title, error), postPlay func(context.Context, int) []core.PostPlayPick,
	related func(context.Context, int) ([]core.Hub, error)) ([]core.Hub, error) {
	if limit <= 0 {
		limit = 12
	}
	seen := map[core.RatingKey]bool{key: true}
	var hubs []core.Hub
	if next != nil {
		row := experience.NextUp(ctx, viewer, key, next, visibility)
		if len(row.Items) > 0 && !seen[row.Items[0].Key] {
			seen[row.Items[0].Key] = true
			hubs = append(hubs, core.Hub{ID: "tv.upnext", Title: "Up Next", Type: core.TypeEpisode, Items: row.Items})
		}
	}
	if postPlay != nil {
		row := experience.LoadPostPlay(ctx, viewer, visibility, limit, seen, postPlay)
		if len(row.Items) > 0 {
			prefix := "movie"
			if key.Kind() == core.KindShow || key.Kind() == core.KindSeason || key.Kind() == core.KindEpisode {
				prefix = "tv"
			}
			hubs = append(hubs, core.Hub{ID: prefix + ".postplay", Title: "Recommended For You", Style: "shelf", Items: row.Items})
		}
	}
	if related == nil {
		return hubs, nil
	}
	var candidates []core.Hub
	rows, err := experience.LoadRelated(ctx, viewer, visibility, limit, seen, func(ctx context.Context, fetch int) ([]experience.RelatedCandidate, error) {
		var err error
		candidates, err = related(ctx, fetch)
		if err != nil {
			return nil, err
		}
		out := make([]experience.RelatedCandidate, len(candidates))
		for i, h := range candidates {
			out[i] = experience.RelatedCandidate{Index: i, Items: h.Items}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		hub := candidates[row.Index]
		hub.Items = row.Items
		hubs = append(hubs, hub)
	}
	return hubs, nil
}
