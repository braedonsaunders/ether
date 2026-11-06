package resolver

import (
	"math"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func cloneQualityOrder(order [][]string) [][]string {
	if order == nil {
		return nil
	}
	out := make([][]string, len(order))
	for i, g := range order {
		out[i] = append([]string(nil), g...)
	}
	return out
}

// qualityPosition is the release's position in the profile's quality order
// (Radarr/Sonarr QualityProfile.GetIndex): higher is better, qualities in one
// group tie, and a quality the profile does not list ranks 0.
func qualityPosition(order [][]string, info core.ReleaseInfo) int {
	for i, group := range order {
		for _, gate := range group {
			if qualityGateMatch(gate, info) {
				return len(order) - i
			}
		}
	}
	return 0
}

// revisionRank is Quality.Revision for ordering: a Proper or Repack outranks
// the original release of the same quality.
func revisionRank(info core.ReleaseInfo) int {
	if info.Proper || info.Repack {
		return 1
	}
	return 0
}

// usenetAgeScore is CompareAgeIfUsenet's bucket: newer is higher.
func usenetAgeScore(pub, now time.Time) float64 {
	hours := now.Sub(pub).Hours()
	days := math.Floor(hours / 24)
	switch {
	case hours < 1:
		return 1000
	case hours <= 24:
		return 100
	case days <= 7:
		return 10
	}
	return -math.Round(math.Log10(days))
}

func log10Rounded(n int) float64 {
	if n <= 0 {
		return 0
	}
	return math.Round(math.Log10(float64(n)))
}

// compareServarr orders candidates like Radarr's and Sonarr's
// DownloadDecisionComparer (src/NzbDrone.Core/DecisionEngine/
// DownloadDecisionComparer.cs):
//
//	Radarr: quality position, revision, custom format score, protocol,
//	        indexer priority, indexer flags, peers (torrent), age (usenet), size.
//	Sonarr: quality position, revision, custom format score, protocol,
//	        full season, episode count, episode number, indexer priority,
//	        peers (torrent), age (usenet), size.
//
// Not available in Ether and so skipped: protocol preference (delay
// profile), indexer priority and flags, episode count/number (Ether ranks
// one episode at a time), and the quality definition's preferred size (size
// falls back to Servarr's "larger is better", rounded to 200 MB).
//
// Ether's own playback safeguards stay, labelled: fallback tiers and the
// owner's Prefer rules, audio-language acceptability and the client's
// connection fit come first as gates; plugin steps, verified-source evidence
// and the soft score only break the ties Servarr leaves.
func compareServarr(a, b candidate, profile core.QualityProfile) bool {
	if a.policy.Fallback != b.policy.Fallback {
		return a.policy.Fallback < b.policy.Fallback
	}
	ladder := profile.EffectiveRanking()
	// Ether gates.
	for _, step := range ladder {
		switch {
		case strings.HasPrefix(step, core.RankRulePrefix):
			id := strings.TrimPrefix(step, core.RankRulePrefix)
			if ap, bp := a.policy.PreferRule(id), b.policy.PreferRule(id); ap != bp {
				return ap > bp
			}
		case step == core.RankLanguage:
			if a.langOK != b.langOK {
				return a.langOK
			}
		case step == core.RankConnection:
			if a.bandwidth != b.bandwidth {
				return a.bandwidth > b.bandwidth
			}
		}
	}
	// Servarr keys.
	if x, y := qualityPosition(profile.QualityOrder, a.rel.Info), qualityPosition(profile.QualityOrder, b.rel.Info); x != y {
		return x > y
	}
	if x, y := revisionRank(a.rel.Info), revisionRank(b.rel.Info); x != y {
		return x > y
	}
	if a.cf != b.cf {
		return a.cf > b.cf
	}
	if profile.ServarrApp() == AppSonarr && a.rel.Info.FullSeason != b.rel.Info.FullSeason {
		return a.rel.Info.FullSeason
	}
	torrent := a.rel.Protocol == core.ProtocolTorrent && b.rel.Protocol == core.ProtocolTorrent
	if torrent {
		if x, y := log10Rounded(a.rel.Seeders), log10Rounded(b.rel.Seeders); x != y {
			return x > y
		}
		if x, y := log10Rounded(a.rel.Peers), log10Rounded(b.rel.Peers); x != y {
			return x > y
		}
	}
	if a.rel.Protocol != core.ProtocolTorrent && b.rel.Protocol != core.ProtocolTorrent && a.hasAge && b.hasAge && a.age != b.age {
		return a.age > b.age
	}
	const round = 200 << 20
	if x, y := math.Round(float64(a.rel.Size)/round), math.Round(float64(b.rel.Size)/round); x != y {
		return x > y
	}
	// Ether tiebreaks.
	for _, step := range ladder {
		if core.IsPluginRankingStep(step) {
			if ak, bk := a.pluginKeys[step], b.pluginKeys[step]; ak != bk {
				return ak > bk
			}
		}
	}
	if a.verified != b.verified {
		return a.verified
	}
	return a.rel.Score > b.rel.Score
}
