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
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/onair/scene"
)

const Profile = "360p30-h264-high-l4.0-yuv420p-g120-aac48k-stereo-v3"

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
	InputArgs, VideoArgs   []string
	Sounds                 []SoundInput
	Background             *VideoInput
	// MaxSeconds is 28 for look moments; plugin segments may opt into 600.
	MaxSeconds          int
	MaxRate, BufferSize string
	Audio               *scene.Audio
	AudioVolume         *float64
}

// StreamWriter encodes sequential RGBA frames into an atomic content-addressed
// master MP4 while a cold moment is simultaneously airing live.
type StreamWriter struct {
	cancel    context.CancelFunc
	cmd       *mediaproc.Cmd
	pipe      io.WriteCloser
	stderr    bytes.Buffer
	tmpPath   string
	audioTemp string
	outPath   string
	width     int
	height    int
	expected  int
	written   int
	closed    bool
}

// StartStream launches an FFmpeg master encoder identical in profile to WithOptions.
func StartStream(ctx context.Context, ffmpegPath string, n, fps int, audio, out string, opts Options) (*StreamWriter, error) {
	limit := opts.MaxSeconds
	if limit == 0 {
		limit = 28
	}
	if limit < 4 || limit > 600 {
		return nil, fmt.Errorf("invalid master duration limit")
	}
	width, height := opts.Width, opts.Height
	if width == 0 {
		width = 1920
	}
	if height == 0 {
		height = 1080
	}
	if width < 2 || width > 1920 || height < 2 || height > 1080 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("invalid encode dimensions")
	}
	var audioTemp string
	if opts.Audio != nil && audio == "" && fps == 30 && n > 0 && n%120 == 0 && n <= limit*30 {
		f, e := os.CreateTemp(filepath.Dir(out), ".onair-audio-*.wav")
		if e != nil {
			return nil, e
		}
		audioTemp = f.Name()
		audio = audioTemp
		e = WriteWAV(f, *opts.Audio, n/fps)
		f.Close()
		if e != nil {
			os.Remove(audioTemp)
			return nil, e
		}
	}
	if n <= 0 || fps != 30 || n%120 != 0 || n > limit*30 {
		if audioTemp != "" {
			os.Remove(audioTemp)
		}
		return nil, fmt.Errorf("master requires 30 fps, 4-second multiples, within the declared duration limit")
	}
	if err := ctx.Err(); err != nil {
		if audioTemp != "" {
			os.Remove(audioTemp)
		}
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	tmp, e := os.CreateTemp(filepath.Dir(out), ".onair-*.mp4")
	if e != nil {
		cancel()
		if audioTemp != "" {
			os.Remove(audioTemp)
		}
		return nil, e
	}
	tmpPath := tmp.Name()
	tmp.Close()

	args, e := masterFFmpegArgs(width, height, n, fps, audio, tmpPath, opts)
	if e != nil {
		cancel()
		os.Remove(tmpPath)
		if audioTemp != "" {
			os.Remove(audioTemp)
		}
		return nil, e
	}
	var cmd *mediaproc.Cmd
	if runtime.GOOS != "windows" {
		cmd = mediaproc.CommandContext(streamCtx, mediaproc.EncodeTimeout, "nice", append([]string{"-n", "10", ffmpegPath}, args...)...)
	} else {
		cmd = mediaproc.CommandContext(streamCtx, mediaproc.EncodeTimeout, ffmpegPath, args...)
		lowerRenderPriority(cmd.Cmd)
	}
	w := &StreamWriter{
		cancel:    cancel,
		cmd:       cmd,
		tmpPath:   tmpPath,
		audioTemp: audioTemp,
		outPath:   out,
		width:     width,
		height:    height,
		expected:  n,
	}
	cmd.Stderr = &w.stderr
	pipe, e := cmd.StdinPipe()
	if e != nil {
		w.Abort()
		return nil, e
	}
	w.pipe = pipe
	if e = cmd.Start(); e != nil {
		w.Abort()
		return nil, e
	}
	return w, nil
}

func (w *StreamWriter) WriteFrame(frame *image.RGBA) error {
	if w == nil || w.closed {
		return fmt.Errorf("stream writer closed")
	}
	if frame == nil || frame.Rect.Dx() != w.width || frame.Rect.Dy() != w.height {
		return fmt.Errorf("frame %d must be %dx%d", w.written, w.width, w.height)
	}
	for y := frame.Rect.Min.Y; y < frame.Rect.Max.Y; y++ {
		off := frame.PixOffset(frame.Rect.Min.X, y)
		row := frame.Pix[off : off+w.width*4]
		if _, err := w.pipe.Write(row); err != nil {
			return fmt.Errorf("frame pipe: %w", err)
		}
	}
	w.written++
	return nil
}

