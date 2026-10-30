package plex

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sanitized client profile supplied by the Samsung DU7100 reporter.
const samsungReportedProfile = "add-transcode-target(type=videoProfile&context=all&protocol=dash&container=mp4&videoCodec=h264,hevc&audioCodec=aac)+add-limitation(scope=videoAudioCodec&scopeName=h264&scopeType=videoCodec&context=all&protocol=dash&type=match&name=video.orientation&list=0)+add-limitation(scope=videoCodec&scopeName=h264&type=upperBound&name=video.level&value=51)+add-limitation(scope=videoTranscodeTarget&scopeName=h264&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.width&value=4096)+add-limitation(scope=videoTranscodeTarget&scopeName=h264&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.height&value=2160)+add-limitation(scope=videoTranscodeTarget&scopeName=h264&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.bitDepth&value=8)+add-limitation(scope=videoTranscodeTarget&scopeName=h264&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.frameRate&value=60)+add-limitation(scope=videoTranscodeTarget&scopeName=h264&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.bitrate&value=60000)+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.width&value=4096)+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.height&value=2160)+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.bitDepth&value=10)+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.frameRate&value=60)+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&context=all&protocol=dash&type=upperBound&name=video.bitrate&value=80000)+add-limitation(scope=videoTranscodeTarget&scopeName=hevc&scopeType=videoCodec&context=all&type=match&name=video.colorTrc&protocol=dash&list=bt709|bt470m|bt470bg|smpte170m|smpte240m|bt2020-10|smpte2084&isRequired=false)+add-limitation(scope=videoCodec&scopeName=hevc&type=notMatch&name=video.DOVIProfile&value=5)+add-transcode-target-settings(type=videoProfile&context=all&protocol=dash&IncludeMapInEmptyVTT=true)"

type samsungRealDecider struct{ *fakeTranscoder }

func (d *samsungRealDecider) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}

func TestSamsungReportedDASHAudioTarget(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = core.MediaInfo{Container: "mkv", Duration: 1335 * time.Second, BitrateKbps: 5147, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, Profile: "High", Level: 40},
		{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, SampleRate: 48000, BitrateKbps: 640},
	}}
	f.srv.d.Transcoder = &samsungRealDecider{f.trans}
	pr := &Request{User: &ownerUser, ClientID: "samsung-fixture", Product: "Plex for Samsung", Platform: "Tizen", PlatformVersion: "9", Version: "5.94.1", Device: "24_KSUE_UB_T09", Model: "UE50DU7100KXXU", ProfileExtra: samsungReportedProfile}
	p := playRequest{key: fightClub.Key, caps: pr.ClientCaps(0), sessionID: "samsung-fixture", protocol: core.ProtocolDASH, audioSel: -1, subSel: -1, opts: core.DecideOptions{DirectPlay: false, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}
	_, _, d, err := f.srv.prepare(context.Background(), pr, &p)
	if err != nil {
		t.Fatal(err)
	}
	if !d.VideoCopy || d.AudioCopy || d.AudioCodec != "aac" {
		t.Fatalf("Samsung advertises AAC DASH target; want copied H264 with encoded AAC, got %+v", d)
	}
}

func TestStreamingAudioTargetScope(t *testing.T) {
	cases := []struct {
		name, extra, protocol string
		want                  string
	}{
		{"reported", samsungReportedProfile, "dash", "aac"},
		{"other protocol", samsungReportedProfile, "hls", ""},
		{"direct play is separate", "add-direct-play-profile(type=videoProfile&protocol=dash&audioCodec=eac3)", "dash", ""},
		{"download is separate", "add-transcode-target(type=videoProfile&context=static&protocol=dash&audioCodec=ac3)", "dash", ""},
		{"protocol scope", "add-transcode-target(type=videoProfile&context=streaming&protocol=hls&audioCodec=eac3)+add-transcode-target(type=videoProfile&context=all&protocol=dash&audioCodec=aac)", "dash", "aac"},
		{"explicit surround", "add-transcode-target(type=videoProfile&context=all&protocol=dash&audioCodec=aac,eac3)", "dash", "aac,eac3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := strings.Join(streamingAudioTarget(c.extra, c.protocol), ","); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestStreamingAudioTargetDoesNotRestrictDirectPlay(t *testing.T) {
	info := core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Height: 1080}, {Index: 1, Type: "audio", Codec: "eac3", Channels: 6, SampleRate: 48000}}}
	caps := (&Request{Product: "Plex for Samsung", Platform: "Tizen"}).ClientCaps(0)
	caps.TranscodeAudioCodecs = []string{"aac"}
	opts := core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}
	d := transcode.Decide(info, caps, opts, transcode.Policy{})
	if d.Kind != core.DecisionDirectPlay || !d.AudioCopy || d.AudioCodec != "eac3" {
		t.Fatalf("direct play changed: %+v", d)
	}
	opts.DirectPlay = false
	d = transcode.Decide(info, caps, opts, transcode.Policy{})
	if !d.VideoCopy || d.AudioCopy || d.AudioCodec != "aac" {
		t.Fatalf("remux missed explicit target: %+v", d)
	}
	caps.TranscodeAudioCodecs = []string{"aac", "eac3"}
	d = transcode.Decide(info, caps, opts, transcode.Policy{})
	if !d.AudioCopy || d.AudioCodec != "eac3" {
		t.Fatalf("explicit surround changed: %+v", d)
	}
}

