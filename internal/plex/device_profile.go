package plex

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type deviceMediaRule struct{ container, video, audio, subtitle, protocol, context string }
type deviceLimit struct {
	scope, codec, field, comparison, value string
	list, encodeOnly                       bool
}
type devicePlaybackRule struct {
	selectAudio, selectedAudioOnly bool
	implicitAlternateAudio         bool
	refuseConversion               bool
	direct, output                 []deviceMediaRule
	limits                         []deviceLimit
}
type deviceProfileResult struct {
	reason string
	target *deviceMediaRule
	code   int
	text   string
}

func profileContains(list, value string) bool {
	for _, v := range strings.Split(list, ",") {
		if v == "*" || strings.EqualFold(v, value) {
			return true
		}
	}
	return false
}

func (rule devicePlaybackRule) directReason(media model.Media, audio, subtitle int, explicitAudio bool) string {
	if len(media.Part) == 0 {
		return "App cannot direct play this item."
	}
	part := media.Part[0]
	var video *model.Stream
	var audios, subs []model.Stream
	for _, s := range part.Stream {
		switch s.StreamType {
		case 1:
			if video == nil {
				v := s
				video = &v
			}
		case 2:
			audios = append(audios, s)
		case 3:
			subs = append(subs, s)
		}
	}
	if video == nil {
		return "App cannot direct play this item."
	}
	slices.SortFunc(audios, func(a, b model.Stream) int {
		if a.Index == nil || b.Index == nil {
			return 0
		}
		return *a.Index - *b.Index
	})
	ai := 0
	for i, s := range audios {
		if s.Index != nil && *s.Index == audio {
			ai = i
		}
	}
	noAudioLanguage := true
	for _, a := range audios {
		if a.LanguageCode != "" || a.Language != "" {
			noAudioLanguage = false
		}
	}
	if !explicitAudio && noAudioLanguage && rule.selectedAudioOnly {
		for i, a := range audios {
			if a.Channels > audios[ai].Channels {
				ai = i
			}
		}
	}
	var selectedAudio *model.Stream
	if len(audios) > 0 {
		selectedAudio = &audios[ai]
	}
	si := -1
	for i, s := range subs {
		if s.Index != nil && *s.Index == subtitle || s.Index == nil && s.Selected != nil && *s.Selected {
			si = i
		}
	}
	if !rule.selectAudio && len(audios) > 1 && ai > 0 {
		return "App cannot direct play this item. Selected audio stream is not the first audio stream and app cannot select audio streams in direct play."
	}

	if !rule.selectAudio && rule.implicitAlternateAudio && len(audios) > 1 && !explicitAudio {
		noLanguage := true
		for _, a := range audios {
			if a.LanguageCode != "" || a.Language != "" {
				noLanguage = false
			}
		}
		if noLanguage {
			return "App cannot direct play this item. Selected audio stream is not the first audio stream and app cannot select audio streams in direct play."
		}
	}
	var matches []deviceMediaRule
	for _, r := range rule.direct {
		if (r.protocol == "" || r.protocol == "http") && profileContains(r.container, media.Container) && profileContains(r.video, video.Codec) {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("App cannot direct play this item. No direct play video profile exists for protocol http, with container %s, and video codec %s.", media.Container, video.Codec)
	}
	hasSubtitleRenderer := false
	for _, r := range append(append([]deviceMediaRule(nil), rule.direct...), rule.output...) {
		if r.context == "subtitle" {
			hasSubtitleRenderer = true
		}
	}
	if si >= 0 && subs[si].Index == nil && !hasSubtitleRenderer {
		selectedCompatible := rule.selectedAudioOnly
		if selectedAudio != nil {
			for _, r := range matches {
				selectedCompatible = selectedCompatible || profileContains(r.audio, selectedAudio.Codec)
			}
		}
		if selectedCompatible {
			return "App cannot direct play this item. Selected sidecar subtitle stream cannot be direct played."
		}
	}
	var audioMatches []deviceMediaRule
	for _, r := range matches {
		compatible := true
		for _, a := range audios {
			if rule.selectedAudioOnly && selectedAudio != nil && a.ID != selectedAudio.ID {
				continue
			}
			if !profileContains(r.audio, a.Codec) {
				compatible = false
				break
			}
		}
		if compatible {
			audioMatches = append(audioMatches, r)
		}
	}
	if len(audioMatches) == 0 {
		codec := "none"
		for _, a := range audios {
			if rule.selectedAudioOnly && selectedAudio != nil && a.ID != selectedAudio.ID {
				continue
			}
			found := false
			for _, r := range matches {
				found = found || profileContains(r.audio, a.Codec)
			}
			if !found {
				codec = a.Codec
				break
			}
		}
		return fmt.Sprintf("App cannot direct play this item. No direct play video profile exists for protocol http, with container %s, video codec %s, and audio codec %s.", media.Container, video.Codec, codec)
	}
	subtitleCodec := "none"
	if si >= 0 {
		subtitleCodec = subs[si].Codec
	}
	subtitleAllowed := hasSubtitleRenderer && si >= 0 && subs[si].Index == nil
	for _, r := range rule.output {
		if r.context == "subtitle" && si >= 0 && (transcode.IsTextSubtitle(subs[si].Codec) || subs[si].Codec == "none") {
			subtitleAllowed = true
		}
	}
	for _, r := range rule.direct {
		if r.context == "subtitle" && si >= 0 && transcode.IsTextSubtitle(subs[si].Codec) {
			subtitleAllowed = true
		}
	}
	for _, r := range audioMatches {
		if si < 0 || profileContains(r.subtitle, subtitleCodec) {
			subtitleAllowed = true
			break
		}
	}
	if !subtitleAllowed {
		audioCodec := "none"
		if selectedAudio != nil {
			audioCodec = selectedAudio.Codec
		}
		return fmt.Sprintf("App cannot direct play this item. No direct play video profile exists for protocol http, with container %s, using video codec %s, audio codec %s, and subtitle codec %s.", media.Container, video.Codec, audioCodec, subtitleCodec)
	}
	if si > 0 && !rule.selectAudio {
		return "App cannot direct play this item. Selected embedded subtitle stream is not the first embedded subtitle stream."
	}
	// Container constraints precede named-codec constraints, then wildcards.
	for _, scope := range []string{"part", "video", "audio"} {
		for _, wildcard := range []bool{false, true} {
			for _, l := range rule.limits {
				if l.encodeOnly || l.scope != scope || (l.codec == "*") != wildcard {
					continue
				}
				codec := media.Container
				if scope == "video" {
					codec = video.Codec
				}
				if scope == "audio" {
					if selectedAudio == nil {
						continue
					}
					codec = selectedAudio.Codec
				}
				if !profileContains(l.codec, codec) {
					continue
				}
				value := deviceLimitValue(l.field, part, *video, selectedAudio)
				if value == "" {
					continue
				}
				failed := false
				op := ""
				switch l.comparison {
				case "UpperBound", "LowerBound":
					a, _ := strconv.ParseFloat(value, 64)
					b, _ := strconv.ParseFloat(l.value, 64)
					if l.comparison == "UpperBound" {
						failed = a > b
						op = ">"
					} else {
						failed = a < b
						op = "<"
					}
				case "Match":
					failed = !slices.Contains(strings.Split(l.value, "|"), value)
					op = "!="
				case "NotMatch":
					failed = slices.Contains(strings.Split(l.value, "|"), value)
					op = "=="
				}
				if failed {
					suffix := ""
					if l.list {
						suffix = " [list]"
					}
					return fmt.Sprintf("This app cannot play this item. The reason is: %s limitation applies: %s %s %s%s.", l.field, value, op, l.value, suffix)
				}
			}
		}
	}
	return ""
}

func deviceLimitValue(field string, p model.Part, v model.Stream, a *model.Stream) string {
	number := func(n int) string { return strconv.Itoa(n) }
	switch field {
	case "video.DOVIProfile":
		return number(v.DOVIProfile)
	case "video.width":
		return number(v.Width)
	case "video.height":
		return number(v.Height)
	case "video.bitDepth":
		return number(v.BitDepth)
	case "video.level":
		return number(v.Level)
	case "video.bitrate":
		return number(v.Bitrate)
	case "video.frameRate":
		return strconv.FormatFloat(v.FrameRate, 'f', -1, 64)
	case "video.profile":
		return v.Profile
	case "video.scanType":
		return v.ScanType
	case "video.frameRateMode":
		return "cfr"
	case "video.refFrames":
		return number(v.RefFrames)
	case "part.optimizedForStreaming":
		if p.OptimizedForStreaming != nil && *p.OptimizedForStreaming {
			return "1"
		}
	case "part.has64bitOffsets":
		if p.Has64bitOffsets != nil && *p.Has64bitOffsets {
			return "1"
		}
	case "video.anamorphic":
		if v.Anamorphic != nil && *v.Anamorphic {
			return "1"
		}
	case "video.hasScalingMatrix":
		if v.HasScalingMatrix != nil && *v.HasScalingMatrix {
			return "1"
		}
	}
	if a != nil {
		switch field {
		case "audio.channels":
			return number(a.Channels)
		case "audio.bitrate":
			return number(a.Bitrate)
		case "audio.profile":
			return a.Profile
		case "audio.samplingRate":
			return number(a.SamplingRate)
		}
	}
	return "0"
}

func deviceProfile(pr *Request) (devicePlaybackRule, bool) {
	name := resolvedPlayerProfile(pr)
	rule, ok := devicePlaybackRules[name]
	return rule, ok
}

// profileTarget selects a streaming target in declaration order. Static download
// targets cannot authorize a streaming conversion.
func profileTarget(rule devicePlaybackRule, pr *Request, protocol string) *deviceMediaRule {
	targets := append([]deviceMediaRule(nil), rule.output...)
	for _, d := range ParseProfileExtra(pr.ProfileExtra) {
		if d.Verb != "add-transcode-target" || d.Args["type"] != "videoProfile" {
			continue
		}
		a := d.Args
		targets = append(targets, deviceMediaRule{container: a["container"], video: a["videoCodec"], audio: a["audioCodec"], subtitle: a["subtitleCodec"], protocol: a["protocol"], context: a["context"]})
	}
	for _, r := range targets {
		if (r.context == "" || r.context == "streaming" || r.context == "all") && r.protocol == protocol {
			t := r
			return &t
		}
	}
	return nil
}

func dv5NeedsUnsupportedConversion(info core.MediaInfo, target *deviceMediaRule) bool {
	v := info.Video()
	return v != nil && v.DolbyVisionProfile == 5 && (target == nil || !profileContains(target.video, plexCodec(v.Codec)))
}

// profileDecisionError retains the per-family decision code when a profile has
// no streaming target, or cannot preserve the source's color representation.
type profileDecisionError struct {
	direct  string
	code    int
	message string
}

func (e *profileDecisionError) Error() string { return e.message }

func (s *Server) applyDeviceProfile(pr *Request, t *core.Title, ps *core.PlaySession, p *playRequest) error {
	rule, ok := deviceProfile(pr)
	if !ok {
		return nil
	}
	if p.mde {
		if p.opts.DirectPlay {
			return nil
		}
		target := profileTarget(rule, pr, p.protocol)
		if target != nil && dv5NeedsUnsupportedConversion(ps.Info, target) {
			return &profileDecisionError{direct: textDirectPlayDisable, code: 2003, message: "File is unplayable. DoVi (Profile 5) color space is not supported."}
		}
		if resolvedPlayerProfile(pr) != "Generic" {
			return nil
		}
	}
	rule = extendDeviceProfile(rule, pr.ProfileExtra)
	source := s.decisionMedia(t, ps, core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: ps.Info.Container}, *p, pr.User)
	sel := selection{audio: -1, subtitle: -1, subtitleSet: true}
	for i, st := range ps.Info.Streams {
		if st.Type == "audio" && st.Index == p.opts.AudioStream {
			sel.audio = i
		}
		if st.Type == "subtitle" && st.Index == p.opts.SubtitleStream {
			sel.subtitle = i
		}
	}
	if len(source.Part) > 0 {
		source.Part[0].Stream = s.buildStreams(t.Key, ps.Info, sel, pr.User)
	}
	explicit := false
	if pr.User != nil {
		explicit = s.selections.get(pr.User.ID, int64(p.key)).audioSet
	}
	reason := rule.directReason(source, p.opts.AudioStream, p.opts.SubtitleStream, explicit)
	if !p.opts.DirectPlay {
		reason = textDirectPlayDisable
	}
	target := profileTarget(rule, pr, p.protocol)
	p.profileResult = &deviceProfileResult{reason: reason, target: target}
	if reason == "" {
		// A file accepted by this concrete profile is not constrained by the
		// fallback registry for requests that did not carry a profile.
		p.caps.Containers = addUnique(p.caps.Containers, ps.Info.Container)
		for _, st := range ps.Info.Streams {
			if st.Type == "video" {
				p.caps.VideoCodecs = addUnique(p.caps.VideoCodecs, st.Codec)
			}
			if st.Type == "audio" {
				p.caps.AudioCodecs = addUnique(p.caps.AudioCodecs, st.Codec)
			}
		}
		return nil
	}
	p.opts.DirectPlay = false
	if rule.refuseConversion {
		return &profileDecisionError{direct: reason, code: 0, message: reason}
	}
	if target == nil {
		return &profileDecisionError{direct: reason, code: 4005, message: "Cannot convert this item. No conversion profile found for protocol " + p.protocol + "."}
	}
	if dv5NeedsUnsupportedConversion(ps.Info, target) {
		return &profileDecisionError{direct: reason, code: 2003, message: "File is unplayable. DoVi (Profile 5) color space is not supported."}
	}
	// Output order is significant; the first declared codec is the preferred
	// target, independently of direct-play codec compatibility.
	if target.video != "" {
		p.caps.VideoCodecs = splitList(target.video)
		if profileContains(target.video, "*") {
			if v := ps.Info.Video(); v != nil {
				p.caps.VideoCodecs = addUnique(p.caps.VideoCodecs, v.Codec)
			}
		}
		p.caps.TranscodeVideoCodecs = append([]string(nil), p.caps.VideoCodecs...)
	}
	if target.audio != "" {
		p.caps.TranscodeAudioCodecs = splitList(target.audio)
	}
	applyDeviceTargetLimits(rule, target, ps.Info, p)
	return nil
}

