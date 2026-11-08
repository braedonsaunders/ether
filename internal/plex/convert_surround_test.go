package plex

import (
	"context"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type testDecideTranscoder struct{}

func (testDecideTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) { return nil, nil }
func (testDecideTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}
func (testDecideTranscoder) StartHLS(context.Context, core.HLSRequest) (core.HLSSession, error) {
	return nil, nil
}
func (testDecideTranscoder) Session(string) (core.HLSSession, bool) { return nil, false }
func (testDecideTranscoder) Stop(string)                            {}
func (testDecideTranscoder) HWAccel() core.HWAccel                  { return core.HWAccel{} }

func TestConvertSurroundAudioPlexAndroidMobile(t *testing.T) {
	q := config.DefaultQuality()
	q.Rules = []config.QualityRule{{
		Protocol:             "plex",
		Product:              "Plex for Android",
		Profile:              "4K",
		ConvertSurroundAudio: true,
	}}

	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3,ac3,dts,flac)+add-transcode-target(type=videoProfile&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=mp3,aac,ac3,eac3,dts,flac,vorbis)"

	pr := &Request{
		Product:      "Plex for Android (Mobile)",
		Platform:     "Android",
		Device:       "Pixel 9 Pro",
		ProfileExtra: extra,
	}

	caps := pr.ClientCaps(0)
	applied := resolver.ApplyClientRulesForMedia(q, caps, nil, core.MediaTV)
	if !applied.ConvertSurroundAudio {
		t.Fatalf("expected ConvertSurroundAudio to be true on applied caps")
	}
	if applied.SupportsAudio("eac3") {
		t.Fatalf("expected applied caps to not support eac3 when ConvertSurroundAudio is true")
	}

	p := &playRequest{
		caps:     applied,
		protocol: "hls",
		opts: core.DecideOptions{
			DirectPlay:   true,
			DirectStream: true,
		},
	}

	p.caps.TranscodeAudioCodecs = streamingAudioTarget(pr.ProfileExtra, p.protocol)
	if p.caps.ConvertSurroundAudio {
		p.caps.TranscodeAudioCodecs = core.FilterOutSurroundAudio(p.caps.TranscodeAudioCodecs)
	}
	if len(p.caps.AudioCodecs) > 0 && len(p.caps.TranscodeAudioCodecs) > 0 {
		var kept []string
		for _, c := range p.caps.TranscodeAudioCodecs {
			if p.caps.SupportsAudio(c) {
				kept = append(kept, c)
			}
		}
		p.caps.TranscodeAudioCodecs = kept
	}

	srv := &Server{}
	info := core.MediaInfo{
		Container:   "mkv",
		BitrateKbps: 5000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Channels: 6},
		},
	}
	ps := &core.PlaySession{
		Info: info,
		Resolution: &core.Resolution{
			Release: core.Release{ID: "nzb-123", Protocol: "usenet"},
		},
	}
	title := &core.Title{Key: 4002133750010001, Type: "episode"}
	err := srv.applyDeviceProfile(pr, title, ps, p)
	if err != nil {
		t.Fatalf("applyDeviceProfile failed: %v", err)
	}

	if p.opts.DirectPlay {
		t.Fatalf("expected DirectPlay to be false when ConvertSurroundAudio is active for surround audio source")
	}
	if slices.Contains(p.caps.AudioCodecs, "eac3") {
		t.Fatalf("AudioCodecs must not contain eac3, got: %v", p.caps.AudioCodecs)
	}
	if slices.Contains(p.caps.TranscodeAudioCodecs, "eac3") {
		t.Fatalf("TranscodeAudioCodecs must not contain eac3, got: %v", p.caps.TranscodeAudioCodecs)
	}

	sessDec, err := sessions.Decide(nil, testDecideTranscoder{}, sessions.DecisionRequest{
		Session:     ps,
		Caps:        p.caps,
		Options:     p.opts,
		PreferRemux: true,
		Protocol:    "hls",
	})
	if err != nil {
		t.Fatalf("sessions.Decide failed: %v", err)
	}

	if sessDec.Kind != core.DecisionRemux {
		t.Fatalf("expected DecisionRemux, got: %v", sessDec.Kind)
	}
	if !sessDec.VideoCopy {
		t.Fatalf("expected VideoCopy to be true, got false")
	}
	if sessDec.AudioCopy {
		t.Fatalf("expected AudioCopy to be false (audio transcoded to AAC), got true")
	}
	if sessDec.AudioCodec != "aac" {
		t.Fatalf("expected AudioCodec aac, got: %v", sessDec.AudioCodec)
	}
	t.Logf("Decision Reason: %s", sessDec.Reason)
}

func TestConvertSurroundAudioDisabledPreservesSurround(t *testing.T) {
	q := config.DefaultQuality()
	q.Rules = []config.QualityRule{{
		Protocol:             "plex",
		Product:              "Plex for Android",
		Profile:              "4K",
		ConvertSurroundAudio: false,
	}}

	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3,ac3,dts,flac)+add-transcode-target(type=videoProfile&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=mp3,aac,ac3,eac3,dts,flac,vorbis)"

	pr := &Request{
		Product:      "Plex for Android (Mobile)",
		Platform:     "Android",
		Device:       "Pixel 9 Pro",
		ProfileExtra: extra,
	}

	caps := pr.ClientCaps(0)
	applied := resolver.ApplyClientRulesForMedia(q, caps, nil, core.MediaTV)
	if applied.ConvertSurroundAudio {
		t.Fatalf("expected ConvertSurroundAudio to be false")
	}

	p := &playRequest{
		caps:     applied,
		protocol: "hls",
		opts: core.DecideOptions{
			DirectPlay:   true,
			DirectStream: true,
		},
	}

	srv := &Server{}
	info := core.MediaInfo{
		Container:   "mkv",
		BitrateKbps: 5000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Channels: 6},
		},
	}
	ps := &core.PlaySession{
		Info: info,
		Resolution: &core.Resolution{
			Release: core.Release{ID: "nzb-123", Protocol: "usenet"},
		},
	}
	title := &core.Title{Key: 4002133750010001, Type: "episode"}
	if err := srv.applyDeviceProfile(pr, title, ps, p); err != nil {
		t.Fatalf("applyDeviceProfile failed: %v", err)
	}

	if !p.opts.DirectPlay {
		t.Fatalf("expected DirectPlay to remain true when ConvertSurroundAudio is off")
	}
	if !slices.Contains(p.caps.AudioCodecs, "eac3") {
		t.Fatalf("expected AudioCodecs to include eac3, got: %v", p.caps.AudioCodecs)
	}
}
