package transcode

import (
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// runSpec is everything needed to build one ffmpeg HLS run.
type runSpec struct {
	HealTimestamps bool
	// RampClock keeps every run of a fast-start session on the steady clock.
	RampClock bool
	// AuthoredTimeline preserves adaptive sessions with a separate clock.
	AuthoredTimeline bool
	QSVMode          string // session-local retry override; never mutates shared capabilities
	SilenceAudio     bool
	// Only a retry of an initial run whose muxer wrote a wrapped tfdt.
	NormalizeNegativeDTS bool
	TextSubtitlePaths    map[int]string
	SoftSubtitlePath     string
	SoftSubtitleIndex    int
	LoopInput            bool
	LiveInput            bool
	LiveFromBacklog      bool // live HLS input: start at its first listed segment
	CopyTimestamps       bool
	// RTXTimeline preserves the native session's clock and rendition-local
	// initialization paths when ordinary FFmpeg takes over.
	RTXTimeline       bool
	InputHeaders      map[string]string
	InputScopeHeaders map[string]string
	Input             string
	Info              core.MediaInfo
	Decision          core.Decision
	// Start is the media timestamp of the run's first segment (StartSeg).
	Start time.Duration
	// SeekFrom is the input seek point of a video copy whose demuxer cannot
	// find the keyframe before Start itself (copyKeyframeBefore), when
	// HasSeekFrom.
	SeekFrom       time.Duration
	HasSeekFrom    bool
	SegDur         time.Duration
	StartSeg       int
	Dir            string
	Playlist       string // ffmpeg's own playlist file name, in Dir
	InitName       string // fMP4 init segment file name, in Dir
	VideoTimescale uint32 // retained fMP4 clock, zero for the first run
	// Accel is the hardware to use; Name AccelNone (or "") means CPU.
	Accel    core.HWAccel
	Features features
	// BigProbe raises probesize/analyzeduration (retry after a failed start).
	BigProbe bool
	// InputTimeout bounds HTTP reads for the progressive stream; a temporary
	// source stall can then reconnect without splicing another Matroska stream.
	InputTimeout time.Duration
	// Repeat encoder parameter sets on recovery so a decoder holding the
	// initial fMP4 header can decode the replacement encoder's keyframes.
	InBandHeaders bool
	// Protocol is core.ProtocolHLS or core.ProtocolDASH. For DASH, Dir is
	// the run's own directory and Playlist ffmpeg's manifest in it.
	Protocol string
	// ChunkDur is the DASH muxer's chunk length (default 1s).
	ChunkDur time.Duration
	// FastStart selects the low-latency encode (fastest presets) used for
	// the fast-start ramp segments.
	FastStart bool
	// Rate is the quality-targeted VBV rate control for this run; the zero
	// value keeps the historic bitrate-target encoding.
	Rate rateControl
	// ChunkDir, when set, makes an HLS run cut at (nearly) every keyframe
	// into this directory, numbered from 0, for the session to regroup by
	// timestamp (chunks.go). ChunkCut is the muxer's segment length then.
	ChunkDir string
	ChunkCut time.Duration
	// KeyTimes, when set, are the output timestamps a video encode forces
	// keyframes at (a chunked run's plan boundaries) instead of every
	// SegDur.
	KeyTimes []time.Duration
	// Target, when set, is the static-header session's video contract
	// every encode must deliver (contract.go).
	Target      *videoContract
	PictureCrop *pictureCrop
	AudioGainDB float64
}

// segmentExt is the segment file extension for a decision's container.
func segmentExt(container string) string {
	if container == ContainerMatroska {
		return "mkv"
	}
	if container == ContainerFMP4 {
		return "m4s"
	}
	return "ts"
}

// segmentFile is the on-disk name of segment n.
func segmentFile(n int, ext string) string { return fmt.Sprintf("%05d.%s", n, ext) }

func secs(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
}

// usesHW reports whether the run encodes video on hardware.
func (s runSpec) usesHW() bool {
	if s.Decision.VideoCopy || s.Accel.Name == "" || s.Accel.Name == AccelNone {
		return false
	}
	enc, hw := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
	return hw && enc != ""
}

// videoEncoder picks the encoder for a target codec: the confirmed hardware
// encoder when there is one, else libx264/libx265.
func videoEncoder(codec string, accel core.HWAccel, f features) (name string, hw bool) {
	if codec == "" {
		codec = "h264"
	}
	if accel.Name != "" && accel.Name != AccelNone {
		suffix := accel.Name
		hwName := codec + "_" + suffix
		if contains(accel.Encoders, hwName) {
			return hwName, true
		}
	}
	switch codec {
	case "hevc":
		// A missing encoder list is not permission to assume libx265 exists.
		if f.hasEncoder("libx265") {
			return "libx265", false
		}
		return "libx264", false // no HEVC encoder at all: caller asked for the impossible
	default:
		return "libx264", false
	}
}

// warnVideoEncoderFallback logs the otherwise silent HEVC->H.264 fallback
// in videoEncoder: HEVC was requested but neither a hardware HEVC encoder
// nor libx265 is available, so the run encodes libx264 instead. It is a
// no-op for every other encoder choice. It reports whether it warned so
// the per-session caller can latch it to at most once per session
// (videoEncoder itself is pure and runs several times per run).
func warnVideoEncoderFallback(log *slog.Logger, codec string, accel core.HWAccel, f features) bool {
	if log == nil || codec != "hevc" {
		return false
	}
	if enc, hw := videoEncoder(codec, accel, f); !hw && enc == "libx264" {
		log.Warn("HEVC requested but no HEVC encoder available; falling back to libx264",
			"requestedCodec", codec, "encoder", enc)
		return true
	}
	return false
}

// buildArgs assembles the ffmpeg command line for a run: HLS, or DASH
// when the spec says so.
func buildArgs(s runSpec) []string {
	args := buildCodecArgs(s)
	if s.Protocol == core.ProtocolHTTP {
		return append(args, "-flush_packets", "1", "-f", "matroska", "pipe:1")
	}
	if s.Protocol == core.ProtocolDASH {
		return appendSoftSubtitleOutput(append(args, dashMuxArgs(s)...), s)
	}
	if s.Decision.Container == ContainerMPEGTS {
		var segmentOptions []string
		if !s.Decision.VideoCopy && s.Decision.AudioCopy && !s.LiveInput && !s.LoopInput && !s.RTXTimeline && s.ChunkDir == "" && len(s.KeyTimes) == 0 && (!s.AuthoredTimeline || s.RampClock) {
			if encoder, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features); encoder == "h264_nvenc" {
				_, _, reorder := nvencFramePlan(s)
				if reorder == 0 {
					// With no video reorder, negative copied-audio priming
					// otherwise shifts only the initial child muxer's clock.
					// Keep the planned zero-reorder clock on every run.
					segmentOptions = append(segmentOptions, "avoid_negative_ts=disabled")
				}
			}
		}
		if s.RTXTimeline {
			// Both HLS and its child TS muxer must preserve timestamps: the
			// child otherwise shifts a zero-origin B-frame run to positive DTS.
			args = append(args, "-muxdelay", "0", "-muxpreload", "0")
			segmentOptions = append(segmentOptions, "mpegts_copyts=1", "avoid_negative_ts=disabled")
		}
		if s.Start > 0 || (s.LiveInput && s.StartSeg > 0) {
			// A new muxer resets each PID's continuity counter. Mark that reset
			// in-band so a decoder retaining buffered TS does not flag the first
			// replacement packet as corrupt. Media timestamps remain unchanged.
			segmentOptions = append(segmentOptions, "mpegts_flags=+initial_discontinuity")
		}
		if len(segmentOptions) > 0 {
			args = append(args, "-hls_segment_options", strings.Join(segmentOptions, ":"))
		}
	}

	if runtime.GOOS == "windows" && s.Decision.Container == ContainerFMP4 && s.InitName != "" && !filepath.IsAbs(s.InitName) {
		// Windows FFmpeg otherwise resolves this against the process's
		// working directory, which is the session root even for lower rungs.
		s.InitName = filepath.Join(s.Dir, s.InitName)
	}
	// Interleave copied audio by the planned encoder delay (two frames for
	// the historic x264 mux clock) so an HLS cut closes with audio reaching
	// the video boundary. Otherwise restarting at that boundary drops the
	// ~80ms of audio left in the next old fragment (at 24fps).
	if s.Decision.Container == ContainerFMP4 && !s.Decision.VideoCopy && s.Decision.AudioCopy {
		if v := s.Info.Video(); v != nil && v.FrameRate >= 1 && !v.VariableFrameRate {
			encoder, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
			preloadFrames := 2
			preloadFPS := v.FrameRate
			if encoder == "h264_nvenc" {
				_, _, preloadFrames = nvencFramePlan(s)
				preloadFPS = hlsOutputFPS(s)
			}
			if encoder == "libx264" || encoder == "h264_nvenc" {
				args = append(args, "-audio_preload", strconv.FormatInt(int64(math.Ceil(float64(preloadFrames)*1_000_000/preloadFPS)), 10))
			}
		}
	}
	return appendSoftSubtitleOutput(append(args, hlsMuxArgs(s)...), s)
}

func appendSoftSubtitleOutput(args []string, s runSpec) []string {
	for _, track := range s.Info.Streams {
		if path := s.TextSubtitlePaths[track.Index]; path != "" {
			args = append(args, "-map", fmt.Sprintf("0:%d", track.Index), "-c:s", "ass", "-f", "ass", "-ignore_readorder", "1", "-flush_packets", "1", path)
		}
	}
	if s.SoftSubtitlePath == "" {
		return args
	}
	return append(args, "-map", fmt.Sprintf("0:%d", s.SoftSubtitleIndex), "-c:s", "ass", "-f", "ass", "-ignore_readorder", "1", "-flush_packets", "1", s.SoftSubtitlePath)
}

