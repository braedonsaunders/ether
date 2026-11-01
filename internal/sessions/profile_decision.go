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

func profileEqualsAny(value, want string) bool {
	for _, v := range strings.Split(want, "|") {
		if strings.EqualFold(value, strings.TrimSpace(v)) {
			return true
		}
	}
	return false
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

// profileVideoRangeValue is the Jellyfin VideoRangeType a codec condition
// compares. Eight-bit untagged 4:2:0 is SDR once Dolby Vision is known to be
// absent, the same evidence as frontend.KnownSDR. A picture whose range cannot
// be classified stays empty so a required condition fails closed and playback
// can fall back to an encode. Equals and NotEquals against the single token
// DOVI still treat every Dolby Vision profile as DOVI, which is how clients
// reject profile 5 and every other profile together.
func profileVideoRangeValue(stream *core.StreamInfo, c core.ClientProfileCondition) string {
	if stream == nil || (stream.Type != "" && stream.Type != "video") {
		return ""
	}
	dv, known := dolbyVisionEvidence(stream)
	if dv {
		if dolbyVisionEquality(c) {
			return "DOVI"
		}
		return dolbyVisionRangeType(stream)
	}
	if !known {
		return ""
	}
	if value, ok := plainVideoRangeType(stream); ok {
		return value
	}
	if dolbyVisionEquality(c) {
		return "non-DOVI"
	}
	return ""
}

func dolbyVisionEquality(c core.ClientProfileCondition) bool {
	if !strings.EqualFold(strings.TrimSpace(c.Value), "DOVI") {
		return false
	}
	switch strings.ToLower(c.Operator) {
	case "equals", "notequals":
		return true
	default:
		return false
	}
}

func dolbyVisionEvidence(stream *core.StreamInfo) (present, known bool) {
	if stream.DolbyVision || (stream.DolbyVisionConfigurationPresent != nil && *stream.DolbyVisionConfigurationPresent) || dolbyVisionCodecTag(stream.CodecTag) {
		return true, true
	}
	if stream.DolbyVisionConfigurationPresent != nil && !*stream.DolbyVisionConfigurationPresent {
		return false, true
	}
	// AVC has no Dolby Vision configuration, so a missing container probe is
	// still enough to classify the picture.
	switch strings.ToLower(stream.Codec) {
	case "h264", "avc":
		return false, true
	default:
		return false, false
	}
}

func dolbyVisionCodecTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "dvh1", "dvhe", "dva1", "dvav", "dovi", "dav1":
		return true
	default:
		return false
	}
}

func dolbyVisionRangeType(stream *core.StreamInfo) string {
	profile := stream.DolbyVisionProfile
	if stream.ProbedDolbyVisionProfile != nil && *stream.ProbedDolbyVisionProfile > 0 {
		profile = *stream.ProbedDolbyVisionProfile
	}
	compat := stream.DolbyVisionCompatibilityID
	hdr10Plus := stream.HDR10Plus != nil && *stream.HDR10Plus
	switch profile {
	case 5:
		return "DOVI"
	case 7:
		if hdr10Plus {
			return "DOVIWithELHDR10Plus"
		}
		return "DOVIWithEL"
	case 8:
		switch compat {
		case 1:
			if hdr10Plus {
				return "DOVIWithHDR10Plus"
			}
			return "DOVIWithHDR10"
		case 2:
			return "DOVIWithSDR"
		case 4:
			return "DOVIWithHLG"
		default:
			return "DOVIInvalid"
		}
	case 10:
		switch compat {
		case 0:
			return "DOVI"
		case 1:
			if hdr10Plus {
				return "DOVIWithHDR10Plus"
			}
			return "DOVIWithHDR10"
		case 2:
			return "DOVIWithSDR"
		case 4:
			return "DOVIWithHLG"
		default:
			return "DOVIInvalid"
		}
	default:
		return "DOVI"
	}
}

func plainVideoRangeType(stream *core.StreamInfo) (string, bool) {
	if stream.HDR10Plus != nil && *stream.HDR10Plus {
		return "HDR10Plus", true
	}
	switch strings.ToLower(stream.ColorTransfer) {
	case "smpte2084":
		return "HDR10", true
	case "arib-std-b67":
		return "HLG", true
	}
	if frontend.KnownSDR(*stream) {
		return "SDR", true
	}
	return "", false
}

