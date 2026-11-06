// onair-samples renders owner-review masters through the production Go encoder.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/onair/encode"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
)

type sample struct {
	name, family, moment, caption string
	slots                         map[string]string
}

func main() {
	input := flag.String("input", "", "master input for stream batch")
	budget := flag.Int("bitrate", 3000, "continuous session total bitrate kbps")
	batch := flag.String("batch", "plates", "plates, families, overlay, one or stream")
	family := flag.String("family", "late-night", "family for one")
	moment := flag.String("moment", "ident", "moment for one")
	out := flag.String("out", "/tmp/onair-samples", "output directory")
	seconds := flag.Int("seconds", 16, "sample seconds, multiple of four")
	width := flag.Int("width", 1920, "sample width, 1920 or 1280")
	workers := flag.Int("workers", 2, "frame workers")
	name := flag.String("name", "", "override output stem for one")
	programme := flag.String("programme", "", "decoded 1080p programme source for overlay")
	flag.Parse()
	if *width != 1920 && *width != 1280 {
		panic("width must be 1920 or 1280")
	}
	if *batch == "stream" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		if err := streamSample(ctx, *input, *out, *width, *seconds, *budget); err != nil {
			panic(err)
		}
		return
	}
	if *batch == "plates" {
		if err := plates(*out); err != nil {
			panic(err)
		}
		return
	}
	if *seconds < 16 || *seconds > 20 || *seconds%4 != 0 {
		panic("samples must be 16 or 20 seconds")
	}
	if err := os.MkdirAll(*out, 0755); err != nil {
		panic(err)
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		panic(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var samples []sample
	switch *batch {
	case "families":
		for _, f := range []string{"late-night", "clean", "kids", "cinema", "retro", "personal"} {
			for _, m := range []string{"ident", "upnext", "break"} {
				samples = append(samples, sample{f + "-" + m, f, m, fmt.Sprintf("%s · %s. Two complete eight-second scene cycles; gentle synthesized audio.", f, m), nil})
			}
		}
	case "overlay":
		samples = []sample{{"late-night-overlay", "late-night", "lowerthird", "Late night bug and lower third composited in Go over a decoded 1080p test programme.", nil}}
	case "one":
		samples = []sample{{*family + "-" + *moment, *family, *moment, *family + " · " + *moment, nil}}
	default:
		panic("unknown batch")
	}
	for _, s := range samples {
		if *name != "" && *batch == "one" {
			s.name = *name
		}
		if err := render(ctx, ffmpeg, s, *out, *seconds, *workers, *width, *programme); err != nil {
			panic(err)
		}
	}
}

func render(ctx context.Context, ffmpeg string, s sample, out string, seconds, workers, width int, programme string) error {
	doc, err := families.Get(s.family, s.moment)
	if err != nil {
		return err
	}
	b := raster.Bindings{Bindings: families.Defaults(s.family)}
	for k, v := range s.slots {
		b.Slots[k] = v
	}
	target := filepath.Join(out, s.name+".mp4")
	height := width * 9 / 16
	suffix := ""
	if width == 1280 {
		suffix = "-720p"
	}
	target = filepath.Join(out, s.name+suffix+".mp4")
	fmt.Printf("START %s %dx%d/30 %ds workers=%d\n", s.name, width, height, seconds, workers)
	started := time.Now()
	var decoder *mediaproc.Cmd
	var reader io.ReadCloser
	var decoderWait func() error
	var lock sync.Mutex
	next := 0
	pending := map[int]*image.RGBA{}
	if programme != "" && s.name == "late-night-overlay" {
		decoder = mediaproc.CommandContext(ctx, mediaproc.EncodeTimeout, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-stream_loop", "-1", "-i", programme, "-an", "-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,fps=30", width, height, width, height), "-t", strconv.Itoa(seconds), "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1")
		reader, err = decoder.StdoutPipe()
		if err != nil {
			return err
		}
		decoder.Stderr = os.Stderr
		if err = decoder.Start(); err != nil {
			return err
		}
		decoderWait = decoder.Wait
		defer func() {
			reader.Close()
			if decoder.ProcessState == nil {
				_ = decoder.Cancel()
				_ = decoder.Wait()
			}
		}()
	} else if s.name == "late-night-overlay" {
		return fmt.Errorf("overlay requires -programme")
	}
	var bug scene.Document
	if reader != nil {
		bug, err = families.Get("late-night", "bug")
		if err != nil {
			return err
		}
	}
	frames := func(i int) *image.RGBA {
		at := math.Mod(float64(i)/30, doc.Duration)
		over := raster.Render(doc, b, at, width, height)
		if reader != nil {
			lock.Lock()
			for next <= i {
				im := image.NewRGBA(image.Rect(0, 0, width, height))
				if _, e := io.ReadFull(reader, im.Pix); e != nil {
					lock.Unlock()
					panic(e)
				}
				pending[next] = im
				next++
			}
			base := pending[i]
			delete(pending, i)
			lock.Unlock()
			draw.Draw(base, base.Bounds(), raster.Render(bug, b, at, width, height), image.Point{}, draw.Over)
			draw.Draw(base, base.Bounds(), over, image.Point{}, draw.Over)
			over = base
		}
		if i%120 == 0 {
			fmt.Printf("FRAME %s %d/%d wall=%s\n", s.name, i, seconds*30, time.Since(started).Round(time.Second))
		}
		return over
	}
	audio := doc.Audio
	if audio == nil {
		audio = &scene.Audio{Generator: "bed", Seed: 7}
	}
	audioPath := ""
	if programme != "" && s.name == "late-night-overlay" {
		f, e := os.CreateTemp(out, ".overlay-programme-*.wav")
		if e != nil {
			return e
		}
		audioPath = f.Name()
		f.Close()
		defer os.Remove(audioPath)
		output, e := mediaproc.CommandContext(ctx, mediaproc.EncodeTimeout, ffmpeg, "-nostdin", "-v", "error", "-y", "-stream_loop", "-1", "-i", programme, "-vn", "-t", strconv.Itoa(seconds), "-ar", "48000", "-ac", "2", "-c:a", "pcm_s16le", audioPath).CombinedOutput()
		if e != nil {
			return fmt.Errorf("programme audio: %w %s", e, output)
		}
	}
	err = encode.WithOptions(ctx, ffmpeg, frames, seconds*30, 30, audioPath, target, encode.Options{Workers: workers, Width: width, Height: height, Audio: audio, MaxRate: "8M", BufferSize: "8M"})
	if err != nil {
		return err
	}
	if decoderWait != nil {
		reader.Close()
		if err = decoderWait(); err != nil {
			return err
		}
	}
	if err = metadata(ctx, target, s.caption); err != nil {
		return err
	}
	fmt.Printf("DONE %s wall=%s\n", target, time.Since(started).Round(time.Second))
	return nil
}

func metadata(ctx context.Context, path, caption string) error {
	raw, err := mediaproc.CommandContext(ctx, mediaproc.ProbeTimeout, "ffprobe", "-v", "error", "-show_entries", "format=duration,bit_rate:stream=codec_type,width,height:packet=pts_time,dts_time,size", "-show_packets", "-of", "json", path).Output()
	if err != nil {
		return err
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			Type          string `json:"codec_type"`
			Width, Height int
		} `json:"streams"`
		Packets []struct {
			PTS  string `json:"pts_time"`
			DTS  string `json:"dts_time"`
			Size string `json:"size"`
		} `json:"packets"`
	}
	if err = json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	type packet struct {
		at    float64
		bytes int
	}
	var packets []packet
	for _, p := range probe.Packets {
		at, e := strconv.ParseFloat(p.DTS, 64)
		if e != nil {
			at, _ = strconv.ParseFloat(p.PTS, 64)
		}
		n, _ := strconv.Atoi(p.Size)
		packets = append(packets, packet{at, n})
	}
	sort.Slice(packets, func(i, j int) bool { return packets[i].at < packets[j].at })
	left, total, peak := 0, 0, 0
	for right, p := range packets {
		total += p.bytes
		for left < right && packets[left].at <= p.at-1 {
			total -= packets[left].bytes
			left++
		}
		peak = max(peak, total)
	}
	duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	bitrate, _ := strconv.ParseFloat(probe.Format.BitRate, 64)
	data := struct {
		Caption    string  `json:"caption"`
		Resolution string  `json:"resolution"`
		Bitrate    float64 `json:"bitrate_kbps"`
		Peak       float64 `json:"peak_1s_kbps"`
		Duration   float64 `json:"duration_s"`
	}{caption, fmt.Sprintf("%dx%d", probe.Streams[0].Width, probe.Streams[0].Height), math.Round(bitrate/100) / 10, math.Round(float64(peak)*8/100) / 10, duration}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path[:len(path)-4]+".json", append(encoded, '\n'), 0644)
}

