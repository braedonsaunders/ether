package transcode

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const maxRTXKeySegments = 32768

// buildRTXArgs builds a direct NVDEC -> CUDA NGX VSR -> NVENC -> libavformat
// pipeline. It never pipes decoded frames through Go or FFmpeg. keyframes is
// a bounded frame-index file whose forced IDRs follow the session clock,
// including fractional frame rates (a fixed rounded GOP drifts over time).
func buildRTXArgs(s runSpec) (args []string, keyframes []byte, err error) {
	fail := func(reason string) ([]string, []byte, error) { return nil, nil, fmt.Errorf("RTX Video: %s", reason) }
	if !enhancementSelected(s.Decision) {
		return fail("enhancement is not selected")
	}
	if reason := rtxSourceReason(s.Info, s.Decision); reason != "" {
		return fail(reason)
	}
	if s.LiveInput || s.Info.Duration <= 0 || s.Info.Duration > 24*time.Hour {
		return fail("only finite VOD up to 24 hours is supported")
	}
	if s.Protocol != core.ProtocolHLS && s.Protocol != core.ProtocolDASH {
		return fail("unsupported transport")
	}
	if s.ChunkDir != "" || len(s.KeyTimes) > 0 || s.FastStart {
		return fail("adaptive keyframe/ramp plan requires the ordinary transcoder")
	}
	if s.Decision.Container != ContainerMPEGTS && s.Decision.Container != ContainerFMP4 {
		return fail("unsupported segment container")
	}
	e := s.Decision.Enhancement
	if e.Width < 2 || e.Width > 3840 || e.Height < 2 || e.Height > 2160 || e.Width%2 != 0 || e.Height%2 != 0 || !validRTXQuality(e.Quality) || s.Decision.VideoCopy || s.Decision.VideoCodec != "h264" {
		return fail("invalid enhanced output plan")
	}
	if s.SegDur < time.Second || s.SegDur > 30*time.Second || s.Start < 0 || s.Start >= s.Info.Duration {
		return fail("unsupported segment duration or start")
	}
	u, parseErr := url.Parse(s.Input)
	if parseErr != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fail("backend requires a safe HTTP(S) media URL")
	}
	headers, headerErr := validatedInputHeaders(s.InputHeaders)
	if headerErr != nil {
		return fail("invalid input headers")
	}
	videoCount := 0
	for _, v := range s.Info.Streams {
		if v.Type == "video" {
			videoCount++
		}
	}
	if videoCount != 1 {
		return fail("multiple video tracks require the ordinary transcoder")
	}
	v := s.Info.Video()
	fps, _ := rtxFrameRate(v.FrameRate)
	seek, trim, offset, seekErr := rtxSeek(s.Info, s.Start)
	if seekErr != nil {
		return nil, nil, seekErr
	}
	if trim > 0 && math.Abs(s.SegDur.Seconds()*fps-math.Round(s.SegDur.Seconds()*fps)) > 1e-5 {
		return fail("fractional-rate seek trimming disables native keyframe scheduling; ordinary transcoder required")
	}
	args = []string{"--avhw", "--device", "0", "--video-track", "1", "-i", s.Input,
		"--input-option", "format_whitelist:" + vodInputFormats,
		"--input-option", "protocol_whitelist:http,https,tcp,tls,crypto",
		"--input-option", "rw_timeout:15000000",
		"--input-analyze", "5", "--input-probesize", "5000000"}
	if s.BigProbe {
		args = append(args, "--input-analyze", "20", "--input-probesize", "20000000")
	}
	for key, value := range s.InputScopeHeaders {
		headers[key] = value
	}
	ha := inputHeaderArgs(headers)
	for i := 0; i+1 < len(ha); i += 2 {
		args = append(args, "--input-option", strings.TrimPrefix(ha[i], "-")+":"+ha[i+1])
	}
	if seek > 0 {
		args = append(args, "--seek", strconv.FormatFloat(seek.Seconds(), 'f', 6, 64))
	}
	if trim > 0 {
		args = append(args, "--trim", strconv.Itoa(trim)+":0")
	}
	level := "4.1"
	if e.Width*e.Height > 1920*1088 || fps > 30 {
		level = "5.1"
	}
	videoKbps := s.Decision.BitrateKbps - decisionAudioKbps(s.Info, s.Decision)
	bufferKbits := videoKbps * 2
	if s.Rate.Quality {
		videoKbps = minPositive(videoKbps, s.Rate.MaxrateKbps)
		if s.Rate.BufsizeKbps > 0 {
			bufferKbits = s.Rate.BufsizeKbps
		}
	}
	if videoKbps < minRTXVideoKbps(e.Height) {
		return fail("insufficient video bitrate")
	}
	args = append(args, "--vpp-resize", fmt.Sprintf("algo=ngx-vsr,vsr-quality=%d", e.Quality), "--output-res", fmt.Sprintf("%dx%d", e.Width, e.Height),
		"-c", "h264", "--profile", "high", "--level", level, "--output-depth", "8", "--output-csp", "yuv420",
		"--vbr", "0", "--vbr-quality", "20", "--max-bitrate", strconv.Itoa(videoKbps), "--vbv-bufsize", strconv.Itoa(bufferKbits),
		"--gop-len", strconv.Itoa(int(math.Ceil(s.SegDur.Seconds()*fps))), "--strict-gop", "--lookahead", "0", "--no-i-adapt", "--no-b-adapt", "--bframes", "0", "--repeat-headers",
		"--colormatrix", "auto", "--colorprim", "auto", "--transfer", "auto", "--metadata", "clear")
	// NVEnc audio track numbers are 1-based audio ordinals, not ffprobe's
	// absolute stream indexes. Preserve exactly the selected track.
	audioTrack := 0
	for _, stream := range s.Info.Streams {
		if stream.Type == "audio" {
			audioTrack++
			if stream.Index == s.Decision.AudioStream {
				args = append(args, "--audio-copy", strconv.Itoa(audioTrack))
				break
			}
		}
	}
	var keys strings.Builder
	for n := 1; ; n++ {
		boundary := s.Start + time.Duration(n)*s.SegDur
		if boundary >= s.Info.Duration {
			break
		}
		if n > maxRTXKeySegments {
			return fail("title exceeds the bounded keyframe plan")
		}
		// First frame on or after each boundary; output_ts_offset is the
		// actual first frame's time, not the nominal segment time.
		frame := int(math.Ceil((boundary-offset).Seconds()*fps - 1e-6))
		if frame > 0 {
			fmt.Fprintln(&keys, frame)
		}
	}
	keyframes = []byte(keys.String())
	args = append(args, "--keyfile", filepath.Join(s.Dir, "rtx-keyframes.txt"))
	args = append(args, "-m", "output_ts_offset:"+strconv.FormatFloat(offset.Seconds(), 'f', 9, 64), "-m", "avoid_negative_ts:disabled")
	mux := hlsMuxArgs(s)
	if s.Protocol == core.ProtocolDASH {
		mux = dashMuxArgs(s)
	}
	// Reuse Ether's filenames and segmentation settings, passing only
	// libavformat options (FFmpeg CLI-only options have no NVEnc equivalent).
	for i := 0; i < len(mux)-1; i += 2 {
		flag, value := mux[i], mux[i+1]
		switch flag {
		case "-f":
			args = append(args, "-f", value)
		case "-avoid_negative_ts", "-max_muxing_queue_size":
		default:
			args = append(args, "-m", strings.TrimPrefix(flag, "-")+":"+filepath.ToSlash(value))
		}
	}
	args = append(args, "-o", filepath.ToSlash(filepath.Join(s.Dir, s.Playlist)))
	return args, keyframes, nil
}

