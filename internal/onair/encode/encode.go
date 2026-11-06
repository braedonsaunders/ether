// Package encode writes content-addressed On Air master clips off request paths.
package encode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/onair/scene"
)

const Profile = "1080p30-h264-high-l4.0-yuv420p-g120-aac48k-stereo-v1"

// Hash uses encoding/json's stable map ordering, with length-delimited inputs.
func Hash(rendererVersion string, doc, tokens, bindings, assetDigests, profile any) string {
	b, e := json.Marshal([]any{rendererVersion, doc, tokens, bindings, assetDigests, profile})
	if e != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Encode uses half the available processors for frames. Audio is an optional path.
func Encode(ctx context.Context, ffmpegPath string, frames func(int) *image.RGBA, n, fps int, audio, out string) error {
	return WithWorkers(ctx, ffmpegPath, frames, n, fps, audio, out, max(1, runtime.GOMAXPROCS(0)/2))
}

// Options allows Signal loops to honor a negotiated session ceiling and size.
type Options struct {
	Workers, Width, Height int
	Sounds                 []SoundInput
	Background             *VideoInput
	// MaxSeconds is 28 for look moments; plugin segments may opt into 600.
	MaxSeconds          int
	MaxRate, BufferSize string
	Audio               *scene.Audio
	AudioVolume         *float64
}

// WithWorkers is also used by the reproducible four-worker Phase 0 measurement.
func WithWorkers(ctx context.Context, ffmpegPath string, frames func(int) *image.RGBA, n, fps int, audio, out string, workers int) error {
	return WithOptions(ctx, ffmpegPath, frames, n, fps, audio, out, Options{Workers: workers})
}
func WithOptions(ctx context.Context, ffmpegPath string, frames func(int) *image.RGBA, n, fps int, audio, out string, opts Options) error {
	workers := opts.Workers
	if workers == 0 {
		workers = max(1, runtime.GOMAXPROCS(0)/2)
	}
	limit := opts.MaxSeconds
	if limit == 0 {
		limit = 28
	}
	if limit < 4 || limit > 600 {
		return fmt.Errorf("invalid master duration limit")
	}
	width, height := opts.Width, opts.Height
	if width == 0 {
		width = 1920
	}
	if height == 0 {
		height = 1080
	}
	if width < 2 || width > 1920 || height < 2 || height > 1080 || width%2 != 0 || height%2 != 0 {
		return fmt.Errorf("invalid encode dimensions")
	}
	if opts.Audio != nil && audio == "" && fps == 30 && n > 0 && n%120 == 0 && n <= limit*30 {
		f, e := os.CreateTemp(filepath.Dir(out), ".onair-audio-*.wav")
		if e != nil {
			return e
		}
		audio = f.Name()
		defer os.Remove(audio)
		e = WriteWAV(f, *opts.Audio, n/fps)
		f.Close()
		if e != nil {
			return e
		}
	}

	if n <= 0 || fps != 30 || n%120 != 0 || n > limit*30 || workers < 1 {
		return fmt.Errorf("master requires 30 fps, 4-second multiples, within the declared duration limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tmp, e := os.CreateTemp(filepath.Dir(out), ".onair-*.mp4")
	if e != nil {
		return e
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgba", "-s", fmt.Sprintf("%dx%d", width, height), "-r", strconv.Itoa(fps), "-i", "pipe:0"}
	if audio != "" {
		args = append(args, "-i", audio)
	} else {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo")
	}

	for _, sound := range opts.Sounds {
		args = append(args, "-i", sound.Path)
	}
	videoMap := "0:v:0"
	audioMap := "1:a:0"
	filters := []string{}
	if len(opts.Sounds) > 0 {
		filters = append(filters, MixFilter("1:a:0", opts.Sounds, 2, float64(n)/float64(fps)))
		audioMap = "[mixed]"
	}
	if bg := opts.Background; bg != nil {
		idx := 2 + len(opts.Sounds)
		args = append(args, "-ss", strconv.FormatFloat(bg.Start, 'f', 6, 64), "-i", bg.Path)
		scale := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", width, height, width, height)
		if bg.Fit == "contain" {
			scale = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2", width, height, width, height)
		}
		filters = append(filters, fmt.Sprintf("[%d:v:0]setpts=PTS-STARTPTS,%s,tpad=stop_mode=clone:stop_duration=%g[background];[background][0:v:0]overlay=0:0:shortest=1[composite]", idx, scale, float64(n)/float64(fps)))
		videoMap = "[composite]"
	}
	if len(filters) > 0 {
		args = append(args, "-filter_complex", strings.Join(filters, ";"))
	}
	args = append(args, "-map", videoMap, "-map", audioMap, "-t", strconv.FormatFloat(float64(n)/float64(fps), 'f', 6, 64), "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-threads", "2", "-profile:v", "high", "-level:v", "4.0", "-pix_fmt", "yuv420p", "-g", "120", "-keyint_min", "120", "-sc_threshold", "0", "-c:a", "aac", "-ar", "48000", "-ac", "2", "-b:a", "192k", "-movflags", "+faststart", path)
	if opts.MaxRate != "" {
		if opts.BufferSize == "" {
			opts.BufferSize = opts.MaxRate
		}
		ceiling, err := rate(opts.MaxRate)
		if err != nil {
			return err
		}
		buffer, err := rate(opts.BufferSize)
		if err != nil {
			return err
		}
		video := ceiling - max(256000, ceiling/10)
		if video < 64000 {
			return fmt.Errorf("bitrate ceiling too low")
		}
		args = append(args[:len(args)-1], "-maxrate", strconv.Itoa(video), "-bufsize", strconv.Itoa(buffer), "-x264-params", "vbv-init=0.1", path)
	}
	if opts.AudioVolume != nil {
		v := *opts.AudioVolume
		if v < 0 || v > 1 {
			return fmt.Errorf("invalid audio volume")
		}
		args = append(args[:len(args)-1], "-af", fmt.Sprintf("volume=%f", v), path)
	}
	var cmd *mediaproc.Cmd
	// nice execs ffmpeg, so CommandContext owns the encoder PID directly.
	if runtime.GOOS != "windows" {
		cmd = mediaproc.CommandContext(ctx, mediaproc.EncodeTimeout, "nice", append([]string{"-n", "10", ffmpegPath}, args...)...)
	} else {
		cmd = mediaproc.CommandContext(ctx, mediaproc.EncodeTimeout, ffmpegPath, args...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, e := cmd.StdinPipe()
	if e != nil {
		return e
	}
	if e = cmd.Start(); e != nil {
		return e
	}
	type result struct {
		i     int
		frame *image.RGBA
		err   error
	}
	jobs := make(chan int)
	results := make(chan result, workers)
	var wg sync.WaitGroup
	// A sliding window bounds both in-flight work and the reorder buffer.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				var im *image.RGBA
				var err error
				func() {
					defer func() {
						if p := recover(); p != nil {
							err = fmt.Errorf("frame %d panicked: %v", i, p)
						}
					}()
					im = frames(i)
				}()
				select {
				case results <- result{i, im, err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	next, submitted := 0, 0
	pending := map[int]result{}
	var renderErr error
	for next < n && renderErr == nil {
		var send chan int
		if submitted < n && submitted-next < workers {
			send = jobs
		}
		select {
		case <-ctx.Done():
			renderErr = ctx.Err()
		case send <- submitted:
			submitted++
		case res, ok := <-results:
			if !ok {
				renderErr = fmt.Errorf("frame workers ended")
				break
			}
			pending[res.i] = res
			for {
				p, ok := pending[next]
				if !ok {
					break
				}
				delete(pending, next)
				if p.err != nil {
					renderErr = p.err
					break
				}
				if p.frame == nil || p.frame.Rect.Dx() != width || p.frame.Rect.Dy() != height {
					renderErr = fmt.Errorf("frame %d must be 1920x1080", next)
					break
				}
				for y := p.frame.Rect.Min.Y; y < p.frame.Rect.Max.Y; y++ {
					off := p.frame.PixOffset(p.frame.Rect.Min.X, y)
					row := p.frame.Pix[off : off+width*4]
					if _, e = pipe.Write(row); e != nil {
						renderErr = fmt.Errorf("frame pipe: %w", e)
						break
					}
				}
				if renderErr != nil {
					break
				}
				next++
			}
		}
	}
	close(jobs)
	if renderErr != nil {
		cancel()
	}
	pipe.Close()
	e = cmd.Wait()
	cancel()
	wg.Wait()
	if renderErr != nil {
		return renderErr
	}
	if e != nil {
		return fmt.Errorf("ffmpeg: %w: %.2048s", e, stderr.String())
	}
	return os.Rename(path, out)
}

func rate(s string) (int, error) {
	factor := 1.0
	if strings.HasSuffix(s, "M") {
		factor = 1e6
		s = strings.TrimSuffix(s, "M")
	} else if strings.HasSuffix(s, "k") || strings.HasSuffix(s, "K") {
		factor = 1e3
		s = s[:len(s)-1]
	}
	f, e := strconv.ParseFloat(s, 64)
	if e != nil || f <= 0 || f*factor > 1e9 {
		return 0, fmt.Errorf("invalid bitrate")
	}
	return int(f * factor), nil
}