// Segmented Matroska carries parameter sets only in the header's
// CodecPrivate, and Plex's native player keeps the header it opened with:
// one EXT-X-MAP, never refetched. A failover to an encode with different
// SPS/PPS then decodes against the old ones. Repeating them in-band at
// every keyframe reconfigures the open decoder. These filters only add
// parameter sets from extradata (Annex B input passes through unchanged);
// matroskaenc stores the packets length-prefixed and still writes the
// avcC/hvcC CodecPrivate.
func matroskaCopyParameterSets(d core.Decision, codec string) string {
	if d.Container != ContainerMatroska {
		return ""
	}
	switch codec {
	case "hevc":
		return "hevc_mp4toannexb"
	case "h264":
		return "h264_mp4toannexb"
	}
	return ""
}

// buildCodecArgs is the input, stream mapping and codec part of a run's
// command line, shared by HLS and DASH.
func buildCodecArgs(s runSpec) []string {
	// A card has no changing content: start its loop at zero, then place it
	// on the programme clock. Never seek through hours of repeated input.
	loopStart := s.Start
	if s.LoopInput {
		s.Start = 0
		keys := make([]time.Duration, len(s.KeyTimes))
		for i, k := range s.KeyTimes {
			keys[i] = k - loopStart
		}
		s.KeyTimes = keys
	}
	d := s.Decision
	v := s.Info.Video()
	a := findStream(s.Info, d.AudioStream, "audio")
	if s.SilenceAudio && a != nil {
		if d.AudioCopy {
			d.AudioCodec, d.AudioChannels, d.AudioSampleRate = a.Codec, a.Channels, a.SampleRate
		}
		d.AudioCopy = false
	}

	// An ADTS PCE may appear only in the source's first audio packet. Seeking
	// the input past it leaves the AAC decoder without its channel layout.
	// Prime the decoder with a short prefix, then seek the same source. When
	// that is unavailable, only local/short seeks may decode from the start.
	pceOutputSeek := !s.SilenceAudio && s.Protocol == core.ProtocolHLS && d.Container != ContainerMatroska && s.Start > 0 && s.Info.Container == "mpegts" && a != nil && a.AACProgramConfig && !d.AudioCopy
	pceInput, pcePrelude, pceBounded := "", time.Duration(0), false
	if !s.SilenceAudio {
		pceInput, pcePrelude, pceBounded = pceSeekInput(s)
	}
	segDur := s.SegDur
	if segDur <= 0 {
		segDur = DefaultSegmentDuration
	}
	container := d.Container
	if container != ContainerFMP4 {
		container = ContainerMPEGTS
	}
	transcodeVideo := v != nil && !d.VideoCopy
	vp := planVideo(s, v)

	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-nostats", "-y"}
	if s.Protocol != core.ProtocolHTTP {
		args = append(args, "-progress", "pipe:1")
	}
	if transcodeVideo {
		if _, hw := videoEncoder(d.VideoCodec, s.Accel, s.Features); !hw || (!vp.hwFrames && vp.graph != "") {
			w, h := outputSize(v.Width, v.Height, d.MaxHeight)
			args = append(args, "-filter_complex_threads", strconv.Itoa(min(4, softwareThreadBudget(w, h, runtime.GOMAXPROCS(0)))))
		}
	}

	// ---- input
	if pceBounded {
		protocols := "data,http,https,tcp,tls,crypto"
		if !isNetworkURL(s.Input) {
			protocols = "data,file"
		}
		args = append(args, "-format_whitelist", "concat,mpegts", "-protocol_whitelist", protocols)
	} else {
		args = append(args, inputSafetyArgs(s.Input, s.LiveInput)...)
	}
	var httpArgs []string
	if !pceBounded {
		httpArgs = httpInputArgs(s.Input, true)
	}
	if s.Protocol == core.ProtocolHTTP && s.InputTimeout > 0 && len(httpArgs) > 0 {
		httpArgs[1] = strconv.FormatInt(s.InputTimeout.Microseconds(), 10)
		httpArgs = append(httpArgs, "-reconnect_streamed", "1")
	}
	args = append(args, httpArgs...)
	if s.LiveInput {
		if isNetworkURL(s.Input) {
			args = append(args, "-reconnect_streamed", "1", "-reconnect_max_retries", "3")
		}
		args = append(args, "-fflags", "+discardcorrupt+genpts", "-err_detect", "ignore_err")
		if s.LiveFromBacklog {
			args = append(args, "-live_start_index", "0")
		}
	}
	if !pceBounded {
		args = appendInputHeaders(args, s.commandHeaders())
	}
	sub := matroskaSubtitle(s)
	bitmap := sub != nil && IsImageSubtitle(sub.Codec)
	if bitmap && sub.Width > 0 && sub.Height > 0 {
		// PGS packets may be minutes apart. The persisted canvas avoids
		// scanning for a subtitle packet at every seek.
		args = append(args, fmt.Sprintf("-canvas_size:%d", sub.Index), fmt.Sprintf("%dx%d", sub.Width, sub.Height))
	}
	if s.BigProbe || (bitmap && (sub.Width <= 0 || sub.Height <= 0)) {
		args = append(args, "-probesize", "50000000", "-analyzeduration", "10000000")
	} else {
		// Persisted probe metadata is not passed to the demuxer. Even a known
		// video size/format does not prove ffmpeg can recover every mapped
		// stream parameter within 100 ms (audio can precede the first picture).
		// Keep normal analysis for copies too; otherwise a successful remux
		// can publish unknown pixel formats and broken DTS without a retry.
		args = append(args, "-probesize", "5000000", "-analyzeduration", "2000000")
	}
	if !transcodeVideo && !s.LiveInput {
		args = append(args, "-fflags", "+genpts")
	}
	if _, hw := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features); hw && s.Accel.Name == AccelQSV && s.Accel.Device != "" && transcodeVideo {
		args = append(args, "-qsv_device", s.Accel.Device)
	}
	args = append(args, vp.inputArgs...)
	args = append(args, inputTimestampArgs(s)...)
	if s.LoopInput {
		args = append(args, "-stream_loop", "-1")
	}
	if s.Start > 0 && !pceOutputSeek {
		seek := secs(s.Start)
		if s.HasSeekFrom {
			seek = secs(s.SeekFrom)
		} else if lead := hlsNVENCPreroll(s); lead > 0 {
			// Keep the measured NVENC seek and its hardware-frame trim.
			seek = secs(max(0, s.Start-lead).Truncate(time.Millisecond))
		} else if start, ok := hlsAudioPreroll(s); ok {
			seek = strconv.FormatFloat(math.Max(0, math.Floor(start)-1), 'f', 0, 64)
			args = append(args, "-noaccurate_seek")
		} else if start, ok := hlsCopiedAudioPreroll(s); ok {
			seek = strconv.FormatFloat(start, 'f', 0, 64)
			args = append(args, "-noaccurate_seek")
		}
		args = append(args, "-ss", seek)
	}
	if s.httpCopyTimestamps() {
		args = append(args, "-copyts")
	}
	if pceBounded {
		args = append(args, "-f", "concat", "-safe", "0", "-i", pceInput)
	} else {
		args = append(args, "-i", s.Input)
	}
	if s.LoopInput {
		args = append(args, "-t", secs(s.Info.Duration-loopStart))
	}
	textSub := matroskaSubtitle(s)
	if textSub != nil && textSub.External && textSub.ExternalURL != "" {
		// A sidecar file is a second input. Without the same -ss, its cues
		// stay on the title's zero while output_ts_offset places the video
		// at the resume point, and the Matroska muxer holds the picture
		// waiting to interleave them.
		if s.Start > 0 {
			args = append(args, "-ss", secs(s.Start))
		}
		args = append(args, "-i", textSub.ExternalURL)
	}
	silenceInput := 1
	if textSub != nil && textSub.External && textSub.ExternalURL != "" {
		silenceInput++
	}
	if s.SilenceAudio && a != nil {
		layout := fmt.Sprintf("%dc", decisionAudioChannels(d, a))
		if s.Decision.AudioCopy {
			switch a.ChannelLayout {
			case "mono", "stereo", "2.1", "3.0", "4.0", "quad", "5.0", "5.0(side)", "5.1", "5.1(side)", "6.1", "7.1", "7.1(wide)":
				layout = a.ChannelLayout
			}
		}

		args = append(args, "-re", "-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s", decisionAudioRate(d), layout))
	}
	if pceOutputSeek {
		// Output seeking rebases packet timestamps even with -copyts. Put
		// the result back on the title clock used by the segment slotter.
		seek := s.Start
		if pceBounded {
			seek = pcePrelude
		}
		args = append(args, "-ss", secs(seek), "-output_ts_offset", secs(s.Start))
	}

	// ---- mapping
	if v != nil {
		if transcodeVideo && vp.graph != "" {
			args = append(args, "-filter_complex", vp.graph, "-map", "[vout]")
		} else {
			args = append(args, "-map", fmt.Sprintf("0:%d", v.Index))
		}
	}
	if a != nil {
		if s.SilenceAudio {
			args = append(args, "-map", fmt.Sprintf("%d:a:0", silenceInput))
		} else {
			args = append(args, "-map", fmt.Sprintf("0:%d", a.Index))
		}
	}
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-dn")
	switch {
	case textSub != nil && textSub.External && textSub.ExternalURL != "":
		args = append(args, "-map", "1:0", "-c:s", matroskaSubtitleCodec(textSub.Codec))
		args = append(args, matroskaSubtitleTrackArgs(textSub)...)
	case textSub != nil:
		// The primary -ss applies to this stream too, so its cues share
		// the run clock that output_ts_offset then places on the title.
		args = append(args, "-map", fmt.Sprintf("0:%d", textSub.Index), "-c:s", matroskaSubtitleCodec(textSub.Codec))
		args = append(args, matroskaSubtitleTrackArgs(textSub)...)
	default:
		args = append(args, "-sn")
	}

	if bitmap {
		// Sparse bitmap cues must not hold video/audio in the interleaver
		// for FFmpeg's default ten seconds while waiting for another cue.
		args = append(args, "-max_interleave_delta", "1000000")
	}

	// ---- video
	if v != nil {
		if d.VideoCopy {
			args = append(args, "-c:v", "copy")
			// A copied stream can carry a coarse demuxer time base. Explicitly
			// retain its rate in Matroska DefaultDuration instead of the muxer's
			// guessed 30fps. This does not duplicate or drop copied packets.
			if d.AndroidMatroskaHLS && container == ContainerMatroska && v.FrameRate > 0 && !v.VariableFrameRate {
				args = append(args, "-r:v", strconv.FormatFloat(v.FrameRate, 'f', 6, 64))
			}
			if d.StripDolbyVision {
				filter := "dovi_rpu=strip=1"
				if v.DolbyVisionProfile == 7 {
					// Profile 7 can carry many enhancement-layer NALs in one
					// access unit. Remove all of them, not just the trailing
					// RPU handled by older dovi_rpu implementations. The
					// HDR10 base NALs remain byte-for-byte unchanged.
					filter += ",filter_units=remove_types=62|63"
				}
				if ps := matroskaCopyParameterSets(d, v.Codec); ps != "" {
					filter += "," + ps
				}
				args = append(args, "-bsf:v", filter)
			} else if ps := matroskaCopyParameterSets(d, v.Codec); ps != "" {
				args = append(args, "-bsf:v", ps)
			}
			if container == ContainerFMP4 && v.Codec != "hevc" {
				tag := "0"
				if v.Codec == "h264" {
					tag = "avc1"
				}
				args = append(args, "-tag:v", tag)
			}
			if v.Codec == "hevc" && container == ContainerFMP4 {
				tag := "hvc1"
				if v.DolbyVision && v.DolbyVisionProfile == 5 {
					tag = "dvh1"
				}
				args = append(args, "-tag:v", tag)
			}
			if v.DolbyVision && !d.StripDolbyVision && container == ContainerFMP4 {
				args = append(args, "-strict", "unofficial") // write the dvcC box
			}
		} else {
			args = append(args, vp.encodeArgs...)
			args = append(args, videotoolboxCaptionArgs(v, vp.encodeArgs)...)
			if d.Container == ContainerMatroska || s.InBandHeaders {
				// Encoders emit Annex B with the global header as extradata;
				// matroskaenc converts both to length-prefixed form.
				args = append(args, "-bsf:v", "dump_extra=freq=keyframe")
			}
			if len(s.KeyTimes) > 0 {
				keys := make([]string, len(s.KeyTimes))
				for i, k := range s.KeyTimes {
					keys[i] = secs(k)
				}
				// Absolute output times (unlike the expression's t, which
				// counts from the run's first frame).
				args = append(args, "-force_key_frames", strings.Join(keys, ","))
			} else {
				args = append(args, "-force_key_frames", "expr:gte(t,n_forced*"+secs(segDur)+")")
			}
		}
	}

	// ---- audio
	if a != nil {
		if d.AudioCopy {
			args = append(args, "-c:a", "copy")
			var audioFilters []string
			if s.Start > 0 {
				// Input seeking retains copied packets from the preceding
				// video keyframe. Drop audio preroll without re-encoding or
				// discarding the video keyframe required to decode a copy
				// rendition. amount=0 explicitly leaves retained bytes intact.
				// Without -copyts, input -ss rebases packet timestamps to the
				// seek point before bitstream filters run.
				dropAt := "0"
				if len(inputTimestampArgs(s)) > 0 {
					dropAt = secs(s.Start)
					if lead := hlsRestartLead(s); lead > 0 {
						dropAt = strconv.FormatFloat(hlsGridVideoStart(s, hlsOutputFPS(s))-lead.Seconds(), 'f', 9, 64)
					}
					if cutoff, ok := hlsFMP4AudioCut(s); ok {
						dropAt = strconv.FormatFloat(cutoff, 'f', 9, 64)
					}
				}
				audioFilters = append(audioFilters, "noise=amount=0:drop='lt(pts*tb,"+dropAt+")'")
			}
			// The HLS muxer's nested MP4 writer does not auto-insert the
			// ADTS-to-ASC filter. Preserve compressed audio while moving its
			// configuration into the initialization segment.
			if s.Protocol == core.ProtocolHLS && container == ContainerFMP4 && a.Codec == "aac" && (s.Info.Container == "mpegts" || s.Info.Container == "aac") {
				audioFilters = append(audioFilters, "aac_adtstoasc")
			}
			if len(audioFilters) > 0 {
				args = append(args, "-bsf:a", strings.Join(audioFilters, ","))
			}
			if container == ContainerFMP4 {
				tag := "0"
				if a.Codec == "aac" {
					tag = "mp4a"
				}
				args = append(args, "-tag:a", tag)
			}
		} else {
			codec := d.AudioCodec
			if codec == "" {
				codec = "aac"
			}
			ch := decisionAudioChannels(d, a)
			enc := codec
			switch codec {
			case "mp3":
				enc = "libmp3lame"
			case "opus":
				enc = "libopus"
				if s.Features.encoders != nil && !s.Features.hasEncoder("libopus") {
					enc = "opus"
				}
			}
			kbps := audioBitrateKbps(codec, ch)
			if d.AudioBitrateKbps > 0 && d.AudioBitrateKbps < kbps {
				kbps = d.AudioBitrateKbps
			}
			args = append(args, "-c:a", enc, "-ac", strconv.Itoa(ch), "-ar", strconv.Itoa(decisionAudioRate(d)),
				"-b:a", fmt.Sprintf("%dk", kbps))
			filters := append([]string(nil), d.AudioFilters...)
			if start, ok := hlsAudioPreroll(s); ok {
				rate := decisionAudioRate(d)
				// Coarse demuxer clocks round packet PTS. Recover the decoded
				// frame grid before resampling, retaining source codec padding.
				// The first frame may be shortened by decoder priming.
				if a.SampleRate > 0 {
					filters = append(filters, fmt.Sprintf("asettb=1/%d,asetpts='if(eq(N,0),PTS,round((PTS+%d)/NB_SAMPLES)*NB_SAMPLES-%d)',atrim=start=%.0f", a.SampleRate, a.DecodedPadding, a.DecodedPadding, math.Floor(start)))
				}
				filters = append(filters, fmt.Sprintf("aresample=%d,atrim=start=%.9f,asettb=1/%d,asetpts=N+%d", rate, start, rate, int64(math.Round(start*float64(rate)))))
				cut := hlsAudioRestartCut(s)
				args = append(args, "-bsf:a", fmt.Sprintf("noise=amount=0:drop='lt(pts*tb,%.9f)'", cut))
			}
			if s.AudioGainDB != 0 {
				filters = append(filters, fmt.Sprintf("volume=%.1fdB", s.AudioGainDB))
				if s.AudioGainDB > 0 {
					filters = append(filters, "alimiter=limit=0.891251:level=false:latency=true")
				}
			}
			if len(filters) > 0 {
				args = append(args, "-af", strings.Join(filters, ","))
			}
			if enc == "opus" {
				args = append(args, "-strict", "-2")
			}
		}
	}

	if s.LoopInput {
		args = append(args, "-output_ts_offset", secs(loopStart))
	}
	// The TS child muxer adds twice its default muxdelay (1.4s), then
	// shifts a zero-origin encoder's negative DTS to zero. A positive-origin
	// restart does not receive that second shift. Restore it explicitly,
	// retaining the initial run's clock and its copied-audio segment cutoff.
	lead := hlsRestartLead(s)
	if fastLead := hlsFastStartClock(s); fastLead > 0 {
		lead = fastLead
		// setts defaults its unspecified PTS expression to TS (the packet DTS).
		// Shift only decode time; presentation time must retain source order.
		args = appendVideoClockBSF(args, "setts=pts=PTS:dts=DTS-"+strconv.FormatFloat(fastLead.Seconds(), 'f', 9, 64)+"/TB")
	}
	if lead > 0 {
		args = append(args, "-output_ts_offset", strconv.FormatFloat(lead.Seconds(), 'f', 9, 64))
	}
	return args
}