// --seek advances to the first source keyframe at/after its argument. Pick
// an indexed keyframe, then trim the short remaining frame span. Without an
// index a long decode-from-zero would be unbounded: use ordinary FFmpeg.
func rtxSeek(info core.MediaInfo, start time.Duration) (seek time.Duration, trim int, offset time.Duration, err error) {
	if start == 0 {
		return 0, 0, 0, nil
	}
	v := info.Video()
	if v == nil || v.FrameRate <= 0 {
		return 0, 0, 0, fmt.Errorf("RTX Video: missing frame rate")
	}
	key := time.Duration(-1)
	for _, k := range info.Keyframes {
		if k.Time <= start && k.Time >= 0 && k.Time > key {
			key = k.Time
		}
	}
	if key < 0 || start-key > 30*time.Second {
		return 0, 0, 0, fmt.Errorf("RTX Video: exact seek requires a nearby source keyframe index")
	}
	fps, ok := rtxFrameRate(v.FrameRate)
	if !ok {
		return 0, 0, 0, fmt.Errorf("RTX Video: unsupported frame-rate rational")
	}
	trim = int(math.Ceil((start-key).Seconds()*fps - 1e-6))
	offset = key + time.Duration(math.Round(float64(trim)*float64(time.Second)/fps))
	// NVEnc stores seek as float32. Seek just before the indexed point to
	// avoid rounding past it; never before the previous distinct keyframe.
	seek = key
	if seek > 0 {
		margin := max(time.Millisecond, time.Duration(math.Ceil(key.Seconds()/8192))*time.Millisecond)
		seek -= min(margin, seek)
	}
	return seek, trim, offset, nil
}

func redactedRTXArgs(args []string) []string {
	out := redactedInputArgs(args)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--input-option" {
			i++
			if strings.HasPrefix(out[i], "headers:") || strings.HasPrefix(out[i], "user_agent:") || strings.HasPrefix(out[i], "referer:") {
				out[i] = "[redacted]"
			}
		}
	}
	return out
}
