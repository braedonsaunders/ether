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

type plexMetaBatch struct {
	rows  map[core.RatingKey]plexmeta.Cached
	guids map[core.RatingKey]string
}

func (s *Server) plexMetaOn() bool {
	if s == nil || s.d.PlexMeta == nil || s.d.Config == nil {
		return false
	}
	cfg := s.d.Config()
	return cfg.Plex.Claimed && cfg.Plex.UsePlexMetadata
}

func (s *Server) withPlexMeta(ctx context.Context, keys []core.RatingKey, guids map[core.RatingKey]string) context.Context {
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
		guid := guids[k]
		if guid == "" {
			continue
		}
		row, ok := cached[k]
		if !ok || row.Stale(now) || row.GUID != guid {
			refresh = append(refresh, k)
		}
	}
	s.d.PlexMeta.Schedule(refresh...)
	return context.WithValue(ctx, plexMetaCtxKey{}, plexMetaBatch{rows: cached, guids: guids})
}

func (s *Server) plexMetaCached(ctx context.Context, k core.RatingKey) (plexmeta.Cached, bool) {
	if !s.plexMetaOn() || k == 0 {
		return plexmeta.Cached{}, false
	}
	var (
		row  plexmeta.Cached
		ok   bool
		guid string
	)
	if batch, has := ctx.Value(plexMetaCtxKey{}).(plexMetaBatch); has {
		row, ok = batch.rows[k]
		guid = batch.guids[k]
	} else {
		row, ok = s.d.PlexMeta.Cached(ctx, k)
		if s.d.PlexGUID != nil {
			guid = s.d.PlexGUID.Cached(ctx, k)
		}
	}
	if !ok || row.Empty() || guid == "" || row.GUID != guid {
		return plexmeta.Cached{}, false
	}
	return row, true
}

func (s *Server) plexMetaOverlay(ctx context.Context, k core.RatingKey) (plexmeta.Cached, bool) {
	return s.plexMetaCached(ctx, k)
}

func (s *Server) plexMetaRatingAllowed(u *core.User, t *core.Title) bool {
	if !s.needPlexRatingFilter(u) || t == nil {
		return true
	}
	row, ok := s.plexMetaCached(context.Background(), t.Key)
	return s.plexMetaRatingOK(u, t, row, ok)
}

func (s *Server) needPlexRatingFilter(u *core.User) bool {
	return u != nil && u.MaxContentRating != "" && s.plexMetaOn()
}

func (s *Server) plexMetaRatingOK(u *core.User, t *core.Title, row plexmeta.Cached, ok bool) bool {
	if !ok || row.ContentRating == "" {
		return true
	}
	return !contentrating.Exceeds(plexmeta.SaferRating(t.ContentRating, row.ContentRating), u.MaxContentRating)
}

func (s *Server) plexMetaRatingAllowedCached(u *core.User, t *core.Title, rows map[core.RatingKey]plexmeta.Cached, guids map[core.RatingKey]string) bool {
	if !s.needPlexRatingFilter(u) || t == nil {
		return true
	}
	guid := guids[t.Key]
	row, ok := rows[t.Key]
	if !ok || row.Empty() || guid == "" || row.GUID != guid {
		return true
	}
	return s.plexMetaRatingOK(u, t, row, true)
}

func (s *Server) plexMetaRemoteImage(ctx context.Context, key core.RatingKey, element string) string {
	o, ok := s.plexMetaCached(ctx, key)
	if !ok {
		return ""
	}
	switch element {
	case "thumb", "poster":
		return o.ThumbURL
	case "art", "background":
		return o.ArtURL
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

func overlayPeople(o plexmeta.Cached, kind string) []plexmeta.Person {
	switch kind {
	case "actor":
		return o.Cast
	case "director":
		return o.Directors
	case "writer":
		return o.Writers
	}
	return nil
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
