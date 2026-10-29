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
	return prof
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
