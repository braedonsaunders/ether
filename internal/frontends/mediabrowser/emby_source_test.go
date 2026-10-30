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