// hlsAudioPreroll warms the AAC encoder on its original sample grid. Only
// replacement runs change: the initial encode's bytes and mux clock are retained.
// One second of encoder preroll covers AAC overlap/lookahead. Input decode
// starts one further integer second earlier to warm decoder/resampler state.
func hlsAudioPreroll(s runSpec) (float64, bool) {
	if s.Start <= 0 || s.Decision.AudioCopy || s.SilenceAudio || s.LiveInput || s.LoopInput || s.RTXTimeline || s.Decision.VideoCopy || s.ChunkDir != "" || len(s.KeyTimes) > 0 || (s.Protocol != core.ProtocolHLS && s.Protocol != core.ProtocolDASH) || (s.Decision.Container != ContainerMPEGTS && s.Decision.Container != ContainerFMP4) || (s.Decision.AudioCodec != "" && s.Decision.AudioCodec != "aac") {
		return 0, false
	}
	if a := findStream(s.Info, s.Decision.AudioStream, "audio"); a == nil || a.SampleRate <= 0 {
		return 0, false
	}
	enc, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
	if enc != "libx264" || hlsOutputFPS(s) <= 0 {
		return 0, false
	}
	rate := float64(decisionAudioRate(s.Decision))
	return math.Max(0, math.Floor((s.Start.Seconds()-1)*rate/1024)*1024/rate), true
}

