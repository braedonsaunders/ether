package livetv

import (
	"context"
	"image"
	"image/color"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type overlaySource interface {
	Overlay(context.Context, core.ClipRequest) (scene.Document, raster.Bindings, bool)
}
type videoOverlay struct {
	fadeBreaks, fadeCredits bool
	rect                    image.Rectangle
	y, a, u, v, uvA         []byte
}
type programmeOverlays struct {
	bug, lower              *videoOverlay
	fadeBreaks, fadeCredits bool
	credits                 []core.Chapter
}

// prepareProgrammeOverlays resolves assets and rasterizes once in the decoder's
// background prepare. The clock only blends the compact non-transparent bounds.
func (p *scheduleBroadcastPlanner) prepareProgrammeOverlays(ctx context.Context, programme Programme, chapters []core.Chapter) *programmeOverlays {
	c := p.channel
	// Overlays is the owner's explicit on/off switch for the corner logo and
	// lower-third ("" is Off, the default); nothing below ever checks it, so
	// a Look with a Bug/lower-third configured renders them unconditionally
	// even when the owner has turned this off.
	if c.Look == nil || c.Look.Overlays == "" || programme.Item.Key == 0 {
		return nil
	}
	source, ok := p.s.o.Graphics.(overlaySource)
	if !ok {
		return nil
	}
	p.overlayMu.Lock()
	defer p.overlayMu.Unlock()
	if existing := p.overlayCards[programme.ID]; existing != nil {
		return existing
	}
	out := &programmeOverlays{}
	for _, c := range chapters {
		if strings.Contains(strings.ToLower(c.Title), "credit") {
			out.credits = append(out.credits, c)
		}
	}
	for _, moment := range []string{"bug", "lowerthird"} {
		if moment == "bug" && p.overlayBug != nil {
			out.bug = p.overlayBug
			out.fadeBreaks = p.overlayBug.fadeBreaks
			out.fadeCredits = p.overlayBug.fadeCredits
			continue
		}
		request := p.s.clipRequest(c, Programme{ChannelID: c.ID, Start: programme.Start, End: programme.End, Item: Item{Slate: moment}})
		request.Bind["now.title"] = programme.Item.Title
		request.Bind["now.meta"] = ProgrammeMeta(programme.Item)
		request.Bind["now.logline"] = Logline(programme.Item.Summary)
		doc, bindings, ok := source.Overlay(ctx, request)
		if !ok {
			continue
		}
		if moment == "bug" {
			out.fadeBreaks, _ = bindings.Tokens["bug.fadeBreaks"].(bool)
			out.fadeCredits, _ = bindings.Tokens["bug.fadeCredits"].(bool)
		}
		img := raster.Render(doc, bindings, doc.Poster, p.format.Width, p.format.Height)
		overlay := compactOverlay(img, p.format)
		if moment == "bug" {
			out.bug = overlay
			if overlay != nil {
				overlay.fadeBreaks = out.fadeBreaks
				overlay.fadeCredits = out.fadeCredits
			}
			p.overlayBug = overlay
		} else {
			out.lower = overlay
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if p.overlayCards == nil {
		p.overlayCards = map[string]*programmeOverlays{}
	}
	if len(p.overlayCards) >= 16 {
		for key := range p.overlayCards {
			delete(p.overlayCards, key)
			break
		}
	}
	p.overlayCards[programme.ID] = out
	return out
}
func compactOverlay(img *image.RGBA, f transcode.RawFormat) *videoOverlay {
	bounds := image.Rectangle{}
	for y := img.Rect.Min.Y; y < img.Rect.Max.Y; y++ {
		for x := img.Rect.Min.X; x < img.Rect.Max.X; x++ {
			if img.RGBAAt(x, y).A != 0 {
				pixel := image.Rect(x, y, x+1, y+1)
				if bounds.Empty() {
					bounds = pixel
				} else {
					bounds = bounds.Union(pixel)
				}
			}
		}
	}
	if bounds.Empty() {
		return nil
	}
	bounds.Min.X &= ^1
	bounds.Min.Y &= ^1
	bounds.Max.X = (bounds.Max.X + 1) &^ 1
	bounds.Max.Y = (bounds.Max.Y + 1) &^ 1
	bounds = bounds.Intersect(image.Rect(0, 0, f.Width, f.Height))
	n := bounds.Dx() * bounds.Dy()
	out := &videoOverlay{rect: bounds, y: make([]byte, n), a: make([]byte, n), u: make([]byte, n/4), v: make([]byte, n/4), uvA: make([]byte, n/4)}
	for y := 0; y < bounds.Dy(); y += 2 {
		for x := 0; x < bounds.Dx(); x += 2 {
			sumA, sumU, sumV := 0, 0, 0
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					c := color.NRGBAModel.Convert(img.At(bounds.Min.X+x+dx, bounds.Min.Y+y+dy)).(color.NRGBA)
					r, g, b := int(c.R), int(c.G), int(c.B)
					i := (y+dy)*bounds.Dx() + x + dx
					out.a[i] = c.A
					out.y[i] = byte(16 + (47*r+157*g+16*b+128)/256)
					u := 128 + (-26*r-87*g+112*b+128)/256
					v := 128 + (112*r-102*g-10*b+128)/256
					sumA += int(c.A)
					sumU += u * int(c.A)
					sumV += v * int(c.A)
				}
			}
			i := y/2*(bounds.Dx()/2) + x/2
			out.uvA[i] = byte((sumA + 2) / 4)
			if sumA > 0 {
				out.u[i] = byte(sumU / sumA)
				out.v[i] = byte(sumV / sumA)
			}
		}
	}
	return out
}
func (o *videoOverlay) blend(dst []byte, f transcode.RawFormat, fade byte) {
	if o == nil || fade == 0 {
		return
	}
	w, h := o.rect.Dx(), o.rect.Dy()
	for y := 0; y < h; y++ {
		src := y * w
		base := (o.rect.Min.Y+y)*f.Width + o.rect.Min.X
		for x := 0; x < w; x++ {
			a := (int(o.a[src+x])*int(fade) + 127) / 255
			if a == 0 {
				continue
			}
			dst[base+x] = byte((int(o.y[src+x])*a + int(dst[base+x])*(255-a) + 127) / 255)
		}
	}
	uv := f.Width * f.Height
	plane := uv / 4
	for y := 0; y < h/2; y++ {
		base := (o.rect.Min.Y/2+y)*(f.Width/2) + o.rect.Min.X/2
		for x := 0; x < w/2; x++ {
			i := y*(w/2) + x
			a := (int(o.uvA[i])*int(fade) + 127) / 255
			if a == 0 {
				continue
			}
			at := base + x
			dst[uv+at] = byte((int(o.u[i])*a + int(dst[uv+at])*(255-a) + 127) / 255)
			dst[uv+plane+at] = byte((int(o.v[i])*a + int(dst[uv+plane+at])*(255-a) + 127) / 255)
		}
	}
}
func overlayFade(age, span time.Duration) byte {
	if age < 0 || age >= span {
		return 0
	}
	fade := 1.0
	if age < 500*time.Millisecond {
		fade = float64(age) / float64(500*time.Millisecond)
	}
	if tail := span - age; tail < 500*time.Millisecond {
		fade = min(fade, float64(tail)/float64(500*time.Millisecond))
	}
	fade = fade * fade * (3 - 2*fade)
	return byte(255 * fade)
}
func (o *programmeOverlays) blend(video []byte, f transcode.RawFormat, now, start, end time.Time) {
	if o == nil {
		return
	}
	age := now.Sub(start)
	bugFade := byte(255)
	if o.fadeBreaks {
		bugFade = overlayFade(age, end.Sub(start))
	}
	if o.fadeCredits {
		for _, cue := range o.credits {
			tail := cue.End
			if tail <= cue.Start {
				tail = end.Sub(start)
			}
			if age >= cue.Start && age < tail {
				bugFade = 0
			} else if age < cue.Start && cue.Start-age < time.Second {
				bugFade = min(bugFade, overlayFade(cue.Start-age, 2*time.Second))
			} else if age >= tail && age-tail < time.Second {
				bugFade = min(bugFade, overlayFade(age-tail, 2*time.Second))
			}
		}
	}
	o.bug.blend(video, f, bugFade)
	lowerAge := age
	half := end.Sub(start) / 2
	if age >= half {
		lowerAge = age - half
	}
	o.lower.blend(video, f, overlayFade(lowerAge, 8*time.Second))
}

type overlayDecoder struct {
	policy    *broadcastPolicyTag
	wallStart time.Time
	format    transcode.RawFormat
	transcode.Decoder
	overlays *programmeOverlays
}
