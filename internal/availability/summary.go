package availability

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Summary is the quiet status line for "When they can play": how many
// in-window movies are ready, how many are still waiting, and when Ether
// last looked.
type Summary struct {
	Ready       int   `json:"ready"`
	Waiting     int   `json:"waiting"`
	LastChecked int64 `json:"lastChecked,omitempty"`
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	cat, ok := s.d.Catalog.(interface {
		RecentMovies(context.Context, time.Time) ([]core.Title, error)
	})
	if !ok {
		return Summary{}, nil
	}
	cfg := s.cfg().Availability
	since := s.now().Add(-time.Duration(cfg.EffectiveMovieEvidenceDays()) * 24 * time.Hour)
	movies, err := cat.RecentMovies(ctx, since)
	if err != nil {
		return Summary{}, err
	}
	visible := map[core.RatingKey]bool{}
	for _, t := range s.FilterDiscovery(ctx, movies) {
		visible[t.Key] = true
	}
	var out Summary
	for _, t := range movies {
		if !evidenceWindow(t, s.now(), cfg) || !t.IsReleased(s.now()) {
			continue
		}
		if visible[t.Key] {
			out.Ready++
		} else {
			out.Waiting++
		}
	}
	_ = s.db.QueryRowContext(ctx, `SELECT MAX(checked_at) FROM availability_title_checks`).Scan(&out.LastChecked)
	return out, nil
}
