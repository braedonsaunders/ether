package artwork

import (
	"bytes"
	"context"
	"errors"
	"image"
	"strings"
	"testing"
	"time"
)

func TestSVGRasterizerBoundsAndCache(t *testing.T) {
	r := NewSVGRasterizer()
	valid := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="#ff0000"/></svg>`)
	opts := RasterOptions{Width: 20, Height: 10, Format: "png", Blur: true}
	data, ct, err := r.Rasterize(context.Background(), "poster", valid, opts)
	if err != nil || ct != "image/png" {
		t.Fatal(ct, err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil || decoded.Bounds().Dx() != 20 || decoded.Bounds().Dy() != 10 {
		t.Fatal(decoded, err)
	}
	r.mu.Lock()
	count := r.cache.Len()
	r.mu.Unlock()
	if count != 1 {
		t.Fatal("raster not cached", count)
	}
	if _, _, err := r.Rasterize(context.Background(), "poster", valid, opts); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	count = r.cache.Len()
	r.mu.Unlock()
	if count != 1 {
		t.Fatal("cache not reused", count)
	}
	for _, invalid := range [][]byte{
		[]byte(`<svg><image href="http://127.0.0.1/"/></svg>`),
		[]byte(`<svg><path fill="url(http://example.com/a)" d="M0 0"/></svg>`),
		[]byte(`<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg>&x;</svg>`),
		[]byte(`<svg><use href="#self"/></svg>`),
		[]byte(`<svg viewBox="0 0 10 10"><path d="M1e100 0"/></svg>`),
		[]byte(`<svg>` + strings.Repeat(`<path d="M0 0"/>`, 257) + `</svg>`),
		[]byte(`<svg viewBox="0 0 10 10">` + strings.Repeat(`<rect width="1" height="1"/>`, 257) + `</svg>`),
		bytes.Repeat([]byte("x"), maxSVGBytes+1),
	} {
		if _, _, err := r.Rasterize(context.Background(), "bad", invalid, opts); !errors.Is(err, ErrSVG) {
			t.Fatalf("unsafe SVG accepted: %v", err)
		}
	}
	if _, _, err := r.Rasterize(context.Background(), "oversize", valid, RasterOptions{Width: 2049}); !errors.Is(err, ErrSVG) {
		t.Fatal("oversize accepted", err)
	}
}

func TestSVGRasterTimeoutRetainsConcurrencySlotUntilWorkEnds(t *testing.T) {
	r := NewSVGRasterizer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, _, err := r.execute(ctx, "blocked", func(context.Context) (image.Image, error) {
			close(started)
			<-release
			return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
		}, RasterOptions{Format: "png"})
		finished <- err
	}()
	<-started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal("timeout", err)
	}
	if len(r.slots) != 1 {
		t.Fatal("timed out work lost its concurrency slot")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for len(r.slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(r.slots) != 0 {
		t.Fatal("completed work retained its slot")
	}
}

func TestGeneratedChannelRasterUsesSourceFields(t *testing.T) {
	r := NewSVGRasterizer()
	data, ct, err := r.ChannelLogo(context.Background(), "channel", "9", "News", RasterOptions{Width: 320, Format: "png"})
	if err != nil || ct != "image/png" {
		t.Fatal(ct, err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != 320 || img.Bounds().Dy() != 180 {
		t.Fatal(img, err)
	}
	background := img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2)
	found := false
	for y := 10; y < img.Bounds().Dy()-10 && !found; y++ {
		for x := 10; x < img.Bounds().Dx()-10; x++ {
			if img.At(x, y) != background {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("channel text was not rendered")
	}
}

func FuzzSVGRasterize(f *testing.F) {
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><rect width="8" height="8"/></svg>`))
	f.Add([]byte(`<svg><path d="M 0 0 L 4 4"/></svg>`))
	r := NewSVGRasterizer()
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		_, _, _ = r.Rasterize(context.Background(), "fuzz", data, RasterOptions{Width: 16, Height: 16, Format: "png"})
	})
}
