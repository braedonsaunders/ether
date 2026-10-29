package mediabrowser

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestEmbyStreamUsesKnownSampleAspectRatio(t *testing.T) {
	i := &instance{protocol: "emby"}
	streams := []frontend.StreamInfo{{Type: "video", Width: 720, Height: 480, SampleAspectRatio: "8:9"}, {Type: "video", Width: 1920, Height: 1080}}
	result := i.mediaStreams(streams)
	first := result[0].(map[string]any)
	if first["AspectRatio"] != "4:3" {
		t.Fatalf("aspect ratio: %#v", first)
	}
	if _, ok := first["IsOriginal"]; ok {
		t.Fatal("Emby stream contains Jellyfin-only field")
	}
	if _, ok := result[1].(map[string]any)["AspectRatio"]; ok {
		t.Fatal("unknown aspect ratio was invented")
	}
	if _, ok := (&instance{protocol: "jellyfin"}).mediaStreams(streams)[0].(map[string]any)["IsOriginal"]; !ok {
		t.Fatal("Jellyfin stream lost its field")
	}
}
