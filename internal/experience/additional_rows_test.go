package experience

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
)

type additionalPlaylistFake struct {
	calls *[]string
	err   error
}

func (f additionalPlaylistFake) List(_ context.Context, id int64) ([]core.Playlist, error) {
	*f.calls = append(*f.calls, "playlist")
	return []core.Playlist{{Key: core.PlaylistKey(1), UserID: id, Title: "Owned"}, {Key: core.PlaylistKey(2), UserID: id + 1}}, f.err
}

type additionalDiscoveryFake struct {
	name   string
	calls  *[]string
	limits *[]int
	items  []core.Title
	err    error
}

func (f additionalDiscoveryFake) Rows(_ context.Context, _ *core.User, limit int) ([]RankedCandidate, error) {
	*f.calls = append(*f.calls, f.name)
	*f.limits = append(*f.limits, limit)
	if f.err != nil {
		return nil, f.err
	}
	return []RankedCandidate{{ID: f.name, Purpose: frontend.RowWatchlist, Items: f.items[:min(max(limit, 0), len(f.items))]}}, nil
}

type additionalLiveFake struct {
	calls      *[]string
	start, end time.Time
}

func (f *additionalLiveFake) AllowedChannels(_ context.Context, u *core.User) ([]livetv.Channel, error) {
	*f.calls = append(*f.calls, "channels")
	if u.ID != 1 {
		return nil, errors.New("unauthorized")
	}
	return []livetv.Channel{{ID: "1", Name: "TV"}}, nil
}
func (f *additionalLiveFake) Guide(_ context.Context, _ *core.User, start, end time.Time) ([]livetv.Programme, error) {
	*f.calls = append(*f.calls, "guide")
	f.start, f.end = start, end
	return nil, nil
}

func TestAdditionalRowsOrderPolicyAndHorizon(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	calls := []string{}
	var limits []int
	items := []core.Title{{Key: core.MovieKey(1), Adult: true}, {Key: core.MovieKey(2), Adult: true}, {Key: core.MovieKey(3)}, {Key: core.MovieKey(4)}}
	live := &additionalLiveFake{calls: &calls}
	source := additionalDiscoveryFake{name: "watchlist", calls: &calls, limits: &limits, items: items}
	c := AdditionalRows{Playlists: additionalPlaylistFake{calls: &calls}, Watchlist: source, Live: live, Restricted: func(*core.User) bool { return true }, Now: func() time.Time { return now }}
	req := frontend.RowRequest{Viewer: &core.User{ID: 1}, Placement: frontend.PlacementHome, Limit: 2}
	rows, err := c.Rows(ctx, req)
	if err != nil || len(rows) != 3 || rows[0].ID != "playlists" || rows[1].ID != "watchlist" || rows[2].ID != "live" || len(rows[0].Entries) != 1 || len(rows[1].Entries) != 2 || rows[1].Entries[0].Title.Key != items[2].Key {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if !reflect.DeepEqual(calls, []string{"playlist", "watchlist", "watchlist", "channels", "guide"}) || !reflect.DeepEqual(limits, []int{2, 4}) || !live.start.Equal(now) || !live.end.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("calls=%v limits=%v horizon=%v..%v", calls, limits, live.start, live.end)
	}
	calls = nil
	limits = nil
	req.Viewer.IsOwner = true
	rows, err = c.Rows(ctx, req)
	if err != nil || len(rows) != 3 || !reflect.DeepEqual(limits, []int{2}) || rows[1].Entries[0].Title.Key != items[0].Key {
		t.Fatalf("owner rows=%+v limits=%v err=%v", rows, limits, err)
	}
}
func TestAdditionalRowsErrorAndPlacement(t *testing.T) {
	failure := errors.New("playlist failure")
	calls := []string{}
	c := AdditionalRows{Playlists: additionalPlaylistFake{calls: &calls, err: failure}}
	req := frontend.RowRequest{Viewer: &core.User{ID: 1}, Placement: frontend.PlacementLibrary}
	if rows, err := c.Rows(context.Background(), req); err != nil || rows != nil || len(calls) != 0 {
		t.Fatalf("non-home %+v %v %v", rows, err, calls)
	}
	req.Placement = frontend.PlacementHome
	if _, err := c.Rows(context.Background(), req); !errors.Is(err, failure) || !reflect.DeepEqual(calls, []string{"playlist"}) {
		t.Fatalf("error %v calls=%v", err, calls)
	}
	calls = nil
	req.Viewer = nil
	if rows, err := c.Rows(context.Background(), req); err != nil || rows != nil || len(calls) != 0 {
		t.Fatalf("nil viewer rows=%+v err=%v calls=%v", rows, err, calls)
	}
}