// stampEncodedVideoRange records the picture an encode or Dolby Vision strip
// actually delivers. An H.264 encode, and any tone-mapped encode, is 8-bit
// SDR. An HEVC encode that keeps HDR keeps the base-layer transfer and drops
// Dolby Vision metadata. Without this, a required VideoRangeType condition
// stays unknown on the output and a playable title is refused.
func stampEncodedVideoRange(s *core.StreamInfo, d core.Decision) {
	if d.VideoCopy {
		if !d.StripDolbyVision {
			return
		}
		no := false
		s.DolbyVision = false
		s.DolbyVisionConfigurationPresent = &no
		s.DolbyVisionProfile = 0
		s.DolbyVisionCompatibilityID = 0
		s.CodecTag = ""
		return
	}
	keepHDR := strings.EqualFold(d.VideoCodec, "hevc") && !d.ToneMap && encodedPictureHDR(s)
	transfer := "bt709"
	if keepHDR {
		if strings.EqualFold(s.ColorTransfer, "arib-std-b67") || s.DolbyVisionCompatibilityID == 4 {
			transfer = "arib-std-b67"
		} else {
			transfer = "smpte2084"
		}
	}
	no := false
	s.DolbyVision = false
	s.DolbyVisionConfigurationPresent = &no
	s.DolbyVisionProfile = 0
	s.DolbyVisionCompatibilityID = 0
	s.CodecTag = ""
	s.HDR10Plus = &no
	s.ColorTransfer = transfer
	if keepHDR {
		if s.BitDepth < 10 {
			s.BitDepth = 10
		}
		depth := s.BitDepth
		s.BitsPerRawSample = &depth
		return
	}
	s.BitDepth = 8
	depth := 8
	s.BitsPerRawSample = &depth
	s.PixelFormat = "yuv420p"
}

// applyProfileRangeCaps lets a device profile's VideoRangeType list drive the
// direct-play decision. Equals and EqualsAny name ranges the client can play.
// A list that only says SDR does not advertise HDR or Dolby Vision.
func applyProfileRangeCaps(caps core.ClientCaps) core.ClientCaps {
	if caps.Profiles == nil {
		return caps
	}
	for _, codec := range caps.Profiles.Codecs {
		for _, cond := range codec.Conditions {
			noteProfileRange(&caps, cond)
		}
		for _, cond := range codec.ApplyConditions {
			noteProfileRange(&caps, cond)
		}
	}
	return caps
}

func noteProfileRange(caps *core.ClientCaps, cond core.ClientProfileCondition) {
	if !strings.EqualFold(cond.Property, "VideoRangeType") {
		return
	}
	switch strings.ToLower(cond.Operator) {
	case "equals", "equalsany":
	default:
		return
	}
	for _, part := range strings.Split(cond.Value, "|") {
		switch strings.ToUpper(strings.TrimSpace(part)) {
		case "HDR10", "HLG", "HDR10PLUS":
			caps.HDR10 = true
		case "DOVI", "DOVIWITHSDR", "DOVIWITHEL":
			caps.DolbyVision = true
		case "DOVIWITHHDR10", "DOVIWITHHLG", "DOVIWITHHDR10PLUS", "DOVIWITHELHDR10PLUS":
			caps.HDR10 = true
			caps.DolbyVision = true
		}
	}
}

func encodedPictureHDR(s *core.StreamInfo) bool {
	dv := s.DolbyVision || (s.DolbyVisionConfigurationPresent != nil && *s.DolbyVisionConfigurationPresent) || dolbyVisionCodecTag(s.CodecTag)
	switch strings.ToLower(s.ColorTransfer) {
	case "smpte2084", "arib-std-b67":
		return true
	}
	if !dv {
		return false
	}
	if s.DolbyVisionProfile == 5 || (s.ProbedDolbyVisionProfile != nil && *s.ProbedDolbyVisionProfile == 5) {
		return false
	}
	if s.DolbyVisionCompatibilityID == 2 {
		return false
	}
	return s.DolbyVisionProfile == 7 || s.DolbyVisionProfile == 8 || s.DolbyVisionProfile == 10
}

