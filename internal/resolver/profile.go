// Package resolver implements core.Resolver: it turns a catalogue
// title plus client capabilities into a health-checked release.
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// defaultProfile is used when config has no usable profiles at all: the
// shipped 1080p profile.
func defaultProfile() core.QualityProfile {
	for _, p := range config.DefaultQuality().Profiles {
		if p.Name == "1080p" {
			p.Name = "default"
			return p
		}
	}
	return core.QualityProfile{Name: "default", MaxHeight: 1080, MinHeight: 720}
}

func clientDevice(caps core.ClientCaps) string {
	if caps.Device != "" {
		return caps.Device
	}
	return caps.DeviceName
}

func clientIdentity(caps core.ClientCaps) clientreg.Identity {
	return clientreg.Identity{Product: caps.Product, Platform: caps.Platform, Device: clientDevice(caps), DeviceName: caps.DeviceName, Model: caps.Model, Version: caps.Version}
}

// defaultCappedAndroidClient reports whether caps describes an Android
// mobile client whose registry playback ceiling is 1080p unless an owner
// quality rule lifts it. The match is the registry quirk
// android-mobile-ceiling, the same fact the capability engine uses.
func defaultCappedAndroidClient(caps core.ClientCaps) bool {
	return clientreg.HasQuirk(clientreg.Identity{Product: caps.Product, Platform: caps.Platform, Device: clientDevice(caps), Version: caps.Version}, "android-mobile-ceiling")
}

// ProfileSource names what picked a play's quality profile.
type ProfileSource string

const (
	ProfileFromRule        ProfileSource = "rule"        // the first matching rule that names a profile
	ProfileFromDefault     ProfileSource = "default"     // no rule named one: Default
	ProfileFromDefaultAway ProfileSource = "defaultAway" // no rule named one, away from home: Default away
	ProfileFromPlugin      ProfileSource = "plugin"      // the plugin chooses its own releases
	ProfileBuiltin         ProfileSource = "builtin"     // no usable profile is configured
)

// Membership reports whether the play's title is in a library. It is
// asked only for libraries a rule names, and lazily.
type Membership func(core.LibraryID) bool

// playTarget is what the rules can know about the title of a play.
type playTarget struct {
	// media is core.MediaMovie, core.MediaTV or "" (unknown).
	media string
	// titled is set once the title is known: rules with a Library
	// condition apply only then. Before that (and for Live TV) only rules
	// without a Library or Kind condition count, except that a known
	// media lets Kind rules apply.
	titled bool
	inLib  Membership
	// Fallback limits apply once the play is known to use a fallback row.
	skipFallbackLimits bool
}

func mediaTarget(media string) playTarget { return playTarget{media: normalizeMedia(media)} }

func titleTarget(t *core.Title, inLib Membership) playTarget {
	return playTarget{
		media: titleMedia(t), titled: t != nil, inLib: inLib,
		skipFallbackLimits: t != nil && t.Key.IsPluginKey(),
	}
}

// ruleLimits are the limits every matching rule contributes; the
// strictest wins.
type ruleLimits struct {
	maxHeight      int
	maxBitrateKbps int
	stereo         bool
}

func (l *ruleLimits) add(r config.QualityLimits) {
	if r.MaxHeight > 0 && (l.maxHeight == 0 || r.MaxHeight < l.maxHeight) {
		l.maxHeight = r.MaxHeight
	}
	if r.MaxBitrateKbps > 0 && (l.maxBitrateKbps == 0 || r.MaxBitrateKbps < l.maxBitrateKbps) {
		l.maxBitrateKbps = r.MaxBitrateKbps
	}
	l.stereo = l.stereo || r.StereoAudio
}

// profileChoice is which configured profile applies to a play, before
// any clamp, and why.
type profileChoice struct {
	profile core.QualityProfile
	source  ProfileSource
	rule    int // index into q.Rules when source is ProfileFromRule, else -1
}

