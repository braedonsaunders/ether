package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type describeSource struct {
	fakeVibe
	enabled bool
	waits   []bool
}

func (s *describeSource) DescriptiveSearchAvailable() bool { return s.enabled }
func (s *describeSource) DescribeSearch(_ context.Context, _ *core.User, _ string, wait bool) (core.DescriptiveResult, error) {
	s.waits = append(s.waits, wait)
	return core.DescriptiveResult{State: "ready", Items: s.items}, nil
}

func TestAISearchPolicyAndAvailability(t *testing.T) {
	viewer := &core.User{ID: 7, MaxContentRating: "PG"}
	source := &describeSource{enabled: true, fakeVibe: fakeVibe{items: []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Family pick", ContentRating: "PG"},
		{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Adult", Adult: true},
		{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Too mature", ContentRating: "R"},
		{Key: core.MovieKey(4), Type: core.TypeMovie, Title: "Unavailable", ContentRating: "PG"},
	}}}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Search: Search{Vibe: VibeSearch{Source: source}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }, FilterAvailable: func(_ context.Context, titles []core.Title) []core.Title {
		var out []core.Title
		for _, title := range titles {
			if title.Key != core.MovieKey(4) {
				out = append(out, title)
			}
		}
		return out
	}}}}
	if !s.AISearchAvailable() || !s.DescriptiveSearchAvailable() {
		t.Fatal("enabled AI unavailable")
	}
	for _, wait := range []bool{false, true} {
		var got frontend.SearchPage
		var err error
		if wait {
			got, err = s.AISearch(context.Background(), viewer, core.Device{}, "a night in", frontend.ItemQuery{Limit: 50})
		} else {
			got, err = s.DescriptiveSearch(context.Background(), viewer, core.Device{}, "a night in", frontend.ItemQuery{Limit: 50})
		}
		if err != nil || len(got.Page.Items) != 1 || got.Page.Items[0].Key != core.MovieKey(1) {
			t.Fatalf("wait %v: %+v %v", wait, got, err)
		}
	}
	if len(source.waits) != 2 || source.waits[0] || !source.waits[1] {
		t.Fatalf("wait lanes %v", source.waits)
	}
	viewer.AllowedLibraries = []core.LibraryID{2}
	page, err := s.AISearch(context.Background(), viewer, core.Device{}, "a night in", frontend.ItemQuery{})
	if err != nil || len(page.Page.Items) != 0 {
		t.Fatalf("changed library policy: %+v %v", page, err)
	}
	source.enabled = false
	if s.AISearchAvailable() || s.DescriptiveSearchAvailable() {
		t.Fatal("advertised after AI disabled")
	}
	if _, err = s.AISearch(context.Background(), viewer, core.Device{}, "a night in", frontend.ItemQuery{}); err == nil {
		t.Fatal("disabled AI was called")
	}
}
