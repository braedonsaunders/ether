package collections

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
)

// Unauthenticated poster requests choose the width; every distinct width
// must not become its own render and cache file.
func TestCollectionPosterWidthsShareRenders(t *testing.T) {
	dir := t.TempDir()
	s := &Service{log: slog.Default(), now: time.Now, idx: &index{}, art: artwork.NewRenderer(nil, dir, slog.Default())}
	row := &specRow{id: 1, spec: core.CollectionSpec{Name: "Widths", MediaType: MediaMovie, Artwork: core.CollectionArtwork{Mode: "auto"}}}
	for w := 301; w <= 340; w++ {
		if _, _, _, err := s.artworkFor(context.Background(), row, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("40 nearby widths produced %d cached posters, want 1", len(files))
	}
	for _, c := range []struct{ in, want int }{{0, 0}, {-5, 0}, {1, 150}, {150, 150}, {151, 300}, {601, 900}, {5000, 1200}} {
		if got := posterWidth(c.in); got != c.want {
			t.Fatalf("posterWidth(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCustomCollectionArtworkAppliesToArtAndBackdrop(t *testing.T) {
	c := (&Service{idx: &index{}}).collectionFor(context.Background(), &specRow{
		id:   7,
		spec: core.CollectionSpec{Name: "Custom", MediaType: MediaMovie, Artwork: core.CollectionArtwork{Mode: "custom", URL: "https://example.com/custom.jpg"}},
	}, nil, core.TypeMovie, []core.RatingKey{core.MovieKey(11)})
	if c.PosterPath != "https://example.com/custom.jpg" || c.BackdropPath != "https://example.com/custom.jpg" {
		t.Fatalf("custom collection paths: poster=%q backdrop=%q", c.PosterPath, c.BackdropPath)
	}
}