// A copied AAC packet needed after the HLS mux cut can precede the nominal
// video seek. MP4 input seeking may otherwise discard it before the audio BSF
// sees it, even though the BSF's reorder-adjusted cutoff is correct. Decode a
// bounded earlier prefix and trim video separately; keep the existing exact AU
// cutoff and all fresh-start arguments.
func hlsCopiedAudioPreroll(s runSpec) (float64, bool) {
	// Hardware plans keep their separately measured input and mux clocks.
	if enc, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features); enc != "libx264" {
		return 0, false
	}
	if s.Start <= 0 || !s.Decision.AudioCopy || s.SilenceAudio || s.LiveInput || s.LoopInput || s.RTXTimeline || s.Decision.VideoCopy || s.ChunkDir != "" || len(s.KeyTimes) > 0 || (s.Protocol != "" && s.Protocol != core.ProtocolHLS) {
		return 0, false
	}
	if a := findStream(s.Info, s.Decision.AudioStream, "audio"); a == nil || a.Codec != "aac" {
		return 0, false
	}
	cut := s.Start.Seconds()
	if lead := hlsRestartLead(s); lead > 0 {
		cut = hlsGridVideoStart(s, hlsOutputFPS(s)) - lead.Seconds()
	}
	if boundary, ok := hlsFMP4AudioCut(s); ok {
		cut = boundary
	}
	if cut >= s.Start.Seconds() {
		return 0, false
	}
	return math.Max(0, math.Floor(cut)-1), true
}

func hlsAudioRestartCut(s runSpec) float64 {
	if s.Protocol == core.ProtocolDASH {
		rate := float64(decisionAudioRate(s.Decision))
		return math.Floor(s.Start.Seconds()*rate/1024) * 1024 / rate
	}
	fps := hlsOutputFPS(s)
	clockSpec := s
	if s.RampClock {
		clockSpec.FastStart = false
	}
	_, _, _, delay := x264FramePlan(clockSpec)
	return hlsGridVideoStart(s, fps) - float64(delay)/fps
}

// hlsRestartLead retains the planned H264 mux clock across restarts.
// Fast-start fMP4 also carries the ramp origin into every steady run; its
// edit-list correction already handles the encoder reorder difference.

// Copies, unplanned encoders, live, alternate muxers and unknown clocks retain
// their existing arguments. Chunked/keyframe-planned runs are slotted on their
// own authored timeline and must not receive a uniform-grid mux shift.
// NVENC delay comes from its explicit B-frame plan, never the driver preset.
func hlsRestartLead(s runSpec) time.Duration {
	_, encodedAudio := hlsAudioPreroll(s)
	if (s.AuthoredTimeline && !s.RampClock && !encodedAudio) || s.ChunkDir != "" || len(s.KeyTimes) > 0 || s.Start <= 0 || s.LiveInput || s.LoopInput || s.RTXTimeline || s.Protocol == core.ProtocolDASH || s.Protocol == core.ProtocolHTTP || (s.Decision.Container != ContainerMPEGTS && !(s.RampClock && s.Decision.Container == ContainerFMP4)) || s.Decision.VideoCopy || (!s.Decision.AudioCopy && !encodedAudio) {
		return 0
	}
	v := s.Info.Video()
	if v == nil || v.FrameRate < 1 {
		return 0
	}
	enc, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
	if enc != "libx264" && enc != "h264_nvenc" {
		return 0
	}
	if enc == "h264_nvenc" && s.Decision.Container != ContainerMPEGTS {
		return 0
	}
	if enc == "libx264" && s.FastStart && hlsFastStartClock(s) == 0 {
		return 0
	}
	fps := hlsOutputFPS(s)
	if fps <= 0 {
		return 0
	}
	clockSpec := s
	if s.FastStart {
		clockSpec.FastStart = false
	}
	delay := h264ReorderFrames(clockSpec)
	return time.Duration(math.Round(float64(delay) / fps * float64(time.Second)))
}

// hlsNVENCPreroll retains the copied audio preceding an NVENC TS boundary.
// Seeking exactly to the video keyframe can omit that audio before the BSF
// sees it. The same planned lead bounds input preroll and the audio cutoff.
func hlsNVENCPreroll(s runSpec) time.Duration {
	enc, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
	if enc != "h264_nvenc" {
		return 0
	}
	return hlsRestartLead(s)
}

// The HLS cut compares audio DTS with video DTS, biased by audio_preload.
// Retain exactly the AUs the initial run put after that cut. In particular,
// ceil-to-microsecond preload can put an AU exactly on the nominal boundary
// in the preceding fragment; dropping only pts < Start would duplicate it.
func hlsFMP4AudioCut(s runSpec) (float64, bool) {
	if (s.AuthoredTimeline && !s.RampClock) || s.ChunkDir != "" || len(s.KeyTimes) > 0 || s.Start <= 0 || s.LiveInput || s.Decision.Container != ContainerFMP4 || s.Decision.VideoCopy || s.Protocol == core.ProtocolDASH || s.Protocol == core.ProtocolHTTP {
		return 0, false
	}
	v := s.Info.Video()
	if v == nil || v.FrameRate < 1 {
		return 0, false
	}
	enc, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
	if enc != "libx264" && enc != "h264_nvenc" {
		return 0, false
	}
	fps := hlsOutputFPS(s)
	if fps <= 0 {
		return 0, false
	}
	clockSpec := s
	if hlsFastStartClock(s) > 0 {
		clockSpec.FastStart = false
	}
	delay := h264ReorderFrames(clockSpec)
	preload := 0.0
	if !v.VariableFrameRate {
		preloadFrames := 2
		preloadFPS := v.FrameRate
		if enc == "h264_nvenc" {
			preloadFrames = delay
			preloadFPS = fps
		}
		preload = math.Ceil(float64(preloadFrames)*1_000_000/preloadFPS) / 1_000_000
	}
	return hlsGridVideoStart(s, fps) + preload - float64(delay)/fps, true
}

// hlsFastStartClock gives the zero-reorder ramp the steady clock without
// adding B-frames or changing its low-latency encoder preset.
func hlsFastStartClock(s runSpec) time.Duration {
	if !s.RampClock || !s.FastStart || s.LiveInput || s.LoopInput || s.RTXTimeline || s.ChunkDir != "" || len(s.KeyTimes) > 0 || s.Decision.VideoCopy || (s.Decision.Container != ContainerMPEGTS && s.Decision.Container != ContainerFMP4) || (s.Protocol != "" && s.Protocol != core.ProtocolHLS) {
		return 0
	}
	enc, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
	if enc != "libx264" {
		return 0
	}
	fps := hlsOutputFPS(s)
	if fps <= 0 {
		return 0
	}
	s.FastStart = false
	_, _, _, delay := x264FramePlan(s)
	return time.Duration(math.Round(float64(delay) / fps * float64(time.Second)))
}

func appendVideoClockBSF(args []string, filter string) []string {
	for i := len(args) - 2; i >= 0; i-- {
		if args[i] == "-bsf:v" {
			args[i+1] += "," + filter
			return args
		}
	}
	return append(args, "-bsf:v", filter)
}

// FFmpeg's HLS CFR output clock uses the reference (nominal) demuxer rate,
// not a VFR source's average rate. Negotiated -r takes precedence.
func hlsOutputFPS(s runSpec) float64 {
	v := s.Info.Video()
	if v == nil {
		return 0
	}
	fps := v.FrameRate
	if v.VariableFrameRate && v.ReferenceFrameRate == nil {
		return 0
	}
	if v.ReferenceFrameRate != nil && *v.ReferenceFrameRate >= 1 {
		fps = *v.ReferenceFrameRate
	}
	if s.Decision.H264Level > 0 && s.Decision.H264FrameRate > 0 {
		fps = s.Decision.H264FrameRate
	}
	return fps
}

// Fractional rates put the first frame at or after the authored boundary.
// Audio must follow that actual video cut, not an integer-second approximation.
func hlsGridVideoStart(s runSpec, fps float64) float64 {
	return math.Ceil(s.Start.Seconds()*fps-1e-7) / fps
}

// x264FramePlan is the single decision for preset, B-frame limit, pyramid and
// decode reorder depth. The seek clock uses exactly the encoder's decision.
func x264FramePlan(s runSpec) (preset string, bframes int, pyramid string, reorder int) {
	preset = "veryfast"
	bframes = 3
	pyramid = "normal"
	reorder = 2
	if s.FastStart {
		preset = "ultrafast"
		bframes = 0
		pyramid = "none"
		reorder = 0
	}
	if s.Decision.H264Level > 0 && !s.FastStart {
		bframes = 2
		pyramid = "none"
		reorder = 1
	}
	return
}

// nvencFramePlan pins H264 B-frames instead of inheriting driver presets.
// FFmpeg nvenc_set_timestamp uses frameIntervalP-1 (= bf) timestamp slots,
// even though the stream reports has_b_frames=2 for any nonzero B-frame count.
// The native 30fps p4 baseline has three slots (100ms); level plans use none.
func nvencFramePlan(s runSpec) (preset string, bframes, reorder int) {
	preset, bframes = "p4", 3
	if s.FastStart {
		preset = "p1"
	}
	if s.Decision.H264Level > 0 {
		bframes = 0
	}
	return preset, bframes, bframes
}

func h264ReorderFrames(s runSpec) int {
	enc, _ := videoEncoder(s.Decision.VideoCodec, s.Accel, s.Features)
	if enc == "h264_nvenc" {
		_, _, delay := nvencFramePlan(s)
		return delay
	}
	_, _, _, delay := x264FramePlan(s)
	return delay
}

