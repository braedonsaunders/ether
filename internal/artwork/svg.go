package artwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"github.com/braedonsaunders/ether/internal/lru"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const maxSVGBytes = 512 << 10

var ErrSVG = errors.New("artwork: SVG cannot be safely rasterized")

type RasterOptions struct {
	Width, Height int
	Format        string
	Blur          bool
}
type rasterResult struct {
	data        []byte
	contentType string
	err         error
}

// SVGRasterizer bounds work on untrusted SVG and caches raster variants. It is
// shared by a runtime, not global. References are never authorization tokens.
type SVGRasterizer struct {
	mu    sync.Mutex
	cache *lru.Cache[string, rasterResult]
	slots chan struct{}
}

func NewSVGRasterizer() *SVGRasterizer {
	return &SVGRasterizer{slots: make(chan struct{}, 2), cache: lru.New(lru.Options[string, rasterResult]{MaxEntries: 512, MaxCost: 32 << 20, Cost: func(_ string, r rasterResult) int64 { return int64(len(r.data)) }})}
}

// validateSVG rejects active content, external references, expansion and large
// inputs before handing the bounded document to the SVG parser.
func validateSVG(data []byte) error {
	if len(data) == 0 || len(data) > maxSVGBytes {
		return ErrSVG
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	elements, paths, depth, pathBytes := 0, 0, 0, 0
	root := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrSVG
		}
		switch node := token.(type) {
		case xml.Directive:
			return ErrSVG
		case xml.ProcInst:
			if node.Target != "xml" || root {
				return ErrSVG
			}
		case xml.StartElement:
			depth++
			elements++
			if depth > 64 || elements > 1024 {
				return ErrSVG
			}
			name := strings.ToLower(node.Name.Local)
			if !root {
				if name != "svg" {
					return ErrSVG
				}
				root = true
			}
			switch name {
			case "script", "foreignobject", "image", "use", "text", "style":
				return ErrSVG
			}
			if name == "path" {
				paths++
				if paths > 128 {
					return ErrSVG
				}
			}
			for _, attr := range node.Attr {
				key, value := strings.ToLower(attr.Name.Local), strings.TrimSpace(attr.Value)
				if len(value) > 16384 {
					return ErrSVG
				}
				if strings.HasPrefix(key, "on") || key == "href" {
					return ErrSVG
				}
				if (name == "path" && key == "d") || key == "points" {
					pathBytes += len(value)
					if len(value) > 8192 || pathBytes > 32768 {
						return ErrSVG
					}
				}
				switch key {
				case "d", "points", "transform", "viewbox", "x", "y", "x1", "x2", "y1", "y2", "cx", "cy", "r", "rx", "ry", "width", "height", "stroke-width":
					if !safeSVGNumbers(value) {
						return ErrSVG
					}
				}
				// Gradients may reference an in-document ID; all other url() values
				// are rejected, including multiple or malformed references.
				lower := strings.ToLower(value)
				if strings.Contains(lower, "url(") && !(strings.HasPrefix(lower, "url(#") && strings.HasSuffix(lower, ")") && strings.Count(lower, "url(") == 1 && !strings.ContainsAny(lower[5:len(lower)-1], "() \t\r\n\"'")) {
					return ErrSVG
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if !root || depth != 0 {
		return ErrSVG
	}
	return nil
}

// Bound numeric values and token counts before fixed-point rasterization.
func safeSVGNumbers(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
		return false
	}
	count := 0
	for i := 0; i < len(s); {
		c := s[i]
		if !(c >= '0' && c <= '9') && c != '.' && c != '-' && c != '+' {
			i++
			continue
		}
		start := i
		if c == '-' || c == '+' {
			i++
		}
		digits := false
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			digits = true
			i++
		}
		if i < len(s) && s[i] == '.' {
			i++
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				digits = true
				i++
			}
		}
		if !digits {
			return false
		}
		if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
			i++
			if i < len(s) && (s[i] == '-' || s[i] == '+') {
				i++
			}
			exponent := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if exponent == i {
				return false
			}
		}
		value, err := strconv.ParseFloat(s[start:i], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1e6 {
			return false
		}
		count++
		if count > 2048 {
			return false
		}
	}
	return true
}

func (r *SVGRasterizer) execute(ctx context.Context, key string, build func() (image.Image, error), opts RasterOptions) ([]byte, string, error) {
	if r == nil || opts.Width < 0 || opts.Height < 0 || opts.Width > 2048 || opts.Height > 2048 || (opts.Format != "" && opts.Format != "png" && opts.Format != "jpeg") {
		return nil, "", ErrSVG
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	r.mu.Lock()
	cached, ok := r.cache.Get(key)
	r.mu.Unlock()
	if ok {
		return append([]byte(nil), cached.data...), cached.contentType, nil
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
	case <-bounded.Done():
		return nil, "", bounded.Err()
	}
	results := make(chan rasterResult, 1)
	go func() {
		var result rasterResult
		defer func() {
			if recover() != nil {
				result = rasterResult{err: ErrSVG}
			}
			<-r.slots
			results <- result
		}()
		img, err := build()
		if err != nil {
			result.err = err
			return
		}
		if opts.Blur {
			img, err = kitimages.Blur(img)
			if err != nil {
				result.err = err
				return
			}
		}
		var out bytes.Buffer
		if opts.Format == "png" {
			result.contentType = "image/png"
			err = png.Encode(&out, img)
		} else {
			result.contentType = "image/jpeg"
			err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 90})
		}
		if err != nil {
			result.err = err
			return
		}
		result.data = out.Bytes()
	}()
	select {
	case result := <-results:
		if result.err != nil {
			return nil, "", result.err
		}
		r.mu.Lock()
		r.cache.Add(key, result)
		r.mu.Unlock()
		return append([]byte(nil), result.data...), result.contentType, nil
	case <-bounded.Done():
		return nil, "", bounded.Err()
	}
}

