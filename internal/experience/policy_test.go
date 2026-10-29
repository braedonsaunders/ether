package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type denyPolicy struct{}

func (denyPolicy) Allowed(*core.User, *core.Title) bool { return false }

func TestTitleAllowed(t *testing.T) {
	viewer := &core.User{}
	if TitleAllowed(nil, viewer, nil) || TitleAllowed(nil, viewer, &core.Title{Adult: true}) {
		t.Fatal("missing or adult titles must be denied")
	}
	title := &core.Title{}
	if !TitleAllowed(nil, viewer, title) || TitleAllowed(denyPolicy{}, viewer, title) {
		t.Fatal("owner policy must restrict otherwise allowed titles")
	}
}

func TestSectionAllowed(t *testing.T) {
	viewer := &core.User{AllowedSections: []int{2}}
	if !SectionAllowed(viewer, 2) || SectionAllowed(viewer, 3) {
		t.Fatal("section restrictions were not applied")
	}
	viewer.IsOwner = true
	if !SectionAllowed(viewer, 3) || !SectionAllowed(nil, 3) || !SectionAllowed(&core.User{}, 3) {
		t.Fatal("owner and unrestricted viewers must see sections")
	}
}

func TestVisibilityInheritedAndAvailability(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7, AllowedSections: []int{1}}
	showKey := core.RatingKey(17)
	episode := &core.Title{Key: 18, ShowKey: showKey, Type: core.TypeEpisode}
	show := &core.Title{Key: showKey, Type: core.TypeShow, Adult: true}
	v := Visibility{
		SectionFor:      func(core.MediaType) (int, bool) { return 1, true },
		GetTitle:        func(context.Context, core.RatingKey) (*core.Title, error) { return show, nil },
		FilterAvailable: func(context.Context, []core.Title) []core.Title { return nil },
		Sessions:        func() []*core.PlaySession { return []*core.PlaySession{{UserID: 7, Key: episode.Key}} },
	}
	if v.Playable(ctx, viewer, episode) || v.Visible(ctx, viewer, episode) {
		t.Fatal("adult parent must hide episode")
	}
	show.Adult = false
	if !v.Visible(ctx, viewer, episode) {
		t.Fatal("existing session must preserve availability")
	}
	viewer.ID = 8
	if v.Visible(ctx, viewer, episode) || !v.Playable(ctx, viewer, episode) {
		t.Fatal("discovery must hide unavailable item without denying playback")
	}
	viewer.AllowedSections = []int{2}
	if v.Playable(ctx, viewer, episode) {
		t.Fatal("library restriction must apply")
	}
}

func TestUpcomingCollectionScope(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v := Visibility{FilterAvailable: func(context.Context, []core.Title) []core.Title { return nil }}
	titles := []core.Title{{Key: 1}, {Key: 2, Released: now.Add(-time.Hour)}, {Key: 3, Released: now.Add(time.Hour)}, {Key: 4, Adult: true, Released: now.Add(time.Hour)}}
	got := v.UpcomingItems(context.Background(), nil, titles, func() time.Time { return now })
	if len(got) != 1 || got[0].Key != 3 {
		t.Fatalf("unexpected visible items: %+v", got)
	}
}

func TestInheritedShowLookupFailure(t *testing.T) {
	episode := &core.Title{ShowKey: 9}
	v := Visibility{}
	if v.Playable(context.Background(), nil, episode) {
		t.Fatal("missing lookup must deny")
	}
	v.GetTitle = func(context.Context, core.RatingKey) (*core.Title, error) { return nil, errors.New("unavailable") }
	if v.Playable(context.Background(), nil, episode) {
		t.Fatal("failed lookup must deny")
	}
	v.GetTitle = func(context.Context, core.RatingKey) (*core.Title, error) { return nil, nil }
	if v.Playable(context.Background(), nil, episode) {
		t.Fatal("missing parent must deny")
	}
}

func TestCollectionSectionRestriction(t *testing.T) {
	v := Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	viewer := &core.User{AllowedSections: []int{2}}
	if v.CollectionAllowed(viewer, &core.Collection{Type: core.TypeMovie}) || v.CollectionAllowed(viewer, &core.Collection{Type: core.TypeShow}) {
		t.Fatal("restricted collection was visible")
	}
	if !v.CollectionAllowed(viewer, &core.Collection{}) {
		t.Fatal("mixed collection must retain existing behavior")
	}
}

func TestAvailabilitySessionIsolation(t *testing.T) {
	titles := []core.Title{{Key: 1}, {Key: 2}}
	v := Visibility{FilterAvailable: func(context.Context, []core.Title) []core.Title { return nil }, Sessions: func() []*core.PlaySession { return []*core.PlaySession{nil, {UserID: 1, Key: 1}, {UserID: 2, Key: 2}} }}
	got := v.Available(context.Background(), &core.User{ID: 1}, titles)
	if len(got) != 1 || got[0].Key != 1 {
		t.Fatalf("sessions leaked across viewers: %+v", got)
	}
}
