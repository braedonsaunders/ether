package plex

import (
	"bytes"
	"context"
	"errors"
	"image"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestFrontendArtworkUsesPlexSelectionAndRechecksVisibility(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	images, complete, err := source.List(context.Background(), req)
	if err != nil || complete {
		t.Fatalf("list: complete=%v err=%v", complete, err)
	}
	var poster frontend.Image
	for _, img := range images {
		if strings.Contains(img.Path, "poster550") {
			t.Fatal("source path leaked")
		}
		if img.Kind == frontend.ImagePoster {
			poster = img
		}
	}
	if poster.Path == "" {
		t.Fatal("poster missing")
	}
	data, ct, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{Width: 300})
	if err != nil || ct != "image/jpeg" {
		t.Fatalf("read: %s %v", ct, err)
	}
	plex := f.get(t, "/library/metadata/"+fightClub.Key.String()+"/poster?width=300")
	if plex.Code != 200 || !bytes.Equal(data, plex.Body.Bytes()) {
		t.Fatal("selected artwork differs from Plex")
	}
	restricted := friendUser
	restricted.MaxContentRating = "PG"
	req.Viewer = &restricted
	if _, _, err := source.List(context.Background(), req); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden item listed: %v", err)
	}
	if _, _, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden item read: %v", err)
	}
	req.Viewer = &ownerUser
	changed := fightClub
	changed.PosterPath = "/different.jpg"
	f.catalog.mu.Lock()
	f.catalog.titles[changed.Key] = changed
	f.catalog.mu.Unlock()
	if _, _, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("stale reference: %v", err)
	}
}

func TestFrontendArtworkVariantsAreScopedAndValidated(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv, Variants: func(*core.Title, string) (*tmdb.Images, bool) {
		return &tmdb.Images{Posters: []tmdb.ImageFile{{FilePath: "/alternative.jpg"}, {FilePath: "https://example.com/image.jpg"}}}, true
	}}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	entries, err := source.entries(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.remotePath == "" {
			continue
		}
		count++
		if entry.remotePath != "/alternative.jpg" || strings.Contains(entry.image.Path, "alternative") {
			t.Fatal("invalid variant reference")
		}
		if data, ct, err := source.Read(context.Background(), req, entry.image, frontend.ArtworkTransform{Width: 300}); err != nil || len(data) == 0 || ct != "image/jpeg" {
			t.Fatalf("variant: %s %v", ct, err)
		}
	}
	if count != 1 {
		t.Fatalf("valid variant count %d", count)
	}
}

func TestFrontendArtworkTransform(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	images, _, err := source.List(context.Background(), req)
	if err != nil || len(images) == 0 {
		t.Fatal(err)
	}
	data, ct, err := source.Read(context.Background(), req, images[0], frontend.ArtworkTransform{Width: 30, Height: 20, Format: "png", Blur: true})
	if err != nil || ct != "image/png" {
		t.Fatalf("transform: %q %v", ct, err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 30 || cfg.Height != 20 {
		t.Fatalf("dimensions: %+v %v", cfg, err)
	}
}
