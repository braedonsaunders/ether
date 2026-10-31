package experience

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// StoredEpisodeDeps let a recursive episode listing read the catalogue's
// stored episodes in key order instead of walking every series. All are
// required for that path; without them the listing walks the library.
type StoredEpisodeDeps struct {
	// Page returns stored, aired, non-adult episodes after a key, in key
	// order, of the series a browse filter admits.
	Page func(context.Context, core.Filter, core.RatingKey, int) ([]core.Title, error)
	// Count counts Page's whole set.
	Count func(context.Context, core.Filter) (int, error)
	// Unranked reports that no stored episode has a popularity.
	Unranked func(context.Context) (bool, error)
	// Customized reports that the viewer, or the owner's defaults, change
	// some series' episode set (an episode group or hidden specials).
	Customized func(context.Context, *core.User) (bool, error)
}

func (d StoredEpisodeDeps) complete() bool {
	return d.Page != nil && d.Count != nil && d.Unranked != nil && d.Customized != nil
}

// storedEpisodeBatch is the episodes read per step.
const storedEpisodeBatch = 500

// episodeCursorTTL bounds how long a page end is remembered for the next
// page of the same listing.
const episodeCursorTTL = 5 * time.Minute

type episodeCursor struct {
	after core.RatingKey
	at    time.Time
}

// episodeCursors remember where a listing's page ended, so a client paging
// through every episode (StartIndex 0, 400, 800, ...) continues from the
// last key instead of re-reading the episodes before it.
type episodeCursors struct {
	mu sync.Mutex
	m  map[string]episodeCursor
}

func (c *episodeCursors) get(key string) (core.RatingKey, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) > episodeCursorTTL {
		return 0, false
	}
	return e.after, true
}

func (c *episodeCursors) put(key string, after core.RatingKey) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= 4096 {
		c.m = map[string]episodeCursor{}
	}
	c.m[key] = episodeCursor{after: after, at: time.Now()}
}

// storedEpisodePage answers a recursive, episodes-only listing of a series
// library from the stored episodes, in key order: one bounded read per page,
// with no season refresh or provider lookup for series the page never
// reaches. It applies only where that order is the sorted order (popularity,
// which no episode has, ties by key) and the episode set is the
// catalogue's: no custom episode order, no per-viewer policy the total could
// not count, and a filter-free query. ok is false otherwise.
func (s *Service) storedEpisodePage(ctx context.Context, v *core.User, library *Library, q frontend.ItemQuery) (core.Page, bool, error) {
	d := s.d.StoredEpisodes
	if !d.complete() || library == nil || library.Type != core.TypeShow || !q.Recursive || len(q.Types) != 1 || q.Types[0] != core.TypeEpisode ||
		q.Watched != nil || q.Favorite != nil || q.Season != nil || q.AdjacentTo != 0 || q.StartAt != 0 || q.Limit <= 0 ||
		q.Sort != core.SortPopularity || !plainItemFilter(q.Filter) || ViewerRestricted(s.d.Visibility.Policy, v) || s.d.ParentTitles == nil {
		return core.Page{}, false, nil
	}
	if unranked, err := d.Unranked(ctx); err != nil || !unranked {
		return core.Page{}, false, err
	}
	if custom, err := d.Customized(ctx, v); err != nil || custom {
		return core.Page{}, false, err
	}
	show := core.Filter{Type: core.TypeShow, MaxContentRating: v.MaxContentRating}
	total, err := d.Count(ctx, show)
	if err != nil {
		return core.Page{}, false, err
	}
	offset := max(q.Offset, 0)
	listing := fmt.Sprintf("%d|%s|%+v", v.ID, library.Type, show)
	var after core.RatingKey
	skip := offset
	if offset > 0 {
		if key, ok := s.cursors.get(fmt.Sprintf("%s|%d", listing, offset)); ok {
			after, skip = key, 0
		}
	}
	items := make([]core.Title, 0, q.Limit)
	for len(items) < q.Limit {
		batch, err := d.Page(ctx, show, after, storedEpisodeBatch)
		if err != nil {
			return core.Page{}, false, err
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].Key
		kept, err := s.visibleEpisodes(ctx, v, batch, q)
		if err != nil {
			return core.Page{}, false, err
		}
		for _, title := range kept {
			if skip > 0 {
				skip--
				continue
			}
			if len(items) == q.Limit {
				break
			}
			items = append(items, title)
		}
		if len(batch) < storedEpisodeBatch {
			break
		}
	}
	if len(items) == q.Limit {
		s.cursors.put(fmt.Sprintf("%s|%d", listing, offset+len(items)), items[len(items)-1].Key)
	}
	return core.Page{Items: items, Total: max(total, offset+len(items))}, true, nil
}

// visibleEpisodes keeps the episodes of batch the viewer may see, by the
// checks the library walk applies: the series is visible, and the episode
// is visible under it and matches the query.
func (s *Service) visibleEpisodes(ctx context.Context, v *core.User, batch []core.Title, q frontend.ItemQuery) ([]core.Title, error) {
	keys := make([]core.RatingKey, 0, 8)
	seen := map[core.RatingKey]bool{}
	for _, t := range batch {
		if t.ShowKey != 0 && !seen[t.ShowKey] {
			seen[t.ShowKey] = true
			keys = append(keys, t.ShowKey)
		}
	}
	parents, err := s.d.ParentTitles(ctx, keys)
	if err != nil {
		return nil, err
	}
	shown := map[core.RatingKey]bool{}
	for k, parent := range parents {
		shown[k] = parent.Key == k && s.visible(ctx, v, &parent)
	}
	vis := s.d.Visibility
	vis.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		title, ok := parents[key]
		if !ok || title.Key != key {
			return nil, core.ErrNotFound
		}
		return &title, nil
	}
	out := make([]core.Title, 0, len(batch))
	for i := range batch {
		title := batch[i]
		if !shown[title.ShowKey] {
			continue
		}
		id, ok := vis.SectionFor(title.Type)
		if !ok || !SectionAllowed(v, id) || !vis.Visible(ctx, v, &title) {
			continue
		}
		match, err := s.matchesUserQuery(ctx, v, title, q)
		if err != nil {
			return nil, err
		}
		if match {
			out = append(out, title)
		}
	}
	return out, nil
}
