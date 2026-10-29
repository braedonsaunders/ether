package mediabrowser

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestDetailSourceOmitsEstimatedMediaFacts(t *testing.T) {
	media := frontend.MediaInfo{Estimated: true, Container: "mkv", Size: 1000, BitrateKbps: 5000, Duration: time.Hour, Streams: []frontend.StreamInfo{{Type: "video", Codec: "h264", Width: 1920}}}
	for _, dialect := range []string{"jellyfin", "emby"} {
		i := &instance{protocol: frontend.Protocol(dialect)}
		got := i.detailMediaSourceDTO("scoped", media)
		for _, field := range []string{"Container", "RunTimeTicks", "Size", "Bitrate", "Chapters"} {
			if _, ok := got[field]; ok {
				t.Fatalf("%s: estimated %s exposed: %#v", dialect, field, got[field])
			}
		}
		if streams, ok := got["MediaStreams"].([]any); !ok || len(streams) != 0 {
			t.Fatalf("%s: streams = %#v", dialect, got["MediaStreams"])
		}
		media.Estimated = false
		actual := i.detailMediaSourceDTO("scoped", media)
		if actual["Container"] != "mkv" || actual["Size"] != int64(1000) || len(actual["MediaStreams"].([]any)) != 1 {
			t.Fatalf("%s: authoritative source lost: %#v", dialect, actual)
		}
		media.Estimated = true
	}
}

func TestEmbyDetailOmitsEstimatedVersionFacts(t *testing.T) {
	parts := 1
	detail := frontend.ItemDetail{Versions: []frontend.Version{{FileName: "movie.mkv", PartCount: &parts, Media: frontend.MediaInfo{Estimated: true, Container: "mkv", Size: 1000, BitrateKbps: 5000, Streams: []frontend.StreamInfo{{Type: "video", Width: 1920, Height: 1080}}}}}}
	got := map[string]any{}
	(&instance{protocol: "emby"}).embyItemDetailDTO(got, detail)
	if got["FileName"] != "movie.mkv" || got["PartCount"] != 1 {
		t.Fatal(got)
	}
	for _, field := range []string{"Container", "Size", "Bitrate", "Width", "Height", "Chapters"} {
		if _, ok := got[field]; ok {
			t.Fatalf("estimated %s exposed: %#v", field, got[field])
		}
	}
	source := map[string]any{}
	(&instance{protocol: "emby"}).embyMediaSourceDTO(source, "item", detail.Versions[0].Media)
	if _, ok := source["MimeType"]; ok {
		t.Fatal("estimated source MIME type exposed", source)
	}
}