func (w *StreamWriter) Close() error {
	if w == nil || w.closed {
		return nil
	}
	w.closed = true
	defer func() {
		w.cancel()
		os.Remove(w.tmpPath)
		if w.audioTemp != "" {
			os.Remove(w.audioTemp)
		}
	}()
	if w.written != w.expected {
		w.cancel()
		if w.pipe != nil {
			_ = w.pipe.Close()
		}
		if w.cmd != nil && w.cmd.Process != nil {
			_ = w.cmd.Wait()
		}
		return fmt.Errorf("incomplete master: wrote %d of %d frames", w.written, w.expected)
	}
	if w.pipe != nil {
		_ = w.pipe.Close()
	}
	if err := w.cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %.2048s", err, w.stderr.String())
	}
	return os.Rename(w.tmpPath, w.outPath)
}

func (w *StreamWriter) Abort() {
	if w == nil || w.closed {
		return
	}
	w.closed = true
	w.cancel()
	if w.pipe != nil {
		_ = w.pipe.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Wait()
	}
	if w.tmpPath != "" {
		_ = os.Remove(w.tmpPath)
	}
	if w.audioTemp != "" {
		_ = os.Remove(w.audioTemp)
	}
}

func masterFFmpegArgs(width, height, n, fps int, audio, path string, opts Options) ([]string, error) {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgba", "-s", fmt.Sprintf("%dx%d", width, height), "-r", strconv.Itoa(fps), "-i", "pipe:0"}
	args = append(append([]string{}, opts.InputArgs...), args...)
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
	args = append(args, "-map", videoMap, "-map", audioMap, "-t", strconv.FormatFloat(float64(n)/float64(fps), 'f', 6, 64), "-c:v", "libx264", "-preset", "superfast", "-crf", "20", "-threads", "1", "-profile:v", "high", "-level:v", "4.0", "-pix_fmt", "yuv420p", "-g", "120", "-keyint_min", "120", "-sc_threshold", "0", "-c:a", "aac", "-ar", "48000", "-ac", "2", "-b:a", "192k", "-movflags", "+faststart", path)
	if len(opts.VideoArgs) > 0 {
		start, end := -1, -1
		for i, a := range args {
			if a == "-c:v" {
				start = i
			}
			if a == "-profile:v" {
				end = i
				break
			}
		}
		if start >= 0 && end > start {
			args = append(append(append([]string{}, args[:start]...), opts.VideoArgs...), args[end:]...)
		}
	}
	// Hardware frames must remain on the device through the encoder.
	for _, a := range opts.VideoArgs {
		if a == "h264_vaapi" {
			for j := range args {
				if args[j] == "-pix_fmt" && j+1 < len(args) && args[j+1] == "yuv420p" {
					args[j+1] = "vaapi"
				}
			}
		}
	}
	if opts.MaxRate != "" {
		if opts.BufferSize == "" {
			opts.BufferSize = opts.MaxRate
		}
		ceiling, err := rate(opts.MaxRate)
		if err != nil {
			return nil, err
		}
		buffer, err := rate(opts.BufferSize)
		if err != nil {
			return nil, err
		}
		video := ceiling - max(256000, ceiling/10)
		if video < 64000 {
			return nil, fmt.Errorf("bitrate ceiling too low")
		}
		args = append(args[:len(args)-1], "-maxrate", strconv.Itoa(video), "-bufsize", strconv.Itoa(buffer), path)
	}
	if opts.AudioVolume != nil {
		v := *opts.AudioVolume
		if v < 0 || v > 1 {
			return nil, fmt.Errorf("invalid audio volume")
		}
		args = append(args[:len(args)-1], "-af", fmt.Sprintf("volume=%f", v), path)
	}
	return args, nil
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
	if workers < 1 {
		return fmt.Errorf("master requires 30 fps, 4-second multiples, within the declared duration limit")
	}
	writer, err := StartStream(ctx, ffmpegPath, n, fps, audio, out, opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
				if p.frame == nil || p.frame.Rect.Dx() != writer.width || p.frame.Rect.Dy() != writer.height {
					renderErr = fmt.Errorf("frame %d must be 1920x1080", next)
					break
				}
				if err := writer.WriteFrame(p.frame); err != nil {
					renderErr = err
					break
				}
				next++
			}
		}
	}
	close(jobs)
	if renderErr != nil {
		cancel()
		writer.Abort()
		wg.Wait()
		return renderErr
	}
	err = writer.Close()
	cancel()
	wg.Wait()
	return err
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