func rasterKey(reference string, data []byte, opts RasterOptions) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s:%s:%d:%d:%s:%t", reference, hex.EncodeToString(sum[:]), opts.Width, opts.Height, opts.Format, opts.Blur)
}

// Rasterize validates and renders an SVG without resolving external resources.
func (r *SVGRasterizer) Rasterize(ctx context.Context, reference string, data []byte, opts RasterOptions) ([]byte, string, error) {
	if err := validateSVG(data); err != nil {
		return nil, "", err
	}
	key := rasterKey(reference, data, opts)
	return r.execute(ctx, key, func() (image.Image, error) {
		icon, err := oksvg.ReadIconStream(bytes.NewReader(data), oksvg.StrictErrorMode)
		if err != nil || icon == nil || icon.ViewBox.W < 1e-6 || icon.ViewBox.H < 1e-6 || math.IsNaN(icon.ViewBox.W) || math.IsNaN(icon.ViewBox.H) || math.IsNaN(icon.ViewBox.X) || math.IsNaN(icon.ViewBox.Y) || math.IsInf(icon.ViewBox.W, 0) || math.IsInf(icon.ViewBox.H, 0) || math.IsInf(icon.ViewBox.X, 0) || math.IsInf(icon.ViewBox.Y, 0) || icon.ViewBox.W > 1e6 || icon.ViewBox.H > 1e6 || math.Abs(icon.ViewBox.X) > 1e6 || math.Abs(icon.ViewBox.Y) > 1e6 {
			return nil, ErrSVG
		}
		width, height := opts.Width, opts.Height
		if width == 0 && height == 0 {
			width = 500
		}
		if width == 0 {
			width = max(1, int(float64(height)*icon.ViewBox.W/icon.ViewBox.H))
		}
		if height == 0 {
			height = max(1, int(float64(width)*icon.ViewBox.H/icon.ViewBox.W))
		}
		if width > 2048 || height > 2048 {
			return nil, ErrSVG
		}
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		icon.SetTarget(0, 0, float64(width), float64(height))
		icon.Draw(rasterx.NewDasher(width, height, rasterx.NewScannerGV(width, height, img, img.Bounds())), 1)
		return img, nil
	}, opts)
}

// ChannelLogo renders the same channel number, name and colours as Ether's
// SVG fallback; oksvg cannot render SVG text, so that generated logo is drawn
// from its source fields instead of rasterizing the SVG.
func (r *SVGRasterizer) ChannelLogo(ctx context.Context, reference, number, name string, opts RasterOptions) ([]byte, string, error) {
	if len(number) > 64 || len(name) > 512 {
		return nil, "", ErrSVG
	}
	key := rasterKey(reference, []byte(number+"\x00"+name), opts)
	return r.execute(ctx, key, func() (image.Image, error) {
		width, height := opts.Width, opts.Height
		if width == 0 && height == 0 {
			width = 640
		}
		if width == 0 {
			width = max(1, height*16/9)
		}
		if height == 0 {
			height = max(1, width*9/16)
		}
		if width > 2048 || height > 2048 {
			return nil, ErrSVG
		}
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		rx := max(1, width*24/640)
		ry := max(1, height*24/360)
		background := color.RGBA{R: 0x18, G: 0x2d, B: 0x45, A: 255}
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				cx, cy := x, y
				if x < rx {
					cx = rx
				} else if x >= width-rx {
					cx = width - rx - 1
				}
				if y < ry {
					cy = ry
				} else if y >= height-ry {
					cy = height - ry - 1
				}
				dx, dy := float64(x-cx)/float64(rx), float64(y-cy)/float64(ry)
				if dx*dx+dy*dy <= 1 {
					img.SetRGBA(x, y, background)
				}
			}
		}
		parsed, err := opentype.Parse(gomedium.TTF)
		if err != nil {
			return nil, err
		}
		for _, line := range []struct {
			text    string
			y, size float64
			color   color.Color
		}{{number, 125, 68, color.RGBA{R: 0x64, G: 0xdf, B: 0xc7, A: 255}}, {name, 220, 30, color.White}} {
			face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: max(1, line.size*float64(height)/360), DPI: 72, Hinting: font.HintingFull})
			if err != nil {
				return nil, err
			}
			d := font.Drawer{Dst: img, Src: image.NewUniform(line.color), Face: face, Dot: fixed.P(40*width/640, int(line.y*float64(height)/360))}
			d.DrawString(line.text)
			_ = face.Close()
		}
		return img, nil
	}, opts)
}
