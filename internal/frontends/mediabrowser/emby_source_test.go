package mediabrowser

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestEmbyMediaSourceKnownIdentityAndMIME(t *testing.T) {
	i := &instance{}
	source := map[string]any{}
	i.embyMediaSourceDTO(source, "item", frontend.MediaInfo{Container: "mp4"})
	if source["ItemId"] != "item" {
		t.Fatal(source)
	}
	if _, ok := source["MimeType"]; ok {
		t.Fatal("MIME type inferred without a video stream")
	}
	i.embyMediaSourceDTO(source, "item", frontend.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Type: "video"}}})
	if source["MimeType"] != "video/mp4" {
		t.Fatal(source)
	}
}

func TestEmbyDirectURLNeverRequiresAccountCredential(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"https://ether.example/emby/sessions/lazy/token_1/file", true},
		{"https://ether.example/emby/sessions/media/session/token/file", true},
		{"/emby/sessions/media/session/token/file", true},
		{"/Videos/item/stream", false},
		{"https://ether.example/emby/sessions/lazy/token/file?api_key=secret", false},
		{"https://other.example/sessions/media/../token/file", false},
		{"https://ether.example/emby/sessions/media/session/token/index.m3u8", false},
	} {
		source := map[string]any{"Path": tc.path}
		(&instance{}).embyMediaSourceDTO(source, "item", frontend.MediaInfo{})
		got, present := source["AddApiKeyToDirectStreamUrl"]
		if present != tc.want || present && got != false {
			t.Errorf("%s: %#v", tc.path, source)
		}
	}
}

func TestEmbySourceDefaultAudioUsesAuthoritativeDisposition(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		estimated bool
		streams   []core.StreamInfo
		want      int
		present   bool
	}{
		{"explicit default", false, []core.StreamInfo{{Type: "video", Index: 0}, {Type: "audio", Index: 1, DefaultDisposition: &yes}}, 1, true},
		{"legacy default", false, []core.StreamInfo{{Type: "audio", Index: 0, Default: true}}, 0, true},
		{"explicit false overrides", false, []core.StreamInfo{{Type: "audio", Index: 1, Default: true, DefaultDisposition: &no}}, 0, false},
		{"multiple defaults", false, []core.StreamInfo{{Type: "audio", Index: 1, DefaultDisposition: &yes}, {Type: "audio", Index: 2, DefaultDisposition: &yes}}, 0, false},
		{"unknown", false, []core.StreamInfo{{Type: "audio", Index: 1}}, 0, false},
		{"estimated", true, []core.StreamInfo{{Type: "audio", Index: 1, DefaultDisposition: &yes}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := map[string]any{"DefaultAudioStreamIndex": 999}
			(&instance{}).embyMediaSourceDTO(source, "item", frontend.MediaInfo{Estimated: tc.estimated, Streams: tc.streams})
			got, ok := source["DefaultAudioStreamIndex"]
			if ok != tc.present || ok && got != tc.want {
				t.Fatalf("got %#v, want %d present=%v", got, tc.want, tc.present)
			}
		})
	}
}
