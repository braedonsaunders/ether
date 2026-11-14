package plex

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func (s *Server) liveGuideFilters(ctx context.Context, u *frontend.User) []model.GridChannelFilter {
	out := []model.GridChannelFilter{}
	if g, ok := s.svc().liveTV.(frontend.LiveTVGuides); ok {
		guides, err := g.Guides(ctx, u)
		if err != nil {
			s.log.Warn("live guides unavailable", "error", err)
			return out
		}
		for _, g := range guides {
			out = append(out, model.GridChannelFilter{Key: liveProviderPath + "/guides/" + url.PathEscape(g.ID) + "/channels", GenreRatingKey: "ether.guide." + g.ID, Title: g.Name})
		}
	}
	for _, c := range []struct{ id, title string }{{"movie", "Movies"}, {"sports", "Sports"}, {"news", "News"}, {"kids", "Kids"}, {"series", "TV Shows"}} {
		out = append(out, model.GridChannelFilter{Key: liveProviderPath + "/categories/" + c.id + "/channels", GenreRatingKey: "ether.category." + c.id, Title: c.title})
	}
	return out
}
func (s *Server) liveCategoryChannels(r *http.Request, cs []frontend.LiveTVChannel, category string) ([]frontend.LiveTVChannel, error) {
	switch category {
	case "movie", "series", "sports", "news", "kids":
	default:
		return nil, frontend.ErrNotFound
	}
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	// A bounded window, no catalogue lookup and no provider/network refresh.
	if len(ids) == 0 {
		return cs, nil
	}
	now := time.Now()
	ps, err := s.svc().liveTV.GuideForChannels(r.Context(), FromContext(r.Context()).User, now, now.Add(2*time.Hour), ids)
	if err != nil {
		return nil, err
	}
	matches := map[string]bool{}
	for _, p := range ps {
		if frontend.LiveCategory(p.Item.Genres, category, p.Item.MediaType) {
			matches[p.ChannelID] = true
		}
	}
	out := []frontend.LiveTVChannel{}
	for _, c := range cs {
		if matches[c.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}
