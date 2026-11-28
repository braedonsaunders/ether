package plex

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// baseCaps is what a product/platform can play when the client sends no
// profile. The values come from the capability registry. Profile-extra
// directives refine them afterwards.
type baseCaps struct {
	id                string
	video             []string
	audio             []string
	containers        []string
	maxHeight         int
	hdr10             bool
	dv                bool
	dvOff             bool // owner override turned Dolby Vision off
	alwaysEncodeVideo bool // owner override disabled video copy on streams
	heightLocked      bool
}

// ClientCaps derives frontend.ClientCaps for the request. remoteMaxKbps caps
// the bitrate of remote clients (0 = unlimited).
func (pr *Request) ClientCaps(remoteMaxKbps int) frontend.ClientCaps {
	return pr.ClientCapsWith(remoteMaxKbps, nil)
}

// ClientCapsWith applies registry defaults, runtime declarations, then any
// explicitly forced owner facts. Remote server limits are applied last.
func (pr *Request) ClientCapsWith(remoteMaxKbps int, overrides []frontend.ClientCapabilityOverride) frontend.ClientCaps {
	if pr == nil {
		return frontend.ClientCaps{}
	}
	// Plex Web's browser split, desktop mpv clients, Plezy's platform split
	// and the Fire TV 4K Max model patch all live in the registry. An owner
	// override sits under the headers this request actually sent.
	b := playbackCaps(pr, overrides)
	c := frontend.ClientCaps{
		Protocol:                   "plex",
		PreserveEmptyVideoCodecs:   true,
		AwaitAdmissionIndex:        true,
		DeferSourceCodecValidation: true,
		Product:                    pr.Product,
		Platform:                   pr.Platform,
		PlatformVersion:            pr.PlatformVersion,
		Device:                     pr.Device,
		DeviceName:                 pr.DeviceName,
		Model:                      pr.Model,
		ClientID:                   pr.ClientID,
		Version:                    pr.Version,
		MaxHeight:                  b.maxHeight,
		MaxHeightSource:            baseMaxHeightSource(pr, b),
		HDR10:                      b.hdr10,
		DolbyVision:                b.dv,
		VideoCodecs:                slices.Clone(b.video),
		AudioCodecs:                slices.Clone(b.audio),
		Containers:                 slices.Clone(b.containers),
		Remote:                     pr.Remote,
	}
	if pr.RemoteIP.IsValid() {
		c.ClientIP = pr.RemoteIP.String()
	}
	if b.heightLocked {
		c.ExplicitMaxHeight = true
	}
	if b.dvOff {
		c.DolbyVisionBaseOnly = true
	}
	if b.alwaysEncodeVideo {
		c.AlwaysEncodeVideo = true
	}
	dv, subs, dvChanged, subsChanged := frontend.PolicyStamp(pr.Product, pr.Platform, pr.Device, pr.DeviceName, pr.Model, pr.Version, overrides)
	if dvChanged {
		c.DVProfiles = dv
	}
	if subsChanged {
		c.SubtitleCodecs = subs
	}
	if pr.ClientCapabilities != "" {
		applyLegacyCapabilities(&c, pr.ClientCapabilities)
	}
	if pr.ProfileExtra != "" {
		ev := applyProfileExtra(&c, pr.ProfileExtra)
		promoteWebHDRFromProfile(&c, pr.Product, ev)
	}
	c = frontend.ApplyForcedOverrides(c, pr.Model, overrides)
	if pr.ClientCapabilities == "" && pr.ProfileExtra == "" {
		c.ForcedOverrideApplied = false
	}
	if c.Remote && remoteMaxKbps > 0 && (c.MaxBitrateKbps == 0 || c.MaxBitrateKbps > remoteMaxKbps) {
		c.DefaultRemoteBitrate = true
		c.MaxBitrateKbps = remoteMaxKbps
	}
	return c
}

func playbackCaps(pr *Request, overrides []frontend.ClientCapabilityOverride) baseCaps {
	got := frontend.PlaybackDefaultsFor(pr.Product, pr.Platform, pr.Device, pr.DeviceName, pr.Model, pr.Version, overrides)
	return baseCaps{id: got.ID, video: got.Video, audio: got.Audio, containers: got.Containers, maxHeight: got.MaxHeight, hdr10: got.HDR10, dv: got.DolbyVision, dvOff: got.DolbyVisionOff, alwaysEncodeVideo: got.AlwaysEncodeVideo, heightLocked: got.HeightLocked}
}

