package caps

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"slices"
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

func TestDeviceProfilesPreservePairsAndConditions(t *testing.T) {
	var p DeviceProfile
	input := `{"DirectPlayProfiles":[{"Type":"Video","Container":"mkv","VideoCodec":"hevc","AudioCodec":"aac,ac3"},{"Type":"Video","Container":"mp4","VideoCodec":"h264","AudioCodec":"aac"}],"TranscodingProfiles":[{"Type":"Video","Container":"ts","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls","SegmentLength":6,"Conditions":[{"Condition":"Equals","Property":"FutureCodecLimit","Value":"x"}]}],"SubtitleProfiles":[{"Format":"srt","Method":"External","Language":"en","Container":"mp4"}],"CodecProfiles":[{"Type":"Video","Codec":"h264","Conditions":[{"Condition":"LessThanEqual","Property":"VideoLevel","Value":"52","IsRequired":false}]}],"ContainerProfiles":[],"MaxStaticBitrate":12000000}`
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatal(err)
	}
	got, err := (Builder{}).Build(context.Background(), DeviceInfo{}, p, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Profiles == nil || len(got.Profiles.DirectPlay) != 2 {
		t.Fatal(got.Profiles)
	}
	first, second := got.Profiles.DirectPlay[0], got.Profiles.DirectPlay[1]
	if !slices.Equal(first.Containers, []string{"mkv"}) || !slices.Equal(first.VideoCodecs, []string{"hevc"}) || !slices.Equal(second.Containers, []string{"mp4"}) || !slices.Equal(second.VideoCodecs, []string{"h264"}) {
		t.Fatal(first, second)
	}
	if len(got.Profiles.Transcoding) != 1 || got.Profiles.Transcoding[0].Delivery != "hls" || got.Profiles.Transcoding[0].SegmentLength != 6 || !got.Profiles.Transcoding[0].Conditions[0].Required {
		t.Fatal(got.Profiles.Transcoding)
	}
	if got.Profiles.Codecs[0].Conditions[0].Required || got.Profiles.Subtitles[0].Method != "external" || got.Profiles.Containers == nil || got.Profiles.MaxStaticBitrate != 12000000 {
		t.Fatal(got.Profiles)
	}
	empty, err := (Builder{}).Build(context.Background(), DeviceInfo{}, DeviceProfile{}, nil, "")
	if err != nil || empty.Profiles != nil {
		t.Fatal(empty, err)
	}
}
