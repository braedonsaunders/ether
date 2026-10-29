package sessions

import (
	"encoding/json"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestCopyRequestDetachesClientProfiles(t *testing.T) {
	yes := true
	p := &core.ClientCapabilityProfiles{
		DirectPlay:  []core.ClientDirectPlayProfile{{Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}},
		Transcoding: []core.ClientTranscodeProfile{{Containers: []string{"ts"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, Conditions: []core.ClientProfileCondition{{Value: "one"}}, BreakOnNonKeyFrames: &yes, AudioVBR: &yes}},
		Subtitles:   []core.ClientSubtitleProfile{{Format: "vtt"}},
		Containers:  []core.ClientContainerProfile{{Containers: []string{"mp4"}, SubContainers: []string{"x"}, Conditions: []core.ClientProfileCondition{{Value: "two"}}}},
		Codecs:      []core.ClientCodecProfile{{Codecs: []string{"h264"}, Containers: []string{"mp4"}, SubContainers: []string{"x"}, Conditions: []core.ClientProfileCondition{{Value: "three"}}, ApplyConditions: []core.ClientProfileCondition{{Value: "four"}}}},
	}
	copy := copyRequest(frontend.PlanRequest{Caps: core.ClientCaps{Profiles: p}}).Caps.Profiles
	want, err := json.Marshal(copy)
	if err != nil {
		t.Fatal(err)
	}
	p.DirectPlay[0].Containers[0], p.DirectPlay[0].VideoCodecs[0], p.DirectPlay[0].AudioCodecs[0] = "changed", "changed", "changed"
	p.Transcoding[0].Containers[0], p.Transcoding[0].VideoCodecs[0], p.Transcoding[0].AudioCodecs[0] = "changed", "changed", "changed"
	p.Transcoding[0].Conditions[0].Value, yes, p.Subtitles[0].Format = "changed", false, "changed"
	p.Containers[0].Containers[0], p.Containers[0].SubContainers[0], p.Containers[0].Conditions[0].Value = "changed", "changed", "changed"
	p.Codecs[0].Codecs[0], p.Codecs[0].Containers[0], p.Codecs[0].SubContainers[0] = "changed", "changed", "changed"
	p.Codecs[0].Conditions[0].Value, p.Codecs[0].ApplyConditions[0].Value = "changed", "changed"
	got, err := json.Marshal(copy)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("captured profiles changed: got %s, want %s", got, want)
	}
	empty := cloneClientProfiles(&core.ClientCapabilityProfiles{DirectPlay: []core.ClientDirectPlayProfile{}})
	if empty.DirectPlay == nil || empty.Transcoding != nil || cloneClientProfiles(nil) != nil {
		t.Fatal("nil and empty profile declarations were not preserved")
	}
}