func baseMaxHeightSource(pr *Request, b baseCaps) string {
	if b.maxHeight <= 0 {
		return ""
	}
	if b.heightLocked {
		return "App compatibility override"
	}
	if b.id == "generic" {
		if pr != nil && pr.Product == "Native player" {
			if plat := strings.TrimSpace(pr.Platform); plat != "" {
				return "generic fallback profile (no client headers on /library/parts request; user-agent " + strconv.Quote(plat) + ")"
			}
			return "generic fallback profile (no client headers on /library/parts request)"
		}
		if pr != nil {
			if p := strings.TrimSpace(pr.Product); p != "" {
				return "generic registry fallback for unknown client " + strconv.Quote(p)
			}
		}
		return "generic registry fallback (no client identity reported)"
	}
	if b.id != "" {
		return "registry preset " + strconv.Quote(b.id)
	}
	return ""
}

func formatLimitationSource(scope, scopeName, name, val string) string {
	sn := strings.TrimSpace(scopeName)
	if sn != "" && sn != "*" {
		return "X-Plex-Client-Profile-Extra add-limitation(scope=" + scope + ", scopeName=" + sn + ", " + name + "=" + val + ")"
	}
	return "X-Plex-Client-Profile-Extra add-limitation(scope=" + scope + ", " + name + "=" + val + ")"
}

// declaredCaps is the registry plus this request's headers, before a quality
// rule changes the ceiling.
func (s *Server) declaredCaps(pr *Request) frontend.ClientCaps {
	if pr == nil {
		return frontend.ClientCaps{}
	}
	return pr.ClientCapsWith(0, s.cfg().Clients.Overrides)
}

// profileExtraEvidence records affirmative codec/HDR claims and HDR denials
// from X-Plex-Client-Profile-Extra. Omission is not support: listing hevc
// alone never implies HDR10 (Braedon review of #136).
type profileExtraEvidence struct {
	affirmativeHEVC        bool
	affirmativeHDR10       bool // bitDepth>=10 and/or colorTrc allow-list includes smpte2084/arib-std-b67
	affirmativeBitDepth10  bool
	affirmativeColorTrcHDR bool
	hdrDenied              bool
}

// promoteWebHDRFromProfile enables HDR10 when profile-extra affirmatively
// declares HEVC and HDR capability without an effective HDR denial.
// Plex Web promotes on HEVC + (bitDepth>=10 or PQ/HLG colorTrc). Non-web
// clients keep registry defaults unless they explicitly declare HEVC +
// bitDepth>=10 + PQ/HLG colorTrc (Bedrock Generic profiles).
func promoteWebHDRFromProfile(c *frontend.ClientCaps, product string, ev profileExtraEvidence) {
	if strings.Contains(strings.ToLower(product), "plex web") {
		if ev.affirmativeHEVC {
			c.VideoCodecs = addUnique(c.VideoCodecs, "hevc")
		}
		if ev.affirmativeHEVC && ev.affirmativeHDR10 && !ev.hdrDenied {
			c.HDR10 = true
		}
		return
	}
	if ev.affirmativeHEVC && ev.affirmativeBitDepth10 && ev.affirmativeColorTrcHDR && !ev.hdrDenied {
		c.HDR10 = true
	}
}

// limitationDeniesHDR reports profile-extra limits that mean this client
// cannot take HEVC HDR10 (8-bit cap or explicit PQ rejection).
func limitationDeniesHDR(a map[string]string) bool {
	sn := normalizeCodec(a["scopeName"])
	if sn != "hevc" && sn != "av1" && sn != "*" && sn != "" {
		return false
	}
	name, typ := a["name"], a["type"]
	val := limitationValue(a)
	switch a["scope"] {
	case "videoCodec":
		if name == "video.bitDepth" && typ == "upperBound" {
			n, err := strconv.Atoi(val)
			return err == nil && n < 10
		}
		if name == "video.colorTrc" && typ == "notMatch" {
			return strings.Contains(val, "smpte2084")
		}
	}
	return false
}

// limitationScopesHDRCodec and the helpers below read profile-extra limits
// that affirmatively claim 10-bit and/or PQ (HDR10) support, the signals PMS
// Firefox sent when it Direct Streamed an HDR10 HEVC remux.
func limitationScopesHDRCodec(scopeName string) bool {
	sn := normalizeCodec(scopeName)
	if sn == "hevc" || sn == "av1" || sn == "*" || sn == "" {
		return true
	}
	for _, part := range splitList(scopeName) {
		if part == "hevc" || part == "av1" {
			return true
		}
	}
	return false
}