// plates deliberately omits post FX: the live engine owns grain/scanlines/flicker.
func plates(out string) error {
	if err := os.MkdirAll(out, 0755); err != nil {
		return err
	}
	type asset struct {
		Name, Caption, Plate, Overlay string
		Width, Height                 int
	}
	var manifest []asset
	for _, width := range []int{1920, 1280} {
		height := width * 9 / 16
		suffix := fmt.Sprintf("-%dx%d", width, height)
		for _, moment := range []string{"bars", "brb", "testcard", "card"} {
			doc, err := families.Get("signal", "screen."+moment)
			if err != nil {
				return err
			}
			base := doc
			base.Post = nil
			base.Layers = nil
			overlay := doc
			overlay.Post = nil
			overlay.Layers = nil
			// All copy lives in overlays; plates remain reusable across channel/copy changes.
			var split func([]scene.Layer) ([]scene.Layer, []scene.Layer)
			split = func(layers []scene.Layer) ([]scene.Layer, []scene.Layer) {
				var graphics, text []scene.Layer
				for _, l := range layers {
					if l.Type == "text" {
						text = append(text, l)
						continue
					}
					if l.Type == "group" {
						g, t := split(l.Layers)
						if len(g) > 0 {
							a := l
							a.Layers = g
							graphics = append(graphics, a)
						}
						if len(t) > 0 {
							a := l
							a.Layers = t
							text = append(text, a)
						}
					} else {
						graphics = append(graphics, l)
					}
				}
				return graphics, text
			}
			base.Layers, overlay.Layers = split(doc.Layers)
			bindings := raster.Bindings{Bindings: families.Defaults("signal")}
			bindings.Slots["channel.name"] = "Nocturne 7"
			bindings.Slots["station.id"] = "NOCTURNE 7"
			plateName := "screen-" + moment + "-plate" + suffix + ".png"
			write := func(name string, scene scene.Document) error {
				f, e := os.Create(filepath.Join(out, name))
				if e != nil {
					return e
				}
				e = png.Encode(f, rgbaPNG{raster.Render(scene, bindings, scene.Poster, width, height)})
				if c := f.Close(); e == nil {
					e = c
				}
				return e
			}
			if err = write(plateName, base); err != nil {
				return err
			}
			variants := []struct{ name, line1, line2 string }{{moment, "", ""}}
			if moment == "card" {
				variants = []struct{ name, line1, line2 string }{{"card-back-at-6pm", "Back at 6:00 PM", "A little pause. More good television this evening."}, {"card-reconnecting", "Reconnecting…", "Your programme will return shortly."}, {"card-waiting", "Waiting for a free connection", "We’ll bring your programme back as soon as a connection is available."}}
			}
			for _, v := range variants {
				if v.line1 != "" {
					bindings.Slots["screen.line1"] = v.line1
					bindings.Slots["screen.line2"] = v.line2
				}
				name := "screen-" + v.name + "-text" + suffix + ".png"
				if err = write(name, overlay); err != nil {
					return err
				}
				caption := v.line1
				if caption == "" {
					caption = map[string]string{"bars": "Nocturne 7 · colour, sound, vision", "brb": "We’ll be right back · Nocturne 7", "testcard": "Nocturne 7 · transmission check"}[moment]
				}
				manifest = append(manifest, asset{v.name, caption, plateName, name, width, height})
			}
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "manifest.json"), append(data, '\n'), 0644)
}

// Retain an explicit alpha channel even when every plate pixel is opaque.
type rgbaPNG struct{ image.Image }

func (rgbaPNG) Opaque() bool            { return false }
func (rgbaPNG) ColorModel() color.Model { return color.NRGBAModel }