// hlsMuxArgs is the HLS muxer part of a run's command line.
func hlsMuxArgs(s runSpec) []string {
	segDur := s.SegDur
	if segDur <= 0 {
		segDur = DefaultSegmentDuration
	}
	container := s.Decision.Container
	if container == ContainerMatroska {
		return matroskaHLSMuxArgs(s, segDur)
	}
	if container != ContainerFMP4 {
		container = ContainerMPEGTS
	}
	ext := segmentExt(container)
	start, segDir := s.StartSeg, s.Dir
	if s.ChunkDir != "" {
		segDur, start, segDir = s.chunkCut(), 0, s.ChunkDir
	}
	args := []string{
		"-avoid_negative_ts", "disabled",
		"-max_muxing_queue_size", "4096",
		"-f", "hls",
		"-hls_time", secs(segDur),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "temp_file",
		"-start_number", strconv.Itoa(start),
	}
	if container == ContainerFMP4 {
		// frag_discont: every fragment's decode time is the media time
		// (with -copyts) rather than counted from the run's first packet,
		// so segments of a run restarted mid-playlist line up with the
		// init segment the player already holds.
		// skip_sidx: hlsenc runs the mp4 muxer in dash mode, which (to
		// keep each fragment's sidx contiguous with the last) rewrites
		// every fragment's first presentation time to the previous
		// fragment's end. That only holds for closed GOPs: an open-GOP
		// keyframe (HEVC CRA) presents after its leading pictures and
		// was moved before them. HLS does not use sidx, and the chunked
		// path already drops it from multi-chunk segments.
		options := "movflags=+frag_discont+skip_sidx"
		if s.NormalizeNegativeDTS {
			options += ":avoid_negative_ts=make_non_negative"
		}
		if s.VideoTimescale != 0 {
			options += ":video_track_timescale=" + strconv.FormatUint(uint64(s.VideoTimescale), 10)
		}
		args = append(args, "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", s.InitName,
			"-hls_segment_options", options)
	} else {
		args = append(args, "-hls_segment_type", "mpegts")
	}
	args = append(args,
		"-hls_segment_filename", filepath.Join(segDir, "%05d."+ext),
		filepath.Join(s.Dir, s.Playlist),
	)
	return args
}

// chunkCut is a chunked run's muxer segment length: short enough that the
// muxer cuts at every keyframe (it cuts at the first keyframe past
// start+k*length, k counting cuts), long enough that all-intra sources do
// not produce a file per frame.
func (s runSpec) chunkCut() time.Duration {
	if s.ChunkCut > 0 {
		return s.ChunkCut
	}
	return chunkCutFor(s.SegDur)
}

// dashMuxArgs is the DASH muxer part of a run's command line: fMP4 chunks
// per representation (video 0, audio 1) with absolute decode times
// (frag_discont), written into the run directory as
// chunk-<rep>-<n>.m4s from n=1 with init-<rep>.m4s.
func dashMuxArgs(s runSpec) []string {
	chunk := s.ChunkDur
	if chunk <= 0 {
		chunk = dashChunkDuration
	}
	var sets []string
	if s.Info.Video() != nil {
		sets = append(sets, fmt.Sprintf("id=%d,streams=v", len(sets)))
	}
	if findStream(s.Info, s.Decision.AudioStream, "audio") != nil {
		sets = append(sets, fmt.Sprintf("id=%d,streams=a", len(sets)))
	}
	return []string{
		"-avoid_negative_ts", "disabled",
		"-max_muxing_queue_size", "4096",
		"-f", "dash",
		"-dash_segment_type", "mp4",
		"-format_options", "movflags=+frag_discont",
		"-use_template", "1",
		"-use_timeline", "0",
		"-seg_duration", secs(chunk),
		"-window_size", "0",
		"-adaptation_sets", strings.Join(sets, " "),
		"-init_seg_name", dashInitPattern,
		"-media_seg_name", dashChunkPattern,
		// The dash muxer places segment files in the manifest's
		// directory, which it finds by scanning the output path for
		// '/'. A Windows path with backslashes has none, so segments
		// landed in ffmpeg's working directory instead of the run
		// directory. Forward slashes work in Windows paths too.
		filepath.ToSlash(filepath.Join(s.Dir, s.Playlist)),
	}
}

// File names ffmpeg's dash muxer writes in a run directory.
const (
	dashInitPattern  = "init-$RepresentationID$.m4s"
	dashChunkPattern = "chunk-$RepresentationID$-$Number%05d$.m4s"
)

func dashInitFile(rep int) string     { return fmt.Sprintf("init-%d.m4s", rep) }
func dashChunkFile(rep, n int) string { return fmt.Sprintf("chunk-%d-%05d.m4s", rep, n) }

func findStream(info core.MediaInfo, idx int, typ string) *core.StreamInfo {
	if idx < 0 {
		return nil
	}
	for i := range info.Streams {
		if info.Streams[i].Index == idx && info.Streams[i].Type == typ {
			return &info.Streams[i]
		}
	}
	return nil
}

// videoPlan is the decode, filter and encode part of a video transcode.
type videoPlan struct {
	inputArgs  []string
	graph      string // -filter_complex, output label [vout]
	encodeArgs []string
	hwFrames   bool
	err        error // the graph cannot be built; do not start ffmpeg
}

// outputSize scales (w,h) to height maxH, keeping aspect and even sizes.
func outputSize(w, h, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		if maxH > 0 {
			return even(maxH * 16 / 9), even(maxH)
		}
		return 0, 0
	}
	if maxH <= 0 || maxH >= h {
		return even(w), even(h)
	}
	nw := int(float64(w)*float64(maxH)/float64(h) + 0.5)
	return even(nw), even(maxH)
}

func even(n int) int {
	if n%2 != 0 {
		n--
	}
	if n < 2 {
		n = 2
	}
	return n
}

// cpuToneMap converts PQ/HLG to SDR BT.709 in software. zscale (zimg) is
// the accurate path when this ffmpeg has it. libplacebo, tonemapx and
// tonemap_opencl are the alternatives. There is no retag-only fallback:
// a washed-out picture is not a tone map. ok is false when none of those
// filters were probed.
func cpuToneMap(f features) (string, bool) {
	if f.hasFilter("zscale") && f.hasFilter("tonemap") {
		return "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv", true
	}
	if f.hasFilter("libplacebo") {
		return "libplacebo=tonemapping=hable:color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv", true
	}
	if f.hasFilter("tonemapx") {
		return "tonemapx=tonemap=hable:desat=0:t=bt709:m=bt709:p=bt709:format=yuv420p", true
	}
	if f.hasFilter("tonemap_opencl") {
		return "format=p010le,hwupload,tonemap_opencl=tonemap=hable:desat=0:format=nv12,hwdownload,format=nv12", true
	}
	return "", false
}

// escapeFilterValue escapes a value for use inside a filter option and then
// inside a filtergraph description.
func escapeFilterValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `:`, `\:`)
	v = r.Replace(v)
	g := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `[`, `\[`, `]`, `\]`, `,`, `\,`, `;`, `\;`)
	return g.Replace(v)
}

// matroskaSubtitle is the selected track Plex tvOS reads from
// inside segmented Matroska. Burned and non-Matroska decisions keep the
// sidecar path. A nil return means the run stays subtitle-free (-sn).
func matroskaSubtitle(s runSpec) *core.StreamInfo {
	if s.Decision.Container != ContainerMatroska || s.Decision.BurnSubtitles {
		return nil
	}
	sub := findStream(s.Info, s.Decision.SubtitleStream, "subtitle")
	if sub == nil || (!IsTextSubtitle(sub.Codec) && !IsImageSubtitle(sub.Codec)) {
		return nil
	}
	if sub.External && (sub.ExternalURL == "" || IsImageSubtitle(sub.Codec)) {
		return nil
	}
	return sub
}

// matroskaSubtitleTrackArgs describes the one subtitle track the segments
// carry. It is the track the viewer picked, so it is flagged default:
// ExoPlayer (Plex for Android and Android TV) picks a text track by its
// default flag or its language, and the source's flags are the release's,
// not the viewer's (an English track a release leaves non-default stayed
// off, #191). -map_metadata -1 drops the source's language, which the
// track gets back, as PMS sets each mapped stream's language.
func matroskaSubtitleTrackArgs(sub *core.StreamInfo) []string {
	args := []string{"-disposition:s:0", "default"}
	if sub.Language != "" {
		args = append(args, "-metadata:s:s:0", "language="+sub.Language)
	}
	return args
}

// httpCopyTimestamps: Plex Desktop's HTTP Matroska stream keeps the
// source's timestamps from a seek or when asked.
func (s runSpec) httpCopyTimestamps() bool {
	return s.Protocol == core.ProtocolHTTP && !s.LiveInput && (s.Start > 0 || s.CopyTimestamps)
}

// titleClock reports whether this run's decoded frames carry the title's
// timestamps (-copyts) rather than starting at zero at its -ss point.
func (s runSpec) titleClock() bool {
	return len(inputTimestampArgs(s)) > 0 || s.httpCopyTimestamps()
}

// matroskaSubtitleCodec copies native Matroska subtitle codecs and
// rewrites the others to SubRip, which the segment muxer can header.
func matroskaSubtitleCodec(codec string) string {
	switch strings.ToLower(codec) {
	case "subrip", "srt", "ass", "ssa", "webvtt", "pgs", "hdmv_pgs_subtitle", "vobsub", "dvd_subtitle", "dvbsub", "dvb_subtitle":
		return "copy"
	default:
		return "srt"
	}
}

// subtitleOrdinal is the position of a stream among the subtitle streams
// (the si= option of the subtitles filter).
func subtitleOrdinal(info core.MediaInfo, idx int) int {
	n := 0
	for _, s := range info.Streams {
		if s.Type != "subtitle" || s.External {
			continue
		}
		if s.Index == idx {
			return n
		}
		n++
	}
	return 0
}