func limitationAffirmsBitDepth10(a map[string]string) bool {
	if !limitationScopesHDRCodec(a["scopeName"]) {
		return false
	}
	scope := a["scope"]
	if a["name"] == "video.bitDepth" && a["type"] == "upperBound" && (scope == "videoCodec" || scope == "videoTranscodeTarget") {
		n, err := strconv.Atoi(limitationValue(a))
		return err == nil && n >= 10
	}
	return false
}

func limitationAffirmsColorTrcHDR(a map[string]string) bool {
	if !limitationScopesHDRCodec(a["scopeName"]) {
		return false
	}
	scope := a["scope"]
	if a["name"] == "video.colorTrc" && a["type"] == "match" && (scope == "videoCodec" || scope == "videoTranscodeTarget") {
		val := limitationValue(a)
		return strings.Contains(val, "smpte2084") || strings.Contains(val, "arib-std-b67")
	}
	return false
}

// limitationValue prefers list= (PMS colorTrc allow-lists) then value=.
func limitationValue(a map[string]string) string {
	if v := a["list"]; v != "" {
		return v
	}
	return a["value"]
}

// clientCaps derives frontend.ClientCaps for the request and applies any
// matching client rule override from the server's quality configuration.
// The override stays behind Host.Capabilities so this package does not import the resolver.
func (s *Server) clientCaps(pr *Request) frontend.ClientCaps {
	return s.clientCapsForKey(pr, 0)
}

func (s *Server) stampViewerTranscoding(u *frontend.User, caps *frontend.ClientCaps) {
	if s == nil || u == nil || caps == nil {
		return
	}
	if policy, ok := s.policy().(frontend.VideoTranscodingPolicy); ok && policy.VideoTranscodingDisabled(u) {
		caps.NoVideoTranscode = true
	}
}

func (s *Server) clientCapsForKey(pr *Request, key frontend.RatingKey) frontend.ClientCaps {
	cfg := s.cfg()
	caps := s.declaredCaps(pr)
	h := s.getHost()
	if h.Capabilities != nil {
		media := frontend.MediaAny
		switch key.Kind() {
		case frontend.KindMovie:
			media = frontend.MediaMovie
		case frontend.KindEpisode:
			media = frontend.MediaTV
		}
		caps = h.Capabilities.ApplyClientRules(cfg.Quality, caps, pr.User, media)
	}
	s.stampViewerTranscoding(pr.User, &caps)
	return caps
}

// Directive is one parsed X-Plex-Client-Profile-Extra directive, e.g.
// add-limitation(scope=videoCodec&scopeName=h264&...).
type Directive struct {
	Verb string
	Args map[string]string
}

// ParseProfileExtra splits a profile-extra string into directives. The value
// may arrive URL-encoded; directives are separated by "+".
func ParseProfileExtra(s string) []Directive {
	// Encoded form: "add-limitation%28scope%3D...%29%2B...". PathUnescape
	// keeps a literal "+" (the directive separator); when the value came
	// through a query string "+" has already become a space, which the verb
	// trimming below tolerates.
	if !strings.Contains(s, "(") {
		if dec, err := url.PathUnescape(s); err == nil {
			s = dec
		}
	}
	var out []Directive
	for len(s) > 0 {
		open := strings.IndexByte(s, '(')
		if open < 0 {
			break
		}
		close := strings.IndexByte(s[open:], ')')
		if close < 0 {
			break
		}
		close += open
		verb := strings.Trim(strings.TrimSpace(s[:open]), "+ ")
		args := map[string]string{}
		for _, kv := range strings.Split(s[open+1:close], "&") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if dec, err := url.QueryUnescape(v); err == nil {
				v = dec
			}
			args[strings.TrimSpace(k)] = v
		}
		if verb != "" {
			out = append(out, Directive{Verb: verb, Args: args})
		}
		s = s[close+1:]
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = normalizeCodec(strings.TrimSpace(p))
		if p != "" && p != "*" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeCodec maps Plex/legacy codec names to Ether's core names.
func normalizeCodec(c string) string {
	c = strings.ToLower(c)
	switch c {
	case "h265", "hev1", "hvc1", "x265":
		return "hevc"
	case "avc", "avc1", "x264":
		return "h264"
	case "dca":
		return "dts"
	case "dca-ma", "dts-hd", "dtshd_ma":
		return "dtshd"
	case "e-ac3", "ec3":
		return "eac3"
	case "ts", "mpegts", "m2ts":
		return "mpegts"
	case "matroska":
		return "mkv"
	case "m4v":
		return "mp4"
	}
	return c
}

func addUnique(xs []string, add ...string) []string {
	for _, a := range add {
		if !slices.Contains(xs, a) {
			xs = append(xs, a)
		}
	}
	return xs
}

// isTranscodeTargetLimitation reports whether an add-limitation directive
// constrains a segmented/transcode output target rather than direct-play
// decoding. Such declarations must never cap direct-play release selection.
func isTranscodeTargetLimitation(a map[string]string) bool {
	switch a["scope"] {
	case "videoTranscodeTarget", "transcodeTarget":
		return true
	}
	if proto := strings.TrimSpace(a["protocol"]); proto != "" && proto != "*" && !strings.EqualFold(proto, frontend.ProtocolHTTP) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(a["context"])) {
	case "transcode", "conversion":
		return true
	}
	return false
}

