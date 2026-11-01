package mediabrowser

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type factExperience struct {
	frontendtest.Experience
	facts frontend.DetailFacts
	page  map[frontend.RatingKey]frontend.DetailFacts
}

func (f factExperience) DetailFacts(context.Context, *frontend.Viewer, frontend.Title) frontend.DetailFacts {
	return f.facts
}

func (f factExperience) PageRatings(context.Context, *frontend.Viewer, []frontend.Title) map[frontend.RatingKey]frontend.DetailFacts {
	return f.page
}

func TestOpenedDetailPlacesHeaderOnceAndOmitsMissingRatings(t *testing.T) {
	community := 8.2
	critic := 94.0
	exp := factExperience{facts: frontend.DetailFacts{
		Header: "96% Match · Directed by Denis Villeneuve", RatingsKnown: true,
		Community: &community, Critic: &critic,
	}}
	i := &instance{host: frontend.Host{Experience: exp}}
	item := map[string]any{"Taglines": []string{"Fear is the mind-killer."}, "Overview": "A linguist.", "CommunityRating": 7.1}
	req := httptest.NewRequest("GET", "/Items/1", nil)
	title := frontend.Title{Key: 1, Type: "movie", Title: "Arrival"}
	i.applyOpenedDetail(req, &frontend.Viewer{ID: 1}, item, title)
	lines, _ := item["Taglines"].([]string)
	if len(lines) != 1 || lines[0] != "Fear is the mind-killer." {
		t.Fatalf("tagline %v", item["Taglines"])
	}
	if item["Overview"] != "96% Match · Directed by Denis Villeneuve\n\nA linguist." {
		t.Fatalf("overview %v", item["Overview"])
	}
	if item["CommunityRating"] != 8.2 || item["CriticRating"] != 94.0 {
		t.Fatalf("ratings %#v", item)
	}

	exp.facts = frontend.DetailFacts{}
	bare := map[string]any{"Overview": "A linguist.", "CommunityRating": 7.1}
	i.host.Experience = exp
	i.applyOpenedDetail(req, &frontend.Viewer{ID: 1}, bare, title)
	if _, ok := bare["Taglines"]; ok || bare["Overview"] != "A linguist." || bare["CommunityRating"] != 7.1 {
		t.Fatalf("thin model invented a match: %#v", bare)
	}

	exp.facts = frontend.DetailFacts{RatingsKnown: true}
	i.host.Experience = exp
	known := map[string]any{"CommunityRating": 7.1, "CriticRating": 50.0}
	i.applyOpenedDetail(req, &frontend.Viewer{ID: 1}, known, title)
	if _, ok := known["CommunityRating"]; ok {
		t.Fatalf("missing audience still shown: %#v", known)
	}
	if _, ok := known["CriticRating"]; ok {
		t.Fatalf("missing critic still shown: %#v", known)
	}
}

func TestListedRatingsReplaceCatalogueScore(t *testing.T) {
	community := 8.2
	key := frontend.RatingKey(9)
	exp := factExperience{page: map[frontend.RatingKey]frontend.DetailFacts{
		key: {RatingsKnown: true, Community: &community},
	}}
	i := &instance{host: frontend.Host{Experience: exp}}
	item := map[string]any{"CommunityRating": 6.0}
	i.applyListedRatings(item, exp.page, key)
	if item["CommunityRating"] != 8.2 {
		t.Fatalf("community %#v", item)
	}
	if _, ok := item["CriticRating"]; ok {
		t.Fatal("critic invented")
	}
}