// normalizeMedia keeps an unknown or empty media hint media-neutral.
func normalizeMedia(media string) string {
	switch strings.ToLower(strings.TrimSpace(media)) {
	case core.MediaMovie:
		return core.MediaMovie
	case core.MediaTV, "show", "episode":
		return core.MediaTV
	}
	return core.MediaAny
}

// titleMedia classifies a title as a movie or an episode; a title with no
// usable type is unknown and returns "".
func titleMedia(t *core.Title) string {
	if t == nil {
		return core.MediaAny
	}
	switch t.Type {
	case core.TypeMovie:
		return core.MediaMovie
	case core.TypeShow, core.TypeSeason, core.TypeEpisode:
		return core.MediaTV
	}
	switch t.Key.Kind() {
	case core.KindMovie:
		return core.MediaMovie
	case core.KindShow, core.KindSeason, core.KindEpisode:
		return core.MediaTV
	}
	return core.MediaAny
}

// ruleKindMedia maps a rule's Kind to the resolver's media.
func ruleKindMedia(kind string) string {
	switch kind {
	case config.KindMovie:
		return core.MediaMovie
	case config.KindEpisode:
		return core.MediaTV
	}
	return core.MediaAny
}

// clientFacts are the device facts rules read, computed once per play.
type clientFacts struct {
	caps                     core.ClientCaps
	done                     bool
	class, deviceType, appID string
}

func (f *clientFacts) load() {
	if f.done {
		return
	}
	f.done = true
	id := clientIdentity(f.caps)
	f.class = clientreg.Class(id)
	f.deviceType = clientreg.DeviceType(id)
	f.appID = clientreg.AppFor(string(f.caps.Protocol), id)
}

func (f *clientFacts) matches(d config.DeviceMatch) bool {
	switch field, v := d.Field(); field {
	case "group":
		f.load()
		return f.class == v
	case "type":
		f.load()
		return clientreg.DeviceTypeMatches(f.deviceType, v)
	case "app":
		f.load()
		return f.appID == v
	case "client":
		return f.caps.ClientID != "" && f.caps.ClientID == v
	case "contains":
		return containsMatches(v, f.caps)
	}
	return false
}

// containsMatches is a case-insensitive substring match on the product or
// platform. "Fire TV" is matched by device instead: the app there reports
// itself as "Plex for Android (TV)".
func containsMatches(want string, caps core.ClientCaps) bool {
	w := strings.ToLower(strings.TrimSpace(want))
	if w == "" {
		return false
	}
	if strings.Contains(strings.ToLower(caps.Product), w) || strings.Contains(strings.ToLower(caps.Platform), w) {
		return true
	}
	return (w == "fire tv" || w == "firetv") && core.IsFireTV(caps.Product, caps.Device)
}

// ruleMatches reports whether every non-empty condition of rule holds for
// the play. Cheap conditions run first; library membership runs last.
func ruleMatches(rule config.QualityRule, facts *clientFacts, user *core.User, target playTarget) bool {
	switch rule.Where {
	case config.WhereHome:
		if facts.caps.Remote {
			return false
		}
	case config.WhereAway:
		if !facts.caps.Remote {
			return false
		}
	}
	if rule.Kind != "" {
		if target.media == core.MediaAny || ruleKindMedia(rule.Kind) != target.media {
			return false
		}
	}
	if len(rule.Viewers) > 0 {
		in := user != nil && slices.Contains(rule.Viewers, user.ID)
		if in == rule.ExceptViewers {
			return false
		}
	}
	if len(rule.Devices) > 0 {
		in := slices.ContainsFunc(rule.Devices, facts.matches)
		if in == rule.ExceptDevices {
			return false
		}
	}
	if len(rule.Libraries) > 0 {
		if !target.titled || target.inLib == nil {
			return false
		}
		in := slices.ContainsFunc(rule.Libraries, func(id int64) bool { return target.inLib(core.LibraryID(id)) })
		if in == rule.ExceptLibraries {
			return false
		}
	}
	return true
}

