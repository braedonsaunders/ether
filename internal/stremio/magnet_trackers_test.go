package stremio

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestConvertPreservesOriginalMagnetTrackers(t *testing.T) {
	tracker := "udp://tracker.example:1337/announce"
	magnet := "magnet:?xt=urn:btih:" + hashA + "&dn=Movie&tr=" + url.QueryEscape(tracker)
	want := "tr=" + url.QueryEscape(tracker)
	for _, tc := range []struct {
		name string
		s    stream
	}{
		{"URL magnet", stream{Title: "Movie 2014 1080p BluRay x264-GRP", URL: magnet}},
		{"query magnet", stream{Title: "Movie 2014 1080p BluRay x264-GRP", URL: "https://addon.test/play?magnet=" + url.QueryEscape(magnet)}},
		{"sources magnet", stream{Title: "Movie 2014 1080p BluRay x264-GRP", Sources: []string{magnet}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rels, skipped, ambiguous := convert([]stream{tc.s}, core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}, false)
			if skipped != 0 || ambiguous != 0 || len(rels) != 1 {
				t.Fatalf("rels=%+v skipped=%d", rels, skipped)
			}
			if !strings.Contains(rels[0].MagnetURL, want) {
				t.Fatalf("MagnetURL %q não preserva tr= do magnet original (%q)", rels[0].MagnetURL, want)
			}
		})
	}
}

// skippedNoHash conta todos os streams sem hash, inclusive os além do
// corte maxResults — sem mudar quais releases saem (os primeiros
// maxResults válidos).
func TestConvertCountsSkippedNoHashBeyondMaxResults(t *testing.T) {
	streams := make([]stream, 0, maxResults+3)
	for i := 0; i < maxResults; i++ {
		streams = append(streams, stream{
			Title:    "Movie 2014 1080p BluRay x264-GRP",
			InfoHash: fmt.Sprintf("%040x", i+1),
		})
	}
	for i := 0; i < 3; i++ {
		streams = append(streams, stream{
			Title: "Movie 2014 720p BluRay x264-GRP",
			URL:   fmt.Sprintf("stremio:///detail/%d", i),
		})
	}
	rels, skipped, ambiguous := convert(streams, core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}, false)
	if len(rels) != maxResults {
		t.Fatalf("len(rels)=%d want %d (corte inalterado)", len(rels), maxResults)
	}
	if skipped != 3 || ambiguous != 0 {
		t.Fatalf("skippedNoHash=%d skippedAmbiguous=%d want 3/0 (conta também além do corte)", skipped, ambiguous)
	}
}
