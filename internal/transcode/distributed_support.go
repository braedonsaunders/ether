package transcode

import (
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// SupportsDistributedRequest checks the actual normalized command dependencies
// against startup-probed inventory. It performs no source, process, assignment
// or reservation work. Transport capability/policy checks remain mandatory too.
// The same fixed dedicated Manager must be used for preflight and actual start;
// this is not a promise of future capacity or successful device/process execution.
func (m *Manager) SupportsDistributedRequest(req core.HLSRequest) bool {
	if m == nil || req.RecoveryStart != nil || req.Info.Estimated || len(req.Info.Streams) > 64 || len(req.Info.Chapters) > 128 || req.StartOffset < 0 || req.SegmentDuration < 0 || req.SegmentDuration > time.Minute || req.LiveInput || req.CopyTimestamps || len(req.SlateText) > 0 || len(req.InputHeaders) > 0 || req.InputLease != nil || !ValidateDistributedKeyframes(req.Info) || !distributedRequestSize(req) {
		return false
	}
	for _, stream := range req.Info.Streams {
		if stream.External || stream.ExternalURL != "" {
			return false
		}
	}
	if req.Decision.Kind != core.DecisionRemux && req.Decision.Kind != core.DecisionTranscode || req.Decision.BurnSubtitles || len(req.Decision.AudioFilters) > 0 || !SupportsDistributedEnhancement(req.Decision.Enhancement) {
		return false
	}
	m.mu.Lock()
	cfg, closed := m.cfg, m.closed
	m.mu.Unlock()
	if closed || cfg.FastStart || cfg.SpikeGovernor || (cfg.RTXVideo.Mode != "" && cfg.RTXVideo.Mode != "off") {
		return false
	}
	f := m.feat
	if len(f.muxers) == 0 || len(f.demuxers) == 0 || len(f.decoders) == 0 || len(f.bsfs) == 0 || !f.inputProtocols["http"] || !f.inputProtocols["https"] || !f.inputProtocols["tcp"] || !f.inputProtocols["tls"] {
		return false
	}
	input := req.Info.Container
	switch input {
	case "mkv":
		input = "matroska"
	case "mp4", "m4v", "m4a", "3gp", "3g2", "mj2":
		input = "mov"
	case "ts":
		input = "mpegts"
	}
	if !f.demuxers[input] {
		return false
	}
	proto := req.Protocol
	if proto == "" {
		proto = core.ProtocolHLS
	}
	if proto != core.ProtocolHLS && proto != core.ProtocolDASH && proto != core.ProtocolHTTP {
		return false
	}
	d := req.Decision
	if v := req.Info.Video(); v == nil {
		d.VideoCopy = true
	} else if d.VideoCopy && d.VideoCodec == "" {
		d.VideoCodec = v.Codec
	}
	if d.AudioStream < 0 {
		if a := pickAudio(req.Info, -1); a != nil {
			d.AudioStream = a.Index
		}
	}
	if a := findStream(req.Info, d.AudioStream, "audio"); a != nil && d.AudioCopy && d.AudioCodec == "" {
		d.AudioCodec = a.Codec
	}
	if proto == core.ProtocolDASH {
		d = dashDecision(req.Info, d)
	}
	if proto == core.ProtocolHTTP {
		d.Container = ContainerMatroska
		if a := findStream(req.Info, d.AudioStream, "audio"); a != nil && d.AudioCopy && strings.EqualFold(a.Codec, "aac") {
			d.AudioCopy = false
			d.AudioCodec = "aac"
		}
	} else if proto != core.ProtocolDASH && d.Container != ContainerFMP4 && d.Container != ContainerMPEGTS && d.Container != ContainerMatroska {
		d.Container = segmentContainer(d.VideoCodec, d.AudioCodec)
	}
	if !d.VideoCopy && d.VideoCodec == "" {
		d.VideoCodec = "h264"
	}
	if !d.AudioCopy && d.AudioCodec == "" {
		d.AudioCodec = "aac"
	}
	if d.VideoCopy && d.ToneMap {
		return false
	}
	decisions := []core.Decision{d}
	if proto != core.ProtocolHTTP {
		rungs := buildRungs(req.Info, d, adaptiveFromConfig(cfg))
		if proto == core.ProtocolHLS && d.Container == ContainerMatroska && len(rungs) > 1 {
			rungs = rungs[:1]
		}
		if proto == core.ProtocolDASH && findStream(req.Info, d.AudioStream, "audio") == nil && len(rungs) > 1 {
			rungs = rungs[:1]
		}
		for _, r := range rungs {
			decisions = append(decisions, r.decision)
		}
	}
	for _, decision := range decisions {
		// Muxers can auto-insert these BSFs without spelling them in argv.
		if decision.VideoCopy && decision.Container == ContainerMPEGTS {
			if v := req.Info.Video(); v != nil && (v.Codec == "h264" || v.Codec == "hevc") && !f.bsfs[v.Codec+"_mp4toannexb"] {
				return false
			}
		}
		if a := findStream(req.Info, decision.AudioStream, "audio"); a != nil && decision.AudioCopy && a.Codec == "aac" && decision.Container == ContainerFMP4 && (req.Info.Container == "mpegts" || req.Info.Container == "aac") && !f.bsfs["aac_adtstoasc"] {
			return false
		}
		if !distributedMuxCodecs(req.Info, decision) {
			return false
		}
		if !decision.VideoCopy {
			v := req.Info.Video()
			if v == nil || !distributedDecoder(f, v.Codec) || !f.hasFilter("scale") {
				return false
			}
			codec := decision.VideoCodec
			if codec == "" {
				codec = "h264"
			}
			if codec != "h264" && codec != "hevc" {
				return false
			}
			enc, _ := videoEncoder(codec, m.hw, f)
			if !f.hasEncoder(enc) || (codec == "hevc" && enc == "libx264") {
				return false
			}
		}
		if a := findStream(req.Info, decision.AudioStream, "audio"); a != nil && !decision.AudioCopy && (!distributedDecoder(f, a.Codec) || !f.hasFilter("aresample")) {
			return false
		}
		spec := runSpec{Info: req.Info, Decision: decision, Protocol: proto, Start: req.StartOffset, SegDur: req.SegmentDuration, Accel: m.hw, Features: f, Dir: "distributed-plan", Playlist: "index.m3u8", InitName: "init.mp4"}
		if v := req.Info.Video(); v != nil && !decision.VideoCopy {
			if planVideo(spec, v).err != nil {
				return false
			}
		}
		if req.SoftSubtitleStream != nil {
			if proto == core.ProtocolHTTP {
				return false
			}
			sub := findStream(req.Info, *req.SoftSubtitleStream, "subtitle")
			if sub == nil || !IsTextSubtitle(sub.Codec) || !distributedDecoder(f, sub.Codec) {
				return false
			}
			spec.SoftSubtitlePath = "distributed-plan/subtitle.ass"
			spec.SoftSubtitleIndex = *req.SoftSubtitleStream
		}
		if !distributedCommandSupported(buildArgs(spec), f) {
			return false
		}
		// Segment seeks are advertised: validate the extra seek-time bitstream
		// dependencies even when this particular start begins at zero.
		if spec.Start == 0 {
			spec.Start = time.Second
			if !distributedCommandSupported(buildArgs(spec), f) {
				return false
			}
		}
	}
	return true
}

func distributedDecoder(f features, codec string) bool {
	if f.decoders[codec] {
		return true
	}
	switch codec {
	case "av1":
		return f.decoders["libdav1d"] || f.decoders["libaom-av1"]
	case "srt":
		return f.decoders["subrip"]
	case "ssa":
		return f.decoders["ass"]
	}
	return false
}

// Reject unknown combinations instead of using a muxer name as proof that every
// codec can be copied into it. This is conservative admission, never a codec swap.
func distributedMuxCodecs(info core.MediaInfo, d core.Decision) bool {
	video, audio := d.VideoCodec, d.AudioCodec
	if v := info.Video(); v != nil && d.VideoCopy {
		video = v.Codec
	}
	if a := findStream(info, d.AudioStream, "audio"); a != nil && d.AudioCopy {
		audio = a.Codec
	}
	switch d.Container {
	case ContainerMPEGTS:
		if info.Video() != nil && video != "h264" && video != "hevc" && video != "mpeg2video" {
			return false
		}
		if findStream(info, d.AudioStream, "audio") != nil && audio != "aac" && audio != "ac3" && audio != "eac3" && audio != "mp2" && audio != "mp3" {
			return false
		}
	case ContainerFMP4:
		if info.Video() != nil && !hlsVideoOK(video) {
			return false
		}
		if findStream(info, d.AudioStream, "audio") != nil && !hlsAudioOK(audio) {
			return false
		}
	case ContainerMatroska:
		if info.Video() != nil && video != "h264" && video != "hevc" && video != "av1" && video != "vp9" && video != "mpeg4" && video != "mpeg2video" {
			return false
		}
		if findStream(info, d.AudioStream, "audio") != nil && !hlsAudioOK(audio) && audio != "dts" && audio != "truehd" && audio != "pcm_s16le" && audio != "pcm_s24le" {
			return false
		}
	default:
		return false
	}
	return true
}

func distributedCommandSupported(args []string, f features) bool {
	for i := 0; i+1 < len(args); i++ {
		flag, value := args[i], args[i+1]
		switch flag {
		case "-c:v", "-c:a", "-c:s":
			if value != "copy" && !f.encoders[value] {
				return false
			}
		case "-f", "-segment_format":
			if !f.muxers[value] {
				return false
			}
			if value == "dash" && !f.muxers["mp4"] {
				return false
			}
		case "-hls_segment_type":
			mux := value
			if mux == "fmp4" {
				mux = "mp4"
			}
			if !f.muxers[mux] {
				return false
			}
		case "-vf", "-af", "-filter_complex":
			if !distributedFilterList(value, f.filters) {
				return false
			}
		case "-bsf:v", "-bsf:a", "-bsf:s":
			if !distributedFilterList(value, f.bsfs) {
				return false
			}
		case "-hwaccel":
			if value != "none" && !f.hwaccels[value] {
				return false
			}
		}
	}
	// HLS defaults its child muxer to MPEG-TS without an explicit segment type.
	if argumentValue(args, "-f") == "hls" && argumentValue(args, "-hls_segment_type") == "" && !f.muxers["mpegts"] {
		return false
	}
	return true
}
func argumentValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// Parse only filter separators outside quoted/escaped expressions and brackets.
// This reads our generated plan, not caller-provided filter expressions.
func distributedFilterList(graph string, inventory map[string]bool) bool {
	start, quote, depth, escaped := 0, rune(0), 0, false
	check := func(part string) bool {
		part = strings.TrimSpace(part)
		for strings.HasPrefix(part, "[") {
			end := strings.IndexByte(part, ']')
			if end < 0 {
				return false
			}
			part = strings.TrimSpace(part[end+1:])
		}
		end := strings.IndexAny(part, "=[:")
		if end >= 0 {
			part = part[:end]
		}
		return inventoryName(part) && inventory[part]
	}
	for i, c := range graph {
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
		}
		if depth < 0 {
			return false
		}
		if (c == ',' || c == ';') && depth == 0 {
			if !check(graph[start:i]) {
				return false
			}
			start = i + 1
		}
	}
	return quote == 0 && depth == 0 && !escaped && check(graph[start:])
}