type heightBound struct {
	height int
	source string
}

func updateHeightBound(cur heightBound, h int, src string, replace bool) heightBound {
	if h <= 0 {
		return cur
	}
	if replace || cur.height == 0 || h < cur.height {
		return heightBound{height: h, source: src}
	}
	return cur
}

func effectiveAxisBound(h, w heightBound) heightBound {
	switch {
	case h.height > 0 && w.height > 0:
		if w.height < h.height {
			return w
		}
		return h
	case h.height > 0:
		return h
	default:
		return w
	}
}

func widthBoundHeight(w int) int {
	switch {
	case w <= 0:
		return 0
	case w <= 1280:
		return 720
	case w <= 1920:
		return 1080
	case w <= 3840:
		return 2160
	default:
		return w * 9 / 16
	}
}

func applyProfileExtra(c *frontend.ClientCaps, extra string) profileExtraEvidence {
	var ev profileExtraEvidence
	wildcardBitDepth := 0
	codecBitDepth := map[string]int{}
	var wildH, wildW heightBound
	codecH := map[string]heightBound{}
	codecW := map[string]heightBound{}
	levelSource := ""
	colorTrcDenied := false
	for _, d := range ParseProfileExtra(extra) {
		a := d.Args
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
			if t := a["type"]; t != "" && t != "videoProfile" {
				continue
			}
			codecs := splitList(a["videoCodec"])
			if slices.Contains(codecs, "hevc") {
				ev.affirmativeHEVC = true
			}
			c.VideoCodecs = addUnique(c.VideoCodecs, codecs...)
			c.AudioCodecs = addUnique(c.AudioCodecs, splitList(a["audioCodec"])...)
			// The enhanced tvOS player declares bitmap decoding for its
			// Matroska HLS target; the general Apple TV policy is text-only.
			if strings.EqualFold(c.Product, "Plex for Apple TV") && a["protocol"] == frontend.ProtocolHLS && slices.Contains(splitList(a["container"]), frontend.ContainerMatroska) {
				for _, codec := range splitList(a["subtitleCodec"]) {
					c.SubtitleCodecs = addUnique(c.SubtitleCodecs, codec)
					switch codec {
					case "pgs":
						c.SubtitleCodecs = addUnique(c.SubtitleCodecs, "hdmv_pgs_subtitle")
					case "vobsub":
						c.SubtitleCodecs = addUnique(c.SubtitleCodecs, "dvd_subtitle")
					}
				}
			}

			if ct := a["container"]; ct != "" {
				c.Containers = addUnique(c.Containers, splitList(ct)...)
				if a["protocol"] == frontend.ProtocolHLS {
					c.HLSContainers = addUnique(c.HLSContainers, splitList(ct)...)
				}
			}
		case "add-direct-play-profile":
			if t := a["type"]; t != "" && t != "videoProfile" {
				continue
			}
			codecs := splitList(a["videoCodec"])
			if slices.Contains(codecs, "hevc") {
				ev.affirmativeHEVC = true
			}
			c.VideoCodecs = addUnique(c.VideoCodecs, codecs...)
			c.AudioCodecs = addUnique(c.AudioCodecs, splitList(a["audioCodec"])...)
			c.Containers = addUnique(c.Containers, splitList(a["container"])...)
		case "add-limitation":
			if limitationAffirmsBitDepth10(a) {
				ev.affirmativeBitDepth10 = true
				ev.affirmativeHDR10 = true
			}
			if limitationAffirmsColorTrcHDR(a) {
				ev.affirmativeColorTrcHDR = true
				ev.affirmativeHDR10 = true
			}
			if isTranscodeTargetLimitation(a) {
				continue
			}
			if (strings.EqualFold(c.Product, "PM4K") || strings.EqualFold(c.Product, "Plex for Kodi")) && a["scope"] == "videoAudioCodec" {
				n, err := strconv.Atoi(limitationValue(a))
				if err == nil && n > 0 && ((a["name"] == "audio.channels" && n <= 32) || (a["name"] == "audio.bitrate" && n <= 100000) || (a["name"] == "audio.samplingRate" && n <= 192000)) && (a["type"] == "upperBound" || a["type"] == "lowerBound") && (a["name"] == "audio.channels" || a["name"] == "audio.bitrate" || a["name"] == "audio.samplingRate") {
					c.AudioLimits = append(c.AudioLimits, frontend.AudioLimit{Codec: normalizeCodec(a["scopeName"]), Name: a["name"], Type: a["type"], Value: n})
				}
			}
			if a["scope"] == "videoCodec" && a["type"] == "upperBound" {
				val := limitationValue(a)
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					repl := strings.EqualFold(a["replace"], "true")
					rawSN := strings.TrimSpace(a["scopeName"])
					switch a["name"] {
					case "video.height":
						src := formatLimitationSource(a["scope"], a["scopeName"], a["name"], val)
						if rawSN == "" || rawSN == "*" {
							wildH = updateHeightBound(wildH, n, src, repl)
						} else {
							for _, codec := range splitList(rawSN) {
								codecH[codec] = updateHeightBound(codecH[codec], n, src, repl)
							}
						}
					case "video.width":
						h := widthBoundHeight(n)
						src := formatLimitationSource(a["scope"], a["scopeName"], a["name"], val)
						if rawSN == "" || rawSN == "*" {
							wildW = updateHeightBound(wildW, h, src, repl)
						} else {
							for _, codec := range splitList(rawSN) {
								codecW[codec] = updateHeightBound(codecW[codec], h, src, repl)
							}
						}
					case "video.bitDepth":
						if rawSN == "" || rawSN == "*" {
							if repl || wildcardBitDepth == 0 || n < wildcardBitDepth {
								wildcardBitDepth = n
							}
						} else {
							for _, codec := range splitList(rawSN) {
								if codec == "hevc" || codec == "av1" {
									if repl || codecBitDepth[codec] == 0 || n < codecBitDepth[codec] {
										codecBitDepth[codec] = n
									}
								}
							}
						}
					}
				}
			}
			if a["scope"] == "videoCodec" && a["name"] == "video.colorTrc" && a["type"] == "notMatch" && limitationScopesHDRCodec(a["scopeName"]) && strings.Contains(limitationValue(a), "smpte2084") {
				colorTrcDenied = true
			}
			if src := applyLimitation(c, a); src != "" {
				levelSource = src
			}
		}
	}
	wild := effectiveAxisBound(wildH, wildW)
	codecBound := map[string]heightBound{}
	for codec, b := range codecH {
		codecBound[codec] = effectiveAxisBound(b, codecW[codec])
	}
	for codec, b := range codecW {
		if _, ok := codecBound[codec]; !ok {
			codecBound[codec] = b
		}
	}
	for codec, b := range codecBound {
		if b.height > 0 {
			if c.VideoMaxHeights == nil {
				c.VideoMaxHeights = map[string]int{}
			}
			if old := c.VideoMaxHeights[codec]; old == 0 || b.height < old {
				c.VideoMaxHeights[codec] = b.height
			}
		}
	}
	var chosen heightBound
	switch {
	case wild.height > 0:
		if len(c.VideoCodecs) == 0 {
			chosen = wild
		} else {
			for _, codec := range c.VideoCodecs {
				b := wild
				if cb, ok := codecBound[codec]; ok && cb.height > 0 {
					b = cb
				}
				if b.height > chosen.height {
					chosen = b
				}
			}
			if chosen.height > wild.height {
				for _, codec := range c.VideoCodecs {
					if _, ok := codecBound[codec]; !ok {
						if c.VideoMaxHeights == nil {
							c.VideoMaxHeights = map[string]int{}
						}
						if old := c.VideoMaxHeights[codec]; old == 0 || wild.height < old {
							c.VideoMaxHeights[codec] = wild.height
						}
					}
				}
			}
		}
	case len(codecBound) > 0:
		for _, b := range codecBound {
			if b.height > chosen.height {
				chosen = b
			}
		}
	}
	if chosen.height > 0 && (c.MaxHeight == 0 || chosen.height < c.MaxHeight) {
		c.MaxHeight = chosen.height
		c.ExplicitMaxHeight = true
		c.MaxHeightSource = chosen.source
	}
	if levelSource != "" {
		if ceiling := decoderCeiling(*c); ceiling > 0 && (c.MaxHeight == 0 || ceiling < c.MaxHeight) {
			c.MaxHeight = ceiling
			c.ExplicitMaxHeight = true
			c.MaxHeightSource = levelSource
		}
	}
	bitDepthDeniesHDR := false
	switch {
	case codecBitDepth["hevc"] > 0 || codecBitDepth["av1"] > 0:
		bitDepthDeniesHDR = codecBitDepth["hevc"] < 10 && codecBitDepth["av1"] < 10
	case wildcardBitDepth > 0:
		bitDepthDeniesHDR = wildcardBitDepth < 10
	}
	if bitDepthDeniesHDR || colorTrcDenied {
		ev.hdrDenied = true
		c.HDR10 = false
		c.ExplicitNoHDR = true
		if bitDepthDeniesHDR {
			c.DolbyVision = false
		}
	}
	return ev
}

