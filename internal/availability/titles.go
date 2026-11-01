package availability

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// NewRelease is one movie inside the evidence window, for the owner list.
type NewRelease struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	Year       int    `json:"year,omitempty"`
	Poster     string `json:"poster,omitempty"`
	Theatrical int64  `json:"theatrical,omitempty"`
	Digital    int64  `json:"digital,omitempty"`
	Code       string `json:"code"`
	Source     string `json:"source,omitempty"`
	NextCheck  int64  `json:"nextCheck,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (s *Service) NewReleases(ctx context.Context) ([]NewRelease, error) {
	cat, ok := s.d.Catalog.(interface {
		RecentMovies(context.Context, time.Time) ([]core.Title, error)
	})
	if !ok {
		return []NewRelease{}, nil
	}
	cfg := s.cfg().Availability
	since := s.now().Add(-time.Duration(cfg.EffectiveMovieEvidenceDays()) * 24 * time.Hour)
	movies, err := cat.RecentMovies(ctx, since)
	if err != nil || len(movies) == 0 {
		if err != nil {
			return nil, err
		}
		return []NewRelease{}, nil
	}
	homes := map[core.RatingKey]time.Time{}
	if h, ok := s.d.Catalog.(interface {
		HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	}); ok {
		keys := make([]core.RatingKey, len(movies))
		for i := range movies {
			keys[i] = movies[i].Key
		}
		if got, e := h.HomeReleases(ctx, keys); e == nil {
			homes = got
		}
	}
	visible := map[core.RatingKey]bool{}
	for _, t := range s.FilterDiscovery(ctx, movies) {
		visible[t.Key] = true
	}
	evidence := s.evidenceSources(ctx, movies)
	out := make([]NewRelease, 0, len(movies))
	for _, t := range movies {
		if !evidenceWindow(t, s.now(), cfg) || !t.IsReleased(s.now()) {
			continue
		}
		row := NewRelease{Key: t.Key.String(), Title: t.Title, Year: t.Year, Poster: posterURL(t.PosterPath), Theatrical: t.Released.Unix()}
		if at, known := homes[t.Key]; known && !at.IsZero() {
			row.Digital = at.Unix()
		}
		_, state, _ := s.titleCheck(ctx, t.Key)
		var next int64
		var reason string
		_ = s.db.QueryRowContext(ctx, `SELECT next_check,reason FROM availability_title_checks WHERE rating_key=?`, int64(t.Key)).Scan(&next, &reason)
		row.NextCheck = next
		row.Reason = reason
		src := evidence[t.Key]
		switch {
		case visible[t.Key] && src != "":
			row.Code = "playable"
			row.Source = src
		case visible[t.Key]:
			row.Code = "playable"
			row.Source = "date"
		case state == "quality":
			row.Code = "quality"
		case homeIsTheatrical(homes, t.Key):
			row.Code = "theatrical"
		default:
			row.Code = "searching"
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Service) evidenceSources(ctx context.Context, movies []core.Title) map[core.RatingKey]string {
	out := map[core.RatingKey]string{}
	if len(movies) == 0 {
		return out
	}
	names := map[string]string{}
	if s.d.Feeds != nil {
		for _, f := range s.d.Feeds.FeedSources() {
			names[f.ID] = f.Name
		}
	}
	for start := 0; start < len(movies); start += 400 {
		batch := movies[start:min(start+400, len(movies))]
		marks := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, t := range batch {
			marks[i] = "?"
			args[i] = int64(t.Key)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT rating_key,source_id FROM availability_releases WHERE state NOT IN ('unavailable','policy') AND rating_key IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var key int64
			var source string
			if rows.Scan(&key, &source) == nil {
				name := names[source]
				if name == "" {
					name = source
				}
				out[core.RatingKey(key)] = name
			}
		}
		rows.Close()
	}
	return out
}

func homeIsTheatrical(homes map[core.RatingKey]time.Time, key core.RatingKey) bool {
	at, known := homes[key]
	return known && at.IsZero()
}

func posterURL(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return "/ether/api/image?path=" + url.QueryEscape(path) + "&w=" + strconv.Itoa(185)
}
