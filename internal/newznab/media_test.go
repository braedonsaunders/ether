package newznab

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestParseFeedStampsCategoryMedia(t *testing.T) {
	cases := []struct {
		category int
		want     core.MediaType
	}{
		{2000, core.TypeMovie},
		{2040, core.TypeMovie},
		{5000, core.TypeEpisode},
		{5040, core.TypeEpisode},
		{3020, ""},
		{0, ""},
	}
	for _, tt := range cases {
		body := []byte(`<rss><channel><item><title>X.2020.1080p</title><attr name="category" value="` +
			strconv.Itoa(tt.category) + `"/></item></channel></rss>`)
		rs, err := parseFeed(body, "fake")
		if err != nil || len(rs) != 1 {
			t.Fatalf("%d: parse: %v (%d)", tt.category, err, len(rs))
		}
		if rs[0].MediaType != tt.want {
			t.Errorf("category %d: media = %q, want %q", tt.category, rs[0].MediaType, tt.want)
		}
	}
}

func TestSearchStampsRequestMedia(t *testing.T) {
	f := newFakeIndexer(t, "")
	c := New(testConfig(f.server.URL), nil, Options{})
	movie, err := c.Search(context.Background(), core.SearchRequest{
		Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0133093"}, Title: "The Signal", Year: 1999,
	})
	if err != nil || len(movie) == 0 {
		t.Fatalf("movie search: %v (%d)", err, len(movie))
	}
	for _, r := range movie {
		if r.MediaType != core.TypeMovie {
			t.Fatalf("movie search release media = %q", r.MediaType)
		}
	}

	other := New(testConfig(f.server.URL), nil, Options{SearchCacheTTL: -1})
	tv, err := other.Search(context.Background(), core.SearchRequest{
		Type: core.TypeEpisode, IDs: core.IDs{TVDB: 81189}, Title: "The Signal", Season: 1, Episode: 1,
	})
	if err != nil || len(tv) == 0 {
		t.Fatalf("TV search: %v (%d)", err, len(tv))
	}
	for _, r := range tv {
		if r.MediaType != core.TypeEpisode {
			t.Fatalf("TV search release media = %q", r.MediaType)
		}
	}
}

func TestReleaseWithoutMediaTypeDecodesEmpty(t *testing.T) {
	var r core.Release
	if err := json.Unmarshal([]byte(`{"id":"x","name":"Old.Release.1080p","category":2040}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.MediaType != "" {
		t.Fatalf("media = %q, want empty for a stored record without the field", r.MediaType)
	}
	raw, err := json.Marshal(core.Release{ID: "y", MediaType: core.TypeEpisode})
	if err != nil {
		t.Fatal(err)
	}
	var back core.Release
	if err := json.Unmarshal(raw, &back); err != nil || back.MediaType != core.TypeEpisode {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if !strings.Contains(string(raw), `"mediaType":"episode"`) {
		t.Fatalf("media type not encoded: %s", raw)
	}
}
