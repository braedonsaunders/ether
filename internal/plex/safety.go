package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// AccessPolicy adds owner-defined restrictions to the mandatory title guard.
type AccessPolicy = experience.AccessPolicy

func (s *Server) visibleTitles(u *core.User, ts []core.Title) []core.Title {
	out := s.visibility().TitlesAllowed(u, ts)
	if !s.needPlexRatingFilter(u) || s.d.PlexMeta == nil {
		return out
	}
	keys := make([]core.RatingKey, 0, len(out))
	for i := range out {
		keys = append(keys, out[i].Key)
	}
	rows := s.d.PlexMeta.CachedMany(context.Background(), keys)
	var guids map[core.RatingKey]string
	if s.d.PlexGUID != nil {
		guids = s.d.PlexGUID.CachedMany(context.Background(), keys)
	}
	kept := out[:0]
	for i := range out {
		if s.plexMetaRatingAllowedCached(u, &out[i], rows, guids) {
			kept = append(kept, out[i])
		}
	}
	return kept
}

func (s *Server) titleAllowed(u *core.User, t *core.Title) bool {
	return experience.TitleAllowed(s.d.AccessPolicy, u, t) && s.plexMetaRatingAllowed(u, t)
}

func (s *Server) visibility() experience.Visibility {
	v := experience.Visibility{Policy: s.d.AccessPolicy, FilterAvailable: s.d.FilterAvailable,
		SectionFor: func(t core.MediaType) (int, bool) { sec, ok := s.untitledSectionFor(t); return sec.ID, ok }}
	if s.d.Catalog != nil {
		v.GetTitle = s.artworkTitle
	}
	if s.d.Playback != nil {
		v.Sessions = s.d.Playback.Sessions
	}
	return v
}

func (s *Server) visibleTitle(ctx context.Context, u *core.User, t *core.Title) bool {
	return s.visibility().Visible(ctx, u, t) && s.plexMetaRatingAllowed(u, t)
}
func (s *Server) playableTitle(ctx context.Context, u *core.User, t *core.Title) bool {
	return s.visibility().Playable(ctx, u, t) && s.plexMetaRatingAllowed(u, t)
}
func (s *Server) availableForViewer(ctx context.Context, u *core.User, ts []core.Title) []core.Title {
	return s.visibility().Available(ctx, u, ts)
}

func (s *Server) viewerRestricted(u *core.User) bool {
	return experience.ViewerRestricted(s.d.AccessPolicy, u)
}
