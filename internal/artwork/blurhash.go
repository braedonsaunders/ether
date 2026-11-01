package artwork

import (
	"errors"
	"image"
	"math"
)

const blurAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$%*+,-.:;=?@[]^_{|}~"

// BlurHash returns a standard BlurHash for img. The input is sampled on a
// bounded grid so computation remains cheap even for a large source image.
func BlurHash(img image.Image, componentsX, componentsY int) (string, error) {
	if img == nil || componentsX < 1 || componentsX > 9 || componentsY < 1 || componentsY > 9 || img.Bounds().Empty() {
		return "", errors.New("artwork: invalid blurhash input")
	}
	bounds := img.Bounds()
	w, h := min(bounds.Dx(), 32), min(bounds.Dy(), 32)
	type rgb struct{ r, g, b float64 }
	pixels := make([]rgb, w*h)
	linear := func(c uint32) float64 {
		v := float64(c) / 65535
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cx := bounds.Min.X + x*bounds.Dx()/w
			cy := bounds.Min.Y + y*bounds.Dy()/h
			r, g, b, _ := img.At(cx, cy).RGBA()
			pixels[y*w+x] = rgb{linear(r), linear(g), linear(b)}
		}
	}
	coefficients := make([]rgb, 0, componentsX*componentsY)
	maximum := 0.0
	for j := 0; j < componentsY; j++ {
		for i := 0; i < componentsX; i++ {
			var c rgb
			norm := 2.0
			if i == 0 && j == 0 {
				norm = 1
			}
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					basis := norm * math.Cos(math.Pi*float64(i*x)/float64(w)) * math.Cos(math.Pi*float64(j*y)/float64(h))
					p := pixels[y*w+x]
					c.r += basis * p.r
					c.g += basis * p.g
					c.b += basis * p.b
				}
			}
			scale := 1 / float64(w*h)
			c.r *= scale
			c.g *= scale
			c.b *= scale
			coefficients = append(coefficients, c)
			if i != 0 || j != 0 {
				maximum = math.Max(maximum, math.Max(math.Abs(c.r), math.Max(math.Abs(c.g), math.Abs(c.b))))
			}
		}
	}
	encode := func(value, n int) string {
		out := make([]byte, n)
		for i := n - 1; i >= 0; i-- {
			out[i] = blurAlphabet[value%83]
			value /= 83
		}
		return string(out)
	}
	linearByte := func(v float64) int {
		v = math.Max(0, math.Min(1, v))
		if v <= 0.0031308 {
			v *= 12.92
		} else {
			v = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		return int(v*255 + 0.5)
	}
	quantized := 0
	if len(coefficients) > 1 {
		quantized = max(0, min(82, int(math.Floor(maximum*166-0.5))))
	}
	maxValue := float64(quantized+1) / 166
	dc := coefficients[0]
	result := encode((componentsX-1)+(componentsY-1)*9, 1) + encode(quantized, 1) + encode((linearByte(dc.r)<<16)+(linearByte(dc.g)<<8)+linearByte(dc.b), 4)
	quant := func(v float64) int {
		v /= maxValue
		if v < 0 {
			v = -math.Sqrt(-v)
		} else {
			v = math.Sqrt(v)
		}
		return max(0, min(18, int(math.Floor(v*9+9.5))))
	}
	for _, c := range coefficients[1:] {
		result += encode(quant(c.r)*19*19+quant(c.g)*19+quant(c.b), 2)
	}
	return result, nil
}
