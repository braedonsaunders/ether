// Package images contains protocol-independent artwork helpers.
package images

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
)

var ErrDimensions = errors.New("invalid image dimensions")

// Tag changes whenever the input artwork bytes change.
func Tag(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:16]) }

// Resize returns a nearest-neighbour copy with explicit bounded dimensions.
func Resize(src image.Image, width, height int) (*image.NRGBA, error) {
	if src == nil || width <= 0 || height <= 0 || width > 4096 || height > 4096 || src.Bounds().Empty() {
		return nil, ErrDimensions
	}
	out := image.NewNRGBA(image.Rect(0, 0, width, height))
	b := src.Bounds()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			out.Set(x, y, src.At(b.Min.X+x*b.Dx()/width, b.Min.Y+y*b.Dy()/height))
		}
	}
	return out, nil
}

// Blur returns a 3x3 box blur, clamping edges to the source image.
func Blur(src image.Image) (*image.NRGBA, error) {
	if src == nil || src.Bounds().Empty() || src.Bounds().Dx() > 4096 || src.Bounds().Dy() > 4096 {
		return nil, ErrDimensions
	}
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			var rr, gg, bb, aa uint32
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					xx := max(0, min(b.Dx()-1, x+dx))
					yy := max(0, min(b.Dy()-1, y+dy))
					c := color.NRGBAModel.Convert(src.At(b.Min.X+xx, b.Min.Y+yy)).(color.NRGBA)
					rr += uint32(c.R)
					gg += uint32(c.G)
					bb += uint32(c.B)
					aa += uint32(c.A)
				}
			}
			out.SetNRGBA(x, y, color.NRGBA{uint8(rr / 9), uint8(gg / 9), uint8(bb / 9), uint8(aa / 9)})
		}
	}
	return out, nil
}