// unknown reports an unsupported or unavailable required value separately
// from a false condition, which can make an ApplyConditions profile inapplicable.
func evaluateProfileConditions(cs []core.ClientProfileCondition, info core.MediaInfo, stream *core.StreamInfo) (bool, bool) {
	for _, c := range cs {
		value, zeroKnown := profileMeasurement(c, info, stream)
		// A missing measurement is Jellyfin's null: a required condition
		// fails closed and an optional one does not constrain playback.
		// A reported zero stays a real zero (reference frames, stream counts).
		if value == "" || (value == "0" && !zeroKnown) {
			if c.Required {
				return false, true
			}
			continue
		}
		// Jellyfin treats HDR10Plus as HDR10 when that comparison succeeds,
		// then still compares the HDR10Plus token itself.
		if strings.EqualFold(c.Property, "videorangetype") && strings.EqualFold(value, "HDR10Plus") {
			if ok, known := profileConditionHolds("HDR10", c); known && ok {
				continue
			}
		}
		ok, known := profileConditionHolds(value, c)
		if !known && c.Required {
			return false, true
		}
		if known && !ok {
			return false, false
		}
	}
	return true, false
}

func profileConditionHolds(value string, c core.ClientProfileCondition) (ok, known bool) {
	want := strings.TrimSpace(c.Value)
	switch strings.ToLower(c.Operator) {
	case "equals":
		return strings.EqualFold(value, want), true
	case "notequals":
		// Jellyfin Android TV rejects an unsupported range by NotEquals
		// against a pipe-separated list, and only applies that profile when
		// the picture is already in the list. A single token stays an exact
		// comparison, which is what NotEquals DOVI clients send.
		if strings.EqualFold(c.Property, "videorangetype") && strings.Contains(want, "|") {
			return !profileEqualsAny(value, want), true
		}
		return !strings.EqualFold(value, want), true
	case "equalsany":
		return profileEqualsAny(value, want), true
	case "lessthanequal", "greaterthanequal":
		a, e1 := strconv.ParseFloat(value, 64)
		b, e2 := strconv.ParseFloat(want, 64)
		if e1 != nil || e2 != nil {
			return false, false
		}
		if strings.EqualFold(c.Operator, "lessthanequal") {
			return a <= b, true
		}
		return a >= b, true
	default:
		return false, false
	}
}

// profileMeasurement reads one Jellyfin ProfileConditionValue.
// zeroKnown is set when "0" is a real measurement rather than "not reported".
// PacketLength, VideoTimestamp, Has64BitOffsets and VideoRotation stay empty:
// the probe does not carry them, so they follow Jellyfin's null rule.
func profileMeasurement(c core.ClientProfileCondition, info core.MediaInfo, stream *core.StreamInfo) (value string, zeroKnown bool) {
	switch strings.ToLower(strings.TrimSpace(c.Property)) {
	case "videorangetype":
		return profileVideoRangeValue(stream, c), false
	case "videowidth", "width":
		if stream != nil {
			return strconv.Itoa(stream.Width), false
		}
	case "videoheight", "height":
		if stream != nil {
			return strconv.Itoa(stream.Height), false
		}
	case "videoframerate", "framerate":
		if stream != nil && stream.FrameRate > 0 {
			return strconv.FormatFloat(stream.FrameRate, 'f', -1, 64), false
		}
	case "videobitdepth":
		if stream != nil {
			return strconv.Itoa(stream.BitDepth), false
		}
	case "videolevel":
		if stream != nil {
			return strconv.Itoa(stream.Level), false
		}
	case "videoprofile", "audioprofile":
		if stream != nil {
			return stream.Profile, false
		}
	case "audiochannels":
		if stream != nil {
			return strconv.Itoa(stream.Channels), false
		}
	case "audiosamplerate":
		if stream != nil {
			return strconv.Itoa(stream.SampleRate), false
		}
	case "audiobitdepth":
		if stream != nil && stream.Type == "audio" && stream.BitsPerRawSample != nil {
			return strconv.Itoa(*stream.BitsPerRawSample), false
		}
	case "videobitrate", "audiobitrate":
		if stream != nil {
			return strconv.Itoa(stream.BitrateKbps * 1000), false
		}
	case "containerbitrate":
		return strconv.Itoa(info.BitrateKbps * 1000), false
	case "videocodectag":
		if stream != nil {
			return stream.CodecTag, false
		}
	case "refframes":
		if stream != nil && stream.ReferenceFrames != nil {
			return strconv.Itoa(*stream.ReferenceFrames), true
		}
	case "isanamorphic":
		return anamorphicValue(stream)
	case "isinterlaced":
		return interlacedValue(stream)
	case "isavc":
		return avcValue(stream)
	case "issecondaryaudio":
		return secondaryAudioValue(info, stream)
	case "numaudiostreams":
		return strconv.Itoa(countStreams(info, "audio")), true
	case "numvideostreams":
		return strconv.Itoa(countStreams(info, "video")), true
	case "numstreams":
		return strconv.Itoa(len(info.Streams)), true
	case "videotimestamp", "packetlength", "has64bitoffsets", "videorotation":
		return "", false
	}
	return "", false
}