func profileErrorContainer(pr *Request, p playRequest, e *profileDecisionError) *model.MediaContainer {
	mc := libraryContainer()
	mc.Extra = []model.Attr{model.A("directPlayDecisionCode", 3000), model.A("directPlayDecisionText", e.direct), model.A("generalDecisionCode", 2000), model.A("generalDecisionText", "Neither direct play nor conversion is available."), model.A("transcodeDecisionCode", e.code), model.A("transcodeDecisionText", e.message), model.A("resourceSession", cmp.Or(pr.SessionID, pr.ClientID))}
	mc.SetDecision()
	return mc
}

// Legacy decoder declarations may provide a playable default track even when
// the source's default codec needs conversion. Explicit viewer picks take
// precedence over this automatic compatibility choice.
func (s *Server) selectLegacyDecoderAudio(pr *Request, p *playRequest, info core.MediaInfo) {
	if pr == nil || pr.ClientCapabilities == "" || p.audioStream != nil {
		return
	}
	if pr.User != nil && s.selections.get(pr.User.ID, int64(p.key)).audioSet {
		return
	}
	var decoders []string
	for _, part := range strings.Split(pr.ClientCapabilities, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(key) != "audioDecoders" {
			continue
		}
		for _, d := range splitDecoders(value) {
			decoders = addUnique(decoders, normalizeCodec(d.name))
		}
	}
	if len(decoders) == 0 {
		return
	}
	for _, st := range info.Streams {
		if st.Type == "audio" && st.Index == p.opts.AudioStream && slices.Contains(decoders, normalizeCodec(st.Codec)) {
			return
		}
	}
	var compatible core.MediaInfo
	for _, st := range info.Streams {
		if st.Type == "audio" && slices.Contains(decoders, normalizeCodec(st.Codec)) {
			compatible.Streams = append(compatible.Streams, st)
		}
	}
	chosen := pickAudio(compatible, s.audioLanguages(pr.User))
	if chosen < 0 {
		return
	}
	index := compatible.Streams[chosen].Index
	for i, st := range info.Streams {
		if st.Type == "audio" && st.Index == index {
			p.audioSel = i
			p.opts.AudioStream = index
			return
		}
	}
}

