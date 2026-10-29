package plex

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
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