// picturePlan is the encoded picture planVideo derives from the source
// and, when present, the static-header contract.
type picturePlan struct {
	w, h     int
	scaling  bool
	tonemap  bool
	tenBit   bool
	outRange string
	convert  string
	blocked  string // owner-visible reason the colour conversion cannot be built
	pad      bool
	setSAR   string
}

// planPicture computes the encoded picture: output size, HDR handling and,
// with a contract, its depth/range/colour conversion/padding/SAR.
func planPicture(s runSpec, v *core.StreamInfo, codec string) picturePlan {
	d := s.Decision
	w, h := outputSize(v.Width, v.Height, d.MaxHeight)
	// Keep HDR only where HEVC can carry the decoded picture's HDR10/HLG;
	// any other HDR picture is tone mapped (8-bit SDR output), and an SDR
	// picture (a Dolby Vision SDR base layer) never is.
	keepHDR := keepsHDR(v) && !d.ToneMap && codec == "hevc"
	pic := picturePlan{
		w: w, h: h, scaling: v.Height > 0 && h < v.Height,
		tonemap: encodedRange(v) != rangeSDR && !keepHDR,
		// tenBit and outRange describe the encoded picture; without a
		// contract they follow keepHDR.
		tenBit: keepHDR, outRange: rangeSDR,
	}
	if keepHDR {
		pic.outRange = encodedRange(v)
	}
	if t := s.Target; t != nil {
		// A static-header session: encode the contract's picture
		// whatever this source is (contract.go).
		picture := *v
		if s.PictureCrop != nil {
			picture.Width, picture.Height = s.PictureCrop.width, s.PictureCrop.height
		}
		pic.w, pic.h = t.fit(&picture)
		pic.scaling = pic.w != picture.Width || pic.h != picture.Height
		pic.pad = pic.w != t.width || pic.h != t.height
		pic.tenBit, pic.outRange = t.depth == 10, t.rng
		pic.tonemap = t.tonemaps(v)
		if t.convertsColour(v) {
			filt, ok := colourFilter(v, t, s.Features)
			if !ok {
				pic.blocked = colourUnavailableMessage()
			} else {
				pic.convert = filt
			}
		}
		// scale keeps the display aspect: its output SAR follows the source's.
		src := pictureOf(v)
		if n, dd := reduce(src.sarNum*picture.Width*pic.h, src.sarDen*picture.Height*pic.w); pic.pad || n != t.pic.sarNum || dd != t.pic.sarDen {
			pic.setSAR = fmt.Sprintf("setsar=%d/%d", t.pic.sarNum, t.pic.sarDen)
		}
	}
	return pic
}

// planFilterChain chooses the GPU or software filter chain: hardware
// decode/scale/tone-map when a pipeline applies, else the software
// convert/tone-map plus finish. size/format size, pad and format software frames.
func planFilterChain(s runSpec, v *core.StreamInfo, pic picturePlan, accel string, burn bool, size, format func([]string) []string) (inputArgs, chain []string, hwFrames bool) {
	// Software filters only: GPU scalers neither pad nor convert SDR to
	// HDR, and only the explicitly probed CUDA path can feed a 10-bit
	// tone-mapped contract. Tone mappers produce limited-range BT.709.
	p010CUDA := accel == AccelNVENC && s.Features.cudaP010ToneMap && !pictureOf(v).full && (s.Target == nil || (s.Target.pic.prim == "bt709" && s.Target.pic.matrix == "bt709" && !s.Target.pic.full))
	gpuOff := v.Interlaced || s.PictureCrop != nil || pic.pad || pic.convert != "" || (pic.tonemap && pic.tenBit && !p010CUDA)
	dv5 := pic.tonemap && v.DolbyVision && v.DolbyVisionProfile == 5
	gpuInput, gpuFilters, gpuOK := gpuPipeline(s, accel, pic.w, pic.h, pic.tonemap, pic.tenBit, burn)
	if dv5 {
		// The VideoToolbox mapper produced visibly different colours from
		// tonemapx on profile 5. Keep decode/encode hardware where possible,
		// but do the RPU reshape and tone map with the validated CPU graph.
		gpuOK = false
		// The legacy VAAPI branch below is separate from gpuPipeline.
		// It must not bypass the RPU-aware CPU graph either.
		gpuOff = true
	}
	switch {
	case gpuOK && !gpuOff:
		inputArgs = append(inputArgs, gpuInput...)
		chain = append(chain, gpuFilters...)
		if pic.setSAR != "" {
			chain = append(chain, pic.setSAR)
		}
		hwFrames = true
	case accel == AccelVideoToolbox && pic.tonemap && !dv5 && !gpuOff && !burn && s.Features.hasFilter("scale_vt"):
		// Decode, tone-map and scale on the GPU.
		inputArgs = append(inputArgs, "-hwaccel", "videotoolbox", "-hwaccel_output_format", "videotoolbox_vld")
		chain = append(chain, fmt.Sprintf("scale_vt=w=%d:h=%d:color_matrix=bt709:color_primaries=bt709:color_transfer=bt709", pic.w, pic.h))
		hwFrames = true
	case accel == AccelVAAPI && !burn && !gpuOff && !s.Accel.NoGPUScale && (!pic.tonemap || (s.Features.hasFilter("tonemap_vaapi") && !s.Accel.NoGPUTonemap)):
		dev := s.Accel.Device
		if dev == "" {
			dev = DefaultVAAPIDevice
		}
		inputArgs = append(inputArgs, "-hwaccel", "vaapi", "-hwaccel_device", dev, "-hwaccel_output_format", "vaapi")
		if pic.tonemap {
			chain = append(chain, "tonemap_vaapi=format=nv12:t=bt709:m=bt709:p=bt709")
		}
		f := "nv12"
		if pic.tenBit {
			f = "p010"
		}
		chain = append(chain, fmt.Sprintf("scale_vaapi=w=%d:h=%d:format=%s", pic.w, pic.h, f))
		if pic.setSAR != "" {
			chain = append(chain, pic.setSAR)
		}
		hwFrames = true
	case accel == AccelVideoToolbox:
		inputArgs = append(inputArgs, "-hwaccel", "videotoolbox")
	case accel == AccelNVENC && !(v.Codec == "h264" && v.BitDepth > 8):
		inputArgs = append(inputArgs, "-hwaccel", "cuda")
	case accel == AccelQSV && s.Features.hwaccels["qsv"] && !(v.Codec == "h264" && v.BitDepth > 8):
		inputArgs = append(inputArgs, "-hwaccel", "qsv")
	case accel == AccelVAAPI:
		dev := s.Accel.Device
		if dev == "" {
			dev = DefaultVAAPIDevice
		}
		if burn {
			inputArgs = append(inputArgs, "-init_hw_device", "vaapi=va:"+dev, "-filter_hw_device", "va")
			if !(v.Codec == "h264" && v.BitDepth > 8) {
				inputArgs = append(inputArgs, "-hwaccel", "vaapi", "-hwaccel_device", "va")
			}
		} else {
			inputArgs = append(inputArgs, "-vaapi_device", dev)
		}
	}

	if !hwFrames {
		// PMS deinterlaces conversions before scaling. Keep these frames
		// in the software graph until device deinterlacers are probed too;
		// selecting a GPU scaler alone would preserve combed fields.
		if v.Interlaced {
			chain = append(chain, "yadif=mode=send_frame:parity=auto:deint=all")
		}
		preScaled := false
		switch {
		case pic.convert != "":
			chain = append(chain, pic.convert)
		case pic.tonemap:
			if dv5 && s.Features.hasFilter("tonemapx") {
				// Jellyfin's DV-aware mapper consumes 10-bit 4:2:0 and uses
				// the decoder's RPU side data for IPT reshaping.
				chain = append(chain, "format=yuv420p10le", "tonemapx=tonemap=hable:desat=0:t=bt709:m=bt709:p=bt709:format=yuv420p:apply_dovi=1")
			} else if !dv5 && !pic.tenBit && s.Features.hasFilter("tonemapx") {
				// Jellyfin's SIMD 10-bit YUV tone-mapper avoids single-threaded
				// float32 RGB conversion on CPU filter paths (such as Windows QSV
				// HDR->SDR, #408). Scaling 10-bit YUV first cuts pixel work 4x+.
				if !burn && s.PictureCrop == nil && pic.scaling && (v.Width <= 0 || (pic.w <= v.Width && pic.h <= v.Height)) {
					chain = size(chain)
					preScaled = true
				}
				chain = append(chain, "format=yuv420p10le", "tonemapx=tonemap=hable:desat=0:t=bt709:m=bt709:p=bt709:format=yuv420p")
			} else if !dv5 {
				if filt, ok := cpuToneMap(s.Features); ok {
					chain = append(chain, filt)
				}
			}
		}
		if !burn {
			if preScaled {
				chain = format(chain)
			} else {
				chain = format(size(chain))
			}
		}
	}
	return inputArgs, chain, hwFrames
}

