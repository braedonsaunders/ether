package sessions

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPublicMediaDetachesDispositionAndVisionFlags(t *testing.T) {
	yes := true
	source := core.MediaInfo{Streams: []core.StreamInfo{{Type: "subtitle", DefaultDisposition: &yes, ForcedDisposition: &yes, HearingImpaired: &yes, Original: &yes, VisualImpaired: &yes, Commentary: &yes, Dub: &yes, DolbyVisionBLPresent: &yes, ExternalURL: "secret"}}}
	visible := publicMedia(source)
	if visible.Streams[0].ExternalURL != "" {
		t.Fatal("source URL exposed")
	}
	*visible.Streams[0].HearingImpaired = false
	*visible.Streams[0].DefaultDisposition = false
	*visible.Streams[0].ForcedDisposition = false
	*visible.Streams[0].Original = false
	*visible.Streams[0].VisualImpaired = false
	*visible.Streams[0].Commentary = false
	*visible.Streams[0].Dub = false
	*visible.Streams[0].DolbyVisionBLPresent = false
	if !yes {
		t.Fatal("returned stream flags mutated source")
	}
}
