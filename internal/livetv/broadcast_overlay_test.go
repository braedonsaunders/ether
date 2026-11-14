package livetv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type overlayAlwaysGraphics struct{}

func (overlayAlwaysGraphics) Duration(string, string) (time.Duration, bool) {
	return 8 * time.Second, true
}
func (overlayAlwaysGraphics) Clip(context.Context, core.ClipRequest) (core.ClipRef, bool) {
	return core.ClipRef{}, false
}
func (overlayAlwaysGraphics) Ensure(context.Context, core.ClipRequest) {}
func (overlayAlwaysGraphics) Overlay(context.Context, core.ClipRequest) (scene.Document, raster.Bindings, bool) {
	doc, _ := families.Get("clean", "bug")
	return doc, raster.Bindings{Bindings: families.Defaults("clean")}, true
}

// The owner's Overlays switch ("" is Off, the default) must actually gate
// whether the corner logo/lower-third render: prepareProgrammeOverlays used
// to only check that a Look existed, so Off still burned both in.
func TestPrepareProgrammeOverlaysRespectsOffToggle(t *testing.T) {
	s := testService(t, Options{Graphics: overlayAlwaysGraphics{}})
	c := testChannel()
	c.Look = &ChannelLook{LookID: "clean"}
	programme := Programme{Item: Item{Key: core.MovieKey(1)}}

	planner := &scheduleBroadcastPlanner{s: s, channel: c, format: transcode.RawFormat{Width: 1920, Height: 1080}}
	if out := planner.prepareProgrammeOverlays(t.Context(), programme, nil); out != nil {
		t.Fatalf("Overlays off: expected no overlays, got %+v", out)
	}

	c.Look.Overlays = "always"
	planner = &scheduleBroadcastPlanner{s: s, channel: c, format: transcode.RawFormat{Width: 1920, Height: 1080}}
	if out := planner.prepareProgrammeOverlays(t.Context(), programme, nil); out == nil {
		t.Fatal("Overlays always: expected overlays to be prepared")
	}
}

func BenchmarkBroadcastOverlays(b *testing.B) {
	for _, height := range []int{720, 1080} {
		b.Run(fmt.Sprint(height), func(b *testing.B) {
			f := transcode.RawFormat{Width: height * 16 / 9, Height: height}
			overlays := &programmeOverlays{}
			bindings := raster.Bindings{Bindings: families.Defaults("clean")}
			bindings.Slots["channel.name"] = "MERIDIAN"
			bindings.Slots["now.title"] = "The Evening Programme"
			bindings.Slots["now.meta"] = "A shared continuous channel"
			for _, moment := range []string{"bug", "lowerthird"} {
				doc, err := families.Get("clean", moment)
				if err != nil {
					b.Fatal(err)
				}
				overlay := compactOverlay(raster.Render(doc, bindings, doc.Poster, f.Width, f.Height), f)
				if moment == "bug" {
					overlays.bug = overlay
				} else {
					overlays.lower = overlay
				}
			}
			raw := blackFrame(f, 0).Video
			video := make([]byte, len(raw))
			start := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				copy(video, raw)
				overlays.blend(video, f, start.Add(2*time.Second), start, start.Add(time.Hour))
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)*30/1e9, "CPU-cores@30fps")
		})
	}
}
