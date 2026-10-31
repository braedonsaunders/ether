package personalize

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"time"
)

func init() {
	store.Register("personalize_discovery", 1, `CREATE TABLE personalize_discovery_hidden(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,hidden_at INTEGER NOT NULL,PRIMARY KEY(user_id,item_key));`)
}

// Discovery adds viewer-owned utility rows without changing Designer layout.
type Discovery struct {
	db      *sql.DB
	catalog core.Catalog
	state   core.State
	allowed func(*core.User, *core.Title) bool
	now     func() time.Time
	// NotInterested applies only to recommendation rows, never Continue.
	NotInterested func(context.Context, *core.User) ([]NotInterestedSignal, error)
}

func NewDiscovery(db *sql.DB, cat core.Catalog, st core.State, allowed func(*core.User, *core.Title) bool) *Discovery {
	return &Discovery{db: db, catalog: cat, state: st, allowed: allowed, now: time.Now}
}
func discoveryIdentity(t *core.Title) core.RatingKey {
	if t.ShowKey != 0 {
		return t.ShowKey
	}
	if t.Type == core.TypeSeason || t.Type == core.TypeEpisode {
		return core.ShowKey(t.Key.TMDB())
	}
	return t.Key
}
func (d *Discovery) visible(u *core.User, t *core.Title) bool {
	if u == nil || t == nil || t.Adult || catalog.RatingExceeds(t.ContentRating, u.MaxContentRating) {
		return false
	}
	section := 1
	if t.Type != core.TypeMovie {
		section = 2
	}
	if len(u.AllowedSections) > 0 {
		ok := false
		for _, id := range u.AllowedSections {
			ok = ok || id == section
		}
		if !ok {
			return false
		}
	}
	return d.allowed == nil || d.allowed(u, t)
}

