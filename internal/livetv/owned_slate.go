package livetv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// Slate segments keep an owned playlist advancing while a source recovers.
// They show the channel's warm "Reconnecting…" screen (the same signal look
// broadcast mode splices in), are encoded once per channel and output
// profile, and are shared by every viewer: publishing one costs a byte copy
// and a timestamp rewrite, never an encode.

type slateProfile struct {
	channel    string
	container  string // "ts" | "fmp4"
	video      string // "h264" | "hevc"
	width      int
	height     int
	fps        float64
	audio      string // "aac" | "ac3" | "eac3"
	channels   int
	sampleRate int
	segment    time.Duration
}

func (p slateProfile) normalized() slateProfile {
	if p.container != "fmp4" {
		p.container = "ts"
	}
	if p.video != "hevc" {
		p.video = "h264"
	}
	if p.width <= 0 || p.height <= 0 {
		p.width, p.height = 1280, 720
	}
	p.width, p.height = max(2, p.width/2*2), max(2, p.height/2*2)
	if p.fps < 15 || p.fps > 60 {
		p.fps = 30
	}
	switch p.audio {
	case "aac", "ac3", "eac3":
	default:
		p.audio = "aac"
	}
	if p.channels < 1 || p.channels > 6 || p.audio == "aac" && p.channels > 2 && p.channels != 6 {
		p.channels = 2
	}
	switch p.sampleRate {
	case 32000, 44100, 48000:
	default:
		p.sampleRate = 48000
	}
	if p.segment <= 0 {
		p.segment = 2 * time.Second
	}
	return p
}

func (p slateProfile) key() string {
	return fmt.Sprintf("%s|%s|%s|%dx%d|%.3f|%s|%d|%d|%s", p.channel, p.container, p.video, p.width, p.height, p.fps, p.audio, p.channels, p.sampleRate, p.segment)
}

func slateProfileFromInfo(channel string, info *core.MediaInfo, container string) slateProfile {
	p := slateProfile{channel: channel, container: container}
	if info != nil {
		if v := info.Video(); v != nil {
			p.video, p.width, p.height, p.fps = v.Codec, v.Width, v.Height, v.FrameRate
		}
		for _, a := range info.Streams {
			if a.Type == "audio" {
				p.audio, p.channels, p.sampleRate = a.Codec, a.Channels, a.SampleRate
				break
			}
		}
	}
	return p
}

type slateClip struct {
	ext     string
	dur     time.Duration
	data    []byte
	init    []byte
	initRes string
	scales  map[uint32]uint32
}

type slateEntry struct {
	clip *slateClip
	err  error
	used time.Time
	done chan struct{}
}

type slateCache struct {
	mu      sync.Mutex
	entries map[string]*slateEntry
	encode  chan struct{} // one encode at a time
}

const slateCacheLimit = 32

// slateFor returns a non-blocking lookup for channel c: the cached clip, or
// nil while the first request encodes it in the background.
func (s *Service) slateFor(c Channel) func(slateProfile) *slateClip {
	cache := &s.owned().slates
	return func(p slateProfile) *slateClip {
		p.channel = c.ID
		p = p.normalized()
		key := p.key()
		cache.mu.Lock()
		e := cache.entries[key]
		if e != nil {
			e.used = time.Now()
			cache.mu.Unlock()
			select {
			case <-e.done:
				return e.clip
			default:
				return nil
			}
		}
		e = &slateEntry{used: time.Now(), done: make(chan struct{})}
		if len(cache.entries) >= slateCacheLimit {
			var oldest string
			for k, v := range cache.entries {
				select {
				case <-v.done:
				default:
					continue
				}
				if oldest == "" || v.used.Before(cache.entries[oldest].used) {
					oldest = k
				}
			}
			delete(cache.entries, oldest)
		}
		cache.entries[key] = e
		cache.mu.Unlock()
		go func() {
			defer close(e.done)
			ctx, cancel := context.WithTimeout(s.serviceContext(), 2*time.Minute)
			defer cancel()
			select {
			case cache.encode <- struct{}{}:
				defer func() { <-cache.encode }()
			case <-ctx.Done():
				e.err = ctx.Err()
				return
			}
			started := time.Now()
			e.clip, e.err = s.encodeSlate(ctx, c, p)
			if e.err != nil {
				s.logger().Warn("live slate unavailable", "channel", c.ID, "profile", key, "err", e.err)
				// Allow a later retry instead of caching the failure.
				go func() {
					time.Sleep(30 * time.Second)
					cache.mu.Lock()
					if cache.entries[key] == e {
						delete(cache.entries, key)
					}
					cache.mu.Unlock()
				}()
				return
			}
			s.logger().Info("live slate encoded", "channel", c.ID, "profile", key, "bytes", len(e.clip.data), "elapsed", time.Since(started))
		}()
		return nil
	}
}

