package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestLocalEpisodesReadsAShowOncePerRequest(t *testing.T) {
	viewer := &core.User{ID: 1}
	calls := 0
	first := core.Title{Key: core.EpisodeKey(9, 1, 1), Type: core.TypeEpisode, Season: 1}
	second := core.Title{Key: core.EpisodeKey(9, 2, 1), Type: core.TypeEpisode, Season: 2}
	s := &Service{d: ServiceDeps{
		Active:     func(context.Context, int64) (*core.User, error) { return viewer, nil },
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }},
		AiredEpisodes: func(context.Context, core.RatingKey, int, int) (core.Page, error) {
			calls++
			return core.Page{Items: []core.Title{first, second}, Total: 2}, nil
		},
	}}
	ctx := frontend.WithRequestMemo(context.Background())
	seasonOne, complete, err := s.LocalEpisodes(ctx, viewer, core.SeasonKey(9, 1))
	seasonTwo, completeTwo, errTwo := s.LocalEpisodes(ctx, viewer, core.SeasonKey(9, 2))
	if err != nil || errTwo != nil || !complete || !completeTwo || calls != 1 {
		t.Fatalf("calls=%d complete=%v %v err=%v %v", calls, complete, completeTwo, err, errTwo)
	}
	if len(seasonOne) != 1 || seasonOne[0].Season != 1 || len(seasonTwo) != 1 || seasonTwo[0].Season != 2 {
		t.Fatalf("seasons %#v %#v", seasonOne, seasonTwo)
	}
}