// streamingVideoTarget keeps segmented video output separate from direct-play
// decoder support. A smart TV (Plex for LG, VIDAA) Direct Plays 4K HEVC files
// over HTTP while its HLS transcode target lists only H.264 (#408).
func streamingVideoTarget(extra, protocol string) []string {
	var codecs []string
	for _, d := range ParseProfileExtra(extra) {
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
		default:
			continue
		}
		a := d.Args
		if typ := a["type"]; typ != "" && typ != "videoProfile" {
			continue
		}
		if context := a["context"]; context != "" && context != "all" && context != "streaming" {
			continue
		}
		if target := a["protocol"]; target != "" && target != protocol {
			continue
		}
		codecs = addUnique(codecs, splitList(a["videoCodec"])...)
	}
	return codecs
}

// streamingAudioTarget keeps output capabilities separate from direct-play
// support. A TV may decode EAC3 files while its DASH player requests AAC.
// Targets for downloads or another streaming protocol cannot widen this one.
func streamingAudioTarget(extra, protocol string) []string {
	var codecs []string
	for _, d := range ParseProfileExtra(extra) {
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
		default:
			continue
		}
		a := d.Args
		if typ := a["type"]; typ != "" && typ != "videoProfile" {
			continue
		}
		if context := a["context"]; context != "" && context != "all" && context != "streaming" {
			continue
		}
		if target := a["protocol"]; target != "" && target != protocol {
			continue
		}
		codecs = addUnique(codecs, splitList(a["audioCodec"])...)
	}
	return codecs
}

