package artwork

import (
	"errors"
	"image"
	"image/color"
	"math"
)

// ThumbHash encodes a bounded sample of img in the standard ThumbHash format.
// It returns the raw encoded bytes.
func ThumbHash(img image.Image) ([]byte, error) {
	if img == nil {
		return nil, errors.New("artwork: nil image")
	}
	bounds := img.Bounds()
	w0, h0 := bounds.Dx(), bounds.Dy()
	if w0 <= 0 || h0 <= 0 {
		return nil, errors.New("artwork: empty image")
	}
	factor := math.Min(1, 100/math.Max(float64(w0), float64(h0)))
	w, h := max(1, int(math.Round(float64(w0)*factor))), max(1, int(math.Round(float64(h0)*factor)))
	pixels := make([]color.NRGBA, w*h)
	var ar, ag, ab, aa float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := color.NRGBAModel.Convert(img.At(bounds.Min.X+x*w0/w, bounds.Min.Y+y*h0/h)).(color.NRGBA)
			pixels[y*w+x] = p
			a := float64(p.A) / 255
			aa += a
			ar += a * float64(p.R) / 255
			ag += a * float64(p.G) / 255
			ab += a * float64(p.B) / 255
		}
	}
	if aa > 0 {
		ar /= aa
		ag /= aa
		ab /= aa
	}
	alpha := aa < float64(w*h)
	limit := 7
	if alpha {
		limit = 5
	}
	longest := float64(max(w, h))
	lx := max(1, int(math.Round(float64(limit*w)/longest)))
	ly := max(1, int(math.Round(float64(limit*h)/longest)))
	channels := [4][]float64{}
	for i := range channels {
		channels[i] = make([]float64, w*h)
	}
	for i, p := range pixels {
		a := float64(p.A) / 255
		r := ar*(1-a) + a*float64(p.R)/255
		g := ag*(1-a) + a*float64(p.G)/255
		b := ab*(1-a) + a*float64(p.B)/255
		channels[0][i] = (r + g + b) / 3
		channels[1][i] = (r+g)/2 - b
		channels[2][i] = r - g
		channels[3][i] = a
	}
	type coeff struct {
		dc, scale float64
		ac        []float64
	}
	transform := func(data []float64, nx, ny int) coeff {
		var c coeff
		for cy := 0; cy < ny; cy++ {
			for cx := 0; cx*ny < nx*(ny-cy); cx++ {
				sum := 0.0
				for y := 0; y < h; y++ {
					fy := math.Cos(math.Pi * float64(cy) * (float64(y) + .5) / float64(h))
					for x := 0; x < w; x++ {
						sum += data[y*w+x] * fy * math.Cos(math.Pi*float64(cx)*(float64(x)+.5)/float64(w))
					}
				}
				sum /= float64(w * h)
				if cx == 0 && cy == 0 {
					c.dc = sum
				} else {
					c.ac = append(c.ac, sum)
					c.scale = math.Max(c.scale, math.Abs(sum))
				}
			}
		}
		if c.scale > 0 {
			for i, v := range c.ac {
				c.ac[i] = .5 + .5*v/c.scale
			}
		}
		return c
	}
	l := transform(channels[0], max(3, lx), max(3, ly))
	p := transform(channels[1], 3, 3)
	q := transform(channels[2], 3, 3)
	quant := func(v float64) int { return int(math.Floor(v + .5)) }
	head := quant(63*l.dc) | (quant(31.5+31.5*p.dc) << 6) | (quant(31.5+31.5*q.dc) << 12) | (quant(31*l.scale) << 18)
	if alpha {
		head |= 1 << 23
	}
	count := lx
	landscape := w > h
	if landscape {
		count = ly
	}
	head2 := count | (quant(63*p.scale) << 3) | (quant(63*q.scale) << 9)
	if landscape {
		head2 |= 1 << 15
	}
	result := []byte{byte(head), byte(head >> 8), byte(head >> 16), byte(head2), byte(head2 >> 8)}
	sets := [][]float64{l.ac, p.ac, q.ac}
	if alpha {
		a := transform(channels[3], 5, 5)
		result = append(result, byte(quant(15*a.dc)|(quant(15*a.scale)<<4)))
		sets = append(sets, a.ac)
	}
	index := 0
	for _, values := range sets {
		for _, v := range values {
			if index%2 == 0 {
				result = append(result, 0)
			}
			result[len(result)-1] |= byte(quant(15*v)) << uint(4*(index%2))
			index++
		}
	}
	return result, nil
}
