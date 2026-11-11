package plex

import (
	"net/url"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestLegacyProfileTargetAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, profile, extra string
		dv                   int
		want                 float64
	}{
		{"generic missing target", "Generic", "", 0, 4005},
		{"generic declared target", "Generic", "add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mpegts&videoCodec=h264&audioCodec=aac)", 0, 1001},
		{"DV8 ordinary conversion", "iOS", "", 8, 1001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			if tc.dv > 0 {
				f.playback.info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
				f.playback.info.Streams[0].Codec = "hevc"
				f.playback.info.Streams[0].DolbyVision = true
				f.playback.info.Streams[0].DolbyVisionProfile = tc.dv
			}
			rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&directPlay=0&protocol=hls", append(jsonAccept, "X-Plex-Client-Profile-Name", tc.profile, "X-Plex-Client-Profile-Extra", tc.extra)...)
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			mc := decodeJSON(t, rec)
			if mc["transcodeDecisionCode"] != tc.want {
				t.Fatalf("code: %v", mc)
			}
			if tc.want >= 2000 && mc["Metadata"] != nil {
				t.Fatalf("refusal leaked source rows: %v", mc["Metadata"])
			}
		})
	}
}

func TestLegacyProfileAudioAndLimitationOrder(t *testing.T) {
	f := newFixture()
	f.srv.setTranscoder(&realDecider{f.trans})
	defer f.srv.Close()
	f.playback.info = core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Profile: "High", BitDepth: 8, Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "aac", Language: "eng", Channels: 2, Default: true},
		{Index: 2, Type: "audio", Codec: "truehd", Language: "eng", Channels: 8},
	}}
	for _, tc := range []struct{ profile, contains string }{
		{"Android", "audio codec truehd"},
		{"Windows", "Direct play OK."},
	} {
		rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&directPlay=1&protocol=hls", append(jsonAccept, "X-Plex-Client-Profile-Name", tc.profile)...)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), tc.contains) {
			t.Fatalf("%s: %d %s", tc.profile, rec.Code, rec.Body.String())
		}
	}
	f.playback.info.Streams = f.playback.info.Streams[:2]
	f.playback.info.Streams[0].Profile = "High 10"
	f.playback.info.Streams[0].BitDepth = 10
	f2 := newFixture()
	defer f2.srv.Close()
	f2.playback.info = f.playback.info
	f2.srv.setTranscoder(&realDecider{f2.trans})
	rec := f2.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&directPlay=1&protocol=hls&session=limit", append(jsonAccept, "X-Plex-Client-Profile-Name", "Android")...)
	if !strings.Contains(rec.Body.String(), "video.profile limitation applies: high 10 != baseline|main|high [list]") {
		t.Fatalf("named limitation did not precede wildcard: %s", rec.Body.String())
	}
}

func TestMalformedProfileQueryRefusesBeforePreparing(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	base := "/video/:/transcode/universal/decision?path=/library/metadata/" + fightClub.Key.String() + "&X-Plex-Client-Profile-Extra="
	directive := "add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mpegts&videoCodec=h264&audioCodec=aac)"
	rec := f.get(t, base+directive)
	if rec.Code != 400 || f.playback.prepared != 0 {
		t.Fatalf("raw query: %d preparations=%d", rec.Code, f.playback.prepared)
	}
	if rec = f.get(t, base+url.QueryEscape(directive)); rec.Code != 200 {
		t.Fatalf("encoded query: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProfileIdentifierAndRendererRefusals(t *testing.T) {
	for _, name := range []string{"Raspberry PI", "Xbox 360"} {
		f := newFixture()
		f.playback.info = core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160}, {Index: 1, Type: "audio", Codec: "truehd", Channels: 8}}}
		rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&directPlay=1&protocol=http", append(jsonAccept, "X-Plex-Client-Profile-Name", name)...)
		f.srv.Close()
		if rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestDecisionResourceSessionUsesPlaybackIdentity(t *testing.T) {
	for _, tc := range []struct{ playback, client, want string }{{"player-session", "client-session", "player-session"}, {"", "client-session", "client-session"}, {"", "", ""}} {
		f := newFixture()
		rec := f.request(t, "GET", "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&session=encoder-only", map[string]string{"Accept": "application/json", "X-Plex-Session-Identifier": tc.playback, "X-Plex-Client-Identifier": tc.client})
		f.srv.Close()
		if rec.Code != 200 {
			t.Fatalf("%+v: %d %s", tc, rec.Code, rec.Body.String())
		}
		if got := decodeJSON(t, rec)["resourceSession"]; got != tc.want {
			t.Fatalf("%+v resourceSession=%v", tc, got)
		}
	}
}

func TestLegacyDecoderAudioSelectionKeepsExplicitChoice(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		f := newFixture()
		f.playback.info = core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080}, {Index: 1, Type: "audio", Codec: "eac3", Language: "eng", Channels: 6, Default: true}, {Index: 2, Type: "audio", Codec: "aac", Language: "eng", Channels: 2}}}
		if explicit {
			f.srv.selections.update(ownerUser.ID, int64(fightClub.Key), func(s *selection) { s.audio = 1; s.audioSet = true })
		}
		rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&directPlay=0", append(jsonAccept, "X-Plex-Client-Capabilities", "audioDecoders=aac")...)
		f.srv.Close()
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		want := 2
		if explicit {
			want = 1
		}
		if f.trans.lastOpts.AudioStream != want {
			t.Fatalf("explicit=%v audio=%d want%d", explicit, f.trans.lastOpts.AudioStream, want)
		}
	}
}