// RemoveContinue stores a sticky show-level dismissal, so the next episode or
// a repeated timeline cannot immediately put a dismissed show back on Home.
func (d *Discovery) RemoveContinue(ctx context.Context, uid int64, key core.RatingKey) error {
	t, e := d.catalog.Get(ctx, key)
	if e != nil {
		return e
	}
	_, e = d.db.ExecContext(ctx, `INSERT INTO personalize_discovery_hidden(user_id,item_key,hidden_at) VALUES(?,?,?) ON CONFLICT(user_id,item_key) DO UPDATE SET hidden_at=excluded.hidden_at`, uid, discoveryIdentity(t), d.now().Unix())
	return e
}
func (d *Discovery) RestoreContinue(ctx context.Context, uid int64, key core.RatingKey) error {
	t, e := d.catalog.Get(ctx, key)
	if e != nil {
		return e
	}
	_, e = d.db.ExecContext(ctx, `DELETE FROM personalize_discovery_hidden WHERE user_id=? AND item_key=?`, uid, discoveryIdentity(t))
	return e
}
func (d *Discovery) ContinueAllowed(ctx context.Context, uid int64, t *core.Title) bool {
	if t == nil {
		return false
	}
	var n int
	e := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_discovery_hidden WHERE user_id=? AND item_key=?`, uid, discoveryIdentity(t)).Scan(&n)
	return e == nil && n == 0
}

// FilterContinue deduplicates episodes by their parent show's TMDB identity.
func (d *Discovery) FilterContinue(ctx context.Context, u *core.User, titles []core.Title) []core.Title {
	out := []core.Title{}
	if u == nil {
		return out
	}
	seen := map[core.RatingKey]bool{}
	for _, t := range titles {
		k := discoveryIdentity(&t)
		if seen[k] || !d.visible(u, &t) || !d.ContinueAllowed(ctx, u.ID, &t) {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}
func (d *Discovery) YearInReview(ctx context.Context, u *core.User, year, limit int) (core.Hub, error) {
	return d.yearInReview(ctx, u, year, limit, false)
}

// yearInReview applies exclusions only when this history is suggested as a
// discovery row; direct collection access continues to show viewing history.
func (d *Discovery) yearInReview(ctx context.Context, u *core.User, year, limit int, recommendation bool) (core.Hub, error) {
	hub := core.Hub{ID: fmt.Sprintf("discovery.review.%d", year), Title: fmt.Sprintf("Your %d in Review", year), Context: "hub.home.yearinreview", Promoted: true}
	if u == nil || d.state == nil {
		return hub, nil
	}
	if year < 1900 || year > d.now().Year() {
		return hub, fmt.Errorf("invalid review year")
	}
	if limit <= 0 {
		limit = 20
	}
	var excluded excludedTitles
	if recommendation {
		var err error
		excluded, _, err = loadExclusions(ctx, u, d.NotInterested)
		if err != nil {
			return hub, err
		}
	}
	// The year is the server's calendar year (d.now's zone), not UTC's: a
	// film finished on New Year's Eve evening belongs to that year.
	start := time.Date(year, 1, 1, 0, 0, 0, 0, d.now().Location())
	end := start.AddDate(1, 0, 0)
	entries, e := d.state.History(ctx, u.ID, start, 10000)
	if e != nil {
		return hub, e
	}
	seen := map[core.RatingKey]bool{}
	// Hydrate a window at a time. The row stops at limit, so the rest of a
	// long year is not loaded onto the home request.
	for startAt := 0; startAt < len(entries) && len(hub.Items) < limit; startAt += 64 {
		window := entries[startAt:min(startAt+64, len(entries))]
		keys := make([]core.RatingKey, 0, len(window)*2)
		for _, h := range window {
			if h.Completed && !h.ViewedAt.Before(start) && h.ViewedAt.Before(end) && !seen[tasteKey(h.Key)] {
				keys = append(keys, h.Key, tasteKey(h.Key))
			}
		}
		cached := mirroredTitles(ctx, d.catalog, keys)
		for _, h := range window {
			if !h.Completed || h.ViewedAt.Before(start) || !h.ViewedAt.Before(end) {
				continue
			}
			// Episode keys already identify their show. Once that show is in the
			// row, do not hydrate every remaining watched episode just to discard it.
			if seen[tasteKey(h.Key)] {
				continue
			}
			t, e := historyTitle(ctx, d.catalog, cached, h.Key)
			if e != nil || !d.visible(u, t) || excluded.contains(t) {
				continue
			}
			k := discoveryIdentity(t)
			if seen[k] {
				continue
			}
			if k != t.Key {
				t, e = historyTitle(ctx, d.catalog, cached, k)
				if e != nil || !d.visible(u, t) || excluded.contains(t) {
					continue
				}
			}
			seen[k] = true
			hub.Items = append(hub.Items, *t)
			if len(hub.Items) >= limit {
				break
			}
		}
	}
	return hub, nil
}
func (d *Discovery) Rediscover(ctx context.Context, u *core.User, limit int) (core.Hub, error) {
	hub := core.Hub{ID: "discovery.rediscover", Title: "Rediscover", Context: "hub.home.rediscover", Promoted: true}
	if u == nil || d.state == nil {
		return hub, nil
	}
	if limit <= 0 {
		limit = 12
	}
	excluded, _, err := loadExclusions(ctx, u, d.NotInterested)
	if err != nil {
		return hub, err
	}
	entries, e := d.state.InProgress(ctx, u.ID, 200)
	if e != nil {
		return hub, e
	}
	seen := map[core.RatingKey]bool{}
	for _, st := range entries {
		if st.LastViewedAt.IsZero() || d.now().Sub(st.LastViewedAt) < 30*24*time.Hour || st.ViewCount > 0 {
			continue
		}
		if seen[tasteKey(st.Key)] {
			continue
		}
		t, e := d.catalog.Get(ctx, st.Key)
		if e != nil || !d.visible(u, t) || excluded.contains(t) {
			continue
		}
		k := discoveryIdentity(t)
		if seen[k] {
			continue
		}
		if k != t.Key {
			t, e = d.catalog.Get(ctx, k)
			if e != nil || !d.visible(u, t) || excluded.contains(t) {
				continue
			}
		}
		seen[k] = true
		hub.Items = append(hub.Items, *t)
		if len(hub.Items) >= limit {
			break
		}
	}
	return hub, nil
}
func (d *Discovery) Hubs(ctx context.Context, u *core.User, limit int) ([]core.Hub, error) {
	var out []core.Hub
	rediscover, e := d.Rediscover(ctx, u, limit)
	if e != nil {
		return nil, e
	}
	if len(rediscover.Items) > 0 {
		out = append(out, rediscover)
	}
	year := d.now().Year()
	if d.now().Month() < time.December {
		year--
	}
	review, e := d.yearInReview(ctx, u, year, limit, true)
	if e != nil {
		return out, e
	}
	if len(review.Items) > 0 {
		out = append(out, review)
	}
	return out, nil
}