// evaluation is the walk of the rule list for one play.
type evaluation struct {
	choice              profileChoice
	limits              ruleLimits
	limitRules          []int // indexes of matching rules that set a limit
	deviceRule          bool  // the profile came from a rule with a Device condition
	matched             []int // every matching rule, in order
	fallbackLimitSource ProfileSource
}

// evaluate walks the rules from the top: the first matching rule that
// names a profile picks it, and every matching rule's limits apply. With
// no profile from a rule, Default away (away from home, when set) or
// Default decides.
func evaluate(q config.Quality, caps core.ClientCaps, user *core.User, target playTarget) evaluation {
	facts := &clientFacts{caps: caps}
	ev := evaluation{choice: profileChoice{rule: -1}}
	for i, rule := range q.Rules {
		if !ruleMatches(rule, facts, user, target) {
			continue
		}
		ev.matched = append(ev.matched, i)
		if rule.HasLimits() {
			ev.limits.add(rule.Limits())
			ev.limitRules = append(ev.limitRules, i)
		}
		if ev.choice.rule < 0 && rule.Profile != "" {
			if p, ok := core.FindProfile(q.Profiles, rule.Profile); ok {
				ev.choice = profileChoice{profile: p, source: ProfileFromRule, rule: i}
				ev.deviceRule = len(rule.Devices) > 0 && !rule.ExceptDevices
			}
		}
	}
	if ev.choice.rule >= 0 {
		return ev
	}
	ev.choice = fallbackProfile(q, caps.Remote)
	if !target.skipFallbackLimits {
		source, limits := fallbackLimits(q, ev.choice)
		if limits.HasLimits() {
			ev.limits.add(limits)
			ev.fallbackLimitSource = source
		}
	}
	return ev
}

func fallbackProfile(q config.Quality, remote bool) profileChoice {
	if remote && q.DefaultAway != "" {
		if p, ok := core.FindProfile(q.Profiles, q.DefaultAway); ok {
			return profileChoice{profile: p, source: ProfileFromDefaultAway, rule: -1}
		}
	}
	if p, ok := core.FindProfile(q.Profiles, q.Default); ok {
		return profileChoice{profile: p, source: ProfileFromDefault, rule: -1}
	}
	if len(q.Profiles) > 0 {
		return profileChoice{profile: q.Profiles[0], source: ProfileFromDefault, rule: -1}
	}
	return profileChoice{profile: defaultProfile(), source: ProfileBuiltin, rule: -1}
}

func fallbackLimits(q config.Quality, choice profileChoice) (ProfileSource, config.QualityLimits) {
	if choice.source == ProfileFromDefaultAway {
		return ProfileFromDefaultAway, q.DefaultAwayLimits
	}
	return ProfileFromDefault, q.DefaultLimits
}

// chooseProfile is the media-neutral choice for callers with no title.
func chooseProfile(q config.Quality, caps core.ClientCaps, user *core.User) profileChoice {
	return evaluate(q, caps, user, playTarget{}).choice
}

// ruleEffect records what the rules did to the client's capabilities in
// applyClientRules.
type ruleEffect struct {
	choice profileChoice
	limits ruleLimits
	// limitRules are the matching rules that set a limit, in order.
	limitRules          []int
	fallbackLimitSource ProfileSource
	// ruleHeight is the strictest rule max height that lowered the
	// client's ceiling (0 when none lowered it).
	ruleHeight int
	// lifted is the height an Android mobile client's usual 1080p ceiling
	// was raised to because a device rule picked a higher profile.
	lifted int
	// hdrLifted is set when that lift also turned HDR10 on.
	hdrLifted bool
}

