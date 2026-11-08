package plex

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestConvertSurroundAudioPlexAndroidMobile(t *testing.T) {
	f := newFixture(func(d *Deps) {
		baseConfig := d.Config
		d.Config = func() config.Config {
			c := baseConfig()
			c.Quality = config.DefaultQuality()
			c.Quality.Rules = []config.QualityRule{{
				Protocol:             "plex",
				Product:              "Plex for Android",
				Profile:              "4K",
				ConvertSurroundAudio: true,
			}}
			return c
		}
	})
	f.srv.d.Transcoder = &realDecider{f.trans}
	defer f.srv.Close()

	f.playback.info = core.MediaInfo{
		Container: "mkv",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "eng", Channels: 6, Default: true},
		},
	}

	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3,ac3,dts,flac)+add-transcode-target(type=videoProfile&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=mp3,aac,ac3,eac3,dts,flac,vorbis)"

	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&hasMDE=1&directPlay=1&protocol=hls&session=convert-surround",
		append(jsonAccept,
			"X-Plex-Product", "Plex for Android (Mobile)",
			"X-Plex-Platform", "Android",
			"X-Plex-Client-Identifier", "android-pixel9",
			"X-Plex-Client-Profile-Extra", extra,
		)...,
	)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mc := decodeJSON(t, rec)
	if mc["transcodeDecisionCode"] != float64(1001) {
		t.Fatalf("expected transcodeDecisionCode 1001, got %v (%s)", mc["transcodeDecisionCode"], rec.Body.String())
	}
	if mc["mdeDecisionCode"] == float64(1000) {
		t.Fatalf("expected mdeDecisionCode not to be 1000 (direct play), got %v", mc["mdeDecisionCode"])
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"codec":"aac"`) && !strings.Contains(body, `codec="aac"`) {
		t.Fatalf("expected audio converted to aac in decision body:\n%s", body)
	}
}

func TestConvertSurroundAudioDisabledPreservesSurround(t *testing.T) {
	f := newFixture(func(d *Deps) {
		baseConfig := d.Config
		d.Config = func() config.Config {
			c := baseConfig()
			c.Quality.Rules = []config.QualityRule{{
				Protocol:             "plex",
				Product:              "Plex for Android",
				Profile:              "4K",
				ConvertSurroundAudio: false,
			}}
			return c
		}
	})
	f.srv.d.Transcoder = &realDecider{f.trans}
	defer f.srv.Close()

	f.playback.info = core.MediaInfo{
		Container: "mkv",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "eac3", Language: "eng", Channels: 6, Default: true},
		},
	}

	extra := "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3,ac3,dts,flac)+add-transcode-target(type=videoProfile&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=mp3,aac,ac3,eac3,dts,flac,vorbis)"

	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&hasMDE=1&directPlay=1&protocol=hls&session=preserve-surround",
		append(jsonAccept,
			"X-Plex-Product", "Plex for Android (Mobile)",
			"X-Plex-Platform", "Android",
			"X-Plex-Client-Identifier", "android-pixel9",
			"X-Plex-Client-Profile-Extra", extra,
		)...,
	)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mc := decodeJSON(t, rec)
	if mc["mdeDecisionCode"] != float64(1000) {
		t.Fatalf("expected mdeDecisionCode 1000 (direct play), got %v (%s)", mc["mdeDecisionCode"], rec.Body.String())
	}
}
