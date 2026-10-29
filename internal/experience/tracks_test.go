package experience

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestPickAudioPreferencesAndFallback(t *testing.T) {
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "subtitle"}, {Type: "audio", Language: "fr", Default: true}, {Type: "audio", Language: "pt-PT"}, {Type: "audio", Language: "pt-BR"}}}
	if got := PickAudio(info, []string{"pt-BR", "fr"}); got != 3 {
		t.Fatalf("exact preference: %d", got)
	}
	if got := PickAudio(info, []string{"de"}); got != 1 {
		t.Fatalf("default: %d", got)
	}
	info.Streams[1].Default = false
	if got := PickAudio(info, nil); got != 1 {
		t.Fatalf("first: %d", got)
	}
	if got := PickAudio(core.MediaInfo{}, nil); got != -1 {
		t.Fatalf("none: %d", got)
	}
}

func TestResolveSubtitlePreservesIdentityAndFallback(t *testing.T) {
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "subtitle", Language: "en", Index: 5}, {Type: "subtitle", Language: "en", External: true, ExternalURL: "sidecar", Forced: true}, {Type: "subtitle", Language: "en", Forced: true}}}
	embedded := &core.StreamInfo{Type: "subtitle", Language: "en", Index: 5}
	if got := ResolveSubtitle(embedded, 0, info); got != 0 {
		t.Fatalf("embedded: %d", got)
	}
	external := &core.StreamInfo{Type: "subtitle", Language: "fr", External: true, ExternalURL: "sidecar"}
	if got := ResolveSubtitle(external, -1, info); got != 1 {
		t.Fatalf("sidecar identity: %d", got)
	}
	forced := &core.StreamInfo{Type: "subtitle", Language: "en", Forced: true}
	if got := ResolveSubtitle(forced, -1, info); got != 2 {
		t.Fatalf("forced same kind: %d", got)
	}
	if got := ResolveSubtitle(nil, 1, info); got != 1 {
		t.Fatalf("position: %d", got)
	}
	if got := ResolveSubtitle(nil, 5, info); got != -1 {
		t.Fatalf("invalid: %d", got)
	}
}