func stripSurroundAudioCaps(caps core.ClientCaps) core.ClientCaps {
	caps.ConvertSurroundAudio = true
	if len(caps.AudioCodecs) == 0 {
		if defaults := clientreg.Playback(clientreg.Identity{Product: caps.Product, Platform: caps.Platform, Device: clientDevice(caps), Version: caps.Version}, nil); len(defaults.Audio) > 0 {
			caps.AudioCodecs = append([]string(nil), defaults.Audio...)
		} else {
			caps.AudioCodecs = []string{"aac", "mp3", "flac", "alac", "opus"}
		}
	}
	caps.AudioCodecs = core.FilterOutSurroundAudio(caps.AudioCodecs)
	if len(caps.AudioCodecs) == 0 {
		caps.AudioCodecs = []string{"aac"}
	}
	if len(caps.TranscodeAudioCodecs) > 0 {
		caps.TranscodeAudioCodecs = core.FilterOutSurroundAudio(caps.TranscodeAudioCodecs)
	}
	if caps.Profiles != nil {
		cp := *caps.Profiles
		cp.DirectPlay = slices.Clone(caps.Profiles.DirectPlay)
		for i := range cp.DirectPlay {
			base := cp.DirectPlay[i].AudioCodecs
			if len(base) == 0 {
				base = caps.AudioCodecs
			}
			filtered := core.FilterOutSurroundAudio(base)
			if len(filtered) == 0 {
				filtered = []string{"aac"}
			}
			cp.DirectPlay[i].AudioCodecs = filtered
		}
		cp.Transcoding = slices.Clone(caps.Profiles.Transcoding)
		for i := range cp.Transcoding {
			filtered := core.FilterOutSurroundAudio(cp.Transcoding[i].AudioCodecs)
			if len(filtered) == 0 {
				filtered = []string{"aac"}
			}
			cp.Transcoding[i].AudioCodecs = filtered
		}
		caps.Profiles = &cp
	}
	return caps
}

// ApplyClientRules returns caps with any matching user-defined client rule
// resolution override applied. When no rule matches (or when the client
// sent an explicit per-request resolution limit), the default 1080p ceiling
// for Android mobile is preserved. When the selected profile for the client
// allows 2160p (4K) and the request carries only the server's default 1080p
// remote bitrate cap (rather than an explicit client bitrate limit), the
// default remote bitrate cap is cleared so 4K releases are not demoted below

// ApplyClientRules applies the limits of the rules that match a client
// before any title is known (rules with no Library or Kind condition).
func ApplyClientRules(q config.Quality, caps core.ClientCaps, user *core.User) core.ClientCaps {
	return ApplyClientRulesForMedia(q, caps, user, core.MediaAny)
}

// ApplyClientRulesForMedia applies only what can be known before the title
// is: rules with a Library condition never apply here, and Kind rules only
// when media is known. Defer fallback limits until the title is known:
// a Library or Kind rule can still pick a profile.
func ApplyClientRulesForMedia(q config.Quality, caps core.ClientCaps, user *core.User, media string) core.ClientCaps {
	target := mediaTarget(media)
	target.skipFallbackLimits = true
	caps, _ = applyClientRules(q, caps, user, target)
	return caps
}

// ApplyClientRulesForTitle applies the rules that match a specific title.
// inLib answers library membership; nil leaves Library rules out.
func ApplyClientRulesForTitle(q config.Quality, caps core.ClientCaps, user *core.User, t *core.Title, inLib Membership) core.ClientCaps {
	caps, _ = applyClientRules(q, caps, user, titleTarget(t, inLib))
	return caps
}

// applyRuleLimits lowers caps to the accumulated rule limits.
func applyRuleLimits(caps core.ClientCaps, eff *ruleEffect) core.ClientCaps {
	l := eff.limits
	caps.QualityRuleMaxHeight = l.maxHeight
	if l.maxHeight > 0 && (caps.MaxHeight == 0 || l.maxHeight < caps.MaxHeight) {
		caps.MaxHeight = l.maxHeight
		caps.MaxHeightSource = fmt.Sprintf("quality rule (max %dp)", l.maxHeight)
		eff.ruleHeight = l.maxHeight
	}
	if l.maxBitrateKbps > 0 && (caps.MaxBitrateKbps == 0 || l.maxBitrateKbps <= caps.MaxBitrateKbps) {
		caps.MaxBitrateKbps = l.maxBitrateKbps
		caps.DefaultRemoteBitrate = false
	}
	if l.stereo {
		caps = stripSurroundAudioCaps(caps)
	}
	return caps
}

