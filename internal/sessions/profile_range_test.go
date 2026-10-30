package sessions

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func TestDVContainerSignallingControlsRequiredDirectProfile(t *testing.T) {
	condition := core.ClientProfileCondition{Property: "VideoRangeType", Operator: "NotEquals", Value: "DOVI", Required: true}
	profiles := &core.ClientCapabilityProfiles{DirectPlay: []core.ClientDirectPlayProfile{{MediaType: "video", Containers: []string{"mp4"}, VideoCodecs: []string{"hevc"}, AudioCodecs: []string{"ac3"}}}, Codecs: []core.ClientCodecProfile{{MediaType: "video", Codecs: []string{"hevc"}, Conditions: []core.ClientProfileCondition{condition}}}}
	caps := core.ClientCaps{Profiles: profiles, Containers: []string{"mp4"}, VideoCodecs: []string{"hevc"}, AudioCodecs: []string{"ac3"}}
	req := frontend.PlanRequest{Caps: caps, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 1, SubtitleStream: -1}}}
	broker := &Broker{deps: BrokerDeps{Transcoder: profileTranscoder{}}}
	source := func(side string) core.MediaInfo {
		t.Helper()
		data := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","codec_tag_string":"hvc1","width":3840,"height":2160` + side + `},{"index":1,"codec_type":"audio","codec_name":"ac3","channels":6,"sample_rate":"48000"}],"format":{"format_name":"mov,mp4","duration":"60"}}`
		info, err := transcode.ParseProbe([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		return *info
	}
	absent := source("")
	d, err := broker.decideProfile(&core.PlaySession{Info: absent}, req)
	if err != nil || d.Kind != core.DecisionDirectPlay {
		t.Fatalf("probed absent: %+v %v", d, err)
	}
	present := source(`,"side_data_list":[{"side_data_type":"DOVI configuration record","dv_profile":8}]`)
	if _, err := broker.decideProfile(&core.PlaySession{Info: present}, req); err == nil {
		t.Fatal("DOVI record bypassed required condition")
	}
	unprobed := absent
	unprobed.Streams = append([]core.StreamInfo(nil), absent.Streams...)
	unprobed.Streams[0].DolbyVisionConfigurationPresent = nil
	if _, err := broker.decideProfile(&core.PlaySession{Info: unprobed}, req); err == nil {
		t.Fatal("unprobed range accepted")
	}
}
