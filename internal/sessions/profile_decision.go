package sessions

import (
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func decisionEncodes(d core.Decision, info core.MediaInfo) bool {
	if d.Kind == core.DecisionTranscode {
		return true
	}
	if d.Kind != core.DecisionRemux {
		return false
	}
	return info.Video() != nil && !d.VideoCopy || profileStream(info, "audio", d.AudioStream) != nil && !d.AudioCopy
}

func applySubtitleProfiles(caps *core.ClientCaps) {
	if caps.Profiles == nil || caps.Profiles.Subtitles == nil {
		return
	}
	caps.SubtitleCodecs = make([]string, 0)
	for _, p := range caps.Profiles.Subtitles {
		if !strings.EqualFold(p.Method, "embed") {
			continue
		}
		codec := strings.ToLower(p.Format)
		switch codec {
		case "pgs":
			caps.SubtitleCodecs = append(caps.SubtitleCodecs, "pgs", "hdmv_pgs_subtitle")
		case "dvdsub", "vobsub":
			caps.SubtitleCodecs = append(caps.SubtitleCodecs, "vobsub", "dvd_subtitle")
		default:
			if codec != "" {
				caps.SubtitleCodecs = append(caps.SubtitleCodecs, codec)
			}
		}
	}
}

func profileName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "ts", "mpegts":
		return "mpegts"
	case "m4v", "mov", "fmp4":
		return "mp4"
	case "avc", "avc1":
		return "h264"
	case "h265":
		return "hevc"
	}
	return s
}
func profileContains(xs []string, name string) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if profileName(x) == profileName(name) {
			return true
		}
	}
	return false
}
func profileStream(info core.MediaInfo, kind string, index int) *core.StreamInfo {
	var first, def *core.StreamInfo
	for i := range info.Streams {
		s := &info.Streams[i]
		if s.Type != kind {
			continue
		}
		if s.Index == index {
			return s
		}
		if first == nil {
			first = s
		}
		if s.Default && def == nil {
			def = s
		}
	}
	if def != nil {
		return def
	}
	return first
}

// Unknown required predicates cannot authorize a profile. Optional unknown
// predicates do not constrain playback, as declared by the client.
func profileConditions(cs []core.ClientProfileCondition, info core.MediaInfo, stream *core.StreamInfo) bool {
	match, unknown := evaluateProfileConditions(cs, info, stream)
	return match && !unknown
}

// unknown reports an unsupported or unavailable required value separately
// from a false condition, which can make an ApplyConditions profile inapplicable.
func evaluateProfileConditions(cs []core.ClientProfileCondition, info core.MediaInfo, stream *core.StreamInfo) (bool, bool) {
	for _, c := range cs {
		var value string
		switch strings.ToLower(c.Property) {
		case "videowidth", "width":
			if stream != nil {
				value = strconv.Itoa(stream.Width)
			}
		case "videoheight", "height":
			if stream != nil {
				value = strconv.Itoa(stream.Height)
			}
		case "videobitdepth":
			if stream != nil {
				value = strconv.Itoa(stream.BitDepth)
			}
		case "videolevel":
			if stream != nil {
				value = strconv.Itoa(stream.Level)
			}
		case "videoprofile", "audioprofile":
			if stream != nil {
				value = stream.Profile
			}
		case "audiochannels":
			if stream != nil {
				value = strconv.Itoa(stream.Channels)
			}
		case "audiosamplerate":
			if stream != nil {
				value = strconv.Itoa(stream.SampleRate)
			}
		case "videobitrate", "audiobitrate":
			if stream != nil {
				value = strconv.Itoa(stream.BitrateKbps * 1000)
			}
		case "containerbitrate":
			value = strconv.Itoa(info.BitrateKbps * 1000)
		}
		if value == "" || value == "0" {
			if c.Required {
				return false, true
			}
			continue
		}
		want := strings.TrimSpace(c.Value)
		ok, known := false, true
		switch strings.ToLower(c.Operator) {
		case "equals":
			ok = strings.EqualFold(value, want)
		case "notequals":
			ok = !strings.EqualFold(value, want)
		case "equalsany":
			for _, v := range strings.Split(want, "|") {
				if strings.EqualFold(value, strings.TrimSpace(v)) {
					ok = true
				}
			}
		case "lessthanequal", "greaterthanequal":
			a, e1 := strconv.ParseFloat(value, 64)
			b, e2 := strconv.ParseFloat(want, 64)
			known = e1 == nil && e2 == nil
			if known {
				if strings.EqualFold(c.Operator, "lessthanequal") {
					ok = a <= b
				} else {
					ok = a >= b
				}
			}
		default:
			known = false
		}
		if !known && c.Required {
			return false, true
		}
		if known && !ok {
			return false, false
		}
	}
	return true, false
}

func profileRestrictions(p *core.ClientCapabilityProfiles, info core.MediaInfo, video, audio string, audioIndex int) bool {
	for _, c := range p.Containers {
		if (c.MediaType == "" || strings.EqualFold(c.MediaType, "video")) && profileContains(c.Containers, info.Container) && !profileConditions(c.Conditions, info, info.Video()) {
			return false
		}
	}
	for _, c := range p.Codecs {
		var stream *core.StreamInfo
		var codec string
		switch strings.ToLower(c.MediaType) {
		case "video":
			stream = info.Video()
			codec = video
		case "audio", "videoaudio":
			stream = profileStream(info, "audio", audioIndex)
			codec = audio
		default:
			for _, x := range append(append([]core.ClientProfileCondition(nil), c.Conditions...), c.ApplyConditions...) {
				if x.Required {
					return false
				}
			}
			continue
		}
		if !profileContains(c.Codecs, codec) || !profileContains(c.Containers, info.Container) {
			continue
		}
		applies, unknown := evaluateProfileConditions(c.ApplyConditions, info, stream)
		if unknown {
			return false
		}
		if !applies {
			continue
		}
		if !profileConditions(c.Conditions, info, stream) {
			return false
		}
	}
	return true
}

