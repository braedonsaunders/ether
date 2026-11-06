// Package raster renders scene v1 with no I/O, global mutable state or frame goroutines.
package raster

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/onair/scene"
	"github.com/braedonsaunders/ether/internal/onair/signal"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/vector"
)

const Version = "onair-raster-4-static-detail-175"

// Bindings keeps decoded artwork outside the document; images are immutable during a job.
type Bindings struct {
	scene.Bindings
	Assets map[string]image.Image    `json:"-"`
	Fonts  map[string]*opentype.Font `json:"-"`
}
type matrix struct{ a, b, c, d, e, f float64 }

func (m matrix) point(p point) point { return point{m.a*p.x + m.c*p.y + m.e, m.b*p.x + m.d*p.y + m.f} }
func (m matrix) mul(n matrix) matrix {
	return matrix{m.a*n.a + m.c*n.b, m.b*n.a + m.d*n.b, m.a*n.c + m.c*n.d, m.b*n.c + m.d*n.d, m.a*n.e + m.c*n.f + m.e, m.b*n.e + m.d*n.f + m.f}
}
func (m matrix) inverse() matrix {
	det := m.a*m.d - m.b*m.c
	if math.Abs(det) < 1e-12 {
		return matrix{}
	}
	return matrix{m.d / det, -m.b / det, -m.c / det, m.a / det, (m.c*m.f - m.d*m.e) / det, (m.b*m.e - m.a*m.f) / det}
}

type render struct {
	b      Bindings
	t      float64
	w, h   int
	parsed map[string]*opentype.Font
}