func applyClientRules(q config.Quality, caps core.ClientCaps, user *core.User, target playTarget) (core.ClientCaps, ruleEffect) {
	if len(caps.VideoCodecs) == 0 && !caps.PreserveEmptyVideoCodecs {
		if defaults := clientreg.Playback(clientreg.Identity{Product: caps.Product, Platform: caps.Platform, Device: clientDevice(caps), Version: caps.Version}, nil); defaults.ID != "" && defaults.ID != "generic" && len(defaults.Video) > 0 {
			caps.VideoCodecs = append([]string(nil), defaults.Video...)
		}
	}
	// Rule conditions read only identity, viewer, remote and the title,
	// none of which this function changes, so the result holds.
	ev := evaluate(q, caps, user, target)
	eff := ruleEffect{choice: ev.choice, limits: ev.limits, limitRules: ev.limitRules, fallbackLimitSource: ev.fallbackLimitSource}
	if caps.Download {
		caps.QualityRuleMaxHeight = 0
		return caps, eff
	}
	prof := eff.choice.profile
	// A device rule that picks a profile above an Android phone's usual
	// 1080p ceiling lifts the ceiling to the profile (#187).
	if ev.deviceRule && !caps.ExplicitMaxHeight && defaultCappedAndroidClient(caps) {
		targetHeight := prof.MaxHeight
		if targetHeight <= 0 {
			targetHeight = 2160
		}
		if caps.MaxHeight < targetHeight {
			caps.MaxHeight = targetHeight
			if targetHeight >= 2160 {
				caps.MaxHeightSource = ""
			} else {
				caps.MaxHeightSource = fmt.Sprintf("quality rule (%s profile)", prof.Name)
			}
			eff.lifted = targetHeight
		}
		if caps.MaxHeight >= 2160 && prof.AllowHDR && !caps.ExplicitNoHDR {
			eff.hdrLifted = !caps.HDR10
			caps.HDR10 = true
		}
	}
	caps = applyRuleLimits(caps, &eff)
	selected := eff.choice.profile
	if eff.choice.source != ProfileBuiltin && caps.DefaultRemoteBitrate && (selected.MaxHeight == 0 || selected.MaxHeight >= 2160) && (caps.MaxHeight == 0 || caps.MaxHeight >= 2160) {
		// An implicit client default may be discarded, but a configured
		// limit still applies even if it was above that implicit default.
		caps.MaxBitrateKbps = eff.limits.maxBitrateKbps
		caps.DefaultRemoteBitrate = false
	}
	return caps, eff
}

// membershipFor is the library membership of t for the rules, or nil
// when no lookup is wired.
func (s *Service) membershipFor(ctx context.Context, t *core.Title) Membership {
	if s.libProfiles == nil || t == nil {
		return nil
	}
	return func(id core.LibraryID) bool { return s.libProfiles.InLibrary(ctx, t, id) }
}

// SelectProfile picks the quality profile for a client and clamps it
// to what the client can direct-play. Playback uses it for
// pre-resolution estimates; the resolver uses it for ranking.
//
// Selection is evaluate's. Clamping: MaxHeight is capped at
// caps.MaxHeight, HDR/DV are only allowed when the client supports them,
// and preferred codecs the client cannot decode are dropped.
func SelectProfile(q config.Quality, caps core.ClientCaps, user *core.User) core.QualityProfile {
	return SelectProfileForMedia(q, caps, user, core.MediaAny)
}

