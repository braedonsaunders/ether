package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// pictureCrop removes only demonstrated baked-in side bars during a
// static-header source change. It is not a general automatic zoom mode.
type pictureCrop struct{ x, y, width, height int }

// swapPictureCrop never holds the session lock while reading the replacement.
// The sample is small and bounded. Unreadable samples reject the candidate
// before publication; complete samples without proven bars retain normal fit.
func (s *Session) swapPictureCrop(ctx context.Context, input string, info core.MediaInfo, at time.Duration) (*pictureCrop, error) {
	s.mu.Lock()
	if input == s.input {
		crop := s.inputCrop
		s.mu.Unlock()
		return crop, nil
	}
	if sample, ok := s.pictureSamples[input]; ok {
		s.mu.Unlock()
		return sample, nil
	}
	c, live, burn := s.contract, s.liveInput, s.decision.BurnSubtitles
	s.mu.Unlock()
	v := info.Video()
	if c == nil || live || burn || v == nil || v.Width <= 0 || v.Height <= 0 || c.rng != rangeSDR || encodedRange(v) != rangeSDR {
		return nil, nil
	}
	pic := pictureOf(v)
	aspect := float64(v.Width*pic.sarNum) / float64(v.Height*pic.sarDen)
	target := float64(c.width*c.pic.sarNum) / float64(c.height*c.pic.sarDen)
	// Only the wider replacement that would otherwise gain horizontal bars.
	if aspect < target*1.08 || aspect > target*1.6 {
		return nil, nil
	}
	const width, frames = 320, 8
	height := even(int(float64(width) * float64(v.Height) / float64(v.Width)))
	if height < 64 || height > 320 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-v", "error", "-nostdin", "-threads", "1", "-filter_threads", "1", "-probesize", "1048576", "-analyzeduration", "500000"}
	args = append(args, inputSafetyArgs(input, false)...)
	args = append(args, httpInputArgs(input, false)...)
	sampleInput := input
	if u, err := url.Parse(input); err == nil && u.Hostname() == "127.0.0.1" && strings.HasPrefix(u.Path, "/src/") {
		q := u.Query()
		q.Set("background", "1")
		u.RawQuery = q.Encode()
		sampleInput = u.String()
	}
	args = append(args, "-ss", secs(max(0, at)), "-i", sampleInput, "-map", fmt.Sprintf("0:%d", v.Index), "-an", "-sn", "-vf", fmt.Sprintf("fps=4,scale=%d:%d,format=gray", width, height), "-frames:v", strconv.Itoa(frames), "-threads", "1", "-f", "rawvideo", "-")
	args, err := s.encoderSampleArgs(sampleInput, args)
	if err != nil {
		return nil, fmt.Errorf("%w: unverified replacement picture input", ErrIncompatibleSwap)
	}
	out := &pictureSample{limit: width * height * frames}
	if err := s.encoderRun(ctx, Command{Path: s.m.ffmpeg, Args: args, Stdout: out, Stderr: io.Discard}); err != nil {
		s.log.Warn("replacement picture sample failed", "at", at, "timedOut", ctx.Err() != nil)
		return nil, fmt.Errorf("%w: replacement picture sample unavailable", ErrIncompatibleSwap)
	}
	if out.Len() != width*height*frames {
		return nil, fmt.Errorf("%w: incomplete replacement picture sample", ErrIncompatibleSwap)
	}
	crop := detectPictureBars(out.Bytes(), width, height, v, c)
	if crop != nil {
		s.log.Info("removing embedded side bars from replacement", "sourceWidth", v.Width, "sourceHeight", v.Height, "x", crop.x, "width", crop.width)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.mu.Lock()
	if s.pictureSamples == nil {
		s.pictureSamples = make(map[string]*pictureCrop)
	}
	if len(s.pictureSamples) >= 64 {
		clear(s.pictureSamples)
	}
	s.pictureSamples[input] = crop
	s.mu.Unlock()
	s.log.Info("replacement picture checked", "at", at, "cropped", crop != nil)
	return crop, nil
}

// PrepareSwap checks real replacement pixels while the old generation still
// owns playback. Failed reads reject only this candidate, never the title.
func (s *Session) PrepareSwap(ctx context.Context, input string, info core.MediaInfo, at time.Duration) error {
	if err := s.CheckSwap(info); err != nil {
		return err
	}
	_, err := s.swapPictureCrop(ctx, input, info, at)
	if err == nil {
		s.prepareAudioMatch(ctx, input, info, at)
	}
	return err
}

type pictureSample struct {
	bytes.Buffer
	limit int
}

func (b *pictureSample) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > b.limit {
		return 0, fmt.Errorf("picture sample exceeds limit")
	}
	return b.Buffer.Write(p)
}

func detectPictureBars(data []byte, w, h int, v *core.StreamInfo, c *videoContract) *pictureCrop {
	const frames = 8
	if w <= 0 || h <= 0 || len(data) != w*h*frames {
		return nil
	}
	// Require both edges to remain dark across all eight sampled pictures.
	// Ignore only isolated compression noise, not logos or changing content.
	left, right := w, -1
	for f := 0; f < frames; f++ {
		frame := data[f*w*h : (f+1)*w*h]
		bright := 0
		for _, p := range frame {
			if p > 40 {
				bright++
			}
		}
		if bright < w*h/10 {
			return nil
		} // fades / dark scenes are not evidence
		fl, fr := w, -1
		for x := 0; x < w; x++ {
			count := 0
			for y := 0; y < h; y++ {
				if frame[y*w+x] > 24 {
					count++
				}
			}
			if count > max(1, h/100) {
				fl = min(fl, x)
				fr = max(fr, x)
			}
		}
		if fl >= fr {
			return nil
		}
		// Changing edge positions indicate content, not fixed pillarboxing.
		if f > 0 && (absInt(fl-left) > 2 || absInt(fr-right) > 2) {
			return nil
		}
		left = min(left, fl)
		right = max(right, fr)
	}
	if left < w/25 || w-1-right < w/25 || absInt(left-(w-1-right)) > 3 {
		return nil
	}
	// Round outwards: do not remove any demonstrated picture pixels.
	x := (left * v.Width / w) / 2 * 2
	end := ((right+1)*v.Width + w - 1) / w
	end = (end + 1) / 2 * 2
	width := min(end, v.Width) - x
	p := pictureOf(v)
	aspect := float64(width*p.sarNum) / float64(v.Height*p.sarDen)
	target := float64(c.width*c.pic.sarNum) / float64(c.height*c.pic.sarDen)
	if math.Abs(aspect/target-1) > .06 {
		return nil
	}
	return &pictureCrop{x: x, width: width, height: v.Height}
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