// Actual muxing, served through the authenticated Plex routes, not a mocked
// playlist: verify the MPD, initialization and first media segment agree.
func TestSamsungDASHMediaEndToEnd(t *testing.T) {
	for _, dp := range []string{"0", "1"} {
		t.Run("directPlay="+dp, func(t *testing.T) { testSamsungDASHMediaEndToEnd(t, dp) })
	}
}

func testSamsungDASHMediaEndToEnd(t *testing.T, directPlay string) {
	if testing.Short() {
		t.Skip("real FFmpeg fixture")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "color=c=blue:s=1920x1080:r=24", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=5.1", "-t", "8", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-g", "24", "-c:a", "eac3", "-b:a", "640k", filepath.Join(dir, "source.mkv"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	source := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer source.Close()
	mgr, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "mux")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithSegmentTimeout(15*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	info, err := mgr.Probe(ctx, source.URL+"/source.mkv")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = *info
	f.srv.d.Playback = &samsungSourcePlayback{fakePlayback: f.playback, url: source.URL + "/source.mkv"}
	f.srv.d.Transcoder = mgr
	hdr := []string{"X-Plex-Product", "Plex for Samsung", "X-Plex-Platform", "Tizen", "X-Plex-Client-Identifier", "samsung-fixture", "X-Plex-Client-Profile-Extra", samsungReportedProfile}
	q := "path=%2Flibrary%2Fmetadata%2F1000000000000550&protocol=dash&directPlay=" + directPlay + "&directStream=1&session=samsung-real"
	decision := f.get(t, "/video/:/transcode/universal/decision?"+q, hdr...)
	if decision.Code != 200 {
		t.Fatalf("decision HTTP%d: %s", decision.Code, decision.Body.String())
	}
	entry, ok := f.srv.transcodes.get("samsung-real")
	if !ok || !entry.decision.VideoCopy || (directPlay == "0" && (entry.decision.AudioCopy || entry.decision.AudioCodec != "aac")) {
		t.Fatalf("wrong decision: %+v", entry)
	}
	manifest := f.get(t, "/video/:/transcode/universal/start.mpd?"+q, hdr...)
	if manifest.Code != 200 || !strings.Contains(manifest.Body.String(), "mp4a.40.2") || strings.Contains(manifest.Body.String(), "ec-3") {
		t.Fatalf("bad MPD HTTP%d: %s", manifest.Code, manifest.Body.String())
	}
	for rep, want := range []string{"h264", "aac"} {
		base := fmt.Sprintf("/video/:/transcode/universal/session/samsung-real/%d/", rep)
		init := f.get(t, base+"header", hdr...)
		seg := f.get(t, base+"0.m4s", hdr...)
		if init.Code != 200 || seg.Code != 200 || init.Body.Len() == 0 || seg.Body.Len() == 0 {
			t.Fatalf("rep%d init=%d/%d bytes segment=%d/%d bytes", rep, init.Code, init.Body.Len(), seg.Code, seg.Body.Len())
		}
		name := filepath.Join(dir, fmt.Sprintf("rep%d.mp4", rep))
		if err := os.WriteFile(name, append(init.Body.Bytes(), seg.Body.Bytes()...), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(ctx, fp, "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", name).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Fatalf("rep%d codec want%s got%s err%v", rep, want, out, err)
		}
		out, err = exec.CommandContext(ctx, ff, "-v", "error", "-nostdin", "-threads", "1", "-i", name, "-f", "null", "-").CombinedOutput()
		if err != nil {
			t.Fatalf("rep%d decode: %v %s", rep, err, out)
		}
		t.Logf("representation %d: %s, init %d bytes, media %d bytes; decoded successfully", rep, want, init.Body.Len(), seg.Body.Len())
	}
}

type samsungSourcePlayback struct {
	*fakePlayback
	url string
}

func (p *samsungSourcePlayback) InputURL(string) string { return p.url }
