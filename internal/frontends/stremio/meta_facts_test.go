package stremio

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type descFacts struct {
	frontendtest.Experience
	facts frontend.DetailFacts
}

func (d descFacts) DetailFacts(context.Context, *frontend.Viewer, frontend.Title) frontend.DetailFacts {
	return d.facts
}

func (d descFacts) PageRatings(context.Context, *frontend.Viewer, []frontend.Title) map[frontend.RatingKey]frontend.DetailFacts {
	return nil
}

func TestStremioDescriptionPrefixesHeaderAndNamesRatings(t *testing.T) {
	exp := descFacts{facts: frontend.DetailFacts{
		Header: "96% Match", RatingsKnown: true,
		Sources: []frontend.ExternalRating{{Source: "imdb", Name: "IMDb", Display: "8.2", Score: 82}},
	}}
	got := stremioDescription(httptest.NewRequest("GET", "/meta", nil), exp, &frontend.Viewer{ID: 1}, frontend.Title{Summary: "A linguist."})
	want := "96% Match\n\nA linguist.\n\nIMDb 8.2"
	if got != want {
		t.Fatalf("description %q", got)
	}
	exp.facts = frontend.DetailFacts{}
	plain := stremioDescription(httptest.NewRequest("GET", "/meta", nil), exp, &frontend.Viewer{ID: 1}, frontend.Title{Summary: "A linguist."})
	if plain != "A linguist." {
		t.Fatalf("thin model %q", plain)
	}
}
