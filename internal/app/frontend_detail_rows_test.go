package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestExperienceDetailRowsFilterBeforeLimitsAndDeduplicate(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	key := core.EpisodeKey(42, 1, 1)
	next := core.Title{Key: core.EpisodeKey(42, 1, 2), Type: core.TypeEpisode, ShowKey: core.ShowKey(42)}
	hidden := core.Title{Key: core.MovieKey(1), Adult: true, Type: core.TypeMovie}
	a := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie}
	b := core.Title{Key: core.MovieKey(3), Type: core.TypeMovie}
	c := core.Title{Key: core.MovieKey(4), Type: core.TypeMovie}
	vis := experience.Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: core.ShowKey(42), Type: core.TypeShow}, nil
	}}
	picks := []core.PostPlayPick{{Title: next}, {Title: hidden}, {Title: a}, {Title: b}}
	rows, err := experience.DetailRows(ctx, viewer, key, 2, vis,
		func(context.Context, core.RatingKey) (*core.Title, error) { return &next, nil },
		func(_ context.Context, n int) []core.PostPlayPick { return picks[:min(n, len(picks))] },
		func(_ context.Context, n int) ([]experience.RankedCandidate, error) {
			items := []core.Title{a, hidden, b, c}
			return []experience.RankedCandidate{{ID: "related", Items: items[:min(n, len(items))]}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].ID != "tv.upnext" || rows[1].ID != "tv.postplay" || rows[2].ID != "related" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if len(rows[1].Items) != 2 || rows[1].Items[0].Key != a.Key || rows[1].Items[1].Key != b.Key || len(rows[2].Items) != 1 || rows[2].Items[0].Key != c.Key {
		t.Fatalf("unexpected filtered or duplicate items: %+v", rows)
	}
}

func TestExperienceDetailRowsRejectsRestrictedParent(t *testing.T) {
	ctx := context.Background()
	key := core.EpisodeKey(42, 1, 1)
	next := core.Title{Key: core.EpisodeKey(42, 1, 2), Type: core.TypeEpisode, ShowKey: core.ShowKey(42)}
	vis := experience.Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: core.ShowKey(42), Type: core.TypeShow, Adult: true}, nil
	}}
	rows, err := experience.DetailRows(ctx, &core.User{ID: 1}, key, 1, vis,
		func(context.Context, core.RatingKey) (*core.Title, error) { return &next, nil }, nil, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("restricted episode leaked: %+v, %v", rows, err)
	}
}

func TestDetailRowsMatchLegacyFrontendOutputAndCalls(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	key := core.EpisodeKey(42, 1, 1)
	next := core.Title{Key: core.EpisodeKey(42, 1, 2), Type: core.TypeEpisode, ShowKey: core.ShowKey(42)}
	hidden := core.Title{Key: core.MovieKey(11), Type: core.TypeMovie, Adult: true}
	visible := core.Title{Key: core.MovieKey(12), Type: core.TypeMovie}
	vis := experience.Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: core.ShowKey(42)}, nil
	}}
	run := func(legacy bool) ([]frontend.Row, []int, []int, error) {
		var pickCalls, relatedCalls []int
		nextSource := func(context.Context, core.RatingKey) (*core.Title, error) { return &next, nil }
		picks := func(_ context.Context, n int) []core.PostPlayPick {
			pickCalls = append(pickCalls, n)
			p := []core.PostPlayPick{{Title: hidden}, {Title: visible}, {Title: next}}
			return p[:min(n, len(p))]
		}
		relatedHubs := func(_ context.Context, n int) ([]core.Hub, error) {
			relatedCalls = append(relatedCalls, n)
			items := []core.Title{hidden, visible, {Key: core.MovieKey(13)}}
			return []core.Hub{{ID: "related", Context: "hub.related", Items: items[:min(n, len(items))]}}, nil
		}
		if legacy {
			h, err := controlLegacyExperienceDetailRows(ctx, viewer, key, 1, vis, nextSource, picks, relatedHubs)
			return experience.HubRows(h), pickCalls, relatedCalls, err
		}
		c, err := experience.DetailRows(ctx, viewer, key, 1, vis, nextSource, picks, func(ctx context.Context, n int) ([]experience.RankedCandidate, error) {
			return rankedCandidates(relatedHubs(ctx, n))
		})
		return experience.CandidateRows(c), pickCalls, relatedCalls, err
	}
	want, wp, wr, we := run(true)
	got, gp, gr, ge := run(false)
	if (we == nil) != (ge == nil) || !reflect.DeepEqual(want, got) || !reflect.DeepEqual(wp, gp) || !reflect.DeepEqual(wr, gr) {
		t.Fatalf("legacy=%+v picks=%v related=%v err=%v; shared=%+v picks=%v related=%v err=%v", want, wp, wr, we, got, gp, gr, ge)
	}
}
