package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/calendar"
	"github.com/braedonsaunders/ether/internal/core"
)

// buildCalendar wires the owner's release calendar to watch state,
// watchlists, Look Ahead collections, the catalogue mirror and the owner's
// content policy.
func (a *App) buildCalendar() {
	a.calendar = calendar.New(calendar.Options{
		Catalog: a.catalog,
		Viewers: a.users.Users,
		Followed: func(ctx context.Context, since time.Time) ([]calendar.Activity, error) {
			acts, err := a.state.FollowedShows(ctx, since)
			if err != nil {
				return nil, err
			}
			out := make([]calendar.Activity, len(acts))
			for i, x := range acts {
				out[i] = calendar.Activity{UserID: x.UserID, Show: x.Show, LastViewed: x.LastViewed}
			}
			return out, nil
		},
		Watched:     a.state.GetMany,
		Watchlisted: a.watchlist.Watchlisted,
		LookAheads: func(ctx context.Context) ([]calendar.LookAhead, error) {
			las, err := a.collections.LookAheads(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]calendar.LookAhead, len(las))
			for i, x := range las {
				out[i] = calendar.LookAhead{Name: x.Name, Members: x.Members}
			}
			return out, nil
		},
		Allowed: func(ctx context.Context, u *core.User, t *core.Title) bool {
			return experienceVisibility(a.safety, a.catalog.StoredTitle, nil, nil, serverLibraries(a.cfg.Read())).Playable(ctx, u, t)
		},
		UpcomingMovies: func(ctx context.Context) error {
			_, err := a.catalog.List(ctx, "upcoming", core.TypeMovie, 100)
			return err
		},
		Image:  admin.ImageURL,
		Logger: a.log.With("component", "calendar"),
	})
}
