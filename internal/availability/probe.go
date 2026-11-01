package availability

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A title probe searches by id for movies still inside the evidence window.
// Feed history is not required. A release counts only when it would pass the
// owner's quality profile.
func (s *Service) probeDue(ctx context.Context) error {
	if s.d.Search == nil || !s.cfg().Availability.Monitor {
		return nil
	}
	cat, ok := s.d.Catalog.(interface {
		RecentMovies(context.Context, time.Time) ([]core.Title, error)
	})
	if !ok {
		return nil
	}
	cfg := s.cfg().Availability
	since := s.now().Add(-time.Duration(cfg.EffectiveMovieEvidenceDays()) * 24 * time.Hour)
	movies, err := cat.RecentMovies(ctx, since)
	if err != nil || len(movies) == 0 {
		return err
	}
	homes := map[core.RatingKey]time.Time{}
	checked := map[core.RatingKey]bool{}
	if h, ok := s.d.Catalog.(interface {
		HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	}); ok {
		keys := make([]core.RatingKey, len(movies))
		for i := range movies {
			keys[i] = movies[i].Key
		}
		if got, e := h.HomeReleases(ctx, keys); e == nil {
			homes = got
			for k := range got {
				checked[k] = true
			}
		}
	}
	watch := map[core.RatingKey]bool{}
	if s.d.Watchlist != nil {
		if keys, e := s.d.Watchlist(ctx); e == nil {
			watch = keys
		}
	}
	type item struct {
		t     core.Title
		score float64
		next  time.Time
	}
	var due []item
	for _, t := range movies {
		if !t.IsReleased(s.now()) {
			continue
		}
		next, state, err := s.titleCheck(ctx, t.Key)
		if err != nil {
			return err
		}
		digital := time.Time{}
		if checked[t.Key] {
			digital = homes[t.Key]
		}
		dueNow := state == "" || !s.now().Before(next)
		// A digital or physical date that has arrived is due immediately,
		// even when an earlier check is still inside its cadence.
		if !digital.IsZero() && !digital.After(s.now()) && (state == "" || next.Before(digital)) {
			dueNow = true
		}
		if !dueNow {
			continue
		}
		score := t.Popularity
		if watch[t.Key] {
			score += 1000
		}
		if !digital.IsZero() && !digital.After(s.now()) {
			score += 2000
		} else if !digital.IsZero() && digital.Sub(s.now()) < 72*time.Hour {
			score += 800
		}
		due = append(due, item{t, score, next})
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].score > due[j].score })
	for _, it := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.probeOne(ctx, it.t, homes[it.t.Key], checked[it.t.Key]); err != nil {
			if benign(err) {
				return err
			}
			s.log.Warn("title probe failed", "title", it.t.Title, "error", err)
		}
	}
	return nil
}

func (s *Service) probeWhen(t core.Title, digital time.Time, known, found bool) time.Time {
	now := s.now()
	if found {
		return now.Add(24 * time.Hour)
	}
	if known && !digital.IsZero() && !digital.After(now) {
		return now
	}
	if known && !digital.IsZero() && digital.Sub(now) < 72*time.Hour {
		return now.Add(3 * time.Hour)
	}
	if known && !digital.IsZero() {
		return now.Add(24 * time.Hour)
	}
	if now.Sub(t.Released) < 14*24*time.Hour {
		return now.Add(6 * time.Hour)
	}
	return now.Add(12 * time.Hour)
}

func (s *Service) probeOne(ctx context.Context, t core.Title, digital time.Time, known bool) error {
	req := core.SearchRequest{Type: t.Type, IDs: t.IDs, Title: t.Title, OriginalTitle: t.OriginalTitle, Year: t.Year, AlternativeTitles: t.AlternativeTitles}
	bctx, cancel := context.WithTimeout(s.budgetContext(ctx, true), 25*time.Second)
	releases, err := s.d.Search(bctx, req)
	cancel()
	if err != nil && len(releases) == 0 {
		if errors.Is(err, core.ErrBudgetExhausted) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return err
		}
		return s.saveTitleCheck(ctx, t.Key, "none", "Search could not be completed", s.probeWhen(t, digital, known, false))
	}
	releases = s.cachedTorrents(ctx, releases)
	policy := s.policy()
	var feeds []core.ReleaseFeed
	if s.d.Feeds != nil {
		feeds = s.d.Feeds.FeedSources()
	}
	quality := ""
	for _, r := range releases {
		accepted, reason, e := s.d.Validator.FeedCandidate(ctx, &t, r)
		if e != nil {
			return e
		}
		if reason != "" {
			if quality == "" {
				quality = reason
			}
			continue
		}
		f := feedFor(feeds, accepted)
		if f.ID == "" {
			f = core.ReleaseFeed{ID: "probe", Name: accepted.Indexer, Generation: "probe", Supported: true}
		}
		if e = s.remember(ctx, f, t, accepted, policy); e != nil {
			return e
		}
		return s.saveTitleCheck(ctx, t.Key, "found", "", s.probeWhen(t, digital, known, true))
	}
	if quality != "" {
		return s.saveTitleCheck(ctx, t.Key, "quality", quality, s.probeWhen(t, digital, known, false))
	}
	return s.saveTitleCheck(ctx, t.Key, "none", "", s.probeWhen(t, digital, known, false))
}

func (s *Service) cachedTorrents(ctx context.Context, rs []core.Release) []core.Release {
	if s.d.CacheCheck == nil {
		return rs
	}
	var hashes []string
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" {
			hashes = append(hashes, r.InfoHash)
		}
	}
	if len(hashes) == 0 {
		return rs
	}
	bctx, cancel := context.WithTimeout(s.budgetContext(ctx, false), 15*time.Second)
	cached, definitive := s.d.CacheCheck(bctx, hashes)
	cancel()
	if !definitive {
		return rs
	}
	out := rs[:0]
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" && !cached[r.InfoHash] {
			continue
		}
		out = append(out, r)
	}
	return out
}

func feedFor(feeds []core.ReleaseFeed, r core.Release) core.ReleaseFeed {
	for _, f := range feeds {
		if f.Name == r.Indexer || f.ID == r.Indexer {
			return f
		}
	}
	return core.ReleaseFeed{}
}

func (s *Service) titleCheck(ctx context.Context, key core.RatingKey) (time.Time, string, error) {
	var at int64
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT next_check,state FROM availability_title_checks WHERE rating_key=?`, int64(key)).Scan(&at, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	return time.Unix(at, 0), state, nil
}

func (s *Service) saveTitleCheck(ctx context.Context, key core.RatingKey, state, reason string, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO availability_title_checks(rating_key,checked_at,next_check,state,reason) VALUES(?,?,?,?,?)
 ON CONFLICT(rating_key) DO UPDATE SET checked_at=excluded.checked_at,next_check=excluded.next_check,state=excluded.state,reason=excluded.reason`,
		int64(key), s.now().Unix(), next.Unix(), state, reason)
	return err
}

func (s *Service) acceptDigitalReleases(ctx context.Context, keys []core.RatingKey, now time.Time, acceptable map[core.RatingKey]bool) {
	h, ok := s.d.Catalog.(interface {
		HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	})
	if !ok {
		return
	}
	got, err := h.HomeReleases(ctx, keys)
	if err != nil {
		return
	}
	for k, at := range got {
		if !at.IsZero() && !at.After(now) {
			acceptable[k] = true
		}
	}
}
