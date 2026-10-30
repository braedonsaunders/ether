package sessions

import (
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// ProfileDiagnostic is a redacted reason emitted only to an explicitly
// installed diagnostic hook. It never includes client profile values.
type ProfileDiagnostic string

const (
	ProfileRequiredConditionUnknown ProfileDiagnostic = "required_condition_unknown"
	ProfileDirectRejected           ProfileDiagnostic = "direct_rejected"
	ProfileDeliveryRejected         ProfileDiagnostic = "delivery_rejected"
	ProfileFallbackTranscode        ProfileDiagnostic = "fallback_transcode"
	ProfileFallbackRemux            ProfileDiagnostic = "fallback_remux"
	ProfileNoDelivery               ProfileDiagnostic = "no_delivery"
)

func (b *Broker) profileDiagnostic(event ProfileDiagnostic) {
	if b.deps.OnProfileDiagnostic != nil {
		b.deps.OnProfileDiagnostic(event)
	}
}

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
		case "videorangetype":
			// This condition can establish DOVI versus non-DOVI without
			// inventing an SDR/HDR subtype. Other comparisons stay unknown.
			if stream != nil && strings.EqualFold(strings.TrimSpace(c.Value), "DOVI") && (strings.EqualFold(c.Operator, "Equals") || strings.EqualFold(c.Operator, "NotEquals")) {
				if stream.DolbyVision || (stream.DolbyVisionConfigurationPresent != nil && *stream.DolbyVisionConfigurationPresent) {
					value = "DOVI"
				} else if stream.DolbyVisionConfigurationPresent != nil && !*stream.DolbyVisionConfigurationPresent {
					value = "non-DOVI"
				}
			}
		case "videowidth", "width":
			if stream != nil {
				value = strconv.Itoa(stream.Width)
			}
		case "videoheight", "height":
			if stream != nil {
				value = strconv.Itoa(stream.Height)
			}
		case "videoframerate", "framerate":
			if stream != nil && stream.FrameRate > 0 {
				value = strconv.FormatFloat(stream.FrameRate, 'f', -1, 64)
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

func profileUnknownRequired(p *core.ClientCapabilityProfiles, info core.MediaInfo, d core.Decision) bool {
	video, audio := info.Video(), profileStream(info, "audio", d.AudioStream)
	for _, c := range p.Containers {
		if (c.MediaType == "" || strings.EqualFold(c.MediaType, "video")) && profileContains(c.Containers, info.Container) {
			_, unknown := evaluateProfileConditions(c.Conditions, info, video)
			if unknown {
				return true
			}
		}
	}
	for _, c := range p.Codecs {
		var stream *core.StreamInfo
		switch strings.ToLower(c.MediaType) {
		case "video":
			stream = video
		case "audio", "videoaudio":
			stream = audio
		default:
			continue
		}
		if stream == nil || !profileContains(c.Codecs, stream.Codec) || !profileContains(c.Containers, info.Container) {
			continue
		}
		applies, unknown := evaluateProfileConditions(c.ApplyConditions, info, stream)
		if unknown {
			return true
		}
		if applies {
			_, unknown = evaluateProfileConditions(c.Conditions, info, stream)
			if unknown {
				return true
			}
		}
	}
	return false
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
				s.Width, s.Height = transcode.OutputGeometry(s.Width, s.Height, d.MaxHeight)
				s.Level = d.H264Level
				s.Profile = ""
				s.BitDepth = 0
				if d.VideoCodec == "h264" {
					s.FrameRate = 0
					if d.H264Level > 0 {
						s.Profile, s.BitDepth, s.FrameRate = "High", 8, d.H264FrameRate
					}
					no := false
					s.DolbyVision, s.DolbyVisionConfigurationPresent = false, &no
				}
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

// constrainedH264 describes only facts guaranteed by the selected encoder
// contract. Unknown geometry or rate stays unknown, so required predicates
// still fail closed.
func constrainedH264(d core.Decision, info core.MediaInfo, caps core.ClientCaps, encoderVerified bool) (core.Decision, bool) {
	if d.VideoCopy || d.VideoCodec != "h264" {
		return d, true
	}
	ordinary := d.Enhancement.Backend == ""
	if !ordinary {
		switch d.Enhancement.State {
		case "off", "ineligible", "unavailable", "fallback":
			ordinary = true
		}
	}
	if !encoderVerified || !ordinary {
		// Do not assert an output profile or level for an unverified hardware
		// encoder. Required profile predicates fail closed in the caller.
		return d, caps.VideoLevels["h264"] <= 0 && len(caps.VideoProfiles["h264"]) == 0
	}
	if profiles := caps.VideoProfiles["h264"]; len(profiles) > 0 && !profileContains(profiles, "High") {
		return d, false
	}
	v := info.Video()
	if v == nil {
		return d, caps.VideoLevels["h264"] <= 0
	}
	fps := v.FrameRate
	if v.ReferenceFrameRate != nil && *v.ReferenceFrameRate > fps {
		fps = *v.ReferenceFrameRate
	}
	if v.AverageFrameRate != nil && *v.AverageFrameRate > fps {
		fps = *v.AverageFrameRate
	}
	w, h := transcode.OutputGeometry(v.Width, v.Height, d.MaxHeight)
	if w == 0 || h == 0 || fps <= 0 || d.BitrateKbps <= 0 {
		return d, caps.VideoLevels["h264"] <= 0 && len(caps.VideoProfiles["h264"]) == 0
	}
	if d.BitrateKbps > 200000 {
		return d, false
	}
	// The encoder's bounded profile uses at most four decoded pictures and a
	// peak no greater than 150% of the total decision bitrate.
	level := transcode.H264HighLevel(w, h, fps, d.BitrateKbps*3/2, 4)
	if level == 0 || (caps.VideoLevels["h264"] > 0 && level > caps.VideoLevels["h264"]) {
		return d, false
	}
	d.H264Level = level
	d.H264FrameRate = fps
	return d, true
}

// h264EncoderVerified matches the manager's actual encoder-selection rule.
// QSV, VAAPI and VideoToolbox remain unknown until their SPS is verified.
func (b *Broker) h264EncoderVerified() bool {
	if b.deps.Transcoder == nil {
		return false
	}
	hw := b.deps.Transcoder.HWAccel()
	if hw.Name == "" || hw.Name == transcode.AccelNone {
		return true
	}
	if !slices.Contains(hw.Encoders, "h264_"+hw.Name) {
		return true
	}
	return hw.Name == transcode.AccelNVENC
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
	if d.Kind == core.DecisionDirectPlay {
		b.profileDiagnostic(ProfileDirectRejected)
		if b.deps.OnProfileDiagnostic != nil && profileUnknownRequired(req.Caps.Profiles, ps.Info, d) {
			b.profileDiagnostic(ProfileRequiredConditionUnknown)
		}
	}
	allowed := func(d core.Decision) bool {
		return req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding || !decisionEncodes(d, ps.Info)
	}
	if output, ok := constrainedH264(d, ps.Info, req.Caps, b.h264EncoderVerified()); d.Kind != core.DecisionDirectPlay && ok && allowed(output) && (profileDeliveryAllowed(req.Caps.Profiles, output, ps.Info, "hls") || profileDeliveryAllowed(req.Caps.Profiles, output, ps.Info, "dash")) {
		return output, nil
	}
	if d.Kind != core.DecisionDirectPlay {
		b.profileDiagnostic(ProfileDeliveryRejected)
	}
	for _, candidate := range req.Caps.Profiles.Transcoding {
		if !strings.EqualFold(candidate.Delivery, "hls") && !strings.EqualFold(candidate.Delivery, "dash") {
			continue
		}
		try := func(videoCodecs []string) (core.Decision, bool) {
			caps := req.Caps
			caps.VideoCodecs = videoCodecs
			caps.AudioCodecs = candidate.AudioCodecs
			caps.HLSContainers = candidate.Containers
			opts := req.Intent.Options
			opts.DirectPlay = false
			// Retry only bounded output heights; never raise a client or request
			// cap. This allows a lower resolution when the required H.264 level
			// or dimensions cannot support the original size.
			for _, height := range []int{0, 2160, 1440, 1080, 720, 576, 480, 360, 240, 144} {
				attempt := opts
				if height > 0 && (attempt.MaxHeight == 0 || height < attempt.MaxHeight) {
					attempt.MaxHeight = height
				}
				decision, err := Decide(b.deps.Playback, b.deps.Transcoder, DecisionRequest{Session: ps, Caps: caps, Options: attempt, PreferRemux: req.Intent.PreferRemux, PreserveSourceOnFailure: true})
				if err != nil || decision.Kind == core.DecisionDirectPlay || !allowed(decision) {
					continue
				}
				decision, ok := constrainedH264(decision, ps.Info, caps, b.h264EncoderVerified())
				if ok && profileDeliveryAllowed(req.Caps.Profiles, decision, ps.Info, candidate.Delivery) {
					return decision, true
				}
				if decision.VideoCopy || decision.VideoCodec != "h264" {
					break
				}
			}
			return core.Decision{}, false
		}
		if decision, ok := try(candidate.VideoCodecs); ok {
			if decision.Kind == core.DecisionTranscode {
				b.profileDiagnostic(ProfileFallbackTranscode)
			} else {
				b.profileDiagnostic(ProfileFallbackRemux)
			}
			return decision, nil
		}
		// A combined codec list may repeatedly select source copy even when
		// its required profile predicates reject that source. Try individually
		// declared alternatives, retaining all audio and container constraints.
		// The transcoder currently emits H.264 or HEVC. Trying these fixed
		// outputs bounds work independently of untrusted profile list length.
		for _, codec := range []string{"h264", "hevc"} {
			if !profileContains(candidate.VideoCodecs, codec) {
				continue
			}
			if len(req.Caps.VideoCodecs) > 0 && !req.Caps.SupportsVideo(codec) {
				continue
			}
			if decision, ok := try([]string{codec}); ok {
				if decision.Kind == core.DecisionTranscode {
					b.profileDiagnostic(ProfileFallbackTranscode)
				} else {
					b.profileDiagnostic(ProfileFallbackRemux)
				}
				return decision, nil
			}
		}
	}
	b.profileDiagnostic(ProfileNoDelivery)
	return core.Decision{}, core.ErrNotAvailable
}
