package simkl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

// ParseHistory accepts only actual timestamps and TMDB identities; anime is
// deliberately skipped because its episode numbering is not necessarily TMDB's.
// https://api.simkl.org/api-reference/simkl/get-all-items
func ParseHistory(user int64, data []byte) ([]state.ImportRecord, int, error) {
	if user <= 0 {
		return nil, 0, errors.New("simkl: invalid viewer")
	}
	type identity struct {
		IDs struct {
			TMDB json.RawMessage `json:"tmdb"`
		} `json:"ids"`
	}
	type episode struct {
		Number    int    `json:"number"`
		WatchedAt string `json:"watched_at"`
	}
	type season struct {
		Number   int       `json:"number"`
		Episodes []episode `json:"episodes"`
	}
	var response struct {
		Movies []struct {
			Movie         identity `json:"movie"`
			LastWatchedAt string   `json:"last_watched_at"`
		} `json:"movies"`
		Shows []struct {
			Show    identity `json:"show"`
			Seasons []season `json:"seasons"`
		} `json:"shows"`
		Anime []json.RawMessage `json:"anime"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, 0, err
	}
	id := func(raw json.RawMessage) int64 {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			s = string(raw)
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	var out []state.ImportRecord
	skipped := len(response.Anime)
	add := func(key core.RatingKey, ts string) {
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil || at.IsZero() || !key.Valid() {
			skipped++
			return
		}
		out = append(out, state.ImportRecord{UserID: user, Key: key, Views: 1, ViewedAt: at.UTC()})
	}
	for _, m := range response.Movies {
		n := id(m.Movie.IDs.TMDB)
		if n <= 0 || n >= 1_000_000_000_000_000 {
			skipped++
			continue
		}
		add(core.MovieKey(n), m.LastWatchedAt)
	}
	for _, s := range response.Shows {
		n := id(s.Show.IDs.TMDB)
		if n <= 0 || n >= 100_000_000 {
			skipped++
			continue
		}
		for _, season := range s.Seasons {
			for _, ep := range season.Episodes {
				if season.Number < 0 || season.Number >= 1000 || ep.Number <= 0 || ep.Number >= 10000 {
					skipped++
					continue
				}
				add(core.EpisodeKey(n, season.Number, ep.Number), ep.WatchedAt)
			}
		}
	}
	return out, skipped, nil
}

// PullHistory reads activities before the sequential movie/show pulls. The
// returned snapshot must only be saved after all imported records commit.
// https://api.simkl.org/guides/sync
func (c AuthClient) PullHistory(ctx context.Context, user int64, token, snapshot string) ([]state.ImportRecord, string, int, error) {
	var activity struct {
		All string `json:"all"`
	}
	if err := c.request(ctx, http.MethodGet, "/sync/activities", token, nil, nil, &activity); err != nil {
		return nil, "", 0, err
	}
	if _, err := time.Parse(time.RFC3339Nano, activity.All); err != nil {
		return nil, "", 0, errors.New("simkl: invalid activities timestamp")
	}
	if snapshot == activity.All {
		return nil, activity.All, 0, nil
	}
	query := url.Values{"extended": {"full"}, "episode_watched_at": {"yes"}, "include_all_episodes": {"original"}}
	if snapshot != "" {
		if _, err := time.Parse(time.RFC3339Nano, snapshot); err != nil {
			return nil, "", 0, errors.New("simkl: invalid saved snapshot")
		}
		query.Set("date_from", snapshot)
	}
	// Stay below Simkl's per-user 10 GET/s, including split responses.
	last := time.Now()
	get := func(path string, dst any) error {
		wait := time.Until(last.Add(110 * time.Millisecond))
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		last = time.Now()
		return c.request(ctx, http.MethodGet, path, token, query, nil, dst)
	}
	parse := func(kind string, data []byte) ([]state.ImportRecord, int, error) {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, 0, err
		}
		if envelope == nil {
			return nil, 0, errors.New("simkl: invalid history response")
		}
		raw, ok := envelope[kind]
		if !ok {
			return nil, 0, nil
		}
		if string(raw) == "null" {
			return nil, 0, errors.New("simkl: invalid history response")
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, 0, errors.New("simkl: invalid history response")
		}
		return ParseHistory(user, data)
	}
	var records []state.ImportRecord
	skipped := 0
	for _, kind := range []string{"movies", "shows"} {
		var data json.RawMessage
		err := get("/sync/all-items/"+kind, &data)
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusBadRequest && he.Code == "max_items" {
			// A response can exceed Simkl's max_items limit. Split by status;
			// any failed split aborts the whole pull and leaves the old snapshot.
			statuses := []string{"plantowatch", "completed", "dropped"}
			if kind == "shows" {
				statuses = []string{"watching", "plantowatch", "hold", "completed", "dropped"}
			}
			for _, status := range statuses {
				data = nil
				if err = get("/sync/all-items/"+kind+"/"+status, &data); err != nil {
					return nil, "", 0, err
				}
				r, n, err := parse(kind, data)
				if err != nil {
					return nil, "", 0, err
				}
				records = append(records, r...)
				skipped += n
			}
			continue
		}
		if err != nil {
			return nil, "", 0, err
		}
		r, n, err := parse(kind, data)
		if err != nil {
			return nil, "", 0, err
		}
		records = append(records, r...)
		skipped += n
	}
	return records, activity.All, skipped, nil
}