// SelectProfileForMedia is SelectProfile with a movie or episode context,
// so Kind rules apply. An empty media is media-neutral.
func SelectProfileForMedia(q config.Quality, caps core.ClientCaps, user *core.User, media string) core.QualityProfile {
	return decideProfile(q, caps, user, mediaTarget(media)).profile
}

// SelectProfileForTitle is SelectProfileForMedia for a known title, so
// Library rules apply too (inLib answers membership).
func SelectProfileForTitle(q config.Quality, caps core.ClientCaps, user *core.User, t *core.Title, inLib Membership) core.QualityProfile {
	return decideProfile(q, caps, user, titleTarget(t, inLib)).profile
}

// profileDecision is SelectProfile's answer with what decided it.
type profileDecision struct {
	profile core.QualityProfile // selected, then clamped to the client
	caps    core.ClientCaps     // the client after quality rules
	effect  ruleEffect
}

func (d profileDecision) ruleDecision(q config.Quality) (source ProfileSource, profileRuleID string, limitRuleIDs []string) {
	source = d.effect.choice.source
	if source == ProfileFromRule && d.effect.choice.rule >= 0 && d.effect.choice.rule < len(q.Rules) {
		profileRuleID = q.Rules[d.effect.choice.rule].ID
	}
	if len(d.effect.limitRules) > 0 {
		limitRuleIDs = make([]string, 0, len(d.effect.limitRules))
		for _, idx := range d.effect.limitRules {
			if idx >= 0 && idx < len(q.Rules) && q.Rules[idx].ID != "" {
				limitRuleIDs = append(limitRuleIDs, q.Rules[idx].ID)
			}
		}
	}
	if d.effect.fallbackLimitSource != "" {
		limitRuleIDs = append(limitRuleIDs, string(d.effect.fallbackLimitSource))
	}
	return source, profileRuleID, limitRuleIDs
}

func decideProfile(q config.Quality, caps core.ClientCaps, user *core.User, target playTarget) profileDecision {
	if caps.Download && caps.DownloadSourceMaxHeight > caps.MaxHeight {
		caps.MaxHeight = caps.DownloadSourceMaxHeight
	}
	caps, eff := applyClientRules(q, caps, user, target)
	prof := eff.choice.profile
	// Deep-copy slices so callers cannot mutate config.
	prof.PreferredLanguages = append([]string(nil), prof.PreferredLanguages...)
	prof.AllowedQualities = append([]string(nil), prof.AllowedQualities...)
	prof.QualityOrder = config.CloneQualityOrder(prof.QualityOrder)
	if prof.PluginScores != nil {
		cloned := make(map[string]json.RawMessage, len(prof.PluginScores))
		for k, v := range prof.PluginScores {
			cloned[k] = append(json.RawMessage(nil), v...)
		}
		prof.PluginScores = cloned
	}
	if prof.CustomFormatScores != nil {
		copyScores := map[string]int{}
		for k, v := range prof.CustomFormatScores {
			copyScores[k] = v
		}
		prof.CustomFormatScores = copyScores
	}
	if len(prof.ReleaseRules) > 0 {
		rules := make([]core.ReleaseRule, len(prof.ReleaseRules))
		for i, r := range prof.ReleaseRules {
			rules[i] = r
			rules[i].Conditions = append([]core.ReleaseCondition(nil), r.Conditions...)
			rules[i].Unless = append([]core.ReleaseCondition(nil), r.Unless...)
			rules[i].Fallback = append([]core.ReleaseCondition(nil), r.Fallback...)
		}
		prof.ReleaseRules = rules
	}

	// Key of the configured definition, taken from this pristine copy
	// before the configured-height snapshot, download reshaping, and
	// the client-cap clamp below, so the same configured profile keys
	// identically for every client.
	prof.SelectionKey = prof.ComputeSelectionKey()

	// Snapshot the owner's configured ceiling before the download
	// reshape below (and the client-cap clamp after it) lowers the
	// effective MaxHeight: "configured" is what the owner set, even
	// when the reshape is what ends up limiting the release.
	prof = snapshotConfiguredMaxHeight(prof)

	if caps.Download {
		before := prof.MaxHeight
		prof = downloadProfile(prof, caps)
		if prof.MaxHeight > 0 && (before == 0 || prof.MaxHeight < before) {
			prof.CeilingSource = "offline download profile"
		}
	}

	// Clamp to client capabilities.
	if caps.MaxHeight > 0 && (prof.MaxHeight == 0 || prof.MaxHeight > caps.MaxHeight) {
		prof.MaxHeight = caps.MaxHeight
		prof.CeilingSource = caps.MaxHeightSource
	}
	if prof.MinHeight > prof.MaxHeight && prof.MaxHeight > 0 {
		prof.MinHeight = prof.MaxHeight
	}
	prof.AllowHDR = prof.AllowHDR && caps.HDR10
	prof.AllowDolbyVision = prof.AllowDolbyVision && caps.DolbyVision
	// A codec the client can't decode earns nothing from its format: it
	// plays only through a transcode, which the streaming formats score.
	if len(caps.VideoCodecs) > 0 {
		for _, c := range core.FormatCodecs {
			if !caps.SupportsVideo(c) {
				delete(prof.CustomFormatScores, core.CodecFormat(c))
			}
		}
	}
	if eff.limits.stereo && len(caps.AudioCodecs) > 0 {
		for _, a := range core.FormatAudioCodecs {
			if !caps.SupportsAudio(a) {
				delete(prof.CustomFormatScores, core.AudioFormat(a))
			}
		}
	}
	return profileDecision{profile: prof, caps: caps, effect: eff}
}

