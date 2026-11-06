package resolver

import (
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// RankingCandidate holds the evidence already computed for a candidate. Keys
// are indexed by the complete plugin step ID and higher keys rank first.
type RankingCandidate struct {
	Index                                         int
	Release                                       core.Release
	Policy                                        releasepolicy.Result
	Verified, LanguageOK, LanguageWanted, Starved bool
	Bandwidth                                     int
	CustomFormatScore                             int // pure custom format score, used by Servarr evaluation mode
	PluginKeys                                    map[string]int64
}

// SortRankingCandidates applies the same ladder used by playback. The sort is
// stable, so equal candidates retain their input order.
func SortRankingCandidates(candidates []RankingCandidate, profile core.QualityProfile) {
	sort.SliceStable(candidates, func(i, j int) bool {
		convert := func(c RankingCandidate) candidate {
			return candidate{rel: c.Release, policy: c.Policy, verified: c.Verified, langOK: c.LanguageOK, langWanted: c.LanguageWanted, starved: c.Starved, bandwidth: c.Bandwidth, quality: rankQuality(c.Release, profile, c.Starved), pluginKeys: c.PluginKeys, cf: c.CustomFormatScore}
		}
		return compareCandidates(convert(candidates[i]), convert(candidates[j]), profile)
	})
}
