package plex

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type realDecideTranscoder struct {
	*fakeTranscoder
}

func (t realDecideTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	t.mu.Lock()
	t.lastOpts, t.lastCaps = opts, caps
	t.mu.Unlock()
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}

const lgWebOSProfileExtra = "add-direct-play-profile(type=videoProfile&container=mkv,mp4&codec=hevc,h264&audioCodec=eac3,ac3,aac)+" +
	"add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mpegts&videoCodec=h264&audioCodec=eac3,ac3,aac)+" +
	"add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.width&value=3840)+" +
	"add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.height&value=2160)+" +
	"add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=10)+" +
	"add-limitation(scope=videoCodec&scopeName=h264&type=upperBound&name=video.level&value=40)"

func lgWebOSHeaders(profileName string) map[string]string {
	return map[string]string{
		"Accept":                      "application/json",
		"X-Plex-Token":                ownerToken,
		"X-Plex-Client-Identifier":    "lg-g4-client",
		"X-Plex-Session-Identifier":   "lg-play-1",
		"X-Plex-Product":              "Plex for LG",
		"X-Plex-Version":              "5.94.4",
		"X-Plex-Platform":             "webOS",
		"X-Plex-Platform-Version":     "9.2.4",
		"X-Plex-Device":               "LG OLED65G46LS",
		"X-Plex-Model":                "OLED65G46LS.DEKQLJP",
		"X-Plex-Client-Profile-Name":  profileName,
		"X-Plex-Client-Profile-Extra": lgWebOSProfileExtra,
	}
}

func sample4KHEVCUsenetInfo() core.MediaInfo {
	return core.MediaInfo{
		Container:   "mkv",
		Duration:    58 * time.Minute,
		Size:        12_000_000_000,
		BitrateKbps: 24500,
		Streams: []core.StreamInfo{
			{
				Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10",
				Width: 3840, Height: 2160, FrameRate: 23.976, BitDepth: 10,
				ColorSpace: "bt2020nc", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020",
				BitrateKbps: 23800,
			},
			{
				Index: 1, Type: "audio", Codec: "eac3", Language: "en",
				Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000,
				Default: true, BitrateKbps: 640,
			},
		},
	}
}