func (s *Service) ffmpegPath() string {
	if f, ok := s.transcoder().(interface{ FFmpegPath() string }); ok {
		return f.FFmpegPath()
	}
	return ""
}

var errNoSlateEncoder = errors.New("livetv: no ffmpeg for slate encoding")

// encodeSlate renders the channel's reconnecting screen and encodes one
// slate segment of exactly p.segment with a keyframe at its start.
func (s *Service) encodeSlate(ctx context.Context, c Channel, p slateProfile) (*slateClip, error) {
	ffmpeg := s.ffmpegPath()
	if ffmpeg == "" {
		return nil, errNoSlateEncoder
	}
	format := transcode.RawFormat{Width: p.width, Height: p.height, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	dec, cleanup, err := s.openSignalDecoder(ctx, s.screenRequest(c, "reconnecting"), format)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	if cleanup != nil {
		defer cleanup()
	}
	dir, err := os.MkdirTemp("", "ether-slate-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	secs := strconv.FormatFloat(p.segment.Seconds(), 'f', 3, 64)
	gop := strconv.Itoa(int(p.fps*p.segment.Seconds()+0.5) + 1)
	layout := map[int]string{1: "mono", 2: "stereo", 3: "2.1", 4: "quad", 5: "5.0", 6: "5.1"}[p.channels]
	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", "-s", fmt.Sprintf("%dx%d", p.width, p.height), "-framerate", "30", "-i", "pipe:0",
		"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s", p.sampleRate, layout),
		"-map", "0:v", "-map", "1:a", "-t", secs,
		"-vf", "fps=" + strconv.FormatFloat(p.fps, 'f', 3, 64) + ",format=yuv420p",
		"-g", gop, "-keyint_min", gop, "-sc_threshold", "0", "-bf", "0"}
	switch p.video {
	case "hevc":
		args = append(args, "-c:v", "libx265", "-preset", "ultrafast", "-crf", "24", "-x265-params", "log-level=error:scenecut=0", "-tag:v", "hvc1")
	default:
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage", "-crf", "20", "-profile:v", "high")
	}
	switch p.audio {
	case "ac3", "eac3":
		args = append(args, "-c:a", p.audio, "-b:a", "192k")
	default:
		args = append(args, "-c:a", "aac", "-b:a", "128k")
	}
	args = append(args, "-ac", strconv.Itoa(p.channels), "-ar", strconv.Itoa(p.sampleRate))
	segType, name := "mpegts", "s%d.ts"
	if p.container == "fmp4" {
		segType, name = "fmp4", "s%d.m4s"
	}
	args = append(args, "-f", "hls", "-hls_time", secs, "-hls_list_size", "0", "-hls_segment_type", segType,
		"-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, name), filepath.Join(dir, "slate.m3u8"))
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	frames := int(p.segment.Seconds()*30) + 2
	for i := 0; i < frames; i++ {
		f, err := dec.Next(ctx)
		if err != nil {
			break
		}
		if _, err := stdin.Write(f.Video); err != nil {
			break // ffmpeg has all it needs
		}
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("slate encode: %w: %s", err, stderr.String())
	}
	body, err := os.ReadFile(filepath.Join(dir, "slate.m3u8"))
	if err != nil {
		return nil, err
	}
	m, ok := parseMediaPlaylist(body, nil)
	if !ok || len(m.segments) == 0 {
		return nil, fmt.Errorf("slate encode produced no segment")
	}
	first := m.segments[0]
	data, err := os.ReadFile(filepath.Join(dir, filepath.Base(first.uri)))
	if err != nil {
		return nil, err
	}
	clip := &slateClip{ext: "ts", dur: first.dur, data: data}
	if p.container == "fmp4" {
		clip.ext = "m4s"
		if clip.init, err = os.ReadFile(filepath.Join(dir, "init.mp4")); err != nil {
			return nil, err
		}
		clip.scales = mp4Timescales(clip.init)
		clip.initRes = "slate-" + stableID(p.key()) + ".mp4"
	} else if !isTS(data) {
		return nil, fmt.Errorf("slate encode produced no transport stream")
	}
	if clip.dur <= 0 {
		clip.dur = p.segment
	}
	return clip, nil
}

// limitedBuffer keeps the tail of a process's stderr.
type limitedBuffer struct{ b []byte }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.b = append(l.b, p...)
	if len(l.b) > 4096 {
		l.b = l.b[len(l.b)-4096:]
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.b) }
