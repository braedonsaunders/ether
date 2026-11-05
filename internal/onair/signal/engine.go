package signal

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

// Engine owns one rendition's live Signal state. Construct off the tick path.
// Render is allocation-free and its output belongs to the caller. Static uses
// the owner-approved hard-edged grain directly in the negotiated output frame.
type Engine struct {
	width, height    int
	style            string
	plate            []byte
	vignette         []uint16
	seed             int64
	staticParameters StaticParameters
}

// NewStatic restores the continuity demo's live, neutral, crisp static.
func NewStatic(width, height int, seed int64) *Engine {
	return NewStaticWithParameters(width, height, seed, DefaultStaticParameters())
}

// NewStaticWithParameters snapshots controls for one engine. Replace the engine
// off the frame clock when controls change; Render stays allocation-free.
func NewStaticWithParameters(width, height int, seed int64, p StaticParameters) *Engine {
	e := newEngine(width, height, "static", seed)
	e.staticParameters = p
	return e
}
func newEngine(w, h int, style string, seed int64) *Engine {
	if w < 2 || h < 2 || w%2 != 0 || h%2 != 0 {
		panic("signal: invalid size")
	}
	return &Engine{width: w, height: h, style: style, seed: seed}
}

// NewPlate accepts straight/premultiplied image.Image plates (PNG decoding
// handles either) and a transparent text overlay. Text is composited only here,
// so replace the Engine when copy changes rather than rerasterizing per tick.
// style is bars, testcard, brb, or card. Every style is silent.
func NewPlate(width, height int, style string, plate, overlay image.Image, seed int64) *Engine {
	e := newEngine(width, height, style, seed)
	if plate == nil {
		panic("signal: plate required")
	}
	combined := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(combined, combined.Rect, plate, plate.Bounds().Min, draw.Src)
	if overlay != nil {
		draw.Draw(combined, combined.Rect, overlay, overlay.Bounds().Min, draw.Over)
	}
	e.plate = make([]byte, width*height*3/2)
	e.vignette = make([]uint16, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			nx, ny := (float64(x)/float64(width)-.5)*2, (float64(y)/float64(height)-.5)*2
			e.vignette[y*width+x] = uint16(256 - 20*(nx*nx+ny*ny)/2)
		}
	}
	for y := 0; y < height; y += 2 {
		for x := 0; x < width; x += 2 {
			r, g, b := 0, 0, 0
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					c := color.NRGBAModel.Convert(combined.At(x+dx, y+dy)).(color.NRGBA)
					rr, gg, bb := int(c.R), int(c.G), int(c.B)
					r += rr
					g += gg
					b += bb
					e.plate[(y+dy)*width+x+dx] = byte(16 + (47*rr+157*gg+16*bb+128)/256)
				}
			}
			r /= 4
			g /= 4
			b /= 4
			i := y/2*(width/2) + x/2
			e.plate[width*height+i] = byte(128 + (-26*r-87*g+112*b+128)/256)
			e.plate[width*height+width*height/4+i] = byte(128 + (112*r-102*g-10*b+128)/256)
		}
	}
	return e
}

// Render supplies tightly packed I420 and stereo 48 kHz PCM for one 30 Hz tick.
func (e *Engine) Render(video []byte, pcm []int16, tick int64) {
	w, h := e.width, e.height
	if len(video) < w*h*3/2 || len(pcm) < 3200 {
		panic("signal: undersized tick buffers")
	}
	if e.style == "static" {
		StaticVideoWithParameters(video, w, h, e.seed, tick, e.staticParameters)
		StaticAudioWithParameters(pcm, tick, 1600, e.seed, e.staticParameters)
		return
	}
	copy(video, e.plate)
	t := float64(tick) / 30
	breath := .006*math.Sin(t*.21) + .003*math.Sin(t*.083*math.Sqrt2)
	// A single 256-value lookup per row applies scanline drift and brightness
	// wander plus tube vignette. Decorative bloom is baked into W6 plates;
	// the typography overlay remains sharp and is never rasterized per tick.
	for y := 0; y < h; y++ {
		gain := 1 + breath + .003*math.Sin(float64(y)*math.Pi/3-t*.14)
		var lut [256]byte
		for v := range lut {
			lut[v] = byte(max(16, min(235, 16+int(float64(v-16)*gain))))
		}
		row := video[y*w : (y+1)*w]
		for x, v := range row {
			row[x] = byte(16 + (int(lut[v])-16)*int(e.vignette[y*w+x])/256)
		}
	}
	// Every signal screen is silent (owner decision: no bars or test-card tone).
	clear(pcm)
}