func applyDeviceTargetLimits(rule devicePlaybackRule, target *deviceMediaRule, info core.MediaInfo, p *playRequest) {
	for _, l := range rule.limits {
		n, err := strconv.Atoi(l.value)
		if err != nil || n <= 0 || l.comparison != "UpperBound" {
			continue
		}
		if l.scope == "audio" {
			for _, codec := range strings.Split(l.codec, ",") {
				if codec != "*" && !profileContains(target.audio, codec) {
					continue
				}
				p.caps.AudioLimits = append(p.caps.AudioLimits, core.AudioLimit{Codec: normalizeCodec(codec), Name: l.field, Type: "upperBound", Value: n})
			}
			continue
		}
		if l.scope != "video" {
			continue
		}
		matches := l.codec == "*"
		for _, codec := range strings.Split(l.codec, ",") {
			matches = matches || profileContains(target.video, codec)
		}
		if !matches {
			continue
		}
		height := 0
		switch l.field {
		case "video.height":
			height = n
		case "video.width":
			if v := info.Video(); v != nil && v.Width > 0 {
				height = v.Height * n / v.Width
			}
		case "video.bitrate":
			if p.caps.MaxBitrateKbps == 0 || n < p.caps.MaxBitrateKbps {
				p.caps.MaxBitrateKbps = n
				p.opts.MaxBitrateKbps = n
			}
		}
		if height > 0 && (p.caps.MaxHeight == 0 || height < p.caps.MaxHeight) {
			p.caps.MaxHeight = height
			p.caps.ExplicitMaxHeight = true
		}
	}
}

