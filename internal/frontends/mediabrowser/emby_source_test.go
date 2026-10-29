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
