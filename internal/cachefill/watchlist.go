package cachefill

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// WarmIntent is created only by a watchlist add or first availability event.
// Acknowledging an intent never changes watch state or watchlist membership.
type WarmIntent struct {
	UserID int64
	User   core.User
	Key    core.RatingKey
	Added  time.Time
}

func (m *Manager) watchlistCandidates(ctx context.Context, cfg config.Cache, intents []WarmIntent) ([]candidate, error) {
	var out []candidate
	if m.deps.State == nil {
		return out, nil
	}
	now := m.deps.Now()
	for _, intent := range intents {
		if intent.User.ID <= 0 {
			m.finishIntent(ctx, intent)
			continue
		}
		history, err := m.deps.State.History(ctx, intent.User.ID, time.Time{}, 2000)
		if err != nil {
			return nil, err
		}
		progress, err := m.deps.State.InProgress(ctx, intent.User.ID, 200)
		if err != nil {
			return nil, err
		}
		active := false
		pos := cursor{}
		for _, h := range history {
			if !h.ViewedAt.Before(now.Add(-7*24*time.Hour)) && !h.ViewedAt.After(now) {
				active = true
			}
			if h.Completed && rootKey(h.Key) == rootKey(intent.Key) && h.ViewedAt.After(pos.at) {
				pos = cursor{key: h.Key, at: h.ViewedAt}
			}
		}
		for _, p := range progress {
			at := p.LastViewedAt
			if at.IsZero() {
				at = p.UpdatedAt
			}
			if !at.Before(now.Add(-7*24*time.Hour)) && !at.After(now) {
				active = true
			}
			if rootKey(p.Key) == rootKey(intent.Key) && at.After(pos.at) {
				pos = cursor{key: p.Key, at: at, resume: true}
			}
		}
		// A primary-key range lookup includes old/imported watch state even when
		// it has fallen outside the bounded recent-history window.
		if intent.Key.Kind() == core.KindShow && m.deps.WatchlistPosition != nil {
			st, err := m.deps.WatchlistPosition(ctx, intent.User.ID, intent.Key)
			if err != nil {
				return nil, err
			}
			if st.Key != 0 && (pos.key == 0 || st.LastViewedAt.After(pos.at)) {
				pos = cursor{key: st.Key, at: st.LastViewedAt, resume: st.ViewOffset > 0 && st.ViewCount == 0}
			}
		}
		if !active {
			m.finishIntent(ctx, intent)
			continue
		}
		title, err := m.deps.Catalog.CachedTitle(ctx, rootKey(intent.Key))
		if err != nil || title == nil {
			if title == nil && (err == nil || errors.Is(err, core.ErrNotFound)) {
				m.finishIntent(ctx, intent)
			}
			continue
		}
		if title.Adult || !title.IsReleased(now) || (m.deps.Allowed != nil && !m.deps.Allowed(&intent.User, title)) {
			m.finishIntent(ctx, intent)
			continue
		}
		if title.Type == core.TypeShow {
			one := cfg
			one.PreloadEpisodesAhead = 24 // bounded lookahead also skips out-of-order scrobbles
			positions := map[int64]cursor{}
			if pos.key != 0 {
				positions[intent.User.ID] = pos
			}
			episodes, err := m.episodeWindow(ctx, *title, positions, one)
			if err != nil {
				return nil, err
			}
			if len(episodes) == 0 {
				m.finishIntent(ctx, intent)
				continue
			}
			if state, ok := m.deps.State.(interface {
				GetMany(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
			}); ok {
				keys := make([]core.RatingKey, len(episodes))
				for i, ep := range episodes {
					keys[i] = ep.Key
				}
				statuses, err := state.GetMany(ctx, intent.User.ID, keys)
				if err != nil {
					return nil, err
				}
				unwatched := episodes[:0]
				for _, ep := range episodes {
					if statuses[ep.Key].ViewCount == 0 {
						unwatched = append(unwatched, ep)
					}
				}
				episodes = unwatched
				if len(episodes) == 0 {
					m.finishIntent(ctx, intent)
					continue
				}
			}
			title = &episodes[0]
		}
		if title.Type != core.TypeMovie && title.Type != core.TypeEpisode {
			continue
		}
		out = append(out, candidate{Title: *title, priority: 1, intent: &intent, Selection: Selection{Key: title.Key, Title: displayTitle(*title), Type: title.Type, Reason: "Watchlist", ReasonCodes: []string{"watchlist"}}})
	}
	return out, nil
}
func (m *Manager) finishIntent(ctx context.Context, intent WarmIntent) {
	if m.deps.WatchlistDone != nil {
		_ = m.deps.WatchlistDone(ctx, intent)
	}
}
func (m *Manager) enabled(c config.Cache) bool {
	m.mu.Lock()
	pending := m.intentPass
	m.mu.Unlock()
	return enabled(c) || (pending && c.Mode != "off" && !c.DisableSpeculation)
}
