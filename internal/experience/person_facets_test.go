package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPersonFacetIDsAuthorizeParentAndCredits(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, AllowedSections: []int{2}}
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow}
	episode := core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Cast: []core.Person{{TMDB: 10, Name: "Actor"}, {TMDB: 10, Name: "Actor"}, {TMDB: 99}}, Directors: []core.Person{{TMDB: 20, Name: "Director"}}, Writers: []core.Person{{TMDB: 30, Name: "Writer"}}, Crew: []core.Person{{TMDB: 40, Name: "Crew"}}}
	titles := map[core.RatingKey]*core.Title{show.Key: &show, episode.Key: &episode}
	reads := 0
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		reads++
		if t := titles[k]; t != nil {
			return t, nil
		}
		return nil, core.ErrNotFound
	}
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id != 1 {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}, Get: get, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(_ context.Context, k core.RatingKey) (*core.Title, error) { return titles[k], nil }}}}
	got, err := s.PersonFacetIDs(ctx, viewer, core.Device{}, episode.Key, []int64{10, 10, 20, 30, 40, 99, 88})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[10] != facetID("person", "10") || got[20] != facetID("person", "20") || got[30] != facetID("person", "30") || got[40] != facetID("person", "40") {
		t.Fatalf("credits: %v", got)
	}
	if reads != 1 {
		t.Fatalf("catalog calls %d", reads)
	}
	show.Adult = true
	if ids, err := s.PersonFacetIDs(ctx, viewer, core.Device{}, episode.Key, []int64{10}); !errors.Is(err, core.ErrNotFound) || ids != nil {
		t.Fatalf("inherited policy: %v %v", ids, err)
	}
	show.Adult = false
	if ids, err := s.PersonFacetIDs(ctx, &core.User{ID: 2}, core.Device{}, episode.Key, []int64{10}); !errors.Is(err, core.ErrNotFound) || ids != nil {
		t.Fatalf("inactive viewer: %v %v", ids, err)
	}
	if ids, err := s.PersonFacetIDs(ctx, viewer, core.Device{}, core.MovieKey(123), []int64{10}); !errors.Is(err, core.ErrNotFound) || ids != nil {
		t.Fatalf("missing title: %v %v", ids, err)
	}
	if _, err := s.PersonFacetIDs(ctx, viewer, core.Device{}, episode.Key, []int64{0}); err == nil {
		t.Fatal("zero ID accepted")
	}
	if _, err := s.PersonFacetIDs(ctx, viewer, core.Device{}, episode.Key, make([]int64, 257)); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}