func TestMDEDecisionProjectsFirstVideoAndSelectedTracks(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160}, {Index: 1, Type: "video", Codec: "hevc", Width: 1920, Height: 1080, DolbyVision: true, DolbyVisionProfile: 7}, {Index: 2, Type: "audio", Codec: "aac", Language: "eng", Channels: 2}, {Index: 3, Type: "audio", Codec: "ac3", Language: "fre", Channels: 6}, {Index: 4, Type: "subtitle", Codec: "subrip", Language: "eng"}, {Index: 5, Type: "subtitle", Codec: "subrip", Language: "fre"}}}
	f.srv.selections.update(ownerUser.ID, int64(fightClub.Key), func(s *selection) { s.audio = 2; s.audioSet = true; s.subtitle = 4; s.subtitleSet = true })
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&hasMDE=1&directPlay=1&subtitles=sidecar", append(jsonAccept, "X-Plex-Product", "Plex Web", "X-Plex-Platform", "Firefox")...)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	md := mc["Metadata"].([]any)[0].(map[string]any)
	media := md["Media"].([]any)[0].(map[string]any)
	part := media["Part"].([]any)[0].(map[string]any)
	streams := part["Stream"].([]any)
	if len(streams) != 3 {
		t.Fatalf("streams=%v", streams)
	}
	for _, raw := range streams {
		st := raw.(map[string]any)
		if st["streamType"] == float64(1) && st["index"] != float64(0) {
			t.Fatalf("secondary video leaked: %v", st)
		}
		if st["streamType"] == float64(3) && st["location"] != "sidecar-subs" {
			t.Fatalf("sidecar location=%v", st)
		}
	}
}

// dv5Encoder decides with the real policy and reports whether its ffmpeg can
// tone-map Dolby Vision profile 5.
type dv5Encoder struct {
	*realDecider
	toneMaps bool
}

func (e dv5Encoder) ToneMapsDolbyVisionProfile5() bool { return e.toneMaps }

// A profile 5 source has no HDR10 base. Every legacy profile whose streaming
// target is SDR H.264 still converts it by tone-mapping, as PMS does; only an
// encoder without the RPU-aware mapper refuses it.
func TestDV5ConversionTargetToneMaps(t *testing.T) {
	for _, tc := range []struct {
		name, header, value, query string
		toneMaps                   bool
	}{
		{"web", "X-Plex-Platform", "Chrome", "", true},
		{"iOS", "X-Plex-Client-Profile-Name", "iOS", "", true},
		{"tvOS", "X-Plex-Client-Profile-Name", "tvOS", "", true},
		{"Android", "X-Plex-Client-Profile-Name", "Android", "", true},
		{"iOS MDE", "X-Plex-Client-Profile-Name", "iOS", "&hasMDE=1", true},
		{"encoder without mapper", "X-Plex-Client-Profile-Name", "iOS", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			f.srv.setTranscoder(dv5Encoder{&realDecider{f.trans}, tc.toneMaps})
			f.playback.info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
			f.playback.info.Streams[0] = core.StreamInfo{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Width: 1920, Height: 1080, FrameRate: 24, BitDepth: 10, DolbyVision: true, DolbyVisionProfile: 5, DolbyVisionLevel: 6}
			headers := append(jsonAccept, tc.header, tc.value)
			query := "path=/library/metadata/" + fightClub.Key.String() + "&directPlay=0&directStream=1&protocol=hls&session=dv5" + tc.query
			mc := decodeJSON(t, f.get(t, "/video/:/transcode/universal/decision?"+query, headers...))
			if !tc.toneMaps {
				if mc["transcodeDecisionCode"] != float64(2003) {
					t.Fatalf("encoder without the mapper was not refused: %v", mc)
				}
				return
			}
			if mc["transcodeDecisionCode"] != float64(1001) {
				t.Fatalf("DV5 conversion was refused: %v", mc)
			}
			if rec := f.get(t, "/video/:/transcode/universal/start.m3u8?"+query, headers...); rec.Code != 200 {
				t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
			}
			f.trans.mu.Lock()
			defer f.trans.mu.Unlock()
			if len(f.trans.sessions) != 1 {
				t.Fatalf("sessions: %d", len(f.trans.sessions))
			}
			for _, h := range f.trans.sessions {
				if d := h.req.Decision; d.Kind != core.DecisionTranscode || d.VideoCopy || !d.ToneMap {
					t.Fatalf("DV5 was not tone-mapped: %+v", d)
				}
			}
		})
	}
}
