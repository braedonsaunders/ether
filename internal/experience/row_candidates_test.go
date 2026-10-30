package experience

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type candidateRankerFake struct {
	rows  []RankedCandidate
	calls int
	seen  core.HomeSituation
	allow func(context.Context, *core.User, *core.Title) bool
	kind  core.MediaType
	err   error
}

func (f *candidateRankerFake) Home(_ context.Context, _ *core.User, s core.HomeSituation, _ int, allow func(context.Context, *core.User, *core.Title) bool) ([]RankedCandidate, error) {
	f.calls++
	f.seen = s
	f.allow = allow
	return append([]RankedCandidate(nil), f.rows...), f.err
}
func (f *candidateRankerFake) Library(_ context.Context, _ *core.User, k core.MediaType, s core.HomeSituation, _ int, allow func(context.Context, *core.User, *core.Title) bool) ([]RankedCandidate, error) {
	f.kind = k
	return f.Home(context.Background(), nil, s, 0, allow)
}

type resumeCandidatesFake struct {
	items []core.Title
	calls int
	err   error
}

func (f *resumeCandidatesFake) Continue(context.Context, *core.User, int) ([]core.Title, error) {
	f.calls++
	return f.items, f.err
}

func TestCandidateComposerHomeVisitAndPolicy(t *testing.T) {
	ctx := context.Background()
	show := core.ShowKey(8)
	child := core.Title{Key: core.EpisodeKey(8, 1, 1), Type: core.TypeEpisode, ShowKey: show}
	hidden := core.Title{Key: core.MovieKey(3), Type: core.TypeMovie, Adult: true}
	good := core.Title{Key: core.MovieKey(4), Type: core.TypeMovie}
	rank := &candidateRankerFake{rows: []RankedCandidate{{ID: "home.continue"}, {ID: "other"}, {ID: "home.continue"}}}
	resume := &resumeCandidatesFake{items: []core.Title{good}}
	composer := CandidateComposer{Ranker: rank, Resume: resume, Restricted: func(*core.User) bool { return true }, Visibility: Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: show, Adult: true}, nil
	}}}
	situation := core.HomeSituation{Now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ClientID: "client", SessionID: "visit", Device: "tv"}
	req := frontend.RowRequest{Viewer: &core.User{ID: 1, AllowedSections: []int{1}}, Placement: frontend.PlacementHome, Limit: 3}
	got, err := composer.Rows(ctx, req, situation)
	if err != nil || rank.calls != 1 || resume.calls != 1 || !reflect.DeepEqual(rank.seen, situation) || len(got) != 3 || got[0].Items[0].Key != good.Key || got[2].Items[0].Key != good.Key {
		t.Fatalf("rows=%+v rank=%+v resume=%+v err=%v", got, rank, resume, err)
	}
	if rank.allow == nil || rank.allow(ctx, req.Viewer, &hidden) || rank.allow(ctx, req.Viewer, &child) || !rank.allow(ctx, req.Viewer, &good) {
		t.Fatal("ranking did not receive inherited viewer policy")
	}
	req.Viewer = &core.User{ID: 2, IsOwner: true}
	rank.rows = nil
	got, err = composer.Rows(ctx, req, situation)
	if err != nil || rank.allow != nil || len(got) != 1 || got[0].ID != "home.continue" || got[0].Items[0].Key != good.Key {
		t.Fatalf("owner=%+v allow=%v err=%v", got, rank.allow != nil, err)
	}
}

func TestCandidateComposerLibraryAndErrors(t *testing.T) {
	ctx := context.Background()
	rank := &candidateRankerFake{}
	resume := &resumeCandidatesFake{}
	c := CandidateComposer{Ranker: rank, Resume: resume, Libraries: []Library{{ID: 2, Type: core.TypeShow}}, Restricted: func(*core.User) bool { return false }}
	req := frontend.RowRequest{Viewer: &core.User{ID: 1, AllowedSections: []int{1}}, Placement: frontend.PlacementLibrary, ViewID: "2"}
	if _, err := c.Rows(ctx, req, core.HomeSituation{}); !errors.Is(err, core.ErrNotFound) || rank.calls != 0 {
		t.Fatal("denied library reached ranker", err)
	}
	req.Viewer.AllowedSections = []int{2}
	if _, err := c.Rows(ctx, req, core.HomeSituation{ClientID: "x"}); err != nil || rank.calls != 1 || rank.kind != core.TypeShow || rank.seen.LibraryID != 2 || rank.seen.ClientID != "x" || rank.allow == nil {
		t.Fatalf("library rank=%+v err=%v", rank, err)
	}
	req.ViewID = "bad"
	if _, err := c.Rows(ctx, req, core.HomeSituation{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("invalid library", err)
	}
	failure := errors.New("rank failed")
	rank.err = failure
	req.Placement = frontend.PlacementHome
	if _, err := c.Rows(ctx, req, core.HomeSituation{}); !errors.Is(err, failure) || resume.calls != 0 {
		t.Fatal("rank error", err)
	}
	rank.err = nil
	resume.err = failure
	if _, err := c.Rows(ctx, req, core.HomeSituation{}); !errors.Is(err, failure) {
		t.Fatal("resume error", err)
	}
}
