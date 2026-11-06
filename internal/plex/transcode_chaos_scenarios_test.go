//go:build transcodechaos

package plex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// These sequences implement the seek/play/pause/track-change actions described
// by gst-validate, using Plex HTTP requests rather than a GStreamer pipeline.
// https://gstreamer.freedesktop.org/documentation/gst-devtools/gst-validate-scenarios.html
// Fast-forward is sampled forward seeks: the server does not set player rate.
func TestTranscodeChaosCorpusScenarios(t *testing.T) {
	corpus, ff, fp := os.Getenv("CHAOS_CORPUS"), os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	entries, _ := filepath.Glob(filepath.Join(corpus, "corpus-*.good"))
	if len(entries) == 0 {
		t.Skip("no public corpus staged")
	}
	source := httptest.NewServer(http.FileServer(http.Dir(corpus)))
	defer source.Close()
	m, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(t.TempDir(), "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithSegmentTimeout(15*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, entry := range entries {
		name := strings.TrimSuffix(filepath.Base(entry), ".good")
		t.Run(name, func(t *testing.T) {
			info, err := m.Probe(context.Background(), source.URL+"/"+name+".good")
			if err != nil {
				t.Fatal("probe failed")
			}
			info.Keyframes = nil // force demuxer seeking in the absence of an Ether index
			if info.Duration < time.Second {
				t.Skip("source has no usable duration for seek scenarios")
			}
			for _, path := range []string{"hls", "hls-fmp4", "dash", "start-mkv", "remux"} {
				t.Run(path, func(t *testing.T) {
					f := newFixture()
					defer f.srv.Close()
					f.playback.info = *info
					f.srv.d.Playback = &chaosPlayback{f.playback, source.URL + "/" + name + ".media"}
					rec := &chaosTranscoderRecorder{Transcoder: m}
					f.srv.d.Transcoder = rec
					headers := []string{"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome"}
					if path == "hls-fmp4" {
						headers = append(headers, "X-Plex-Client-Profile-Extra", "add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mp4&videoCodec=h264,hevc&audioCodec=aac)")
					}
					for _, scenario := range []string{"seek-forward-backward", "scrub-fast-forward", "pause-resume-eos", "track-quality-change"} {
						t.Run(scenario, func(t *testing.T) {
							id := "corpus-client-" + path + "-" + scenario
							defer f.get(t, "/video/:/transcode/universal/stop?session="+id, headers...)
							get := func(p string) []byte {
								t.Helper()
								start := time.Now()
								r := f.get(t, p, headers...)
								if r.Code != 200 {
									t.Fatalf("client request HTTP %d", r.Code)
								}
								if time.Since(start) > 20*time.Second {
									t.Fatal("client wait exceeded 20 seconds")
								}
								return r.Body.Bytes()
							}
							decode := func(b []byte) {
								t.Helper()
								p := filepath.Join(t.TempDir(), "output.media")
								if len(b) == 0 {
									t.Fatal("empty client media")
								}
								if err := os.WriteFile(p, b, 0600); err != nil {
									t.Fatal(err)
								}
								if capture := os.Getenv("CHAOS_CAPTURE"); capture != "" {
									if err := os.WriteFile(capture, b, 0600); err != nil {
										t.Fatal(err)
									}
								}
								chaosCmd(t, fp, "-v", "error", "-show_streams", p)
								// Stock FFmpeg's unindexed seek in this HLS TS excerpt
								// emits an incomplete leading AAC packet too. A copied
								// warning is not a playback failure: require a decoded
								// opening keyframe, audio frames and normal whole decode.
								stockSeekWarning := path != "dash" && name == "corpus-apple-bipbop.ts" && len(rec.requests) > 0 && rec.requests[len(rec.requests)-1].Decision.VideoCopy
								chaosDecodeMedia(t, ff, fp, p, stockSeekWarning)
								if stockSeekWarning {
									ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
									out, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "quiet", "-select_streams", "a:0", "-show_frames", "-show_entries", "frame=nb_samples", "-of", "csv=p=0", p).Output()
									cancel()
									if err != nil || len(strings.TrimSpace(string(out))) == 0 {
										t.Fatal("stock seek warning concealed all audio")
									}
								}
							}
							play := func(offset time.Duration, extra string) {
								t.Helper()
								direct := "1"
								if strings.Contains(extra, "&directStream=0") {
									direct = "0"
									extra = strings.ReplaceAll(extra, "&directStream=0", "")
								}
								q := fmt.Sprintf("path=%%2Flibrary%%2Fmetadata%%2F%s&session=%s&directPlay=0&directStream=%s&offset=%.3f%s", fightClub.Key.String(), id, direct, offset.Seconds(), extra)
								base := "/video/:/transcode/universal/session/" + id + "/"
								switch path {
								case "hls", "hls-fmp4":
									get("/video/:/transcode/universal/start.m3u8?protocol=hls&" + q)
									ext := "ts"
									var init []byte
									if len(rec.requests) > 0 && rec.requests[len(rec.requests)-1].Decision.Container == "fmp4" {
										ext = "m4s"
										init = get(base + "base/init.mp4")
									}
									decode(append(init, get(base+"base/00000."+ext)...))
								case "dash":
									get("/video/:/transcode/universal/start.mpd?protocol=dash&" + q)
									init := get(base + "0/header")
									decode(append(init, get(base+"0/0.m4s")...))
								default:
									media := get("/video/:/transcode/universal/start.mkv?protocol=http&" + q)
									if path == "remux" && (len(rec.requests) == 0 || !rec.requests[len(rec.requests)-1].Decision.VideoCopy) {
										t.Skip("client cannot copy this video; transcode covered separately")
									}
									decode(media)
								}
							}
							duration := info.Duration
							switch scenario {
							case "seek-forward-backward":
								for _, pos := range []time.Duration{0, duration * 2 / 3, duration / 4} {
									play(pos, "")
								}
							case "scrub-fast-forward":
								for _, pos := range []time.Duration{duration / 2, duration / 10, duration / 3, duration * 2 / 3} {
									play(pos, "")
								}
							case "pause-resume-eos":
								play(0, "")
								// A paused client stops fetching but keeps its session alive.
								if path != "start-mkv" && path != "remux" {
									get("/video/:/transcode/universal/ping?session=" + id)
									time.Sleep(25 * time.Millisecond)
									if _, ok := m.Session(id); !ok {
										t.Fatal("paused session ended")
									}
								}
								play(max(time.Duration(0), duration-1500*time.Millisecond), "")
								// start.mkv consumes to EOF; segmented output must supply its final
								// media segment and retain a finite VOD playlist/manifest.
							case "track-quality-change":
								play(0, "")
								for _, st := range info.Streams {
									if st.Type == "audio" {
										track := f.bindStream(fightClub.Key, st.Index, st)
										play(duration/4, fmt.Sprintf("&audioStreamID=%d", track))
									}
									if st.Type == "subtitle" {
										track := f.bindStream(fightClub.Key, st.Index, st)
										play(duration/4, fmt.Sprintf("&subtitles=burn&subtitleStreamID=%d", track))
										play(duration/4, "&subtitleStreamID=0")
									}
								}
								if path != "remux" {
									play(duration/3, "&directStream=0&maxVideoBitrate=200&videoResolution=160x90")
								}
							}
						})
					}
				})
			}
		})
	}
}