func Render(doc scene.Document, bindings Bindings, t float64, w, h int) *image.RGBA {
	if w < 1 || h < 1 || w > 4096 || h > 2160 {
		return image.NewRGBA(image.Rectangle{})
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if doc.Validate() != nil {
		return dst
	}
	r := render{bindings, t, w, h, map[string]*opentype.Font{}}
	r.layers(dst, doc.Layers, matrix{a: float64(w) / 1920, d: float64(h) / 1080}, 1, nil)
	r.post(dst, doc.Post)
	return dst
}
func (r *render) n(v any, def float64) float64 {
	return scene.Number(scene.Eval(v, r.b.Bindings, r.t), def)
}
func (r *render) s(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(scene.Eval(v, r.b.Bindings, r.t))
}
func random(seed uint32, i int) float64 {
	x := (seed ^ uint32(i+1)*0x9e3779b1) * 0x85ebca6b
	x ^= x >> 13
	x *= 0xc2b2ae35
	x ^= x >> 16
	return float64(x) / 4294967296
}
func (r *render) layers(dst *image.RGBA, ls []scene.Layer, parent matrix, opacity float64, clip *image.Alpha) {
	for _, l := range ls {
		if l.Hidden || (l.Start > 0 && r.t < l.Start) || (l.End > 0 && r.t >= l.End) {
			continue
		}
		for rep := 0; rep < max(1, l.Repeat); rep++ {
			x, y := r.n(l.X, 0), r.n(l.Y, 0)
			sx, sy := r.n(l.Scale, 1)*r.n(l.ScaleX, 1), r.n(l.Scale, 1)*r.n(l.ScaleY, 1)
			rot := r.n(l.Rotate, 0)
			alpha := opacity * r.n(l.Opacity, 1)
			energy := r.b.Number("energy", 1)
			for _, b := range l.Behave {
				for _, item := range []struct {
					kind string
					p    *scene.Params
				}{{"wiggle", b.Wiggle}, {"bounce", b.Bounce}, {"drift", b.Drift}, {"pop", b.Pop}, {"squash", b.Squash}, {"flicker", b.Flicker}} {
					kind, p := item.kind, item.p
					if p == nil {
						continue
					}
					u := max(0, r.t-p.Start)
					speed := p.Speed
					if speed == 0 {
						speed = 1
					}
					a := p.Amplitude * energy
					switch kind {
					case "wiggle":
						rot += math.Sin(u*speed*6.28) * a
					case "bounce":
						y -= math.Abs(math.Sin(u*speed*math.Pi)) * a
					case "drift":
						x += u * a
						y += math.Sin(u*speed) * a * .2
					case "pop":
						k := scene.Ease("back", u*speed, energy)
						sx *= k
						sy *= k
					case "squash":
						k := math.Sin(u*speed*6.28) * a * .01
						sx *= 1 + k
						sy /= max(.1, 1+k)
					case "flicker":
						alpha *= 1 - a*random(l.Seed+p.Seed, int(r.t*24))
					}
				}
			}
			if alpha <= 0 || sx == 0 || sy == 0 {
				continue
			}
			co, si := math.Cos(rot), math.Sin(rot)
			m := parent.mul(matrix{co * sx, si * sx, -si * sy, co * sy, x, y})
			seed := l.Seed + uint32(rep) + uint32(r.b.Number("seed", 7))
			localClip := clip
			if l.Clip != "" {
				cs, _ := parsePath(l.Clip)
				localClip = r.mask(cs, m, "nonzero")
				if clip != nil {
					for i := range localClip.Pix {
						localClip.Pix[i] = uint8(uint16(localClip.Pix[i]) * uint16(clip.Pix[i]) / 255)
					}
				}
			}
			if l.Type == "group" {
				if l.Blur > 0 || l.Glow > 0 || l.Blend != "" && l.Blend != "normal" || alpha != 1 {
					tmp := image.NewRGBA(dst.Bounds())
					r.layers(tmp, l.Layers, m, 1, localClip)
					r.finish(dst, tmp, l, alpha, nil)
				} else {
					r.layers(dst, l.Layers, m, alpha, localClip)
				}
				continue
			}

			w, h := r.n(l.W, 1920), r.n(l.H, 1080)
			// Aligned solid rectangles need neither a full-frame mask nor a temporary.
			if l.Type == "rect" && l.Stroke == nil && l.Blur == 0 && l.Glow == 0 && l.Reveal == nil && localClip == nil && len(l.Behave) == 0 && (l.Blend == "" || l.Blend == "normal") && m.b == 0 && m.c == 0 {
				if _, ok := l.Fill.(string); ok {
					a, z := m.point(point{0, 0}), m.point(point{w, h})
					if a.x == math.Floor(a.x) && a.y == math.Floor(a.y) && z.x == math.Floor(z.x) && z.y == math.Floor(z.y) {
						c := rgba(r.s(l.Fill))
						op := max(0, min(1, alpha))
						c.R = uint8(float64(c.R) * op)
						c.G = uint8(float64(c.G) * op)
						c.B = uint8(float64(c.B) * op)
						c.A = uint8(float64(c.A) * op)
						draw.Draw(dst, image.Rect(int(a.x), int(a.y), int(z.x), int(z.y)), image.NewUniform(c), image.Point{}, draw.Over)
						continue
					}
				}
			}
			tmp := image.NewRGBA(dst.Bounds())
			switch l.Type {
			case "text":
				r.text(tmp, l, m)
			case "image", "art", "logo":
				r.sample(tmp, l, m, w, h)
			case "noise":
				source := signal.ImageAt(r.w, r.h, int64(seed), int64(max(0, r.t)*30))
				inv := m.inverse()
				for yy := 0; yy < r.h; yy++ {
					for xx := 0; xx < r.w; xx++ {
						p := inv.point(point{float64(xx), float64(yy)})
						if p.x >= 0 && p.y >= 0 && p.x < w && p.y < h {
							tmp.SetRGBA(xx, yy, source.RGBAAt(min(r.w-1, int(p.x/w*float64(r.w))), min(r.h-1, int(p.y/h*float64(r.h)))))
						}
					}
				}
			default:
				var cs []contour
				switch l.Type {
				case "rect", "gradient":
					cs = []contour{{[]point{{0, 0}, {w, 0}, {w, h}, {0, h}}, true}}
				case "ellipse", "blob":
					c := contour{closed: true}
					for j := 0; j < 96; j++ {
						a := float64(j) * 2 * math.Pi / 96
						k := 1.0
						if l.Type == "blob" {
							k += .06*math.Sin(a*7+r.t*2+random(seed, 1)*6.28) + .03*math.Sin(a*11-r.t)
						}
						c.p = append(c.p, point{w/2 + math.Cos(a)*w/2*k, h/2 + math.Sin(a)*h/2*k})
					}
					cs = []contour{c}
				case "path":
					cs, _ = parsePath(l.Path)
				case "particles":
					for j := 0; j < l.Count; j++ {
						xx := random(seed, j)*w + math.Sin(r.t+float64(j))*15
						yy := math.Mod(random(seed, j+2050)*h+r.t*(8+random(seed, j+4100)*35), h)
						rad := 1 + random(seed, j+6200)*4
						cs = append(cs, contour{[]point{{xx - rad, yy - rad}, {xx + rad, yy - rad}, {xx + rad, yy + rad}, {xx - rad, yy + rad}}, true})
					}
				case "grid":
					for j := 0; j < 24; j++ {
						xx := float64(j-12) * w / 8
						cs = append(cs, contour{p: []point{{w/2 + xx*.05, 0}, {w/2 + xx, h}}})
					}
					for j := 0; j < 16; j++ {
						yy := math.Pow(math.Mod(float64(j)/16+r.t*.035, 1), 2) * h
						cs = append(cs, contour{p: []point{{0, yy}, {w, yy}}})
					}
				}
				if l.Fill != nil {
					r.paint(tmp, r.mask(cs, m, l.FillRule), l.Fill, m)
				}
				if l.Stroke != nil {
					r.paint(tmp, r.mask(stroke(cs, r.n(l.StrokeWidth, 1), l.Join, l.Cap), m, "nonzero"), l.Stroke, m)
				}
			}
			if l.Reveal != nil {
				u := scene.Ease(l.Reveal.Ease, (r.t-l.Reveal.From)/(l.Reveal.To-l.Reveal.From), energy)
				if l.Reveal.Out {
					u = 1 - u
				}
				for yy := 0; yy < r.h; yy++ {
					for xx := 0; xx < r.w; xx++ {
						p := m.inverse().point(point{float64(xx), float64(yy)})
						visible := p.x <= max(l.MaxWidth, w)*u
						if l.Reveal.Wipe == "rtl" {
							visible = p.x >= max(l.MaxWidth, w)*(1-u)
						}
						if !visible {
							tmp.SetRGBA(xx, yy, color.RGBA{})
						}
					}
				}
			}
			r.finish(dst, tmp, l, alpha, localClip)
		}
	}
}
func (r *render) mask(cs []contour, m matrix, rule string) *image.Alpha {
	mask := image.NewAlpha(image.Rect(0, 0, r.w, r.h))
	if rule == "evenodd" { // four vertical samples, analytic horizontal coverage.
		type edge struct{ x float64 }
		for yy := 0; yy < r.h; yy++ {
			coverage := make([]float64, r.w)
			for sub := 0; sub < 4; sub++ {
				scan := float64(yy) + (float64(sub)+.5)/4
				var xs []float64
				for _, c := range cs {
					for j := range c.p {
						a, b := m.point(c.p[j]), m.point(c.p[(j+1)%len(c.p)])
						if (a.y <= scan && b.y > scan) || (b.y <= scan && a.y > scan) {
							xs = append(xs, a.x+(scan-a.y)*(b.x-a.x)/(b.y-a.y))
						}
					}
				}
				sort.Float64s(xs)
				for j := 0; j+1 < len(xs); j += 2 {
					a, b := max(0, xs[j]), min(float64(r.w), xs[j+1])
					for x := int(a); x < int(math.Ceil(b)); x++ {
						if x >= 0 && x < r.w {
							coverage[x] += max(0, min(b, float64(x+1))-max(a, float64(x))) / 4
						}
					}
				}
			}
			for x, v := range coverage {
				mask.Pix[yy*mask.Stride+x] = uint8(min(255, v*255+.5))
			}
		}
		return mask
	}
	z := vector.NewRasterizer(r.w, r.h)
	for _, c := range cs {
		if len(c.p) == 0 {
			continue
		}
		p := m.point(c.p[0])
		z.MoveTo(float32(p.x), float32(p.y))
		for _, q := range c.p[1:] {
			p = m.point(q)
			z.LineTo(float32(p.x), float32(p.y))
		}
		z.ClosePath()
	}
	z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	return mask
}
func rgba(s string) color.RGBA {
	a := 1.0
	if k := strings.LastIndex(s, "@"); k >= 0 {
		a, _ = strconv.ParseFloat(s[k+1:], 64)
		s = s[:k]
	}
	s = strings.TrimPrefix(s, "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	v, _ := strconv.ParseUint(s, 16, 32)
	if len(s) == 8 {
		a *= float64(v&255) / 255
		v >>= 8
	}
	a = max(0, min(1, a))
	return color.RGBA{uint8(float64(v>>16&255) * a), uint8(float64(v>>8&255) * a), uint8(float64(v&255) * a), uint8(a * 255)}
}
func (r *render) paint(dst *image.RGBA, mask *image.Alpha, fill any, m matrix) {
	base := rgba(r.s(fill))
	inv := m.inverse()
	g, isg := fill.(map[string]any)
	var coords []any
	radial := false
	if isg {
		coords, _ = g["linear"].([]any)
		if v, ok := g["radial"].([]any); ok {
			coords = v
			radial = true
		}
	}
	var stops []struct {
		at float64
		c  color.RGBA
	}
	if isg {
		arr, _ := g["stops"].([]any)
		for _, v := range arr {
			a, ok := v.([]any)
			if ok && len(a) == 2 {
				stops = append(stops, struct {
					at float64
					c  color.RGBA
				}{r.n(a[0], 0), rgba(r.s(a[1]))})
			}
		}
	}
	var cv [4]float64
	for i := 0; i < min(4, len(coords)); i++ {
		cv[i] = r.n(coords[i], 0)
	}
	for y := 0; y < r.h; y++ {
		for x := 0; x < r.w; x++ {
			a := mask.Pix[y*mask.Stride+x]
			if a == 0 {
				continue
			}
			c := base
			if len(coords) >= 4 && len(stops) > 0 {
				p := inv.point(point{float64(x) + .5, float64(y) + .5})
				u := 0.0
				if radial {
					dx, dy := p.x-cv[0], p.y-cv[1]
					u = (math.Hypot(dx, dy) - cv[2]) / max(.001, cv[3]-cv[2])
				} else {
					dx, dy := cv[2]-cv[0], cv[3]-cv[1]
					u = ((p.x-cv[0])*dx + (p.y-cv[1])*dy) / max(.001, dx*dx+dy*dy)
				}
				c = stops[len(stops)-1].c
				if u <= stops[0].at {
					c = stops[0].c
				} else {
					for j := 1; j < len(stops); j++ {
						if u <= stops[j].at {
							k := (u - stops[j-1].at) / max(.001, stops[j].at-stops[j-1].at)
							c = mix(stops[j-1].c, stops[j].c, k)
							break
						}
					}
				}
			}
			c.R = uint8(uint16(c.R) * uint16(a) / 255)
			c.G = uint8(uint16(c.G) * uint16(a) / 255)
			c.B = uint8(uint16(c.B) * uint16(a) / 255)
			c.A = uint8(uint16(c.A) * uint16(a) / 255)
			dst.SetRGBA(x, y, c)
		}
	}
}
func mix(a, b color.RGBA, u float64) color.RGBA {
	return color.RGBA{uint8(float64(a.R)*(1-u) + float64(b.R)*u), uint8(float64(a.G)*(1-u) + float64(b.G)*u), uint8(float64(a.B)*(1-u) + float64(b.B)*u), uint8(float64(a.A)*(1-u) + float64(b.A)*u)}
}
