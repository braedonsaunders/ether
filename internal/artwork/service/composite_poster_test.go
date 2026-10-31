package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/store"
)

type storedTitle struct{ title *core.Title }

func (s storedTitle) StoredTitle(context.Context, core.RatingKey) (*core.Title, error) {
	if s.title == nil {
		return nil, core.ErrNotFound
	}
	return s.title, nil
}

func TestReadReturnsCompositedPosterBytes(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	img := image.NewRGBA(image.Rect(0, 0, 200, 300))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{20, 30, 40, 255}), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err = jpeg.Encode(&raw, img, nil); err != nil {
		t.Fatal(err)
	}
	images := &jpegImages{data: raw.Bytes()}
	over := overlays.New(db, overlays.Options{Images: images, Facts: func(context.Context, *core.User, *core.Title) overlays.Facts {
		return overlays.Facts{NewSeason: true}
	}})
	if err = over.SaveSettings(ctx, overlays.Settings{Enabled: true, Status: true}); err != nil {
		t.Fatal(err)
	}
	title := &core.Title{Key: core.MovieKey(9), Type: core.TypeMovie, Title: "Arrival", PosterPath: "/default.jpg", Released: time.Date(2016, 11, 11, 0, 0, 0, 0, time.UTC)}
	src := Source{Catalog: storedTitle{title: title}, Discovery: &discovery.Features{Overlays: over}, Now: func() time.Time { return time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC) }}
	viewer := &core.User{ID: 1}
	imagesListed, _, err := src.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Item: title.Key})
	if err != nil {
		t.Fatal(err)
	}
	var poster frontend.Image
	for _, image := range imagesListed {
		if image.Kind == frontend.ImagePoster {
			poster = image
		}
	}
	if poster.Tag == "" {
		t.Fatal("poster was not selected")
	}
	data, contentType, err := src.Read(ctx, frontend.ArtworkRequest{Viewer: viewer, Item: title.Key}, poster, frontend.ArtworkTransform{})
	if err != nil || contentType != "image/jpeg" || bytes.Equal(data, raw.Bytes()) {
		t.Fatalf("composited poster missing ct=%s err=%v equal=%v", contentType, err, bytes.Equal(data, raw.Bytes()))
	}
}

type jpegImages struct{ data []byte }

func (j *jpegImages) Get(context.Context, string, int) ([]byte, string, error) {
	return j.data, "image/jpeg", nil
}