func countStreams(info core.MediaInfo, kind string) int {
	n := 0
	for _, s := range info.Streams {
		if s.Type == kind {
			n++
		}
	}
	return n
}

func anamorphicValue(stream *core.StreamInfo) (string, bool) {
	if stream == nil || stream.Type == "audio" || stream.Type == "subtitle" {
		return "", false
	}
	sar := strings.TrimSpace(stream.SampleAspectRatio)
	if sar == "" || strings.EqualFold(sar, "N/A") || sar == "0:1" {
		return "", false
	}
	sar = strings.ReplaceAll(sar, "/", ":")
	n, d, ok := strings.Cut(sar, ":")
	if !ok {
		return "", false
	}
	num, e1 := strconv.ParseFloat(n, 64)
	den, e2 := strconv.ParseFloat(d, 64)
	if e1 != nil || e2 != nil || den == 0 {
		return "", false
	}
	if num/den > 0.99 && num/den < 1.01 {
		return "false", false
	}
	return "true", false
}

func interlacedValue(stream *core.StreamInfo) (string, bool) {
	if stream == nil || (stream.Type != "" && stream.Type != "video") {
		return "", false
	}
	if stream.ProbedInterlaced != nil {
		return strconv.FormatBool(*stream.ProbedInterlaced), false
	}
	switch strings.ToLower(stream.FieldOrder) {
	case "progressive":
		return "false", false
	case "tt", "bb", "tb", "bt":
		return "true", false
	}
	if stream.Interlaced {
		return "true", false
	}
	return "", false
}

func avcValue(stream *core.StreamInfo) (string, bool) {
	if stream == nil || (stream.Type != "" && stream.Type != "video") {
		return "", false
	}
	switch strings.ToLower(stream.CodecTag) {
	case "avc1", "avc3":
		return "true", false
	}
	if stream.NALLengthSize != nil {
		return strconv.FormatBool(*stream.NALLengthSize > 0), false
	}
	return "", false
}

func secondaryAudioValue(info core.MediaInfo, stream *core.StreamInfo) (string, bool) {
	if stream == nil || stream.Type != "audio" {
		return "", false
	}
	first := -1
	for _, s := range info.Streams {
		if s.Type == "audio" {
			first = s.Index
			break
		}
	}
	if first < 0 {
		return "", false
	}
	return strconv.FormatBool(stream.Index != first), false
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

func encodedAudioChannels(d core.Decision, info core.MediaInfo) int {
	if d.AudioChannels > 0 {
		return d.AudioChannels
	}
	ch := 2
	if source := profileStream(info, "audio", d.AudioStream); source != nil && source.Channels > 0 {
		ch = source.Channels
	}
	switch strings.ToLower(strings.TrimSpace(d.AudioCodec)) {
	case "ac3", "eac3":
		return min(ch, 6)
	default:
		return min(ch, 2)
	}
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
				}
				s.BitrateKbps = 0
				stampEncodedVideoRange(s, d)
			} else if d.StripDolbyVision {
				stampEncodedVideoRange(s, d)
			}
		} else if s.Type == "audio" && s.Index == d.AudioStream {
			s.Codec = d.AudioCodec
			if !d.AudioCopy {
				s.Channels = encodedAudioChannels(d, info)
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
			if channels == 0 {
				if d.AudioCopy {
					if source := profileStream(info, "audio", d.AudioStream); source != nil {
						channels = source.Channels
					}
				} else {
					channels = encodedAudioChannels(d, info)
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
	req.Caps = applyProfileRangeCaps(req.Caps)
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
