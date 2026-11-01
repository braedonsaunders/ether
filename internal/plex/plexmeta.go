package plex

import (
	"context"
	"net/url"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/plexmeta"
)

type plexMetaCtxKey struct{}

func (s *Server) plexMetaOn() bool {
	if s == nil || s.d.PlexMeta == nil || s.d.Config == nil {
		return false
	}
	cfg := s.d.Config()
	return cfg.Plex.Claimed && cfg.Plex.UsePlexMetadata
}

func (s *Server) withPlexMeta(ctx context.Context, keys []core.RatingKey) context.Context {
	if !s.plexMetaOn() {
		return ctx
	}
	cached := s.d.PlexMeta.CachedMany(ctx, keys)
	now := s.now()
	refresh := make([]core.RatingKey, 0, len(keys))
	seen := map[core.RatingKey]bool{}
	for _, k := range keys {
		if k == 0 || seen[k] {
			continue
		}
		seen[k] = true
		if row, ok := cached[k]; !ok || row.Stale(now) {
			refresh = append(refresh, k)
		}
	}
	s.d.PlexMeta.Schedule(refresh...)
	return context.WithValue(ctx, plexMetaCtxKey{}, cached)
}

func (s *Server) plexMetaOverlay(ctx context.Context, k core.RatingKey) (plexmeta.Overlay, bool) {
	if !s.plexMetaOn() || k == 0 {
		return plexmeta.Overlay{}, false
	}
	if cached, ok := ctx.Value(plexMetaCtxKey{}).(map[core.RatingKey]plexmeta.Cached); ok {
		row, ok := cached[k]
		return row.Overlay, ok && !row.Empty()
	}
	row, ok := s.d.PlexMeta.Cached(ctx, k)
	return row.Overlay, ok && !row.Empty()
}

func (s *Server) plexMetaRatingAllowed(u *core.User, t *core.Title) bool {
	if u == nil || t == nil || u.MaxContentRating == "" || !s.plexMetaOn() {
		return true
	}
	o, ok := s.plexMetaOverlay(context.Background(), t.Key)
	if !ok || o.ContentRating == "" {
		return true
	}
	return !contentrating.Exceeds(plexmeta.SaferRating(t.ContentRating, o.ContentRating), u.MaxContentRating)
}

func (s *Server) plexMetaRemoteImage(ctx context.Context, key core.RatingKey, element string) string {
	o, ok := s.plexMetaOverlay(ctx, key)
	if !ok {
		return ""
	}
	switch element {
	case "thumb", "poster":
		return o.ThumbURL
	case "art", "background":
		if o.ArtURL != "" {
			return o.ArtURL
		}
		return o.ThumbURL
	}
	return ""
}

func plexImageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return false
	}
	return plexmeta.ImageHost(u.Host)
}

func applyPlexMetaTitle(t *core.Title, o plexmeta.Overlay) *core.Title {
	cp := *t
	plexmeta.ApplyDisplay(&cp, o)
	return &cp
}

func applyPlexMetaDates(t *core.Title, m *model.Metadata, o plexmeta.Overlay) {
	if o.AvailableAt.IsZero() && o.Year == 0 {
		return
	}
	if t.Type == core.TypeSeason {
		return
	}
	if !o.AvailableAt.IsZero() {
		m.OriginallyAvailableAt = o.AvailableAt.Format("2006-01-02")
	}
	if t.Type == core.TypeEpisode {
		return
	}
	if o.Year > 0 {
		m.Year = o.Year
	} else if !o.AvailableAt.IsZero() {
		m.Year = o.AvailableAt.Year()
	}
}