// This independent control preserves the previous app ordering and discovery
// widening algorithm while using the same fake ports as the shared composer.
func legacyAdditionalRowsControl(ctx context.Context, req frontend.RowRequest, c AdditionalRows) ([]frontend.Row, error) {
	if req.Placement != frontend.PlacementHome || req.Viewer == nil {
		return nil, nil
	}
	var rows []frontend.Row
	if c.Playlists != nil {
		entries, more, err := PlaylistEntries(ctx, req.Viewer, c.Playlists, req.Limit)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "playlists", Title: "Recent Playlists", Purpose: core.RowPurpose("playlist"), Entries: entries, More: more})
		}
	}
	restricted := !req.Viewer.IsOwner && (req.Viewer.MaxContentRating != "" || len(req.Viewer.AllowedSections) > 0 || c.Restricted(req.Viewer))
	for _, source := range []DiscoveryRowSource{c.Watchlist, c.Personal} {
		if source == nil {
			continue
		}
		var candidates []RankedCandidate
		var loadErr error
		if !restricted {
			candidates, loadErr = source.Rows(ctx, req.Viewer, req.Limit)
		} else {
			var current []RankedCandidate
			filtered := LoadDiscovery(ctx, req.Viewer, c.Visibility, req.Limit, func(ctx context.Context, n int) []DiscoveryCandidate {
				current, loadErr = source.Rows(ctx, req.Viewer, n)
				if loadErr != nil {
					return nil
				}
				out := make([]DiscoveryCandidate, len(current))
				for i, row := range current {
					out[i] = DiscoveryCandidate{Index: i, Items: row.Items}
				}
				return out
			})
			candidates = make([]RankedCandidate, 0, len(filtered))
			for _, row := range filtered {
				c := current[row.Index]
				c.Items = row.Items
				candidates = append(candidates, c)
			}
		}
		if loadErr != nil {
			return nil, loadErr
		}
		for _, c := range candidates {
			row := frontend.Row{ID: c.ID, Title: c.Title, Purpose: c.Purpose, Style: c.Style, Promoted: c.Promoted, More: c.More, Collection: c.Key}
			for i := range c.Items {
				item := c.Items[i]
				row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &item})
			}
			rows = append(rows, row)
		}
	}
	if c.Live != nil {
		now := c.Now()
		entries, err := LiveEntries(ctx, req.Viewer, c.Live, now, now.Add(24*time.Hour), nil)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "live", Title: "On Now", Purpose: frontend.RowLive, Entries: entries})
		}
	}
	return rows, nil
}

func TestAdditionalRowsMatchesLegacyCallsAndResults(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	items := []core.Title{{Key: core.MovieKey(1), Adult: true}, {Key: core.MovieKey(2)}, {Key: core.MovieKey(3)}}
	for _, tc := range []struct {
		name  string
		owner bool
		fail  string
	}{{"restricted", false, ""}, {"owner", true, ""}, {"watchlist error", false, "watchlist"}, {"personal error", false, "personal"}} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(legacy bool) ([]frontend.Row, error, []string, []int, time.Time, time.Time) {
				calls := []string{}
				var limits []int
				live := &additionalLiveFake{calls: &calls}
				source := additionalDiscoveryFake{name: "watchlist", calls: &calls, limits: &limits, items: items}
				personal := additionalDiscoveryFake{name: "personal", calls: &calls, limits: &limits, items: items}
				if tc.fail == "watchlist" {
					source.err = errors.New("discovery unavailable")
				}
				if tc.fail == "personal" {
					personal.err = errors.New("personal unavailable")
				}
				c := AdditionalRows{Playlists: additionalPlaylistFake{calls: &calls}, Watchlist: source, Personal: personal, Live: live, Restricted: func(*core.User) bool { return true }, Now: func() time.Time { return now }}
				req := frontend.RowRequest{Viewer: &core.User{ID: 1, IsOwner: tc.owner}, Placement: frontend.PlacementHome, Limit: 1}
				var rows []frontend.Row
				var err error
				if legacy {
					rows, err = legacyAdditionalRowsControl(context.Background(), req, c)
				} else {
					rows, err = c.Rows(context.Background(), req)
				}
				return rows, err, calls, limits, live.start, live.end
			}
			want, we, wc, wl, ws, wt := run(true)
			got, ge, gc, gl, gs, gt := run(false)
			if (we == nil) != (ge == nil) || we != nil && we.Error() != ge.Error() || !reflect.DeepEqual(want, got) || !reflect.DeepEqual(wc, gc) || !reflect.DeepEqual(wl, gl) || !ws.Equal(gs) || !wt.Equal(gt) {
				t.Fatalf("legacy=%+v %v %v %v %v %v; shared=%+v %v %v %v %v %v", want, we, wc, wl, ws, wt, got, ge, gc, gl, gs, gt)
			}
			if tc.fail == "personal" && (len(gc) == 0 || gc[len(gc)-1] != "personal" || !gs.IsZero() || !gt.IsZero()) {
				t.Fatalf("personal failure did not stop live: calls=%v horizon=%v..%v", gc, gs, gt)
			}
			if tc.owner && tc.fail == "" && !reflect.DeepEqual(gc, []string{"playlist", "watchlist", "personal", "channels", "guide"}) {
				t.Fatalf("source order %v", gc)
			}
		})
	}
}
