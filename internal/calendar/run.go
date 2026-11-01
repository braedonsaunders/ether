package calendar

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Run keeps followed shows' current seasons mirrored, and TMDB's upcoming
// movies, until ctx ends. Run it on one node (the cluster leader).
func (s *Service) Run(ctx context.Context) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	var upcomingAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if s.o.UpcomingMovies != nil && s.o.Now().Sub(upcomingAt) >= upcomingEvery {
			if err := s.o.UpcomingMovies(ctx); err != nil {
				s.o.Logger.Debug("calendar: upcoming movies", "err", err)
			} else {
				upcomingAt = s.o.Now()
			}
		}
		s.Refresh(ctx)
		timer.Reset(refreshEvery)
	}
}

// Refresh mirrors the seasons of up to refreshBudget followed shows, most
// recently active first, then drops cached calendars.
func (s *Service) Refresh(ctx context.Context) {
	shows, err := s.followedShows(ctx)
	if err != nil {
		s.o.Logger.Warn("calendar: followed shows", "err", err)
		return
	}
	if len(shows) > refreshBudget {
		shows = shows[:refreshBudget]
	}
	for _, k := range shows {
		if ctx.Err() != nil {
			return
		}
		if err := s.o.Catalog.RefreshSchedule(ctx, k); err != nil {
			s.o.Logger.Debug("calendar: refresh show", "show", k, "err", err)
		}
	}
	s.mu.Lock()
	s.lastPass = s.o.Now()
	s.cache = map[string]cached{}
	s.mu.Unlock()
}

// followedShows lists every viewer's followed shows, watched ones by most
// recent activity, then watchlisted ones.
func (s *Service) followedShows(ctx context.Context) ([]core.RatingKey, error) {
	acts, err := s.o.Followed(ctx, s.o.Now().Add(-followWindow))
	if err != nil {
		return nil, err
	}
	seen := map[core.RatingKey]bool{}
	var out []core.RatingKey
	for _, a := range acts {
		if !seen[a.Show] {
			seen[a.Show] = true
			out = append(out, a.Show)
		}
	}
	if s.o.Watchlisted != nil {
		lists, err := s.o.Watchlisted(ctx)
		if err != nil {
			return nil, err
		}
		for _, keys := range lists {
			for _, k := range keys {
				if k.Kind() == core.KindShow && !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
		}
	}
	return out, nil
}
