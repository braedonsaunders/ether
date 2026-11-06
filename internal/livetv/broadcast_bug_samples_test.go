package livetv

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var bugSamples = flag.String("bug-samples", "", "offline overlay evidence directory (requires programme-bright.png, programme-dark.png and before/ scene documents)")

// TestBroadcastBugSamples is an opt-in artifact recorder, never a lab or stream.
// Every still and clip frame uses the production compactOverlay/blend path.
func TestBroadcastBugSamples(t *testing.T) {
	if *bugSamples == "" {
		t.Skip("opt-in offline evidence")
	}
	ctx := t.Context()
	db, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	f := transcode.RawFormat{Width: 1920, Height: 1080}
	logoDoc := scene.Document{V: 1, Moment: "bug", Duration: 8, Poster: 3, Layers: []scene.Layer{{ID: "logo", Type: "text", Text: "M", Font: "fraunces", Size: 1000.0, X: 300.0, Y: 950.0, Fill: "#daa65c"}}}
	logo := raster.Render(logoDoc, raster.Bindings{}, 3, 240, 135)
	g, e := onair.New(onair.Options{DB: db, Dir: t.TempDir(), Artwork: func(context.Context, string) (image.Image, error) { return logo, nil }})
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	backgrounds := map[string][]byte{}
	for _, name := range []string{"bright", "dark"} {
		file, e := os.Open(filepath.Join(*bugSamples, "programme-"+name+".png"))
		if e != nil {
			t.Fatal(e)
		}
		im, _, e := image.Decode(file)
		file.Close()
		if e != nil {
			t.Fatal(e)
		}
		backgrounds[name] = sampleYUV(im, f)
	}
	for _, v := range [][3]string{{"clean", "meridian", "ETHER"}, {"kids", "blorp", "BLORP!"}, {"lateNight", "nocturne", "NOCTURNE"}, {"cinema", "meridian", "MERIDIAN"}, {"retro", "neon", "NEON 84"}, {"personal", "personal", "Braedon TV"}} {
		look, e := g.Look(ctx, v[1])
		if e != nil {
			t.Fatal(e)
		}
		look.ID = "sample-" + v[0]
		templates, err := g.Templates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		templateID := v[0]
		if templateID == "lateNight" {
			templateID = "late-night"
		}
		for _, template := range templates {
			if template.ID == templateID {
				look = template.Style
				look.ID = "sample-" + v[0]
				break
			}
		}
		look.Name = v[2]
		if v[0] == "clean" {
			look.Brand.Type.Display = "Inter"
			look.Brand.Type.Body = "Inter"
			look.Brand.Tagline = ""
		}
		if _, e = g.SaveLook(ctx, look); e != nil {
			t.Fatal(e)
		}
		family := v[0]
		if family == "lateNight" {
			family = "late-night"
		}
		for _, source := range []string{"logo", "wordmark"} {
			for _, corner := range []string{"topLeft", "topRight", "bottomLeft", "bottomRight"} {
				r := core.ClipRequest{LookID: look.ID, Moment: "bug", Bug: &core.BugSettings{Position: corner, Source: source}, Bind: map[string]string{"channel.name": v[2]}, Art: map[string]string{"channel.logo": "sample-logo"}}
				d, b, ok := g.Overlay(ctx, r)
				if !ok {
					t.Fatal("overlay", v)
				}
				after := compactOverlay(raster.Render(d, b, 3, f.Width, f.Height), f)
				old, e := os.ReadFile(filepath.Join(*bugSamples, "before", family+"-bug.json"))
				if e != nil {
					t.Fatal(e)
				}
				beforeDoc, e := scene.Decode(old)
				if e != nil {
					t.Fatal(e)
				}
				before := compactOverlay(raster.Render(beforeDoc, b, 5.5, f.Width, f.Height), f)
				for bg, raw := range backgrounds {
					for state, overlay := range map[string]*videoOverlay{"before": before, "after": after} {
						frame := append([]byte(nil), raw...)
						overlay.blend(frame, f, 255)
						name := fmt.Sprintf("%s-%s-%s-%s-%s.jpg", v[0], source, corner, bg, state)
						sampleJPEG(t, filepath.Join(*bugSamples, name), frame, f)
					}
				}
			}
		}
		for _, moment := range []string{"ident", "lowerthird"} {
			r := core.ClipRequest{LookID: look.ID, Moment: moment, Bind: map[string]string{"channel.name": v[2], "now.title": "The Alpine Journey", "next.title": "Beyond the Ridge", "next.time": "21:00", "viewer.name": "Braedon", "viewer.greeting": "Good evening"}}
			d, b, ok := g.Overlay(ctx, r)
			if !ok {
				t.Fatal(moment)
			}
			old, e := os.ReadFile(filepath.Join(*bugSamples, "before", family+"-"+moment+".json"))
			if e != nil {
				t.Fatal(e)
			}
			before, e := scene.Decode(old)
			if e != nil {
				t.Fatal(e)
			}
			for state, doc := range map[string]scene.Document{"before": before, "after": d} {
				raw := append([]byte(nil), backgrounds["dark"]...)
				compactOverlay(raster.Render(doc, b, doc.Poster, 1920, 1080), f).blend(raw, f, 255)
				sampleJPEG(t, filepath.Join(*bugSamples, fmt.Sprintf("%s-%s-%s.jpg", v[0], moment, state)), raw, f)
			}
		}
	}
	// Programme -> break -> programme, with real compositor smoothstep ramps.
	d, _ := families.Get("clean", "bug")
	b := raster.Bindings{Bindings: families.Defaults("clean")}
	b.Slots["channel.name"] = "ETHER"
	o := &programmeOverlays{bug: compactOverlay(raster.Render(d, b, 3, 1920, 1080), f), fadeBreaks: true}
	cmd := mediaproc.CommandContext(ctx, 2*time.Minute, "ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", "yuv420p", "-s", "1920x1080", "-r", "30", "-i", "pipe:0", "-an", "-c:v", "libx264", "-threads", "2", "-preset", "fast", "-crf", "20", "-movflags", "+faststart", filepath.Join(*bugSamples, "break-fade.mp4"))
	cmd.Stderr = os.Stderr
	pipe, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	breakDoc, _ := families.Get("clean", "break")
	breakRaw := sampleYUV(raster.Render(breakDoc, b, breakDoc.Poster, 1920, 1080), f)
	start := time.Unix(0, 0)
	for i := 0; i < 240; i++ {
		age := time.Duration(i) * time.Second / 30
		raw := append([]byte(nil), backgrounds["dark"]...)
		if age < 3*time.Second {
			o.blend(raw, f, start.Add(age), start, start.Add(3*time.Second))
		} else if age < 5*time.Second {
			raw = append([]byte(nil), breakRaw...)
		} else if age >= 5*time.Second {
			o.blend(raw, f, start.Add(age), start.Add(5*time.Second), start.Add(9*time.Second))
		}
		if _, e = pipe.Write(raw); e != nil {
			t.Fatal(e)
		}
	}
	pipe.Close()
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
}
func sampleYUV(im image.Image, f transcode.RawFormat) []byte {
	raw := make([]byte, f.Width*f.Height*3/2)
	n := f.Width * f.Height
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			c := color.NRGBAModel.Convert(im.At(x, y)).(color.NRGBA)
			r, g, b := int(c.R), int(c.G), int(c.B)
			raw[y*f.Width+x] = byte(16 + (47*r+157*g+16*b+128)/256)
			if x%2 == 0 && y%2 == 0 {
				i := y/2*(f.Width/2) + x/2
				raw[n+i] = byte(128 + (-26*r-87*g+112*b+128)/256)
				raw[n+n/4+i] = byte(128 + (112*r-102*g-10*b+128)/256)
			}
		}
	}
	return raw
}
func sampleJPEG(t *testing.T, path string, raw []byte, f transcode.RawFormat) {
	t.Helper()
	im := image.NewYCbCr(image.Rect(0, 0, f.Width, f.Height), image.YCbCrSubsampleRatio420)
	n := f.Width * f.Height
	copy(im.Y, raw[:n])
	copy(im.Cb, raw[n:n+n/4])
	copy(im.Cr, raw[n+n/4:])
	out, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	if e = jpeg.Encode(out, im, &jpeg.Options{Quality: 92}); e != nil {
		t.Fatal(e)
	}
}