// downloadProfile reshapes the selected profile for an offline download
// (caps.Download): a compact, mobile-native file that stream-copies into
// an MP4. No 4K/HDR/Dolby Vision (a copy cannot tone-map, and phones do
// not need it), WEB-DL first, H.264 — or HEVC when the device decodes
// it — and MP4-friendly AAC/AC3/EAC3 audio only (no TrueHD/DTS-HD,
// which neither copies into a broadly playable MP4 nor helps a phone).
// The profile's release type, codec and audio format scores are
// reassigned in that order, so each keeps the weight the profile gives
// it. The size cap (~10 Mbps average) keeps a two-hour film near 9 GB
// worst case and typical 1080p WEB-DLs well inside it. caps.MaxHeight and
// caps.MaxBitrateKbps from the client's videoQuality/videoResolution
// request still clamp on top of this in SelectProfile.
func downloadProfile(prof core.QualityProfile, caps core.ClientCaps) core.QualityProfile {
	prof.Name += "+download"
	ceiling := 1080
	if caps.DownloadSourceMaxHeight > ceiling {
		ceiling = caps.DownloadSourceMaxHeight
	}
	if prof.MaxHeight == 0 || prof.MaxHeight > ceiling {
		prof.MaxHeight = ceiling
	}
	if prof.MinHeight > prof.MaxHeight {
		prof.MinHeight = prof.MaxHeight
	}
	prof.AllowHDR = false
	prof.AllowDolbyVision = false
	if prof.CustomFormatScores == nil {
		prof.CustomFormatScores = map[string]int{}
	}
	sources := []string{}
	for _, s := range core.FormatSources {
		sources = append(sources, core.SourceFormat(s))
	}
	reorderFormats(prof.CustomFormatScores, sources, []string{core.SourceFormat(core.SourceWEBDL), core.SourceFormat(core.SourceWEBRip), core.SourceFormat(core.SourceBluRay), core.SourceFormat(core.SourceHDTV)}, 10)
	codecs := []string{}
	for _, c := range core.FormatCodecs {
		codecs = append(codecs, core.CodecFormat(c))
	}
	if len(caps.VideoCodecs) == 0 || caps.SupportsVideo("hevc") {
		reorderFormats(prof.CustomFormatScores, codecs, []string{core.CodecFormat("h264"), core.CodecFormat("hevc")}, 6)
	} else {
		reorderFormats(prof.CustomFormatScores, codecs, []string{core.CodecFormat("h264")}, 6)
	}
	audio := []string{}
	for _, a := range core.FormatAudioCodecs {
		audio = append(audio, core.AudioFormat(a))
	}
	reorderFormats(prof.CustomFormatScores, audio, []string{core.AudioFormat("aac"), core.AudioFormat("eac3"), core.AudioFormat("ac3")}, 4)
	// A source for conversion can be larger than the compact copy target;
	// keep the owner's size ceilings instead of adding the copy-only cap.
	if caps.DownloadSourceMaxHeight == 0 {
		if prof.MaxSizeMBPerMin == 0 || prof.MaxSizeMBPerMin > 80 {
			prof.MaxSizeMBPerMin = 80
		}
		if prof.EpisodeMaxSizeMBPerMin > 80 {
			prof.EpisodeMaxSizeMBPerMin = 80
		}
	}
	return prof
}

