package artwork

import (
	"image"
	"image/color"
	"testing"
)

func TestBlurHashSolidRedReference(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	got, err := BlurHash(img, 1, 1)
	if err != nil || got != "00TI:j" {
		t.Fatalf("red reference: %q %v", got, err)
	}
	full, err := BlurHash(img, 4, 3)
	if err != nil || len(full) != 28 {
		t.Fatalf("unexpected hash: %q %v", full, err)
	}
	if _, err = BlurHash(img, 0, 1); err == nil {
		t.Fatal("invalid components accepted")
	}
}