func extendDeviceProfile(rule devicePlaybackRule, extra string) devicePlaybackRule {
	// Clone the slices before adding request-local declarations to shared rules.
	rule.direct = append([]deviceMediaRule(nil), rule.direct...)
	rule.limits = append([]deviceLimit(nil), rule.limits...)
	for _, d := range ParseProfileExtra(extra) {
		a := d.Args
		switch d.Verb {
		case "add-direct-play-profile":
			if a["type"] != "" && a["type"] != "videoProfile" {
				continue
			}
			rule.direct = append(rule.direct, deviceMediaRule{container: a["container"], video: a["videoCodec"], audio: a["audioCodec"], subtitle: a["subtitleCodec"], protocol: a["protocol"]})
		case "add-limitation":
			scope := map[string]string{"videoCodec": "video", "audioCodec": "audio", "videoAudioCodec": "audio", "videoContainer": "part"}[a["scope"]]
			if scope == "" {
				continue
			}
			comparison := map[string]string{"upperbound": "UpperBound", "lowerbound": "LowerBound", "match": "Match", "notmatch": "NotMatch"}[strings.ToLower(a["type"])]
			_, list := a["list"]
			rule.limits = append(rule.limits, deviceLimit{scope: scope, codec: a["scopeName"], field: a["name"], comparison: comparison, value: limitationValue(a), list: list})
		}
	}
	return rule
}
