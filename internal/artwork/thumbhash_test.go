package artwork

import (
	"encoding/base64"
	"image"
	"image/color"
	"testing"
)

func TestThumbHashReferenceVectors(t *testing.T) {
	cases := []struct {
		w, h   int
		pixels []color.NRGBA
		want   string
	}{
		{1, 1, []color.NRGBA{{R: 255, A: 255}}, "1fsrB38I9wiIh4hwj3CI+AiIgIAICIgA"},
		{2, 1, []color.NRGBA{{R: 255, A: 255}, {B: 255, A: 128}}, "1Uers9S7eAiIiICPiKiICIkGiIhgePg="},
		{1, 1, []color.NRGBA{{}}, "AAiCBQAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="},
	}
	for _, tc := range cases {
		img := image.NewNRGBA(image.Rect(0, 0, tc.w, tc.h))
		for i, p := range tc.pixels {
			img.SetNRGBA(i%tc.w, i/tc.w, p)
		}
		got, err := ThumbHash(img)
		if err != nil || base64.StdEncoding.EncodeToString(got) != tc.want {
			t.Fatalf("%dx%d: %s %v", tc.w, tc.h, base64.StdEncoding.EncodeToString(got), err)
		}
	}
}
func TestThumbHashRejectsEmptyAndBoundsSampling(t *testing.T) {
	if _, err := ThumbHash(nil); err == nil {
		t.Fatal("nil accepted")
	}
	if _, err := ThumbHash(image.NewNRGBA(image.Rect(0, 0, 0, 0))); err == nil {
		t.Fatal("empty accepted")
	}
	img := image.NewNRGBA(image.Rect(5, 10, 1205, 210))
	for y := 10; y < 210; y++ {
		for x := 5; x < 1205; x++ {
			img.SetNRGBA(x, y, color.NRGBA{G: 200, A: 255})
		}
	}
	hash, err := ThumbHash(img)
	if err != nil || len(hash) < 5 || len(hash) > 100 {
		t.Fatalf("large image: %d %v", len(hash), err)
	}
}
