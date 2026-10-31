package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestLibraryCollageUsesChildPostersAndChangesWithThem(t *testing.T) {
	ctx := context.Background()
	red := posterPNG(t, color.NRGBA{R: 220, A: 255})
	blue := posterPNG(t, color.NRGBA{B: 220, A: 255})
	green := posterPNG(t, color.NRGBA{G: 180, A: 255})
	images := map[string][]byte{"/w500/red.jpg": red, "/w500/blue.jpg": blue, "/w500/green.jpg": green}
	var fetches int
	cache := NewRemoteCache(1<<20, &http.Client{Transport: imageTransport(func(r *http.Request) (*http.Response, error) {
		fetches++
		body, ok := images[r.URL.Path]
		if !ok {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(bytes.NewReader(nil)), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})})
	source := Source{
		Images:   cache,
		ImageURL: func(path, size string) string { return "https://fixture.invalid/" + size + path },
		Collages: &LibraryCollageCache{Dir: t.TempDir()},
		Now:      time.Now,
	}
	viewer := &core.User{ID: 1}
	first := []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, PosterPath: "/red.jpg"},
		{Key: core.MovieKey(2), Type: core.TypeMovie, PosterPath: "/blue.jpg"},
	}
	view := "view-1"
	got, err := source.LibraryCollage(ctx, viewer, core.Device{}, view, first)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != libraryCollageSize || got.Height != libraryCollageSize || got.BlurHash == "" || got.Subject != view || got.Path != got.Tag || got.Kind != frontend.ImagePoster {
		t.Fatalf("collage = %+v", got)
	}
	body, err := source.readLibraryCollage(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != libraryCollageSize || decoded.Bounds().Dy() != libraryCollageSize {
		t.Fatalf("jpeg bounds %v", decoded.Bounds())
	}
	after := fetches
	again, err := source.LibraryCollage(ctx, viewer, core.Device{}, view, first)
	if err != nil || again.Path != got.Path || fetches != after {
		t.Fatalf("cache hit = %+v fetches %d->%d err %v", again, after, fetches, err)
	}
	changed, err := source.LibraryCollage(ctx, viewer, core.Device{}, view, []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, PosterPath: "/red.jpg"},
		{Key: core.MovieKey(2), Type: core.TypeMovie, PosterPath: "/green.jpg"},
	})
	if err != nil || changed.Path == got.Path {
		t.Fatalf("changed collage = %+v err %v", changed, err)
	}
	otherView, err := source.LibraryCollage(ctx, viewer, core.Device{}, "view-2", first)
	if err != nil || otherView.Path == got.Path || otherView.Subject != "view-2" {
		t.Fatalf("other view = %+v err %v", otherView, err)
	}
	if _, err = source.LibraryCollage(ctx, viewer, core.Device{}, view, []core.Title{{Key: core.MovieKey(3), Type: core.TypeMovie}}); err == nil {
		t.Fatal("missing posters produced a collage")
	}
}

func posterPNG(t *testing.T, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
