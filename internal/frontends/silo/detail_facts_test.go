package silo

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSiloRatingsShowNamedSourcesAndNothingWhenAbsent(t *testing.T) {
	out := map[string]any{}
	applySiloRatings(out, frontend.Title{Rating: 7.4}, frontend.DetailFacts{RatingsKnown: true, Sources: []frontend.ExternalRating{
		{Source: "imdb", Name: "IMDb", Score: 82, Display: "8.2"},
		{Source: "rt_critic", Name: "Rotten Tomatoes", Score: 94, Display: "94%"},
		{Source: "letterboxd", Name: "Letterboxd", Score: 82, Display: "4.1"},
	}})
	if out["rating_imdb"] != 8.2 || out["rating_rt_critic"] != 94 {
		t.Fatalf("scalars %#v", out)
	}
	ratings, _ := out["ratings"].([]any)
	if len(ratings) != 4 {
		t.Fatalf("ratings %#v", out["ratings"])
	}
	last, _ := ratings[3].(map[string]any)
	if last["source"] != "tmdb" {
		t.Fatalf("catalogue tmdb %#v", last)
	}

	empty := map[string]any{"ratings": []any{map[string]string{"source": "tmdb"}}, "rating_sources": []any{"tmdb"}}
	applySiloRatings(empty, frontend.Title{}, frontend.DetailFacts{RatingsKnown: true})
	if ratings, _ := empty["ratings"].([]any); len(ratings) != 0 {
		t.Fatalf("absent ratings %#v", empty)
	}
	if _, ok := empty["rating_sources"]; ok {
		t.Fatal("rating_sources kept")
	}
	if _, ok := empty["rating_imdb"]; ok {
		t.Fatal("imdb invented")
	}
}
