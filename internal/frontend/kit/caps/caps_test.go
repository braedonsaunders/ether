package caps

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestBuildLayers(t *testing.T) {
	b := Builder{Learn: func(_ context.Context, c core.ClientCaps) (core.ClientCaps, error) { c.MaxHeight = 1080; return c, nil }, Rules: func(_ context.Context, c core.ClientCaps) (core.ClientCaps, error) {
		if c.MaxHeight != 1080 {
			t.Fatal("learning was not first")
		}
		c.MaxHeight = 720
		return c, nil
	}}
	c, err := b.Build(context.Background(), DeviceInfo{VideoCodecs: []string{"H264"}}, DeviceProfile{MaxStreamingBitrate: 8000000, DirectPlayProfiles: []DirectPlayProfile{{Container: "mp4,mkv", VideoCodec: "h264,hevc", AudioCodec: "aac"}}}, []string{"http-get:*:video/mp2t:*"}, "App/1")
	if err != nil || c.Product != "App/1" || c.MaxHeight != 720 || c.MaxBitrateKbps != 8000 || !c.SupportsVideo("hevc") || !c.SupportsContainer("mpegts") || len(c.VideoCodecs) != 2 {
		t.Fatal(c, err)
	}
}
func TestLayerFailure(t *testing.T) {
	want := errors.New("failed")
	b := Builder{Learn: func(context.Context, core.ClientCaps) (core.ClientCaps, error) { return core.ClientCaps{}, want }, Rules: func(context.Context, core.ClientCaps) (core.ClientCaps, error) {
		t.Fatal("rules called")
		return core.ClientCaps{}, nil
	}}
	_, err := b.Build(context.Background(), DeviceInfo{}, DeviceProfile{}, nil, "")
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}
