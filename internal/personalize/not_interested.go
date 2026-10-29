package personalize

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// NotInterestedSignal is explicit viewer feedback for one title.
type NotInterestedSignal struct {
	Key     core.RatingKey
	AddedAt time.Time
}

type excludedTitles map[core.RatingKey]bool

func (x excludedTitles) contains(t *core.Title) bool {
	if t == nil {
		return false
	}
	if x[t.Key] {
		return true
	}
	show := t.ShowKey
	if show == 0 && (t.Key.Kind() == core.KindSeason || t.Key.Kind() == core.KindEpisode) {
		show = core.ShowKey(t.Key.TMDB())
	}
	return show != 0 && x[show]
}

func (e *Engine) exclusions(ctx context.Context, u *core.User) (excludedTitles, string, error) {
	if e.opts.NotInterestedSnapshot != nil && u != nil && u.ID > 0 {
		signals, revision, err := e.opts.NotInterestedSnapshot(ctx, u)
		if err != nil {
			return nil, "", err
		}
		set, sig := snapshotSet(signals, revision)
		return set, sig, nil
	}
	if e.opts.NotInterested == nil || u == nil || u.ID <= 0 {
		return nil, "", nil
	}
	return loadExclusions(ctx, u, e.opts.NotInterested)
}

func snapshotSet(signals []NotInterestedSignal, revision uint64) (excludedTitles, string) {
	set, sig := exclusionSet(signals)
	if revision != 0 {
		sig += "|r" + strconv.FormatUint(revision, 10)
	}
	return set, sig
}

func loadExclusions(ctx context.Context, u *core.User, source func(context.Context, *core.User) ([]NotInterestedSignal, error)) (excludedTitles, string, error) {
	if source == nil || u == nil || u.ID <= 0 {
		return nil, "", nil
	}
	signals, err := source(ctx, u)
	if err != nil {
		return nil, "", err
	}
	set, sig := exclusionSet(signals)
	return set, sig, nil
}

func exclusionSet(signals []NotInterestedSignal) (excludedTitles, string) {
	if len(signals) == 0 {
		return nil, ""
	}
	keys := make([]int64, 0, len(signals))
	set := make(excludedTitles, len(signals))
	for _, signal := range signals {
		if signal.Key != 0 && !set[signal.Key] {
			set[signal.Key] = true
			keys = append(keys, int64(signal.Key))
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var signature strings.Builder
	for _, key := range keys {
		signature.WriteString(strconv.FormatInt(key, 36))
		signature.WriteByte(',')
	}
	return set, signature.String()
}

// FilterRecommendations applies explicit exclusions without changing the
// catalogue, watchlist or direct item access.
func (e *Engine) FilterRecommendations(ctx context.Context, u *core.User, items []core.Title) ([]core.Title, error) {
	excluded, _, err := e.exclusions(ctx, u)
	if err != nil || len(excluded) == 0 {
		return items, err
	}
	out := make([]core.Title, 0, len(items))
	for _, item := range items {
		if !excluded.contains(&item) {
			out = append(out, item)
		}
	}
	return out, nil
}

// NotInterestedChanged requests taste rebuild and cluster invalidation after a
// committed mutation. Request-time snapshots prevent stale cached exposure.
func (e *Engine) NotInterestedChanged(userID int64) {
	e.request(userID)
	go e.notify("overrides:" + strconv.FormatInt(userID, 10))
}
