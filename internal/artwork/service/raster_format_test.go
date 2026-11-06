package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"
)

func formatSource(t *testing.T, w, h int, col color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, col)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRasterFormatReusesVariantsAndCoalesces(t *testing.T) {
	c := NewRemoteCache(8<<20, nil)
	now := time.Now()
	ctx := context.Background()
	data := formatSource(t, 300, 450, color.NRGBA{R: 200, A: 255})
	const count = 16
	var got [count][]byte
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, ct, err := c.RasterFormat(ctx, data, "image/png", "jpeg", now)
			if err != nil || ct != "image/jpeg" {
				t.Errorf("format: %s %v", ct, err)
				return
			}
			got[i] = out
		}(i)
	}
	close(start)
	wg.Wait()
	for _, out := range got {
		if len(out) == 0 || &out[0] != &got[0][0] {
			t.Fatal("concurrent requests did not reuse one encoded variant")
		}
	}
	// Content identity, rather than the caller's buffer address, keys the cache.
	out, _, err := c.RasterFormat(ctx, append([]byte(nil), data...), "image/png", "jpeg", now)
	if err != nil || &out[0] != &got[0][0] {
		t.Fatal("repeat re-encoded")
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || kind != "jpeg" || cfg.Width != 300 || cfg.Height != 450 {
		t.Fatal(cfg, kind, err)
	}
	if c.items.Len() != 1 {
		t.Fatal("duplicate variants")
	}

	changed := formatSource(t, 300, 450, color.NRGBA{G: 200, A: 255})
	fresh, _, err := c.RasterFormat(ctx, changed, "image/png", "jpeg", now)
	if err != nil || bytes.Equal(out, fresh) {
		t.Fatal("changed source reused stale conversion")
	}
	resized := formatSource(t, 100, 150, color.NRGBA{R: 200, A: 255})
	small, _, err := c.RasterFormat(ctx, resized, "image/png", "jpeg", now)
	cfg, _, decodeErr := image.DecodeConfig(bytes.NewReader(small))
	if err != nil || decodeErr != nil || cfg.Width != 100 || cfg.Height != 150 {
		t.Fatal("wrong size", cfg, err, decodeErr)
	}
	asPNG, ct, err := c.RasterFormat(ctx, out, "image/jpeg", "png", now)
	if err != nil || ct != "image/png" {
		t.Fatal(ct, err)
	}
	if _, kind, err := image.DecodeConfig(bytes.NewReader(asPNG)); err != nil || kind != "png" {
		t.Fatal(kind, err)
	}
	expired, _, err := c.RasterFormat(ctx, data, "image/png", "jpeg", now.Add(imageTTL+time.Second))
	if err != nil || &expired[0] == &out[0] {
		t.Fatal("expired variant retained")
	}
}

func TestRasterFormatSharesByteBudgetAndDoesNotCacheErrors(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	source := formatSource(t, 20, 30, color.NRGBA{B: 200, A: 255})
	encoded, _, err := encodeRasterFormat(source, "jpeg")
	if err != nil {
		t.Fatal(err)
	}
	c := NewRemoteCache(int64(len(encoded)), nil)
	c.mu.Lock()
	c.addLocked(&RemoteImage{URL: "https://source.invalid/image", Data: source, FetchedAt: now})
	c.mu.Unlock()
	out, _, err := c.RasterFormat(ctx, source, "image/png", "jpeg", now)
	if err != nil || !bytes.Equal(encoded, out) || c.items.Len() != 1 || c.items.Contains("https://source.invalid/image") {
		t.Fatal("raw and converted variants do not share eviction budget", err)
	}
	if _, _, err := c.RasterFormat(ctx, []byte("invalid"), "image/png", "jpeg", now); err == nil || c.items.Len() != 1 {
		t.Fatal("failed conversion cached")
	}
	pass, ct, err := c.RasterFormat(ctx, source, "image/png", "png", now)
	if err != nil || ct != "image/png" || &pass[0] != &source[0] {
		t.Fatal("matching format was converted")
	}
}