// subtitleProfileTargetFormat returns the HTTP subtitle transcode format
// ("ass", "srt", or "vtt") declared in X-Plex-Client-Profile-Extra via
// type=subtitleProfile directives (e.g. Plex for LG's container=srt&subtitleCodec=srt),
// or "" when none is declared.
func subtitleProfileTargetFormat(extra string) string {
	target := ""
	for _, d := range ParseProfileExtra(extra) {
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
		default:
			continue
		}
		a := d.Args
		if a["type"] != "subtitleProfile" {
			continue
		}
		if ctx := a["context"]; ctx != "" && ctx != "all" && ctx != "streaming" {
			continue
		}
		if proto := a["protocol"]; proto != "" && !strings.EqualFold(proto, frontend.ProtocolHTTP) {
			continue
		}
		if strings.EqualFold(a["replace"], "true") {
			target = ""
		}
		if target != "" {
			continue
		}
		for _, field := range []string{a["subtitleCodec"], a["container"]} {
			for _, codec := range splitList(field) {
				if f := normalizeSoftSubtitleFormat(codec); f != "" {
					target = f
					break
				}
			}
			if target != "" {
				break
			}
		}
	}
	return target
}

// applyMusicDirectPlay adds support for the actual audio file as a pair.
// The registry's video container list does not enumerate raw music files.
func applyMusicDirectPlay(c *frontend.ClientCaps, extra string, info frontend.MediaInfo, audioIndex int) {
	if !info.AudioOnly() {
		return
	}
	if audioIndex < 0 {
		audioIndex = info.Streams[pickAudio(info, nil)].Index
	}
	audio := selectedAudioStream(info, frontend.Decision{AudioStream: audioIndex})
	codec := normalizeCodec(audio.Codec)
	container := info.Container
	switch {
	case container == "flac" && codec == "flac",
		container == "mp3" && codec == "mp3",
		(container == "ogg" || container == "opus") && (codec == "opus" || codec == "vorbis" || codec == "flac"):
		if c.SupportsAudio(codec) {
			c.Containers = addUnique(c.Containers, container)
		}
	}
	for _, d := range ParseProfileExtra(extra) {
		if d.Verb != "add-direct-play-profile" || d.Args["type"] != "musicProfile" {
			continue
		}
		if protocol := d.Args["protocol"]; protocol != "" && !strings.EqualFold(protocol, frontend.ProtocolHTTP) {
			continue
		}
		containers, codecs := splitList(d.Args["container"]), splitList(d.Args["audioCodec"])
		declared := frontend.ClientCaps{Containers: containers, AudioCodecs: codecs}
		if (profileContains(d.Args["container"], "*") || declared.SupportsContainer(container)) &&
			(profileContains(d.Args["audioCodec"], "*") || declared.SupportsAudio(codec)) {
			c.Containers = addUnique(c.Containers, container)
			c.AudioCodecs = addUnique(c.AudioCodecs, codec)
			return
		}
	}
}