func profileDirectAllowed(p *core.ClientCapabilityProfiles, info core.MediaInfo, d core.Decision) bool {
	if p == nil {
		return true
	}
	if p.MaxStaticBitrate > 0 && info.BitrateKbps > 0 && int64(info.BitrateKbps)*1000 > int64(p.MaxStaticBitrate) {
		return false
	}
	video, audio := "", ""
	if s := info.Video(); s != nil {
		video = s.Codec
	}
	if s := profileStream(info, "audio", d.AudioStream); s != nil {
		audio = s.Codec
	}
	if !profileRestrictions(p, info, video, audio, d.AudioStream) {
		return false
	}
	for _, candidate := range p.DirectPlay {
		if (candidate.MediaType == "" || strings.EqualFold(candidate.MediaType, "video")) && profileContains(candidate.Containers, info.Container) && profileContains(candidate.VideoCodecs, video) && profileContains(candidate.AudioCodecs, audio) {
			return true
		}
	}
	return false
}

func profileDeliveryAllowed(p *core.ClientCapabilityProfiles, d core.Decision, info core.MediaInfo, delivery string) bool {
	if p == nil {
		return true
	}
	output := info
	output.Container = d.Container
	output.Streams = slices.Clone(info.Streams)
	for i := range output.Streams {
		s := &output.Streams[i]
		if s.Type == "video" {
			s.Codec = d.VideoCodec
			if !d.VideoCopy {
				s.Width = 0
				s.Height = d.MaxHeight
				s.Level = 0
				s.Profile = ""
				s.BitDepth = 0
				s.BitrateKbps = 0
			}
		} else if s.Type == "audio" && s.Index == d.AudioStream {
			s.Codec = d.AudioCodec
			if !d.AudioCopy {
				s.Channels = d.AudioChannels
				s.SampleRate = d.AudioSampleRate
				s.BitrateKbps = 0
				s.Profile = ""
			}
		}
	}
	for _, c := range p.Transcoding {
		if !strings.EqualFold(c.Delivery, delivery) || (c.MediaType != "" && !strings.EqualFold(c.MediaType, "video")) || !profileContains(c.Containers, d.Container) || !profileContains(c.VideoCodecs, d.VideoCodec) || !profileContains(c.AudioCodecs, d.AudioCodec) {
			continue
		}
		if c.MaxAudioChannels != "" && (d.AudioCodec != "" || profileStream(info, "audio", d.AudioStream) != nil) {
			limit, err := strconv.Atoi(c.MaxAudioChannels)
			channels := d.AudioChannels
			if channels == 0 && d.AudioCopy {
				if source := profileStream(info, "audio", d.AudioStream); source != nil {
					channels = source.Channels
				}
			}
			if err != nil || limit <= 0 || channels <= 0 || channels > limit {
				continue
			}
		}
		if profileConditions(c.Conditions, info, info.Video()) && profileRestrictions(p, output, d.VideoCodec, d.AudioCodec, d.AudioStream) {
			return true
		}
	}
	return false
}

func (b *Broker) decideProfile(ps *core.PlaySession, req frontend.PlanRequest) (core.Decision, error) {
	r := DecisionRequest{Session: ps, Caps: req.Caps, Options: req.Intent.Options, PreferRemux: req.Intent.PreferRemux, PreserveSourceOnFailure: true}
	d, err := Decide(b.deps.Playback, b.deps.Transcoder, r)
	if err != nil || req.Caps.Profiles == nil {
		return d, err
	}
	if d.Kind == core.DecisionDirectPlay && profileDirectAllowed(req.Caps.Profiles, ps.Info, d) {
		return d, nil
	}
	allowed := func(d core.Decision) bool {
		return req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding || !decisionEncodes(d, ps.Info)
	}
	if d.Kind != core.DecisionDirectPlay && allowed(d) && (profileDeliveryAllowed(req.Caps.Profiles, d, ps.Info, "hls") || profileDeliveryAllowed(req.Caps.Profiles, d, ps.Info, "dash")) {
		return d, nil
	}
	for _, candidate := range req.Caps.Profiles.Transcoding {
		if !strings.EqualFold(candidate.Delivery, "hls") && !strings.EqualFold(candidate.Delivery, "dash") {
			continue
		}
		caps := req.Caps
		caps.VideoCodecs = candidate.VideoCodecs
		caps.AudioCodecs = candidate.AudioCodecs
		caps.HLSContainers = candidate.Containers
		opts := req.Intent.Options
		opts.DirectPlay = false
		d, err = Decide(b.deps.Playback, b.deps.Transcoder, DecisionRequest{Session: ps, Caps: caps, Options: opts, PreferRemux: req.Intent.PreferRemux, PreserveSourceOnFailure: true})
		if err == nil && d.Kind != core.DecisionDirectPlay && allowed(d) && profileDeliveryAllowed(req.Caps.Profiles, d, ps.Info, candidate.Delivery) {
			return d, nil
		}
	}
	return core.Decision{}, core.ErrNotAvailable
}
