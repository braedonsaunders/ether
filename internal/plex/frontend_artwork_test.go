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
	if err != nil || !complete {
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

func TestColdArtworkSelectionIsSharedAcrossSurfaces(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	var scheduled int
	source := FrontendArtwork{Server: f.srv, Variants: func(*core.Title, string) (*tmdb.Images, bool) { scheduled++; return nil, false }}
	var chosen frontend.Image
	for _, protocol := range []string{"jellyfin", "emby", "silo"} {
		req := frontend.ArtworkRequest{Viewer: &ownerUser, Device: core.Device{ID: protocol, Language: "en"}, Item: fightClub.Key}
		images, complete, err := source.List(context.Background(), req)
		if err != nil || !complete {
			t.Fatalf("%s cold list: %v", protocol, err)
		}
		var poster frontend.Image
		for _, img := range images {
			if img.Kind == frontend.ImagePoster {
				poster = img
				break
			}
		}
		if poster.Path == "" {
			t.Fatalf("%s cold poster missing", protocol)
		}
		if chosen.Path != "" && chosen != poster {
			t.Fatalf("%s selected a different poster", protocol)
		}
		chosen = poster
		data, _, err := source.Read(context.Background(), req, poster, frontend.ArtworkTransform{Width: 300})
		plex := f.get(t, "/library/metadata/"+fightClub.Key.String()+"/poster?width=300")
		if err != nil || !bytes.Equal(data, plex.Body.Bytes()) {
			t.Fatalf("%s differs from Plex: %v", protocol, err)
		}
	}
	if scheduled == 0 {
		t.Fatal("cold variants were not scheduled")
	}
}

func TestFrontendArtworkUsesGeneratedCollectionPoster(t *testing.T) {
	f := newFixture()
	f.srv.Close()
	key := core.SmartKey(12)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Owner Picks", Kind: "smart", Type: core.TypeMovie, ChildCount: 1}
	f.cols.items[key] = []core.Title{alien}
	d := f.srv.d
	d.Collections = &artCollections{fakeCollections: f.cols}
	f.srv = NewServer(d)
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: key}
	images, complete, err := source.List(context.Background(), req)
	if err != nil || !complete {
		t.Fatalf("collection list: %v", err)
	}
	var thumb frontend.Image
	for _, img := range images {
		if img.Kind == frontend.ImageThumb {
			thumb = img
			break
		}
	}
	if thumb.Path == "" {
		t.Fatal("generated collection thumb missing")
	}
	data, ct, err := source.Read(context.Background(), req, thumb, frontend.ArtworkTransform{Width: 400})
	if err != nil || ct != "image/jpeg" || string(data) != "GENERATED-owner" {
		t.Fatalf("generated image: %q %q %v", data, ct, err)
	}
}

func TestFrontendArtworkVariantsAreScopedAndValidated(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	source := FrontendArtwork{Server: f.srv, Variants: func(*core.Title, string) (*tmdb.Images, bool) {
		return &tmdb.Images{Posters: []tmdb.ImageFile{{FilePath: "/alternative.jpg"}, {FilePath: "https://example.com/image.jpg"}}}, true
	}}
	req := frontend.ArtworkRequest{Viewer: &ownerUser, Item: fightClub.Key}
	entries, complete, err := source.entries(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("fresh variants did not complete title artwork")
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
