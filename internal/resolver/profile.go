// Package resolver implements core.Resolver: it turns a catalogue
// title plus client capabilities into a health-checked release.
package resolver

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// defaultProfile is used when config has no usable profiles at all.
func defaultProfile() core.QualityProfile {
	return core.QualityProfile{
		Name:             "default",
		MaxHeight:        1080,
		MinHeight:        720,
		PreferredSources: []core.Source{core.SourceWEBDL, core.SourceBluRay, core.SourceWEBRip, core.SourceHDTV},
		PreferredCodecs:  []string{"h264", "hevc"},
		PreferredAudio:   []string{"eac3", "ac3", "aac"},
		MinSizeMBPerMin:  5,
		MaxSizeMBPerMin:  150,
	}
}

// defaultCappedAndroidClient reports whether caps describes an Android
// mobile client whose default base ceiling is 1080p unless overridden by
// a user-configured client rule.
func defaultCappedAndroidClient(caps core.ClientCaps) bool {
	pr := strings.ToLower(caps.Product)
	pl := strings.ToLower(caps.Platform)
	dv := strings.ToLower(caps.Device)
	isAndroid := strings.Contains(pr, "android") || (pl == "android" && !strings.Contains(pr, "web"))
	if !isAndroid {
		return false
	}
	return !strings.Contains(pr, "tv") && !strings.Contains(dv, "shield") && !strings.Contains(dv, "tv")
}

// ApplyClientRules returns caps with any matching user-defined client rule
// resolution override applied. When no rule matches (or when the client
// sent an explicit per-request resolution limit), the default 1080p ceiling
// for Android mobile is preserved. When the selected profile for the client
// allows 2160p (4K) and the request carries only the server's default 1080p
// remote bitrate cap (rather than an explicit client bitrate limit), the
// default remote bitrate cap is cleared so 4K releases are not demoted below
// 8 Mbps.
func ApplyClientRules(q config.Quality, caps core.ClientCaps, user *core.User) core.ClientCaps {
	if caps.Download {
		return caps
	}
	byName := map[string]core.QualityProfile{}
	for _, p := range q.Profiles {
		byName[p.Name] = p
	}
	var selected core.QualityProfile
	var hasSelected bool
	if caps.Remote && q.Remote != "" {
		if p, ok := byName[q.Remote]; ok {
			selected, hasSelected = p, true
		}
	}
	if !hasSelected {
		for _, rule := range q.Rules {
			if !ruleMatches(rule, caps, user) {
				continue
			}
			prof, ok := byName[rule.Profile]
			if !ok {
				continue
			}
			selected, hasSelected = prof, true
			targetHeight := rule.MaxHeight
			if targetHeight <= 0 {
				if prof.MaxHeight > 0 {
					targetHeight = prof.MaxHeight
				} else {
					targetHeight = 2160
				}
			}
			if !caps.ExplicitMaxHeight {
				if rule.MaxHeight > 0 {
					caps.MaxHeight = rule.MaxHeight
				} else if defaultCappedAndroidClient(caps) && caps.MaxHeight < targetHeight {
					caps.MaxHeight = targetHeight
				}
			}
			if caps.MaxHeight >= 2160 && prof.AllowHDR && !caps.ExplicitNoHDR && defaultCappedAndroidClient(caps) {
				caps.HDR10 = true
			}
			break
		}
	}
	if !hasSelected && q.Default != "" {
		if p, ok := byName[q.Default]; ok {
			selected, hasSelected = p, true
		}
	}
	if !hasSelected && len(q.Profiles) > 0 {
		selected, hasSelected = q.Profiles[0], true
	}
	if hasSelected && caps.DefaultRemoteBitrate && (selected.MaxHeight == 0 || selected.MaxHeight >= 2160) && (caps.MaxHeight == 0 || caps.MaxHeight >= 2160) {
		caps.MaxBitrateKbps = 0
		caps.DefaultRemoteBitrate = false
	}
	return caps
}