func TestLGWebOSUsenet4KHEVCDirectPlaysInsteadOfBrokenHLS(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setTranscoder(realDecideTranscoder{fakeTranscoder: f.trans})
	f.playback.info = sample4KHEVCUsenetInfo()
	f.playback.resolution = &core.Resolution{
		Title: bbE1.Key,
		Release: core.Release{
			ID:       "nzb:mobland-s01e01-2160p",
			Protocol: core.ProtocolUsenet,
		},
	}

	rec := f.request(
		t,
		http.MethodGet,
		"/video/:/transcode/universal/decision?path=/library/metadata/"+bbE1.Key.String()+"&protocol=hls&hasMDE=1&directPlay=1&directStream=1&directStreamAudio=1&session=lg-s1",
		lgWebOSHeaders("HTML TV App"),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("decision status = %d: %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	if got, _ := mc["mdeDecisionCode"].(float64); int(got) != codeDirectPlayOK {
		t.Fatalf("mdeDecisionCode = %v, want %d; body=%s", mc["mdeDecisionCode"], codeDirectPlayOK, rec.Body.String())
	}
	md := mc["Metadata"].([]any)[0].(map[string]any)
	media := md["Media"].([]any)[0].(map[string]any)
	part := media["Part"].([]any)[0].(map[string]any)
	if part["decision"] != "directplay" {
		t.Fatalf("Part.decision = %v, want directplay (must not force HEVC HLS copy on LG webOS)", part["decision"])
	}
}

func TestLGWebOSFallbackTranscodeCapsH264At1080pAndSingleRung(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.setTranscoder(realDecideTranscoder{fakeTranscoder: f.trans})
	f.playback.info = sample4KHEVCUsenetInfo()
	f.playback.resolution = &core.Resolution{
		Title: bbE1.Key,
		Release: core.Release{
			ID:       "nzb:mobland-s01e01-2160p",
			Protocol: core.ProtocolUsenet,
		},
	}
	f.trans.rungs = []core.HLSRung{
		{ID: "base", Bandwidth: 20_000_000, Width: 1920, Height: 1080, Codecs: "avc1.640028,mp4a.40.2", FrameRate: 23.976},
		{ID: "720p4m", Bandwidth: 4_000_000, Width: 1280, Height: 720, Codecs: "avc1.64001f,mp4a.40.2", FrameRate: 23.976},
		{ID: "360p800k", Bandwidth: 800_000, Width: 640, Height: 360, Codecs: "avc1.64001e,mp4a.40.2", FrameRate: 23.976},
	}

	headers := lgWebOSHeaders("Generic")
	query := "path=/library/metadata/" + bbE1.Key.String() + "&protocol=hls&hasMDE=1&directPlay=0&directStream=0&directStreamAudio=1&session=lg-fallback"
	rec := f.request(t, http.MethodGet, "/video/:/transcode/universal/decision?"+query, headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("decision status = %d: %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	md := mc["Metadata"].([]any)[0].(map[string]any)
	media := md["Media"].([]any)[0].(map[string]any)
	if got := int(media["height"].(float64)); got != 1080 {
		t.Fatalf("transcode height = %d, want 1080 (H.264 level 4.0 ceiling, never 4K 2160p)", got)
	}
	if media["videoCodec"] != "h264" || media["container"] != "mpegts" {
		t.Fatalf("transcode media = videoCodec=%v container=%v, want h264/mpegts", media["videoCodec"], media["container"])
	}

	startHeaders := map[string]string{
		"X-Plex-Token":              ownerToken,
		"X-Plex-Client-Identifier":  "lg-g4-client",
		"X-Plex-Session-Identifier": "lg-play-1",
		"X-Plex-Product":            "Plex for LG",
		"X-Plex-Platform":           "webOS",
	}
	m3u8 := f.request(t, http.MethodGet, "/video/:/transcode/universal/start.m3u8?"+query, startHeaders)
	if m3u8.Code != http.StatusOK {
		t.Fatalf("start.m3u8 status = %d: %s", m3u8.Code, m3u8.Body.String())
	}
	body := m3u8.Body.String()
	if !strings.Contains(body, "session/lg-fallback/base/index.m3u8") {
		t.Fatalf("master playlist missing base rung:\n%s", body)
	}
	if strings.Contains(body, "720p4m") || strings.Contains(body, "360p800k") {
		t.Fatalf("LG webOS master playlist must not expose multi-rung ABR variants:\n%s", body)
	}

	// Non-LG clients keep the full multi-rung HLS ladder.
	iosHeaders := map[string]string{
		"Accept":                    "application/json",
		"X-Plex-Token":              ownerToken,
		"X-Plex-Client-Identifier":  "ios-client",
		"X-Plex-Session-Identifier": "ios-play-1",
		"X-Plex-Product":            "Plex for iOS",
		"X-Plex-Platform":           "iOS",
	}
	iosQuery := "path=/library/metadata/" + bbE1.Key.String() + "&protocol=hls&hasMDE=1&directPlay=0&directStream=0&session=ios-ladder"
	if d := f.request(t, http.MethodGet, "/video/:/transcode/universal/decision?"+iosQuery, iosHeaders); d.Code != http.StatusOK {
		t.Fatalf("iOS decision status = %d", d.Code)
	}
	iosM3U8 := f.request(t, http.MethodGet, "/video/:/transcode/universal/start.m3u8?"+iosQuery, iosHeaders)
	if !strings.Contains(iosM3U8.Body.String(), "360p800k/index.m3u8") {
		t.Fatalf("iOS master playlist lost ABR ladder:\n%s", iosM3U8.Body.String())
	}
}

func TestTranscodeStopAfterTimelineStoppedIsIdempotentForOwnerOnly(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	headers := map[string]string{
		"X-Plex-Token":              ownerToken,
		"X-Plex-Client-Identifier":  "lg-g4-client",
		"X-Plex-Session-Identifier": "lg-play-stop",
		"X-Plex-Product":            "Plex for LG",
		"X-Plex-Platform":           "webOS",
	}
	query := "path=/library/metadata/" + fightClub.Key.String() + "&protocol=hls&directPlay=0&directStream=1&session=stop-race"
	if rec := f.request(t, http.MethodGet, "/video/:/transcode/universal/decision?"+query, headers); rec.Code != http.StatusOK {
		t.Fatalf("decision: %d", rec.Code)
	}
	if rec := f.request(t, http.MethodGet, "/video/:/transcode/universal/start.m3u8?"+query, headers); rec.Code != http.StatusOK {
		t.Fatalf("start.m3u8: %d", rec.Code)
	}

	// Plex for LG sends /:/timeline?state=stopped right before /universal/stop.
	tl := f.request(t, http.MethodGet, "/:/timeline?ratingKey="+fightClub.Key.String()+"&key=/library/metadata/"+fightClub.Key.String()+"&state=stopped&time=570260&duration=8340000", headers)
	if tl.Code != http.StatusOK {
		t.Fatalf("timeline stopped: %d", tl.Code)
	}

	// Another viewer still gets 404, and unknown sessions still get 404.
	other := f.request(t, http.MethodGet, "/video/:/transcode/universal/stop?session=stop-race", map[string]string{"X-Plex-Token": friendToken})
	if other.Code != http.StatusNotFound {
		t.Fatalf("other user stop = %d, want 404", other.Code)
	}
	unknown := f.request(t, http.MethodGet, "/video/:/transcode/universal/stop?session=never-started", headers)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown session stop = %d, want 404", unknown.Code)
	}

	// The owning viewer gets an idempotent 200 OK instead of a 404.
	own := f.request(t, http.MethodGet, "/video/:/transcode/universal/stop?session=stop-race", headers)
	if own.Code != http.StatusOK {
		t.Fatalf("owner stop after timeline stopped = %d, want 200", own.Code)
	}
}