// planEncodeArgs builds the encoder invocation: rate control, the selected
// encoder's preset/quality options and the output colour tags.
func planEncodeArgs(s runSpec, pic picturePlan, enc, codec string, hwFrames bool) []string {
	// Encoder.
	videoKbps := videoBitrate(s.Info, s.Decision)
	if s.Rate.TargetKbps > 0 {
		videoKbps = s.Rate.TargetKbps
	}
	peakKbps := videoKbps * 3 / 2
	bufferKbits := videoKbps * 2
	if s.Rate.MaxrateKbps > 0 {
		peakKbps = s.Rate.MaxrateKbps
	}
	if s.Rate.BufsizeKbps > 0 {
		bufferKbits = s.Rate.BufsizeKbps
	}
	if codec == "h264" && s.Decision.H264Level > 0 {
		if maxRate, maxBuffer, ok := H264HighLevelLimits(s.Decision.H264Level); ok {
			videoKbps = min(videoKbps, maxRate)
			peakKbps = min(peakKbps, maxRate)
			bufferKbits = min(bufferKbits, maxBuffer)
		}
	}
	b := fmt.Sprintf("%dk", videoKbps)
	maxr := fmt.Sprintf("%dk", peakKbps)
	buf := fmt.Sprintf("%dk", bufferKbits)
	e := []string{"-c:v", enc}
	xPreset := "veryfast"
	if s.FastStart {
		xPreset = "ultrafast"
	}
	switch enc {
	case "libx264":
		preset, bframes, pyramid, _ := x264FramePlan(s)
		xPreset = preset
		e = append(e, "-bf", strconv.Itoa(bframes))
		if s.Decision.H264Level == 0 {
			e = append(e, "-x264-params", "b-pyramid="+pyramid)
		}
		// Preserve fine detail at the bounded software thread budget. The
		// natural-footage baseline validates CRF 18 at the same VBV cap;
		// throughput improvements must not rely on lowering image quality.
		crf := "18"
		threads := strconv.Itoa(softwareThreadBudget(pic.w, pic.h, runtime.GOMAXPROCS(0)))
		if s.Rate.Quality {
			e = append(e, "-preset", xPreset, "-crf", crf, "-maxrate", maxr, "-bufsize", buf, "-profile:v", "high", "-threads", threads)
		} else {
			e = append(e, "-preset", xPreset, "-crf", crf, "-maxrate", b, "-bufsize", buf, "-profile:v", "high", "-threads", threads)
		}
		if s.FastStart {
			e = append(e, "-tune", "zerolatency")
		}
	case "libx265":
		params := "log-level=error"
		if pic.outRange != rangeSDR {
			params += hdrX265Params(pic.outRange)
		}
		peak := b
		if s.Rate.Quality {
			peak = maxr
		}
		e = append(e, "-preset", xPreset, "-crf", "23", "-maxrate", peak, "-bufsize", buf, "-x265-params", params)
	case "h264_videotoolbox", "hevc_videotoolbox":
		// Disable open GOPs so forced keyframes are independent HLS cuts.
		e = append(e, "-flags", "+cgop")
		if s.Rate.Quality {
			e = append(e, "-q:v", "65", "-maxrate", maxr, "-bufsize", buf, "-allow_sw", "0")
		} else {
			e = append(e, "-b:v", b, "-maxrate", maxr, "-bufsize", buf, "-allow_sw", "0")
		}
		if s.FastStart {
			e = append(e, "-realtime", "1")
		}
		if enc == "h264_videotoolbox" {
			e = append(e, "-profile:v", "high")
		} else if pic.tenBit {
			e = append(e, "-profile:v", "main10")
		}
	case "h264_nvenc", "hevc_nvenc":
		preset := "p4"
		if s.FastStart {
			preset = "p1"
		}
		if enc == "h264_nvenc" {
			var bframes int
			preset, bframes, _ = nvencFramePlan(s)
			e = append(e, "-bf", strconv.Itoa(bframes))
		}
		if s.Rate.Quality {
			e = append(e, "-preset", preset, "-rc", "vbr", "-cq", "21", "-maxrate", maxr, "-bufsize", buf, "-forced-idr", "1")
		} else {
			e = append(e, "-preset", preset, "-rc", "vbr", "-b:v", b, "-maxrate", maxr, "-bufsize", buf, "-forced-idr", "1")
		}
		if enc == "h264_nvenc" {
			e = append(e, "-profile:v", "high")
		} else if pic.tenBit {
			e = append(e, "-profile:v", "main10")
		}
	case "h264_qsv", "hevc_qsv", "av1_qsv":
		// HLS cuts require IDR pictures, rather than ordinary forced I frames.
		if enc != "av1_qsv" {
			e = append(e, "-forced_idr", "1")
		}
		e = append(e, qsvRateArgs(qsvMode(s, enc), b, maxr, buf)...)
		if enc == "h264_qsv" {
			e = append(e, "-profile:v", "high")
		} else if enc == "hevc_qsv" && pic.tenBit {
			e = append(e, "-profile:v", "main10")
		}
	case "h264_vaapi", "hevc_vaapi":
		rcMode := "VBR"
		if s.Rate.Quality {
			rcMode = "QVBR"
			if m := s.Accel.VAAPIQualityRC[enc]; m != "" {
				rcMode = m
			}
		} else if m := s.Accel.VAAPIBitrateRC[enc]; m != "" {
			rcMode = m
		}
		switch rcMode {
		case "QVBR":
			// FFmpeg VAAPI QVBR still requires a target bitrate (unlike QSV
			// quality modes). Without -b:v, encode fails with "Bitrate must
			// be set for QVBR RC mode" and falls back to CPU.
			e = append(e, "-rc_mode", "QVBR", "-global_quality", "23", "-b:v", b, "-maxrate", maxr, "-bufsize", buf)
		case "CBR":
			e = append(e, "-rc_mode", "CBR", "-b:v", b, "-maxrate", b, "-bufsize", buf)
		case "CQP":
			e = append(e, "-rc_mode", "CQP", "-global_quality", strconv.Itoa(vaapiCQPForBitrate(pic.w, pic.h, videoKbps, codec)))
		default:
			e = append(e, "-rc_mode", "VBR", "-b:v", b, "-maxrate", maxr, "-bufsize", buf)
		}
		if enc == "h264_vaapi" {
			e = append(e, "-profile:v", "high")
		} else if pic.tenBit {
			e = append(e, "-profile:v", "main10")
		}
	}
	if codec == "h264" && s.Decision.H264Level > 0 {
		// Apply only to the negotiated output contract. Legacy Plex decisions
		// leave H264Level zero and retain their byte-identical arguments.
		e = append(e, "-level:v", fmt.Sprintf("%d.%d", s.Decision.H264Level/10, s.Decision.H264Level%10))
		if enc == "h264_nvenc" {
			// NVENC adds references internally; refs=3,bf=2 requires level
			// 6.0 at 4K even when the generic four-picture budget fits 5.1.
			e = append(e, "-refs", "1")
		} else if enc == "libx264" {
			// x264FramePlan already emitted the B-frame limit. A level limits
			// the reference budget; it must not re-enable ramp reordering.
			e = append(e, "-refs", "3")
		} else {
			e = append(e, "-refs", "3", "-bf", "2")
		}
		if s.Decision.H264FrameRate > 0 {
			e = append(e, "-r", strconv.FormatFloat(s.Decision.H264FrameRate, 'f', -1, 64))
		}
		if enc == "libx264" {
			// ultrafast otherwise disables the High-only transform and can emit
			// Constrained Baseline despite -profile:v high.
			_, _, pyramid, _ := x264FramePlan(s)
			e = append(e, "-x264-params", "8x8dct=1:b-pyramid="+pyramid)
		}
	}
	if codec == "hevc" {
		e = append(e, "-tag:v", "hvc1")
	}
	// Tag the output colours. Not with GPU frames: the hardware filters set
	// them, and output colour options make ffmpeg insert a software
	// conversion that cannot take hardware frames.
	switch {
	case hwFrames:
	case s.Target != nil:
		trc := "bt709"
		switch pic.outRange {
		case rangeHDR10:
			trc = "smpte2084"
		case rangeHLG:
			trc = "arib-std-b67"
		}
		// Range follows the frames: zscale converts it, else the source's
		// already matches (convertsColour).
		e = append(e, "-color_primaries", s.Target.pic.prim, "-color_trc", trc, "-colorspace", s.Target.pic.matrix)
	case pic.outRange != rangeSDR:
		trc := "smpte2084"
		if pic.outRange == rangeHLG {
			trc = "arib-std-b67"
		}
		e = append(e, "-color_primaries", "bt2020", "-color_trc", trc, "-colorspace", "bt2020nc")
	default:
		e = append(e, "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	}
	return e
}

// planVideo builds decode/filter/encode arguments for a video transcode.
func planVideo(s runSpec, v *core.StreamInfo) videoPlan {
	d := s.Decision
	var p videoPlan
	if v == nil || d.VideoCopy {
		return p
	}
	codec := d.VideoCodec
	if codec == "" {
		codec = "h264"
	}
	enc, hw := videoEncoder(codec, s.Accel, s.Features)
	if !hw {
		p.inputArgs = append(p.inputArgs, "-threads", strconv.Itoa(softwareThreadBudget(v.Width, v.Height, runtime.GOMAXPROCS(0))))
	}
	accel := AccelNone
	if hw {
		accel = s.Accel.Name
	}
	pic := planPicture(s, v, codec)
	if pic.blocked != "" {
		p.err = &GraphError{Message: pic.blocked}
		return p
	}
	sub := findStream(s.Info, d.SubtitleStream, "subtitle")
	burn := d.BurnSubtitles && sub != nil

	// Software pixel format the encoder receives.
	swFmt := "yuv420p"
	if pic.tenBit {
		swFmt = "yuv420p10le"
		if hw {
			swFmt = "p010le"
		}
	} else if accel == AccelQSV || accel == AccelVAAPI {
		swFmt = "nv12"
	}
	// size crops and scales software frames to the encoded picture; finish
	// sizes, pads and formats them for the encoder.
	size := func(c []string) []string {
		if crop := s.PictureCrop; crop != nil {
			c = append(c, fmt.Sprintf("crop=%d:%d:%d:%d", crop.width, crop.height, crop.x, crop.y))
		}
		if pic.scaling {
			c = append(c, fmt.Sprintf("scale=%d:%d", pic.w, pic.h))
		}
		return c
	}
	format := func(c []string) []string {
		if pic.pad {
			c = append(c, fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2", s.Target.width, s.Target.height))
		}
		if pic.setSAR != "" {
			c = append(c, pic.setSAR)
		}
		c = append(c, "format="+swFmt)
		if accel == AccelVAAPI {
			c = append(c, "hwupload")
		}
		return c
	}
	finish := func(c []string) []string { return format(size(c)) }

	in := fmt.Sprintf("[0:%d]", v.Index)
	chainInput, chain, hwFrames := planFilterChain(s, v, pic, accel, burn, size, format)
	p.hwFrames = hwFrames
	// For bitmap burns, scale on VAAPI before downloading for the software
	// overlay. Upload the result back to the same device for encoding.
	vaapiBitmap := burn && IsImageSubtitle(sub.Codec) && !sub.External && accel == AccelVAAPI &&
		!(v.Codec == "h264" && v.BitDepth > 8) && !s.Accel.NoGPUScale &&
		pic.w > 0 && pic.h > 0 && s.PictureCrop == nil && !pic.pad && pic.convert == "" && !pic.tonemap && !pic.tenBit && pic.setSAR == ""
	if vaapiBitmap {
		chainInput = append(chainInput, "-hwaccel_output_format", "vaapi")
		p.hwFrames = true
		chain = []string{fmt.Sprintf("scale_vaapi=w=%d:h=%d:format=nv12", pic.w, pic.h), "hwdownload", "format=nv12"}
	}
	if pic.tonemap && !toneMapped(chain, hwFrames) {
		p.err = &GraphError{Message: toneMapUnavailableMessage()}
		return p
	}
	_, encodedPreroll := hlsAudioPreroll(s)
	_, copiedPreroll := hlsCopiedAudioPreroll(s)
	if encodedPreroll || copiedPreroll || hlsNVENCPreroll(s) > 0 {
		chain = append([]string{"trim=start=" + secs(s.Start)}, chain...)
	}
	p.inputArgs = append(p.inputArgs, chainInput...)

	var graph string
	switch {
	case burn && IsImageSubtitle(sub.Codec) && !sub.External:
		post := finish(nil)
		subW, subH := v.Width, v.Height
		if vaapiBitmap {
			post = format(nil)
			subW, subH = pic.w, pic.h
		} else if pic.scaling && s.PictureCrop == nil {
			// Overlay at the output size: scale the picture first and
			// draw the subtitle onto the smaller frame, instead of
			// overlaying every source-size frame and scaling after
			// (#313). A cropped picture keeps the source-size overlay,
			// where the subtitle canvas still matches it.
			chain = size(chain)
			post = format(nil)
			subW, subH = pic.w, pic.h
		}
		pre := strings.Join(chain, ",")
		if pre == "" {
			pre = "null"
		}
		subScale := "null"
		if subW > 0 && subH > 0 {
			subScale = fmt.Sprintf("scale=%d:%d", subW, subH)
		}
		// Without -copyts, -ss puts this run's frames at PTS 0 while a
		// bitmap subtitle packet can keep its title timestamp. Reset both
		// before overlay so the filter does not hold the picture waiting
		// for a clock that never meets, and so the segment muxer's
		// output_ts_offset is what places the run on the title timeline.
		// A -copyts run already has both on the title clock, which its
		// audio keeps too.
		if s.Start > 0 && !s.titleClock() {
			pre += ",setpts=PTS-STARTPTS"
			if subScale == "null" {
				subScale = "setpts=PTS-STARTPTS"
			} else {
				subScale = "setpts=PTS-STARTPTS," + subScale
			}
		}
		graph = fmt.Sprintf("%s%s[base];[0:%d]%s[sub];[base][sub]overlay=eof_action=pass,%s[vout]",
			in, pre, sub.Index, subScale, strings.Join(post, ","))
	case burn && s.Features.hasFilter("subtitles"):
		src := s.Input
		opt := ""
		if sub.External && sub.ExternalURL != "" {
			src = sub.ExternalURL
		} else {
			opt = fmt.Sprintf(":si=%d", subtitleOrdinal(s.Info, sub.Index))
		}
		// The subtitles filter looks up cues by frame PTS. Without -copyts,
		// input -ss resets that to 0, so a seeked run would burn the opening
		// cues (or stall if the filter then emits the file's own timeline).
		// Shift onto the title clock for the lookup, then back to the
		// run-relative clock the muxer offsets. The filter's own offset
		// option saturates at ten minutes, which is shorter than a resume.
		// A -copyts run's frames are on the title clock already: shifting
		// them burned the cues one offset later and moved the picture off
		// its audio (#191).
		shift := s.Start > 0 && !s.titleClock()
		if shift {
			chain = append(chain, "setpts=PTS+"+secs(s.Start)+"/TB")
		}
		chain = append(chain, fmt.Sprintf("subtitles=filename=%s%s", escapeFilterValue(src), opt))
		if shift {
			chain = append(chain, "setpts=PTS-STARTPTS")
		}
		chain = finish(chain)
		graph = in + strings.Join(chain, ",") + "[vout]"
	default:
		if burn {
			// Text burn-in needs libass (the subtitles filter); without it
			// the client gets the subtitle as a sidecar instead.
			if !hwFrames {
				chain = finish(chain)
			}
		}
		if len(chain) == 0 {
			chain = []string{"null"}
		}
		graph = in + strings.Join(chain, ",") + "[vout]"
	}
	p.graph = graph

	// VAAPI's software-filter paths end in hwupload too. Output colour
	// options after that upload cause FFmpeg to insert an impossible software
	// conversion between VAAPI surfaces and the encoder.
	p.encodeArgs = planEncodeArgs(s, pic, enc, codec, hwFrames || (accel == AccelVAAPI && burn))
	return p
}

// toneMapped reports whether the filter chain actually converts HDR to SDR.
// Hardware frames are a tone map only when the GPU branch that set them
// was selected for a tone-map (scale_vt, tonemap_vaapi, tonemap_cuda).
func toneMapped(chain []string, hwFrames bool) bool {
	if hwFrames {
		return true
	}
	g := strings.Join(chain, ",")
	return strings.Contains(g, "tonemap") || strings.Contains(g, "libplacebo") || strings.Contains(g, "zscale")
}

func filterGraph(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-filter_complex" {
			return args[i+1]
		}
	}
	return ""
}

func hdrX265Params(outRange string) string {
	trc := "smpte2084"
	if outRange == rangeHLG {
		trc = "arib-std-b67"
	}
	return ":hdr10-opt=1:repeat-headers=1:colorprim=bt2020:colormatrix=bt2020nc:transfer=" + trc
}

// videoBitrate is the video share of a transcode decision's bitrate.
func videoBitrate(info core.MediaInfo, d core.Decision) int {
	audio := 0
	if a := findStream(info, d.AudioStream, "audio"); a != nil {
		if d.AudioCopy {
			audio = copiedAudioKbps(a)
		} else {
			audio = audioBitrateKbps(d.AudioCodec, decisionAudioChannels(d, a))
		}
	}
	k := d.BitrateKbps - audio
	if d.BitrateKbps <= 0 {
		k = bitrateCeiling(d.MaxHeight, d.VideoCodec)
	}
	if k < 200 {
		k = 200
	}
	return k
}

// subtitleArgs extracts one embedded text subtitle stream as WebVTT on
// stdout.
func subtitleArgs(input string, streamIndex int) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	input = backgroundReadURL(input)
	args = append(args, inputSafetyArgs(input, false)...)
	args = append(args, httpInputArgs(input, true)...)
	return append(args, "-i", input, "-map", fmt.Sprintf("0:%d", streamIndex), "-c:s", "webvtt", "-f", "webvtt", "pipe:1")
}

