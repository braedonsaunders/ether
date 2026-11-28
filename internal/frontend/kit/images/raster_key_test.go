package images

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"
)

type keyRecorder struct{ keys map[string]int }

func (k *keyRecorder) Variant(ctx context.Context, data []byte, contentType, key string, render func(context.Context) ([]byte, string, error)) ([]byte, string, error) {
	k.keys[key]++
	return render(ctx)
}

// Fit boxes larger than the source render the same picture and must share
// one cached variant instead of one per requested size.
func TestRasterFitKeyClampsToRenderedSize(t *testing.T) {
	var src bytes.Buffer
	if err := png.Encode(&src, image.NewRGBA(image.Rect(0, 0, 200, 100))); err != nil {
		t.Fatal(err)
	}
	rec := &keyRecorder{keys: map[string]int{}}
	for _, w := range []int{300, 301, 999, 5000} {
		if _, _, err := RasterVariant(context.Background(), rec, src.Bytes(), "image/png", RasterTransform{Width: w, Height: 100, Fit: true, Format: "webp"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.keys) != 1 {
		t.Fatalf("variant keys = %v, want one shared key", rec.keys)
	}
	if _, _, err := RasterVariant(context.Background(), rec, src.Bytes(), "image/png", RasterTransform{Width: 100, Height: 100, Fit: true, Format: "webp"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.keys) != 2 {
		t.Fatalf("a smaller box must get its own variant: %v", rec.keys)
	}
}
