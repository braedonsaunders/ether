// Command transcodechaos runs destructive-input experiments against an isolated
// transcode.Manager and loopback HTTP source. It never contacts real servers.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type scenario struct {
	Name, Container, HW, Fault string
	Copy                       bool
}
type result struct {
	Scenario, Path, Status, Detail string
	Milliseconds                   int64
}

func main() {
	ff := flag.String("ffmpeg", "ffmpeg", "ffmpeg executable")
	fp := flag.String("ffprobe", "ffprobe", "ffprobe executable")
	only := flag.String("only", "", "run scenarios whose name contains this text")
	fixtures := flag.String("fixture-dir", "", "optional directory for generated fixtures (default: temporary)")
	corpus := flag.Bool("corpus", false, "download a curated public corpus into the local cache")
	cacheDefault, _ := os.UserCacheDir()
	cache := flag.String("cache-dir", filepath.Join(cacheDefault, "ether", "transcodechaos"), "public corpus cache, outside the repository (10 GB maximum)")
	mode := flag.String("mode", "short", "short or full scenario matrix")
	serverURL := flag.String("server-url", "", "explicit Ether lab server URL")
	mediaKey := flag.String("media-key", "", "comma-separated rating keys of staged test titles on the lab server")
	report := flag.String("report", "transcodechaos-report.json", "output JSON report")
	flag.Parse()
	if err := run(*ff, *fp, *report, *only, *fixtures, *cache, *mode, *corpus, *serverURL, *mediaKey); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func command(ctx context.Context, exe string, args ...string) error {
	out, err := mediaproc.CommandContext(ctx, mediaproc.EncodeTimeout, exe, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(exe), err, strings.TrimSpace(string(out)))
	}
	return nil
}
func run(ff, fp, report, only, fixtures, cache, mode string, useCorpus bool, serverURL, mediaKey string) error {
	if mode != "short" && mode != "full" {
		return fmt.Errorf("-mode must be short or full")
	}
	root, err := os.MkdirTemp("", "ether-chaos-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if fixtures == "" {
		fixtures = filepath.Join(root, "fixtures")
	}
	if useCorpus {
		if err := outsideRepo(fixtures); err != nil {
			return err
		}
	}
	fixture := filepath.Join(root, "input.ts")
	if err = command(ctx, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "12", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-f", "mpegts", fixture); err != nil {
		return err
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		return err
	}
	cases := []scenario{{"healthy", "mpegts", "none", "", true}, {"healthy", "mpegts", "none", "", false}, {"healthy", "fmp4", "none", "", false}, {"seek", "mpegts", "none", "", false}, {"corrupt-ts", "mpegts", "none", "corrupt", false}, {"truncated", "mpegts", "none", "truncate", false}, {"source-503", "mpegts", "none", "503", false}, {"transient-503", "mpegts", "none", "once-503", false}, {"transient-416", "mpegts", "none", "once-416", false}, {"slow-first-byte", "mpegts", "none", "slow-first", false}, {"midrange-stall", "mpegts", "none", "stall", false}, {"persistent-stall-then-recover", "mpegts", "none", "long-stall", false}, {"connection-drop", "mpegts", "none", "drop", false}, {"short-reads", "mpegts", "none", "short-writes", false}, {"changing-source", "mpegts", "none", "changing", false}, {"live-corrupt", "mpegts", "none", "corrupt", false}, {"hardware-failure", "mpegts", "videotoolbox", "hwfail", false}, {"hardware-failure-fmp4", "fmp4", "videotoolbox", "hwfail", false}, {"hardware-failure-dash", "dash", "videotoolbox", "hwfail", false}, {"healthy", "mpegts", "videotoolbox", "", false}}
	for _, name := range []string{"input-backward-dts", "input-timestamp-jump", "input-corrupt-mkv", "input-corrupt-mp4", "input-text-subtitle", "input-ass-subtitle", "input-broken-text", "input-broken-ass", "input-pgs", "input-dolby-vision", "input-missing-keyframe", "input-open-gop", "input-long-gop", "input-pts-wrap", "input-resolution-change", "input-samplerate-change", "input-audio-late", "input-audio-early-end", "input-vfr", "input-interlaced", "input-hevc10-hdr", "input-multi-audio"} {
		cases = append(cases, scenario{name, "mpegts", "none", "", false})
	}
	if fixtures != "" {
		if err := os.MkdirAll(fixtures, 0700); err != nil {
			return err
		}
	}
	results := make([]result, 0, len(cases))
	failures := 0
	for _, sc := range cases {
		if only != "" && !strings.Contains(sc.Name, only) {
			continue
		}
		if only == "" && mode == "short" && sc.Name != "healthy" && sc.Name != "seek" && sc.Name != "corrupt-ts" && sc.Name != "input-multi-audio" {
			continue
		}
		start := time.Now()
		fixture, fixtureErr := makeFixture(ctx, ff, root, sc.Name, data)
		detail := ""
		status := "PASS"
		if fixtureErr != nil {
			status = "SKIP"
			detail = "fixture unavailable: " + fixtureErr.Error()
		} else {
			if fixtures != "" && strings.HasPrefix(sc.Name, "input-") {
				altered, e := mutateFixture(fixture, sc)
				if e != nil {
					return e
				}
				if e = os.WriteFile(filepath.Join(fixtures, sc.Name+".good"), fixture, 0600); e != nil {
					return e
				}
				if e = os.WriteFile(filepath.Join(fixtures, sc.Name+".media"), altered, 0600); e != nil {
					return e
				}
			}
			detail = exercise(ctx, ff, fp, root, fixture, sc)
		}
		if detail != "" && status != "SKIP" {
			status = "FAIL"
			// With no alternate input, permanently unavailable bytes cannot
			// produce playable media. Assert a prompt explicit error and clean
			// cleanup, rather than treating a successful rejection as a hang.
			want := map[string]string{"source-503": "segment 0 HTTP 502", "truncated": "segment 2 HTTP 502"}[sc.Name]
			if want != "" && strings.HasPrefix(detail, want) && !strings.Contains(detail, "cleanup:") && time.Since(start) < 15*time.Second {
				status, detail = "EXPECTED_ERROR", "source has no recoverable bytes or alternate; "+detail
			} else {
				failures++
			}
		}
		r := result{sc.Name, sc.Container + "/" + sc.HW, status, detail, time.Since(start).Milliseconds()}
		results = append(results, r)
		fmt.Printf("%-14s %-20s %-4s %6dms %s\n", r.Scenario, r.Path, r.Status, r.Milliseconds, r.Detail)
	}
	if useCorpus {
		files, downloads, err := downloadCorpus(ctx, cache, mode, ff)
		if err != nil {
			return err
		}
		results = append(results, downloads...)
		for _, item := range downloads {
			if item.Status == "FAIL" {
				failures++
			}
		}
		for _, asset := range publicAssets {
			path, ok := files[asset.name]
			if !ok {
				continue
			}
			if strings.HasPrefix(asset.name, "dashif-") {
				original, e := os.ReadFile(filepath.Join(cache, asset.name))
				if e != nil {
					return e
				}
				if e = os.WriteFile(filepath.Join(fixtures, asset.name+".relay"), original, 0600); e != nil {
					return e
				}
				if asset.name == "dashif-audio.mp4" {
					continue
				} // paired with video in the relay vector
			}
			start := time.Now()
			raw, err := os.ReadFile(path)
			detail := ""
			status := "PASS"
			if err != nil {
				status = "FAIL"
				detail = "cache read failed"
			} else if err = command(ctx, ff, "-v", "error", "-threads", "2", "-i", path, "-t", "12", "-f", "null", "-"); err != nil {
				reason, listErr := expectedCorpusFailure(asset)
				if listErr != nil {
					return fmt.Errorf("invalid expected-failures list: %w", listErr)
				}
				if reason != "" {
					status = "XFAIL"
					detail = reason
					rejectStart := time.Now()
					rejection := exercise(ctx, ff, fp, root, raw, scenario{"undecodable-" + asset.name, "mpegts", "none", "", false})
					if rejection == "" || time.Since(rejectStart) > 10*time.Second || strings.Contains(rejection, "cleanup:") {
						status, detail = "FAIL", "unsupported source was not rejected promptly with clean resource cleanup"
					}
				} else {
					status = "FAIL"
					detail = "stock ffmpeg failed; needs triage"
				}
			} else {
				reason, listErr := expectedCorpusFailure(asset)
				if listErr != nil {
					return fmt.Errorf("invalid expected-failures list: %w", listErr)
				}
				if reason != "" {
					status = "FAIL"
					detail = "stock ffmpeg now decodes an expected failure; review the list"
				} else {
					detail = exercise(ctx, ff, fp, root, raw, scenario{"corpus-" + asset.name, "mpegts", "none", "", false})
					if detail != "" {
						status = "FAIL"
					}
				}
			}
			// Exercise the real endpoints even when this manager scenario
			// fails, provided stock FFmpeg could open the source.
			if fixtures != "" && err == nil && status != "XFAIL" {
				name := "corpus-" + asset.name
				if err := os.WriteFile(filepath.Join(fixtures, name+".good"), raw, 0600); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(fixtures, name+".media"), raw, 0600); err != nil {
					return err
				}
			}
			if status == "FAIL" {
				failures++
			}
			item := result{asset.name, "corpus/hls-ts", status, detail, time.Since(start).Milliseconds()}
			results = append(results, item)
			fmt.Printf("%-32s %-20s %-4s %6dms %s\n", item.Scenario, item.Path, item.Status, item.Milliseconds, item.Detail)
		}
	}
	if only == "" {
		var plexResults []result
		var err error
		if serverURL != "" {
			plexResults, err = runLabServer(ctx, serverURL, mediaKey, os.Getenv("ETHER_CHAOS_TOKEN"), ff, fp, root, mode)
		} else {
			plexResults, err = runPlex(ctx, ff, fp, fixtures, mode)
		}
		if err != nil {
			return err
		}
		for _, item := range plexResults {
			if item.Status == "FAIL" {
				failures++
			}
		}
		results = append(results, plexResults...)
	}
	passes, skips, expected := 0, 0, 0
	for _, item := range results {
		if item.Status == "PASS" {
			passes++
		}
		if item.Status == "SKIP" {
			skips++
		}
		if item.Status == "XFAIL" || item.Status == "EXPECTED_ERROR" {
			expected++
		}
	}
	fmt.Printf("score: %d passed, %d failed, %d expected errors, %d skipped\n", passes, failures, expected, skips)
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(report, append(b, '\n'), 0644); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d scenarios failed; report: %s", failures, report)
	}
	return nil
}
func mutateFixture(good []byte, sc scenario) ([]byte, error) {
	data := bytes.Clone(good)
	if sc.Name == "input-broken-text" || sc.Name == "input-broken-ass" {
		at := bytes.Index(data, []byte("First subtitle"))
		if at < 0 {
			return nil, fmt.Errorf("subtitle packet payload not found")
		}
		for i := at; i < at+8; i++ {
			data[i] = 0xff
		}
	}
	if sc.Name == "input-corrupt-mkv" || sc.Name == "input-corrupt-mp4" {
		for i := len(data) / 2; i < len(data)/2+4096 && i < len(data); i++ {
			data[i] = 0xff
		}
	}
	if sc.Fault == "corrupt" { // destroy 30 consecutive packets, preserving the TS sync byte
		for i := len(data) / 2 / 188 * 188; i < len(data)/2/188*188+30*188 && i < len(data); i++ {
			if i%188 != 0 {
				data[i] = 0xff
			}
		}
	}
	if sc.Fault == "changing" {
		for i := len(data) / 2 / 188 * 188; i < len(data)/2/188*188+30*188 && i < len(data); i++ {
			if i%188 != 0 {
				data[i] = 0xff
			}
		}
	}
	if sc.Fault == "truncate" {
		data = data[:len(data)/2]
	}
	return data, nil
}
func exercise(parent context.Context, ff, fp, root string, good []byte, sc scenario) (detail string) {
	baseline := takeBaseline()
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	data, err := mutateFixture(good, sc)
	if err != nil {
		return err.Error()
	}
	source := &faultSource{good: good, altered: data, fault: sc.Fault}
	srv := httptest.NewServer(source)
	defer srv.Close()
	dir, err := os.MkdirTemp(root, "session-")
	if err != nil {
		return err.Error()
	}
	defer os.RemoveAll(dir)
	var injector *hardwareFaultRunner
	var inner transcode.Runner = transcode.ExecRunner{}
	opts := []transcode.Option{transcode.WithSegmentTimeout(12 * time.Second)}
	if sc.Fault == "long-stall" {
		opts = []transcode.Option{transcode.WithSegmentTimeout(5 * time.Second), transcode.WithStallTimeout(1500 * time.Millisecond)}
	}
	if sc.Fault == "hwfail" {
		injector = &hardwareFaultRunner{}
		inner = injector
	}
	runner := &trackingRunner{inner: inner}
	opts = append(opts, transcode.WithRunner(runner))
	transcodeDir := filepath.Join(dir, "transcodes")
	m, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: sc.HW, TempDir: transcodeDir}, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	if err != nil {
		return err.Error()
	}
	defer func() {
		_ = m.Close()
		srv.Close()
		if err := baseline.check(transcodeDir, runner); err != nil {
			if detail != "" {
				detail += "; "
			}
			detail += "cleanup: " + err.Error()
		}
	}()
	if sc.HW == "videotoolbox" && m.HWAccel().Name != "videotoolbox" {
		return "VideoToolbox not detected"
	}
	info, err := m.Probe(ctx, srv.URL+"/fixture.ts")
	if err != nil {
		return "probe: " + err.Error()
	}
	d := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: sc.Container, AudioStream: -1, SubtitleStream: -1}
	if sc.Name == "input-text-subtitle" || sc.Name == "input-ass-subtitle" || sc.Name == "input-broken-text" || sc.Name == "input-broken-ass" {
		for _, st := range info.Streams {
			if st.Type == "subtitle" {
				d.SubtitleStream = st.Index
				d.BurnSubtitles = true
				break
			}
		}
		if !d.BurnSubtitles {
			return "subtitle stream missing from probe"
		}
	}
	if sc.Copy {
		d.Kind = core.DecisionRemux
		d.VideoCopy = true
		d.AudioCopy = true
	}
	source.armed.Store(true)
	live := sc.Name == "live-corrupt"
	if live {
		info.Duration = 0
	}
	protocol := core.ProtocolHLS
	if sc.Container == "dash" {
		protocol = core.ProtocolDASH
		d.Container = "fmp4"
	}
	s, err := m.StartHLS(ctx, core.HLSRequest{SessionID: "chaos", LiveInput: live, InputURL: srv.URL + "/fixture.ts", Info: *info, Decision: d, SegmentDuration: 4 * time.Second, Protocol: protocol})
	if err != nil {
		return "start: " + err.Error()
	}
	defer s.Close()
	playlist := httptest.NewRecorder()
	s.ServePlaylist(playlist, httptest.NewRequestWithContext(ctx, "GET", "/index.m3u8", nil))
	// An unknown-duration VOD uses an EVENT playlist. It is initially empty
	// while the first segment is encoded, so poll as an HLS client would.
	if !live && protocol == core.ProtocolHLS && info.Duration <= 0 {
		deadline := time.NewTimer(12 * time.Second)
		defer deadline.Stop()
		for playlist.Code == 200 && !strings.Contains(playlist.Body.String(), "#EXTINF") {
			select {
			case <-ctx.Done():
				return "playlist wait canceled"
			case <-deadline.C:
				return "unknown-duration playlist did not advance"
			case <-time.After(100 * time.Millisecond):
			}
			playlist = httptest.NewRecorder()
			s.ServePlaylist(playlist, httptest.NewRequestWithContext(ctx, "GET", "/index.m3u8", nil))
		}
	}
	if playlist.Code != 200 || (sc.Container == "dash" && !strings.Contains(playlist.Body.String(), "<MPD")) || (sc.Container != "dash" && !live && !strings.Contains(playlist.Body.String(), "#EXTINF")) {
		return fmt.Sprintf("playlist HTTP %d", playlist.Code)
	}
	var init, audioInit []byte
	if sc.Container == "fmp4" || sc.Container == "dash" {
		w := httptest.NewRecorder()
		if sc.Container == "dash" {
			s.(core.DASHSession).ServeInit(w, httptest.NewRequestWithContext(ctx, "GET", "/init", nil), 0)
		} else {
			s.ServeSegment(w, httptest.NewRequestWithContext(ctx, "GET", "/init", nil), transcode.InitSegment)
		}
		if w.Code != 200 {
			return fmt.Sprintf("init HTTP %d", w.Code)
		}
		init = w.Body.Bytes()
		if sc.Container == "dash" {
			audio := httptest.NewRecorder()
			s.(core.DASHSession).ServeInit(audio, httptest.NewRequestWithContext(ctx, "GET", "/audio-init", nil), 1)
			if audio.Code != 200 || audio.Body.Len() == 0 {
				return fmt.Sprintf("audio init HTTP %d", audio.Code)
			}
			audioInit = audio.Body.Bytes()
		}
	}
	order := []int{0, 1, 2}
	if strings.HasPrefix(sc.Name, "corpus-") && info.Duration > 0 {
		order = nil
		for n := 0; n < 3 && time.Duration(n)*4*time.Second < info.Duration; n++ {
			order = append(order, n)
		}
	}
	if sc.Name == "seek" {
		order = []int{0, 2, 1, 2, 0}
	}
	timeline := timelineCheck{discontinuities: discontinuities(playlist.Body.String())}
	for _, n := range order {
		w := httptest.NewRecorder()
		start := time.Now()
		if sc.Container == "dash" {
			s.(core.DASHSession).ServeMedia(w, httptest.NewRequestWithContext(ctx, "GET", "/segment", nil), 0, n)
		} else {
			s.ServeSegment(w, httptest.NewRequestWithContext(ctx, "GET", "/segment", nil), n)
		}
		if w.Code != 200 {
			return fmt.Sprintf("segment %d HTTP %d after %s", n, w.Code, time.Since(start).Round(time.Millisecond))
		}
		if w.Body.Len() == 0 {
			return fmt.Sprintf("segment %d empty", n)
		}
		p := filepath.Join(dir, fmt.Sprintf("check-%d.%s", n, map[bool]string{true: "mp4", false: "ts"}[len(init) > 0]))
		out := append(bytes.Clone(init), w.Body.Bytes()...)
		if err := os.WriteFile(p, out, 0600); err != nil {
			return err.Error()
		}
		if err := command(ctx, fp, "-v", "error", "-show_streams", p); err != nil {
			return fmt.Sprintf("segment %d probe: %v", n, err)
		}
		if err := command(ctx, ff, "-v", "error", "-xerror", "-i", p, "-f", "null", "-"); err != nil {
			return fmt.Sprintf("segment %d decode: %v", n, err)
		}
		if size, err := diskBytes(transcodeDir); err != nil {
			return err.Error()
		} else if size > 128<<20 {
			return fmt.Sprintf("transcode disk usage %d exceeds 128 MiB", size)
		}
		if err := timeline.check(ctx, fp, p, n); err != nil {
			return err.Error()
		}
		if sc.Container == "dash" {
			audio := httptest.NewRecorder()
			s.(core.DASHSession).ServeMedia(audio, httptest.NewRequestWithContext(ctx, "GET", "/audio", nil), 1, n)
			if audio.Code != 200 || audio.Body.Len() == 0 {
				return fmt.Sprintf("audio segment %d HTTP %d", n, audio.Code)
			}
			p := filepath.Join(dir, fmt.Sprintf("audio-%d.mp4", n))
			if err := os.WriteFile(p, append(bytes.Clone(audioInit), audio.Body.Bytes()...), 0600); err != nil {
				return err.Error()
			}
			if err := command(ctx, fp, "-v", "error", "-show_streams", p); err != nil {
				return fmt.Sprintf("audio segment %d probe: %v", n, err)
			}
			if err := command(ctx, ff, "-v", "error", "-xerror", "-i", p, "-f", "null", "-"); err != nil {
				return fmt.Sprintf("audio segment %d decode: %v", n, err)
			}
		}
		if n == 0 && injector != nil {
			if err := injector.fail(); err != nil {
				return err.Error()
			}
		}
		if n == 0 && sc.Fault == "changing" {
			if err := s.SwapInput(ctx, srv.URL+"/fixture.ts", *info, 4*time.Second); err != nil {
				return "restart on changed source: " + err.Error()
			}
		}
	}
	if sc.Fault == "changing" && source.requests.Load() < 2 {
		return "content change was not exercised: fewer than two source requests"
	}
	if injector != nil && !injector.software() {
		return "software fallback did not start"
	}
	m.Stop("chaos")
	if _, ok := m.Session("chaos"); ok {
		return "session survived stop"
	}
	return ""
}
