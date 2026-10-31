package experience

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSelectCinemaTrailersSkipsHiddenWatchedAndTheFeature(t *testing.T) {
	feature := core.MovieKey(1)
	trailer := func(key int64, kind string) frontend.Extra {
		return frontend.Extra{Key: core.ExtraKey(key), Kind: kind, Title: kind}
	}
	got := SelectCinemaTrailers(feature, []CinemaCandidate{
		{Title: core.Title{Key: feature, Type: core.TypeMovie}, Extras: []frontend.Extra{trailer(1, "trailer")}},
		{Title: core.Title{Key: core.MovieKey(2), Type: core.TypeMovie}, Watched: true, Extras: []frontend.Extra{trailer(2, "trailer")}},
		{Title: core.Title{Key: core.ShowKey(3), Type: core.TypeShow}, Extras: []frontend.Extra{trailer(3, "trailer")}},
		{Title: core.Title{Key: core.MovieKey(4), Type: core.TypeMovie}, Extras: []frontend.Extra{{Kind: "trailer"}, trailer(4, "teaser")}},
		{Title: core.Title{Key: core.MovieKey(5), Type: core.TypeMovie}, Extras: []frontend.Extra{trailer(5, "featurette"), trailer(6, "trailer")}},
	}, 2)
	if len(got) != 2 || got[0].Key != core.ExtraKey(4) || got[1].Key != core.ExtraKey(6) {
		t.Fatalf("trailers: %+v", got)
	}
	if SelectCinemaTrailers(feature, nil, 0) != nil {
		t.Fatal("limit zero still selected")
	}
}
