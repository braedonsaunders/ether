package resolver

import (
	"math"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// uhdDiscMaxMBPerMin is the most an untouched UHD Blu-ray can average
// (the format tops out near 144 Mbps for video and audio together).
const uhdDiscMaxMBPerMin = 1100

// effectiveMaxSizeMBPerMin is a profile's size ceiling for one release. A
// ceiling already sized for remuxes (400 MB/min and up, 1080p remuxes run
// 200-300) accepts every UHD disc remux or encode up to what the disc
// format can hold: 4K remuxes average 600-800 MB/min, so the default 4K
// profile's 600 would otherwise turn every one of them away. A lower
// ceiling is a deliberate bandwidth or storage limit and stands.
func effectiveMaxSizeMBPerMin(profileMax float64, info core.ReleaseInfo) float64 {
	if profileMax >= 400 && info.Height >= 2160 && (info.Source == core.SourceRemux || info.Source == core.SourceBluRay) {
		return max(profileMax, uhdDiscMaxMBPerMin)
	}
	return profileMax
}

// bitrateFloorMbps is the average bitrate below which a release of this
// resolution and source is a starved re-encode rather than what its name
// claims: a "2160p" BluRay rip at 5 Mbps looks worse than a good 1080p.
// Untouched web streams are allowed less (services ship efficient HEVC);
// 0 means no floor applies.
func bitrateFloorMbps(info core.ReleaseInfo, animation bool) float64 {
	var floor float64
	switch {
	case info.Height >= 2160:
		switch info.Source {
		case core.SourceRemux:
			floor = 25
		case core.SourceBluRay:
			floor = 12
		case core.SourceWEBDL:
			floor = 5
		case core.SourceHDTV:
			floor = 10
		default:
			floor = 7
		}
	case info.Height >= 1080:
		switch info.Source {
		case core.SourceRemux:
			floor = 15
		case core.SourceBluRay:
			floor = 5
		case core.SourceHDTV:
			floor = 3
		default:
			floor = 2
		}
	case info.Height >= 720:
		floor = 1
	default:
		return 0
	}
	if animation {
		// Flat colour compresses far better than live action.
		floor /= 2
	}
	return floor
}

// starvedPenalty drops a release below its floor under a well-encoded
// release one resolution down, HDR, codec and audio bonuses included: a
// starved "4K" keeps no advantage from its label.
func starvedPenalty(height int) float64 {
	switch {
	case height >= 2160:
		return 60
	case height >= 1080:
		return 30
	default:
		return 10
	}
}

// averageMbps is a release's average bitrate over the title's runtime.
func averageMbps(size int64, t *core.Title) (float64, bool) {
	if size <= 0 || t == nil || t.Runtime <= 0 {
		return 0, false
	}
	return float64(size) * 8 / t.Runtime.Seconds() / 1e6, true
}

// bitrateScore rates how well a release's bitrate serves its resolution:
// a penalty below the floor, and up to 8 points for twice to four times
// the floor, so a fuller encode beats a starved one of the same name.
func bitrateScore(rel core.Release, t *core.Title) float64 {
	mbps, ok := averageMbps(rel.Size, t)
	if !ok {
		return 0
	}
	floor := bitrateFloorMbps(rel.Info, isAnimation(t))
	if floor <= 0 {
		return 0
	}
	if mbps < floor {
		return -starvedPenalty(rel.Info.Height)
	}
	return min(8, 4*math.Log2(mbps/floor))
}

func isAnimation(t *core.Title) bool {
	if t == nil {
		return false
	}
	for _, g := range t.Genres {
		if g == "Animation" {
			return true
		}
	}
	return isAnime(t)
}

// sameTier reports releases of one tier for hedged validation: same height
// and HDR kind (qualityTier), both above or both below their bitrate floor,
// both in an accepted audio language or both not, and equal on any moved
// score-tail ladder steps.
func sameTier(a, b candidate) bool {
	return qualityTier(a.rel.Info) == qualityTier(b.rel.Info) &&
		a.starved == b.starved &&
		a.langOK == b.langOK &&
		a.langWanted == b.langWanted &&
		a.quality.movedEqual(b.quality)
}

// sameQuality reports releases that are interchangeable for source
// routing: one policy and quality tier (sameTier) and the same kind of
// source, since a remux is not a re-encode, nor a disc encode a web file.
// A faster or preferred source never swaps one for another.
func sameQuality(a, b candidate) bool {
	return releasepolicy.Equivalent(a.policy, b.policy) &&
		sameTier(a, b) &&
		sourceClass(a.rel.Info.Source) == sourceClass(b.rel.Info.Source)
}

func sourceClass(s core.Source) string {
	switch s {
	case core.SourceRemux:
		return "remux"
	case core.SourceBluRay:
		return "disc"
	case core.SourceWEBDL, core.SourceWEBRip:
		return "web"
	}
	return "other"
}

// qualityRank holds a release's per-dimension ranks for its profile's
// ranking ladder. Higher is better; the zero value ranks with unknowns.
type qualityRank struct {
	height, source, hdr    int
	codec, audio           int
	size                   int64
	movedCodec, movedAudio int
	movedSize              int64
}

func (a qualityRank) movedEqual(b qualityRank) bool {
	return a.movedCodec == b.movedCodec && a.movedAudio == b.movedAudio && a.movedSize == b.movedSize
}

func (a qualityRank) better(b qualityRank) bool {
	if a.height != b.height {
		return a.height > b.height
	}
	if a.source != b.source {
		return a.source > b.source
	}
	return a.hdr > b.hdr
}

func hasDefaultScoreTail(ladder []string) bool {
	n := len(ladder)
	return n >= 3 &&
		ladder[n-3] == core.RankVideoCodec &&
		ladder[n-2] == core.RankAudioFormat &&
		ladder[n-1] == core.RankSize
}

func audioFormatRank(info core.ReleaseInfo, profile core.QualityProfile) int {
	r := indexRank(profile.PreferredAudio, info.AudioCodec) * 2
	if info.Atmos {
		r++
	}
	return r
}

// rankQuality places a release in the profile's quality order. A release
// below its bitrate floor ranks one resolution step down: a starved
// "2160p" is not better than a good 1080p, HDR or not. Unknown heights
// rank lowest.
func rankQuality(rel core.Release, profile core.QualityProfile, starved bool) qualityRank {
	info := rel.Info
	h := info.Height
	if starved {
		switch {
		case h >= 2160:
			h = 1080
		case h >= 1080:
			h = 720
		case h >= 720:
			h = 480
		}
	}
	hdr := hdrRank(info, profile)
	if starved {
		hdr = 0 // a starved or upscaled encode keeps no HDR advantage either
	}
	codec := indexRank(profile.PreferredCodecs, info.VideoCodec)
	audio := audioFormatRank(info, profile)
	var size int64
	if profile.PreferLargest {
		size = sizedForEpisode(rel).Size
	}
	r := qualityRank{
		height: h,
		source: indexRank(profile.PreferredSources, info.Source),
		hdr:    hdr,
		codec:  codec,
		audio:  audio,
		size:   size,
	}
	if !hasDefaultScoreTail(profile.EffectiveRanking()) {
		r.movedCodec = codec
		r.movedAudio = audio
		r.movedSize = size
	}
	return r
}

// compareCandidates walks the profile's ranking ladder from top to bottom,
// breaking remaining ties by the soft score (profile score + custom formats +
// client compatibility). Fallback tiers on require rules always hold first so
// a fallback candidate never outranks a primary candidate.
func compareCandidates(a, b candidate, profile core.QualityProfile) bool {
	if a.policy.Fallback != b.policy.Fallback {
		return a.policy.Fallback < b.policy.Fallback
	}
	ladder := profile.EffectiveRanking()
	defaultTail := hasDefaultScoreTail(ladder)
	for _, step := range ladder {
		switch {
		case strings.HasPrefix(step, core.RankRulePrefix):
			id := strings.TrimPrefix(step, core.RankRulePrefix)
			if ap, bp := a.policy.PreferRule(id), b.policy.PreferRule(id); ap != bp {
				return ap > bp
			}
		case step == core.RankLanguage:
			// A release whose declared audio is only in languages nobody
			// asked for is a last resort, whatever its quality or fit.
			if a.langOK != b.langOK {
				return a.langOK
			}
		case step == core.RankConnection:
			// A previously working LAN remux must not outrank a source that
			// fits this client's slower connection. Unknown sizes remain usable
			// and an oversized source can still be transcoded as a fallback.
			if a.bandwidth != b.bandwidth {
				return a.bandwidth > b.bandwidth
			}
		case step == core.RankResolution:
			if a.quality.height != b.quality.height {
				return a.quality.height > b.quality.height
			}
			// On a foreign-language title, a release within this resolution tier
			// that explicitly declares the viewer's audio language (or MULTI)
			// ranks ahead of untagged original-language or bare .DUAL releases.
			if a.langWanted != b.langWanted {
				return a.langWanted
			}
		case step == core.RankReleaseType:
			if a.quality.source != b.quality.source {
				return a.quality.source > b.quality.source
			}
		case step == core.RankHDR:
			if a.quality.hdr != b.quality.hdr {
				return a.quality.hdr > b.quality.hdr
			}
		case step == core.RankVerified:
			if a.verified != b.verified {
				return a.verified
			}
		case step == core.RankVideoCodec:
			if !defaultTail && a.quality.codec != b.quality.codec {
				return a.quality.codec > b.quality.codec
			}
		case step == core.RankAudioFormat:
			if !defaultTail && a.quality.audio != b.quality.audio {
				return a.quality.audio > b.quality.audio
			}
		case step == core.RankSize:
			if !defaultTail && a.quality.size != b.quality.size {
				return a.quality.size > b.quality.size
			}
		}
	}
	return a.rel.Score > b.rel.Score
}

// hdrRank orders dynamic range the profile can use: Dolby Vision, then
// HDR, then SDR. A 2160p remux is a UHD disc, which carries HDR10 even
// when the name does not say so.
func hdrRank(info core.ReleaseInfo, profile core.QualityProfile) int {
	switch {
	case info.DolbyVision && profile.AllowDolbyVision:
		return 2
	case profile.AllowHDR && (info.HDR10 || info.DolbyVision || (info.Height >= 2160 && info.Source == core.SourceRemux)):
		return 1
	}
	return 0
}

// upscaleRe matches names that say they were upscaled or AI-enhanced.
// Frame-interpolated 60 fps copies of film and TV are made the same way.
var upscaleRe = regexp.MustCompile(`(?i)(^|[ ._\-\[(])(upscaled?|upscaler|uprez|upres|ai[ ._-]?(upscaled?|enhanced|upscale)|60fps|interpolated)($|[ ._\-\])])`)

// presumedUpscales marks the 2160p releases of a pool that are upscales
// rather than UHD sources: any that say so, and, when the pool has a
// 1080p remux but nothing from a UHD disc (a 2160p remux or a UHD-labelled
// Blu-ray), every 2160p "Blu-ray" or unlabelled file. Titles without a UHD
// release (most anime, older TV) get 4K files made from the 1080p disc;
// they are no better than that disc. Anime is mastered at 1080p, so there a 2160p
// web file without a UHD disc is a service-side upscale too; live-action
// web 4K is often a genuine 4K master and stays.
func presumedUpscales(rels []core.Release, anime bool) map[int]bool {
	hdRemux, uhdDisc := false, false
	for _, r := range rels {
		switch {
		case r.Info.Height == 1080 && r.Info.Source == core.SourceRemux:
			hdRemux = true
		case r.Info.Height >= 2160 && (r.Info.Source == core.SourceRemux || (r.Info.Source == core.SourceBluRay && uhdLabelled(r.Name))):
			uhdDisc = true
		}
	}
	out := map[int]bool{}
	for i, r := range rels {
		if r.Info.Height < 2160 {
			continue
		}
		if upscaleRe.MatchString(r.Name) || (hdRemux && !uhdDisc && (r.Info.Source == core.SourceBluRay || r.Info.Source == core.SourceUnknown || anime)) {
			out[i] = true
		}
	}
	return out
}

func uhdLabelled(name string) bool {
	return uhdRe.MatchString(name)
}

var uhdRe = regexp.MustCompile(`(?i)(^|[ ._\-\[(])uhd($|[ ._\-\])])`)

// indexRank is len(list)-i for v at index i, 0 when absent.
func indexRank[T comparable](list []T, v T) int {
	for i, x := range list {
		if x == v {
			return len(list) - i
		}
	}
	return 0
}