func streamingMusicTarget(extra, protocol string) (containers, codecs []string) {
	for _, d := range ParseProfileExtra(extra) {
		switch d.Verb {
		case "add-transcode-target", "add-transcode-target-codec", "append-transcode-target-codec":
		default:
			continue
		}
		a := d.Args
		if a["type"] != "musicProfile" {
			continue
		}
		if context := a["context"]; context != "" && context != "all" && context != "streaming" {
			continue
		}
		if target := a["protocol"]; target != "" && !strings.EqualFold(target, protocol) {
			continue
		}
		containers = addUnique(containers, splitList(a["container"])...)
		codecs = addUnique(codecs, splitList(a["audioCodec"])...)
	}
	return containers, codecs
}

// h264LevelMaxHeight maps an H.264 level*10 upper bound (e.g. 40, 41, 42) to
// the tallest standard frame height that level's macroblock ceiling supports.
// Levels >= 5.0 support 4K and return 0 (unbounded).
func h264LevelMaxHeight(level int) int {
	switch {
	case level <= 0:
		return 0
	case level <= 30:
		return 480
	case level <= 32:
		return 720
	case level <= 42:
		return 1080
	default:
		return 0
	}
}

func applyLimitation(c *frontend.ClientCaps, a map[string]string) string {
	scope := a["scope"]
	if scope != "videoCodec" && scope != "videoAudioCodec" && scope != "videoContainer" {
		return ""
	}
	name := a["name"]
	typ := a["type"]
	// PMS sends colorTrc denials as list=, the same shape as its allow-lists.
	val := limitationValue(a)
	n, numErr := strconv.Atoi(val)
	switch {
	case scope == "videoCodec" && name == "video.level" && typ == "upperBound" && numErr == nil && n > 0:
		sn := normalizeCodec(a["scopeName"])
		if sn != "" && sn != "*" {
			if c.VideoLevels == nil {
				c.VideoLevels = map[string]int{}
			}
			if old := c.VideoLevels[sn]; old == 0 || n < old {
				c.VideoLevels[sn] = n
			}
			if sn == "h264" {
				if h := h264LevelMaxHeight(n); h > 0 {
					if c.VideoMaxHeights == nil {
						c.VideoMaxHeights = map[string]int{}
					}
					if old := c.VideoMaxHeights["h264"]; old == 0 || h < old {
						c.VideoMaxHeights["h264"] = h
					}
					return formatLimitationSource(scope, a["scopeName"], name, val)
				}
			}
		}
	case scope == "videoCodec" && name == "video.bitrate" && typ == "upperBound" && numErr == nil && n > 0:
		c.DefaultRemoteBitrate = false
		if c.MaxBitrateKbps == 0 || n < c.MaxBitrateKbps {
			c.MaxBitrateKbps = n
		}
	case scope == "videoCodec" && strings.EqualFold(name, "video.DOVIProfile"):
		if strings.EqualFold(typ, "notMatch") {
			if val == "*" || val == "" {
				c.DolbyVision = false
			} else if numErr == nil && n > 0 {
				// A profile-specific refusal must not disable other supported
				// Dolby Vision profiles (recorded PMS measurement, 2026-10-17).
				profiles := frontend.ClientPolicyFor(*c).DolbyVisionProfiles
				c.DVProfiles = slices.DeleteFunc(profiles, func(profile int) bool { return profile == n })
				if len(c.DVProfiles) == 0 {
					c.DolbyVision = false
				}
			}
		}
	}
	return ""
}

