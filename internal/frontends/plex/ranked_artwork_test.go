package plex

import (
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"strings"
	"testing"
)

func TestRankedHubNativeTypesAndIsolation(t *testing.T) {
	source := model.Metadata{Type: "movie", RatingKey: "123", Key: "/library/metadata/123", Thumb: "/original/poster", Art: "/original/art", Image: []model.Image{{Type: "coverPoster", URL: "/original/poster"}}}
	rows := stableHomeHubs([]model.Hub{{Type: "movie", HubIdentifier: "ranked", RankedTop10: true, Metadata: []model.Metadata{source}}})
	if len(rows) != 1 || rows[0].Meta == nil || rows[0].Meta.DisplayImage[0].ImageType != "coverArt" {
		t.Fatal(rows)
	}
	got := rows[0].Metadata[0]
	if got.Type != source.Type || got.Key != source.Key || got.Art != source.Art || !strings.Contains(got.Thumb, "ranked=1") {
		t.Fatal(got)
	}
	if source.Image[0].URL != "/original/poster" {
		t.Fatal("mutated ordinary art")
	}
	found := false
	for _, img := range got.Image {
		if img.Type == "coverArt" && strings.Contains(img.URL, "rankshape=wide") {
			found = true
		}
	}
	if !found {
		t.Fatal("no wide image")
	}
}