// SelectProfile picks the quality profile for a client and clamps it
// to what the client can direct-play. Playback uses it for
// pre-resolution estimates; the resolver uses it for ranking.
//
// Selection order: remote clients get q.Remote (when set); otherwise
// the first rule whose non-empty matchers all match; otherwise
// q.Default; otherwise the first profile. Clamping: MaxHeight is
// capped at caps.MaxHeight, HDR/DV are only allowed when the client
// supports them, and preferred codecs the client cannot decode are
// dropped.
func SelectProfile(q config.Quality, caps core.ClientCaps, user *core.User) core.QualityProfile {
	caps = ApplyClientRules(q, caps, user)
	byName := map[string]core.QualityProfile{}
	for _, p := range q.Profiles {
		byName[p.Name] = p
	}
	var prof core.QualityProfile
	found := false
	pick := func(name string) bool {
		if name == "" {
			return false
		}
		if p, ok := byName[name]; ok {
			prof, found = p, true
			return true
		}
		return false
	}
	switch {
	case caps.Remote && pick(q.Remote):
	default:
		for _, rule := range q.Rules {
			if ruleMatches(rule, caps, user) && pick(rule.Profile) {
				break
			}
		}
		if !found {
			pick(q.Default)
		}
	}
	if !found {
		if len(q.Profiles) > 0 {
			prof = q.Profiles[0]
		} else {
			prof = defaultProfile()
		}
	}
	// Deep-copy slices so callers cannot mutate config.
	prof.PreferredSources = append([]core.Source(nil), prof.PreferredSources...)
	prof.PreferredCodecs = append([]string(nil), prof.PreferredCodecs...)
	prof.PreferredAudio = append([]string(nil), prof.PreferredAudio...)
	prof.PreferredLanguages = append([]string(nil), prof.PreferredLanguages...)
	prof.AllowedQualities = append([]string(nil), prof.AllowedQualities...)
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
	// reshape below (and the client clamp after it) lowers the
	// effective MaxHeight: "configured" is what the owner set, even
	// when the reshape is what ends up limiting the release.
	prof = snapshotConfiguredMaxHeight(prof)

	if caps.Download {
		prof = downloadProfile(prof, caps)
	}

	// Clamp to client capabilities.
	if caps.MaxHeight > 0 && (prof.MaxHeight == 0 || prof.MaxHeight > caps.MaxHeight) {
		prof.MaxHeight = caps.MaxHeight
	}
	if prof.MinHeight > prof.MaxHeight && prof.MaxHeight > 0 {
		prof.MinHeight = prof.MaxHeight
	}
	prof.AllowHDR = prof.AllowHDR && caps.HDR10
	prof.AllowDolbyVision = prof.AllowDolbyVision && caps.DolbyVision
	if len(caps.VideoCodecs) > 0 {
		kept := prof.PreferredCodecs[:0:0]
		for _, c := range prof.PreferredCodecs {
			if caps.SupportsVideo(c) {
				kept = append(kept, c)
			}
		}
		prof.PreferredCodecs = kept
	}
	return prof
}

// downloadProfile reshapes the selected profile for an offline download
// (caps.Download): a compact, mobile-native file that stream-copies into
// an MP4. No 4K/HDR/Dolby Vision (a copy cannot tone-map, and phones do
// not need it), WEB-DL first, H.264 — or HEVC when the device decodes
// it — and MP4-friendly AAC/AC3/EAC3 audio only (no TrueHD/DTS-HD,
// which neither copies into a broadly playable MP4 nor helps a phone).
// The size cap (~10 Mbps average) keeps a two-hour film near 9 GB worst
// case and typical 1080p WEB-DLs well inside it. caps.MaxHeight and
// caps.MaxBitrateKbps from the client's videoQuality/videoResolution
// request still clamp on top of this in SelectProfile.
func downloadProfile(prof core.QualityProfile, caps core.ClientCaps) core.QualityProfile {
	prof.Name += "+download"
	if prof.MaxHeight == 0 || prof.MaxHeight > 1080 {
		prof.MaxHeight = 1080
	}
	if prof.MinHeight > prof.MaxHeight {
		prof.MinHeight = prof.MaxHeight
	}
	prof.AllowHDR = false
	prof.AllowDolbyVision = false
	prof.PreferredSources = []core.Source{core.SourceWEBDL, core.SourceWEBRip, core.SourceBluRay, core.SourceHDTV}
	if len(caps.VideoCodecs) == 0 || caps.SupportsVideo("hevc") {
		prof.PreferredCodecs = []string{"h264", "hevc"}
	} else {
		prof.PreferredCodecs = []string{"h264"}
	}
	prof.PreferredAudio = []string{"aac", "eac3", "ac3"}
	if prof.MaxSizeMBPerMin == 0 || prof.MaxSizeMBPerMin > 80 {
		prof.MaxSizeMBPerMin = 80
	}
	if prof.EpisodeMaxSizeMBPerMin > 80 {
		prof.EpisodeMaxSizeMBPerMin = 80
	}
	prof.PreferLargest = false
	return prof
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

func ruleMatches(rule config.QualityRule, caps core.ClientCaps, user *core.User) bool {
	if rule.Protocol != "" && !strings.EqualFold(string(rule.Protocol), string(caps.Protocol)) {
		return false
	}
	if rule.Product != "" && !strings.Contains(strings.ToLower(caps.Product), strings.ToLower(rule.Product)) {
		return false
	}
	if rule.Platform != "" && !strings.Contains(strings.ToLower(caps.Platform), strings.ToLower(rule.Platform)) {
		return false
	}
	if rule.UserID != 0 {
		if user == nil || user.ID != rule.UserID {
			return false
		}
	}
	// A rule with no matchers at all would match everything; require
	// at least one matcher to avoid surprises from empty config rows.
	return rule.Protocol != "" || rule.Product != "" || rule.Platform != "" || rule.UserID != 0
}
