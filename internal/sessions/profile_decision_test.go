package sessions

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type profileTranscoder struct{ fakeBrokerTranscoder }

func (profileTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}

func TestClientProfilePairsAndDelivery(t *testing.T) {
	info := core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}, {Index: 1, Type: "audio", Codec: "aac"}}}
	d := core.Decision{Kind: core.DecisionDirectPlay, AudioStream: 1}
	p := &core.ClientCapabilityProfiles{DirectPlay: []core.ClientDirectPlayProfile{{MediaType: "video", Containers: []string{"mkv"}, VideoCodecs: []string{"hevc"}, AudioCodecs: []string{"aac"}}, {MediaType: "video", Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}}}
	if profileDirectAllowed(p, info, d) {
		t.Fatal("accepted a cross-profile container and codec combination")
	}
	p.DirectPlay[1].VideoCodecs = []string{"hevc"}
	if !profileDirectAllowed(p, info, d) {
		t.Fatal("rejected declared direct-play pair")
	}
	p.Codecs = []core.ClientCodecProfile{{MediaType: "video", Codecs: []string{"hevc"}, Conditions: []core.ClientProfileCondition{{Property: "future", Operator: "Equals", Value: "1", Required: true}}}}
	if profileDirectAllowed(p, info, d) {
		t.Fatal("accepted unknown required condition")
	}
	p.Codecs[0].Conditions = []core.ClientProfileCondition{{Property: "VideoWidth", Operator: "LessThanEqual", Value: "1920", Required: true}}
	if !profileDirectAllowed(p, info, d) {
		t.Fatal("rejected supported passing condition")
	}
	p.Codecs[0].Conditions[0].Value = "1280"
	if profileDirectAllowed(p, info, d) {
		t.Fatal("accepted failed required condition")
	}
	p.Codecs[0].ApplyConditions = []core.ClientProfileCondition{{Property: "VideoWidth", Operator: "LessThanEqual", Value: "1280", Required: true}}
	if !profileDirectAllowed(p, info, d) {
		t.Fatal("enforced an inapplicable codec profile")
	}
	p.Codecs[0].ApplyConditions[0].Property = "unknown"
	if profileDirectAllowed(p, info, d) {
		t.Fatal("accepted an unknown required applicability condition")
	}
	p.Codecs = nil
	p.Transcoding = []core.ClientTranscodeProfile{{MediaType: "video", Delivery: "dash", Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}}
	out := core.Decision{Kind: core.DecisionTranscode, Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"}
	if profileDeliveryAllowed(p, out, info, "hls") || !profileDeliveryAllowed(p, out, info, "dash") {
		t.Fatal("accepted undeclared delivery or rejected declared delivery")
	}
	p.Transcoding[0].MaxAudioChannels = "2"
	out.AudioCopy = true
	out.AudioStream = 1
	info.Streams[1].Channels = 6
	if profileDeliveryAllowed(p, out, info, "dash") {
		t.Fatal("accepted output exceeding declared audio channels")
	}
	info.Streams[1].Channels = 2
	if !profileDeliveryAllowed(p, out, info, "dash") {
		t.Fatal("rejected output within declared audio channels")
	}
	if !profileDirectAllowed(nil, info, d) || !profileDeliveryAllowed(nil, out, info, "hls") {
		t.Fatal("legacy nil profile path changed")
	}
}

func TestProfileDecisionChoosesDeclaredFallbackAndSubtitle(t *testing.T) {
	info := core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}, {Index: 1, Type: "audio", Codec: "aac"}, {Index: 2, Type: "subtitle", Codec: "hdmv_pgs_subtitle"}}}
	profiles := &core.ClientCapabilityProfiles{DirectPlay: []core.ClientDirectPlayProfile{{MediaType: "video", Containers: []string{"mkv"}, VideoCodecs: []string{"hevc"}, AudioCodecs: []string{"aac"}}}, Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", Delivery: "hls", Containers: []string{"mpegts"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}}, Subtitles: []core.ClientSubtitleProfile{}}
	caps := core.ClientCaps{Profiles: profiles, Containers: []string{"mp4", "mkv"}, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac"}, Device: "NVIDIA Shield"}
	applySubtitleProfiles(&caps)
	req := frontend.PlanRequest{Caps: caps, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 1, SubtitleStream: 2}}}
	b := &Broker{deps: BrokerDeps{Transcoder: profileTranscoder{}}}
	d, err := b.decideProfile(&core.PlaySession{Info: info}, req)
	if err != nil || d.Kind != core.DecisionTranscode || d.VideoCodec != "h264" || !d.BurnSubtitles {
		t.Fatalf("decision %+v err=%v", d, err)
	}
	if decisionEncodes(core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: false, AudioStream: 1}, info) != true {
		t.Fatal("audio conversion must count as transcoding")
	}
}