// subtitleStreamArgs streams one embedded text subtitle stream from from
// as an ASS script on stdout: input seek with -copyts keeps the cue times
// on the title clock, and every event is written as soon as it is read.
// throttle reads the first minute at full speed and then 1.5x real time.
func subtitleStreamArgs(input string, streamIndex int, from time.Duration, throttle bool) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	input = backgroundReadURL(input)
	args = append(args, inputSafetyArgs(input, false)...)
	args = append(args, httpInputArgs(input, true)...)
	if throttle {
		args = append(args, "-readrate", "1.5", "-readrate_initial_burst", "60")
	}
	if from > 0 {
		args = append(args, "-ss", secs(from))
	}
	return append(args, "-copyts", "-i", input, "-map", fmt.Sprintf("0:%d", streamIndex),
		"-c:s", "ass", "-f", "ass", "-ignore_readorder", "1", "-flush_packets", "1", "pipe:1")
}

// vaapiCQPForBitrate derives a constant-quantizer value (18..36) when a VAAPI
// driver only supports CQP (for example low-power EncSliceLP without EU rate-
// control shaders), scaling QP to roughly match the requested bitrate budget
// for the output resolution.
func vaapiCQPForBitrate(w, h, kbps int, codec string) int {
	if kbps <= 0 {
		return 23
	}
	pixels := w * h
	if pixels <= 0 {
		pixels = 1920 * 1080
	}
	// Reference bitrate for QP 23 at 1080p: ~8000 kbps for H.264, ~5000 kbps for HEVC.
	ref1080 := 8000.0
	if codec == "hevc" {
		ref1080 = 5000.0
	}
	refKbps := ref1080 * (float64(pixels) / float64(1920*1080))
	if refKbps < 600 {
		refKbps = 600
	}
	// Every ~6 QP steps halves/doubles bitrate: deltaQP ≈ 6 * log2(refKbps / kbps).
	qp := int(math.Round(23.0 + 6.0*math.Log2(refKbps/float64(kbps))))
	if qp < 18 {
		qp = 18
	}
	if qp > 36 {
		qp = 36
	}
	return qp
}