// reorderFormats gives the formats of order (best first) the family's
// scores, highest first; the family's other formats get its lowest. A
// family the profile does not score gets step points a place.
func reorderFormats(scores map[string]int, family, order []string, step int) {
	vals := make([]int, len(family))
	for i, id := range family {
		vals[i] = scores[id]
	}
	slices.SortFunc(vals, func(a, b int) int { return b - a })
	low := vals[len(vals)-1]
	for _, id := range family {
		scores[id] = low
	}
	for i, id := range order {
		v := vals[min(i, len(vals)-1)]
		if vals[0] <= 0 {
			v = step * (len(order) - i)
		}
		scores[id] = v
	}
}

// configuredUnlimited marks a profile whose owner set no max-height
// ceiling ("Any", MaxHeight 0 in config). It keeps "no ceiling" apart
// from ConfiguredMaxHeight 0 ("field not set") on hand-built profiles.
const configuredUnlimited = -1

// snapshotConfiguredMaxHeight records the owner's ceiling on p before
// any reshape or clamp lowers MaxHeight.
func snapshotConfiguredMaxHeight(p core.QualityProfile) core.QualityProfile {
	if p.MaxHeight == 0 {
		p.ConfiguredMaxHeight = configuredUnlimited
	} else {
		p.ConfiguredMaxHeight = p.MaxHeight
	}
	return p
}

// configuredMaxHeight is the profile's own ceiling before any download
// reshape or client clamp: configuredUnlimited when the owner set
// none, otherwise a positive height. Profiles built outside
// SelectProfile (ConfiguredMaxHeight unset) report their MaxHeight.
func configuredMaxHeight(p core.QualityProfile) int {
	if p.ConfiguredMaxHeight != 0 {
		return p.ConfiguredMaxHeight
	}
	return p.MaxHeight
}

// traceConfiguredMaxHeight is the configured ceiling as recorded on a
// trace or history record, with presence: a profile from SelectProfile
// reports its configured ceiling (a pointer to 0 when the owner set
// none — explicitly unlimited), while a profile built outside
// SelectProfile (ConfiguredMaxHeight unset) reports nil, unknown,
// rather than falling back to its effective MaxHeight.
func traceConfiguredMaxHeight(p core.QualityProfile) *int {
	switch {
	case p.ConfiguredMaxHeight == configuredUnlimited:
		v := 0
		return &v
	case p.ConfiguredMaxHeight > 0:
		v := p.ConfiguredMaxHeight
		return &v
	default:
		return nil
	}
}

// ProfileForTitle is the shared library-aware policy used by playback caches,
// resolution, and discovery. Context is used only for bounded local membership reads.
func (s *Service) ProfileForTitle(ctx context.Context, caps core.ClientCaps, user *core.User, t *core.Title) core.QualityProfile {
	return SelectProfileForTitle(s.qualityCfg(), caps, user, t, s.membershipFor(ctx, t))
}
