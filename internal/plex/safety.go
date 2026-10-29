package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

// AccessPolicy adds owner-defined restrictions to the mandatory title guard.
type AccessPolicy = experience.AccessPolicy

func (s *Server) visibleTitles(u *core.User, ts []core.Title) []core.Title {
	out := make([]core.Title, 0, len(ts))
	for i := range ts {
		if s.titleVisible(u, &ts[i]) {
			out = append(out, ts[i])
		}
	}
	return out
}

func (s *Server) titleAllowed(u *core.User, t *core.Title) bool {
	return experience.TitleAllowed(s.d.AccessPolicy, u, t)
}

func (s *Server) visibility() experience.Visibility {
	v := experience.Visibility{Policy: s.d.AccessPolicy, FilterAvailable: s.d.FilterAvailable,
		SectionFor: func(t core.MediaType) (int, bool) { sec, ok := s.sectionFor(t); return sec.ID, ok }}
	if s.d.Catalog != nil {
		v.GetTitle = s.d.Catalog.Get
	}
	if s.d.Playback != nil {
		v.Sessions = s.d.Playback.Sessions
	}
	return v
}

func (s *Server) visibleTitle(ctx context.Context, u *core.User, t *core.Title) bool {
	return s.visibility().Visible(ctx, u, t)
}
func (s *Server) playableTitle(ctx context.Context, u *core.User, t *core.Title) bool {
	return s.visibility().Playable(ctx, u, t)
}
func (s *Server) availableForViewer(ctx context.Context, u *core.User, ts []core.Title) []core.Title {
	return s.visibility().Available(ctx, u, ts)
}