// applyLegacyCapabilities parses the old X-Plex-Client-Capabilities header,
// e.g. "protocols=http-live-streaming,http-mp4-streaming;videoDecoders=h264{profile:high&resolution:1080&level:51},h265;audioDecoders=mp3,aac{bitrate:160000},ac3".
func applyLegacyCapabilities(c *frontend.ClientCaps, s string) {
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "videoDecoders":
			// A decoder's resolution bounds that codec only (#394). Plex for
			// Roku lists just h264{resolution:1080} while it plays 4K HEVC,
			// which PMS serves it. The client-wide ceiling drops only when
			// every codec the client decodes is bounded.
			for _, dec := range splitDecoders(v) {
				codec := normalizeCodec(dec.name)
				c.VideoCodecs = addUnique(c.VideoCodecs, codec)
				if r, err := strconv.Atoi(dec.params["resolution"]); err == nil && r > 0 {
					if c.VideoMaxHeights == nil {
						c.VideoMaxHeights = map[string]int{}
					}
					if old := c.VideoMaxHeights[codec]; old == 0 || r < old {
						c.VideoMaxHeights[codec] = r
					}
				}
			}
			if ceiling := decoderCeiling(*c); ceiling > 0 && ceiling < c.MaxHeight {
				c.MaxHeight = ceiling
				c.ExplicitMaxHeight = true
				c.MaxHeightSource = "X-Plex-Client-Capabilities (videoDecoders resolution=" + strconv.Itoa(ceiling) + ")"
			}
		case "audioDecoders":
			for _, dec := range splitDecoders(v) {
				c.AudioCodecs = addUnique(c.AudioCodecs, normalizeCodec(dec.name))
			}
		}
	}
}

// decoderCeiling is the tallest per-decoder bound across the client's video
// codecs, or 0 when any of them is unbounded.
func decoderCeiling(c frontend.ClientCaps) int {
	ceiling := 0
	for _, codec := range c.VideoCodecs {
		h := c.VideoMaxHeights[codec]
		if h <= 0 {
			return 0
		}
		ceiling = max(ceiling, h)
	}
	return ceiling
}

type decoder struct {
	name   string
	params map[string]string
}

func splitDecoders(s string) []decoder {
	var out []decoder
	depth := 0
	start := 0
	flush := func(end int) {
		item := strings.TrimSpace(s[start:end])
		if item == "" {
			return
		}
		d := decoder{params: map[string]string{}}
		if i := strings.IndexByte(item, '{'); i >= 0 {
			d.name = item[:i]
			inner := strings.TrimSuffix(item[i+1:], "}")
			for _, kv := range strings.Split(inner, "&") {
				if k, v, ok := strings.Cut(kv, ":"); ok {
					d.params[k] = v
				}
			}
		} else {
			d.name = item
		}
		out = append(out, d)
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(s))
	return out
}

// applyDecisionQuery refines caps with per-request transcode parameters
// (maxVideoBitrate, videoResolution, audio limits). Locality (Remote) is
// decided solely by the authenticated source IP resolved in ParseRequest,
// never by the client-claimed location parameter.
func applyDecisionQuery(c *frontend.ClientCaps, q url.Values) {
	for _, name := range []string{"maxVideoBitrate", "videoBitrate", "musicBitrate"} {
		if n, err := strconv.Atoi(q.Get(name)); err == nil && n > 0 {
			c.DefaultRemoteBitrate = false
			if c.MaxBitrateKbps == 0 || n < c.MaxBitrateKbps {
				c.MaxBitrateKbps = n
			}
		}
	}
	if n, err := strconv.Atoi(q.Get("maxAudioChannels")); err == nil && n > 0 {
		c.AudioLimits = append(c.AudioLimits, frontend.AudioLimit{Codec: "*", Name: "audio.channels", Type: "upperBound", Value: n})
	}
	if raw := strings.TrimSpace(q.Get("audioCodec")); raw != "" {
		var codecs []string
		for _, part := range strings.Split(raw, ",") {
			if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
				codecs = append(codecs, p)
			}
		}
		if len(codecs) > 0 {
			c.TranscodeAudioCodecs = codecs
		}
	}
	if raw := strings.TrimSpace(q.Get("videoResolution")); raw != "" {
		if h := parseResolutionHeight(raw); h > 0 {
			c.ExplicitMaxHeight = true
			if c.MaxHeight == 0 || h < c.MaxHeight {
				c.MaxHeight = h
				c.MaxHeightSource = "request parameter videoResolution=" + raw
			}
		}
	}
}

// parseResolutionHeight parses "1920x1080" / "1280:720" / "1080" into a
// height.
func parseResolutionHeight(s string) int {
	if s == "" {
		return 0
	}
	if i := strings.IndexAny(s, "x:X"); i >= 0 {
		w, err1 := strconv.Atoi(s[:i])
		h, err2 := strconv.Atoi(s[i+1:])
		if err1 != nil || err2 != nil {
			return 0
		}
		// Portrait requests (e.g. 1080x1920) still mean 1080p.
		return min(w, h)
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(strings.ToLower(s), "p"))
	return n
}
