package transcodebench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

type ffprobeDoc struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Profile   string `json:"profile"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Channels  int    `json:"channels"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
}

// ExpectedDimensions returns the exact output size implied by the source
// dimensions and the decision, mirroring transcode's outputSize scaling
// (aspect preserved, even sizes) for encodes and the untouched source size
// for stream copy. ok is false when the source size is unknown.
func ExpectedDimensions(dec core.Decision, cfg Config, srcW, srcH int) (w, h int, ok bool) {
	if srcW <= 0 || srcH <= 0 {
		return 0, 0, false
	}
	if dec.VideoCopy || cfg.Mode == ModeRemux {
		return srcW, srcH, true // copy never scales, not even to even sizes
	}
	maxH := dec.MaxHeight
	if maxH <= 0 {
		maxH = cfg.MaxHeight
	}
	if maxH <= 0 || maxH >= srcH {
		return evenDim(srcW), evenDim(srcH), true
	}
	nw := int(float64(srcW)*float64(maxH)/float64(srcH) + 0.5)
	return evenDim(nw), evenDim(maxH), true
}

func evenDim(n int) int {
	if n%2 != 0 {
		n--
	}
	if n < 2 {
		n = 2
	}
	return n
}

// VerifyOutput probes a retained output file and checks the actual video
// codec/dimensions against the decision. Pure checking lives in checkOutput
// so it can be unit-tested without ffprobe.
func verifyOutput(ctx context.Context, ffprobe, path string, dec core.Decision, cfg Config, src *StreamSummary) OutputProbe {
	op := OutputProbe{}
	if ffprobe == "" {
		op.Error = "no ffprobe path from Manager"
		op.Detail = "ffprobe unavailable"
		return op
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := mediaproc.CommandContext(ctx, mediaproc.ProbeTimeout, ffprobe, "-hide_banner", "-loglevel", "error",
		"-show_streams", "-show_format", "-of", "json", path)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		op.Error = fmt.Sprintf("%v: %s", err, strings.TrimSpace(truncate(errb.String(), 300)))
		op.Detail = "ffprobe failed on retained output"
		return op
	}
	var doc ffprobeDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		op.Error = err.Error()
		op.Detail = "parse ffprobe output"
		return op
	}
	op.Container = doc.Format.FormatName
	if doc.Format.Duration != "" {
		var f float64
		if _, err := fmt.Sscanf(doc.Format.Duration, "%g", &f); err == nil {
			op.DurationSec = f
		}
	}
	for _, s := range doc.Streams {
		op.Streams = append(op.Streams, StreamSummary{Type: s.CodecType, Codec: s.CodecName,
			Profile: s.Profile, Width: s.Width, Height: s.Height, Channels: s.Channels})
	}
	op.Verified, op.Detail = checkOutput(op, dec, cfg, src)
	return op
}

// checkOutput verifies codec and exact dimensions against the decision.
// For a remux the expected codec/size are the copied source's. Dimensions
// must match ExpectedDimensions exactly (source/aspect/cap), not merely
// stay under a height cap.
func checkOutput(op OutputProbe, dec core.Decision, cfg Config, src *StreamSummary) (bool, string) {
	var v *StreamSummary
	for i := range op.Streams {
		if op.Streams[i].Type == "video" {
			v = &op.Streams[i]
			break
		}
	}
	if v == nil {
		// Audio-only output is acceptable only when the decision had no video.
		if dec.VideoCodec == "" {
			return true, "no video stream (audio-only decision)"
		}
		return false, "output has no video stream"
	}
	want := strings.ToLower(dec.VideoCodec)
	if want != "" && !strings.EqualFold(v.Codec, want) {
		return false, fmt.Sprintf("output video codec %q != expected %q", v.Codec, want)
	}
	if v.Width <= 0 || v.Height <= 0 {
		return false, "output video has no dimensions"
	}
	if src != nil {
		if expW, expH, ok := ExpectedDimensions(dec, cfg, src.Width, src.Height); ok {
			if v.Width != expW || v.Height != expH {
				return false, fmt.Sprintf("output dimensions %dx%d != expected %dx%d (source %dx%d, maxHeight %d)",
					v.Width, v.Height, expW, expH, src.Width, src.Height, dec.MaxHeight)
			}
			return true, fmt.Sprintf("video %s %dx%d (expected %dx%d) profile=%q", v.Codec, v.Width, v.Height, expW, expH, v.Profile)
		}
	}
	// Source size unknown: cannot compute the exact expectation; fall
	// back to the cap check rather than inventing dimensions.
	maxH := dec.MaxHeight
	if cfg.Mode == ModeTranscode && cfg.MaxHeight > 0 {
		maxH = cfg.MaxHeight
	}
	if maxH > 0 && v.Height > maxH {
		return false, fmt.Sprintf("output height %d exceeds max-height %d", v.Height, maxH)
	}
	return true, fmt.Sprintf("video %s %dx%d profile=%q (source dimensions unknown; cap-checked only)", v.Codec, v.Width, v.Height, v.Profile)
}

func ffprobeVersion(ctx context.Context, ffprobe string) string {
	if ffprobe == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := mediaproc.CommandContext(ctx, mediaproc.ProbeTimeout, ffprobe, "-hide_banner", "-version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}
