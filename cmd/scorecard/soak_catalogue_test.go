package main

import (
	"encoding/json"
	"testing"
)

func TestSynthesizeCatalogue(t *testing.T) {
	raw, err := synthesizeCatalogue([]catalogFile{
		{TMDB: 9761, Path: "/media/Movies/Elephants Dream (2006)/feature.mov", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{TMDB: 91461, Season: 1, Episode: 1, Path: "/media/TV/The Scene (2004)/Season 01/The Scene S01E01.mp4", SHA256: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"},
		{TMDB: 91461, Season: 1, Episode: 2, Path: "/media/TV/The Scene (2004)/Season 01/The Scene S01E02.mp4", SHA256: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543211"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var items []catalogItem
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Type]++
		if item.Type == "Episode" && (item.SeriesId != "show-91461" || item.ProviderIds["Tmdb"] != "91461" || item.Path == "") {
			t.Fatalf("episode: %+v", item)
		}
		if item.Type == "Movie" && (item.ProductionYear != 2006 || item.Path == "") {
			t.Fatalf("movie: %+v", item)
		}
		if item.Type == "Series" && item.Name != "The Scene" {
			t.Fatalf("series name: %+v", item)
		}
	}
	if counts["Movie"] != 1 || counts["Series"] != 1 || counts["Season"] != 1 || counts["Episode"] != 2 {
		t.Fatalf("counts: %+v", counts)
	}
}
