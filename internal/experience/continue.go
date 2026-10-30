package experience

import (
	"context"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Continue coordinates resume and next-episode selection using viewer-scoped dependencies.
type Continue struct {
	Filter       func(context.Context, *core.User, []core.Title) []core.Title
	State        core.State
	EpisodeOrder interface {
		Grouped(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error)
	}
	Now                func() time.Time
	Grandchildren      func(context.Context, core.RatingKey) ([]core.Title, error)
	NextEpisode        func(context.Context, core.RatingKey) (*core.Title, error)
	Visible            func(context.Context, *core.User, *core.Title) bool
	SameDayUnavailable func(context.Context, *core.Title) bool
	GetTitles          func(context.Context, []core.RatingKey) []core.Title
	SectionAllowed     func(*core.User, core.MediaType) bool
	OnError            func(error)
	FavoriteShows      func(context.Context, *core.User) (map[core.RatingKey]bool, error)
	// Unrestricted is a trusted policy capability; nil fails closed. It is
	// supplied only when the catalogue and policy authorize the legacy path.
	Unrestricted func(*core.User) bool
}

func (c Continue) appendNext(ctx context.Context, viewer *core.User, titles []core.Title, state map[core.RatingKey]core.WatchStatus, limit int) []core.Title {
	history, err := c.State.History(ctx, viewer.ID, c.Now().Add(-90*24*time.Hour), 200)
	if err != nil {
		return titles
	}
	seen := map[int64]bool{}
	for _, t := range titles {
		if t.Type == core.TypeEpisode {
			seen[t.Key.TMDB()] = true
		}
	}
	var recent []core.HistoryEntry
	var keys, shows []core.RatingKey
	for _, h := range history {
		if !h.Completed || h.Key.Kind() != core.KindEpisode || seen[h.Key.TMDB()] {
			continue
		}
		seen[h.Key.TMDB()] = true
		recent = append(recent, h)
		keys = append(keys, h.Key)
		shows = append(shows, core.ShowKey(h.Key.TMDB()))
		if len(recent) >= min(max(limit, 1), 50) {
			break
		}
	}
	statuses, err := c.State.GetMany(ctx, viewer.ID, keys)
	if err != nil {
		return titles
	}
	grouped := map[core.RatingKey]bool{}
	if c.EpisodeOrder != nil {
		grouped, err = c.EpisodeOrder.Grouped(ctx, viewer, shows)
		if err != nil {
			return titles
		}
	}
	for _, h := range recent {
		if ctx.Err() != nil {
			break
		}
		if statuses[h.Key].ViewCount == 0 {
			continue
		} // subsequently marked unwatched
		current := h.Key
		var ordered []core.Title
		if grouped[core.ShowKey(current.TMDB())] {
			eps, e := c.Grandchildren(ctx, core.ShowKey(current.TMDB()))
			if e != nil {
				continue
			}
			for i := range eps {
				if eps[i].Key == current {
					ordered = eps[i+1:]
					break
				}
			}
		}
		for n := 0; n < 32; n++ {
			var next *core.Title
			if grouped[core.ShowKey(current.TMDB())] {
				if n >= len(ordered) {
					break
				}
				next = &ordered[n]
			} else {
				next, err = c.NextEpisode(ctx, current)
				if err != nil || next == nil || next.Key == current {
					break
				}
			}
			current = next.Key
			if next.Season == 0 {
				continue
			}
			if !next.Released.IsZero() && next.Released.After(c.Now()) {
				break
			}
			if !c.Visible(ctx, viewer, next) || c.SameDayUnavailable(ctx, next) {
				break
			}
			st, e := c.State.Get(ctx, viewer.ID, next.Key)
			if e != nil {
				break
			}
			if st.ViewCount > 0 && st.ViewOffset <= 0 {
				continue
			}
			st.Key = next.Key
			if st.LastViewedAt.Before(h.ViewedAt) {
				st.LastViewedAt = h.ViewedAt
			}
			state[next.Key] = st
			titles = append(titles, *next)
			break
		}
	}
	return titles
}

func (c Continue) InProgress(ctx context.Context, viewer *core.User, t core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus) {
	if c.State == nil || viewer == nil {
		return nil, nil
	}
	if c.Unrestricted != nil && c.Unrestricted(viewer) {
		sts, err := c.State.InProgress(ctx, viewer.ID, limit*2+10)
		if err != nil {
			if c.OnError != nil {
				c.OnError(err)
			}
			return nil, nil
		}
		return c.inProgressBatchMode(ctx, viewer, t, limit, sts, true)
	}
	// State exposes a limit rather than an offset, so widen the prefix only
	// while filtering leaves gaps. Bound work for viewers with large histories.
	request := limit*2 + 10
	maxRequest := max(request, 1000)
	var previous []core.Title
	var previousState map[core.RatingKey]core.WatchStatus
	for {
		sts, err := c.State.InProgress(ctx, viewer.ID, request)
		if err != nil {
			if c.OnError != nil {
				c.OnError(err)
			}
			return previous, previousState
		}
		out, state := c.inProgressBatch(ctx, viewer, t, limit, sts)
		if limit <= 0 || len(out) >= limit || len(sts) < request || request >= maxRequest || ctx.Err() != nil {
			return out, state
		}
		previous, previousState = out, state
		request = min(request*2, maxRequest)
	}
}

func (c Continue) inProgressBatch(ctx context.Context, viewer *core.User, t core.MediaType, limit int, sts []core.WatchStatus) ([]core.Title, map[core.RatingKey]core.WatchStatus) {
	return c.inProgressBatchMode(ctx, viewer, t, limit, sts, false)
}

func (c Continue) inProgressBatchMode(ctx context.Context, viewer *core.User, t core.MediaType, limit int, sts []core.WatchStatus, unrestricted bool) ([]core.Title, map[core.RatingKey]core.WatchStatus) {
	keys := make([]core.RatingKey, 0, len(sts))
	state := make(map[core.RatingKey]core.WatchStatus, len(sts))
	for _, st := range sts {
		keys = append(keys, st.Key)
		state[st.Key] = st
	}
	candidates := c.GetTitles(ctx, keys)
	titles := make([]core.Title, 0, len(candidates))
	for i := range candidates {
		allowed := unrestricted || c.Visible(ctx, viewer, &candidates[i])
		if allowed {
			titles = append(titles, candidates[i])
		}
	}
	// Resume unfinished media and advance recently completed series. A
	// completed episode has no offset, so InProgress alone loses the show.
	if t != core.TypeMovie {
		titles = c.appendNext(ctx, viewer, titles, state, limit)
	}
	sort.SliceStable(titles, func(i, j int) bool {
		a, b := state[titles[i].Key].LastViewedAt, state[titles[j].Key].LastViewedAt
		if !a.Equal(b) {
			return a.After(b)
		}
		return titles[i].Key < titles[j].Key
	})
	if c.FavoriteShows != nil {
		if favorites, err := c.FavoriteShows(ctx, viewer); err == nil {
			PrioritizeFavoriteShows(titles, favorites)
		}
	}
	seenShows := map[int64]bool{}
	out := titles[:0:0]
	for _, ti := range titles {
		if t != "" {
			want := t
			if t == core.TypeShow {
				want = core.TypeEpisode
			}
			if ti.Type != want {
				continue
			}
		}
		allowed := unrestricted || c.Visible(ctx, viewer, &ti)
		if !c.SectionAllowed(viewer, ti.Type) || !allowed {
			continue
		}
		if ti.Type == core.TypeEpisode {
			if seenShows[ti.Key.TMDB()] {
				continue
			}
			seenShows[ti.Key.TMDB()] = true
		}
		out = append(out, ti)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if unrestricted {
		return out, state
	}
	visibleState := make(map[core.RatingKey]core.WatchStatus, len(out))
	for _, title := range out {
		visibleState[title.Key] = state[title.Key]
	}
	return out, visibleState
}

// PrioritizeFavoriteShows stably promotes episodes of favorite shows without changing order within groups.
func PrioritizeFavoriteShows(titles []core.Title, favorites map[core.RatingKey]bool) {
	if len(favorites) == 0 {
		return
	}
	sort.SliceStable(titles, func(i, j int) bool {
		favorite := func(t core.Title) bool {
			if t.Type != core.TypeEpisode {
				return false
			}
			key := t.ShowKey
			if key == 0 {
				key = core.ShowKey(t.Key.TMDB())
			}
			return favorites[key]
		}
		return favorite(titles[i]) && !favorite(titles[j])
	})
}

// Row applies the viewer's Continue Watching preferences after resume selection.
func (c Continue) Row(ctx context.Context, viewer *core.User, kind core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus) {
	items, state := c.InProgress(ctx, viewer, kind, limit)
	if c.Filter != nil {
		items = c.Filter(ctx, viewer, items)
	}
	return items, state
}

// Shelf selects one overflow candidate before applying preferences and the shelf limit.
func (c Continue) Shelf(ctx context.Context, viewer *core.User, kind core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus, bool) {
	items, state := c.Row(ctx, viewer, kind, limit+1)
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, state, more
}
