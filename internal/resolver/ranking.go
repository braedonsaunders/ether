package resolver

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// The ranking is Radarr's and Sonarr's DownloadDecisionComparer with Ether's
// streaming context in front of it and a few tie-breaks after it
// (docs/quality-ranking.md):
//
//	gates       Only-play fallback tier, accepted audio language, fits the connection
//	standard    quality position, revision, custom format score, preferred
//	            protocol, indexer priority, seeders and peers (torrents) or age
//	            (Usenet), size
//	tie-breaks  played fine recently, indexer reliability, a single episode
//	            over a season pack
//
// Sonarr's season-pack, episode-count and first-episode keys are left out:
// Sonarr prefers packs because it downloads whole seasons, while Ether
// streams one episode. A pack competes with single-episode releases like
// any release, and its total size, which is not the episode's, never
// compares. Source routing (routing.go) then puts the fastest-starting
// source first among releases this order leaves equivalent, packs
// included.

// rankKeys are a candidate's standard comparer keys.
type rankKeys struct {
	position    int     // place in the quality order: higher is better, 0 unlisted
	height      int     // graded height (a starved or upscaled release one step down)
	revision    int     // Proper/Repack, when the profile prefers revisions
	protocol    bool    // the owner's preferred source protocol
	pack        bool    // a season pack streamed for one episode
	priority    int     // indexer priority: lower is better
	torrent     bool    // a torrent (seeders and peers compare)
	usenet      bool    // a Usenet post (age compares)
	seeders     float64 // round(log10(seeders))
	peers       float64 // round(log10(peers))
	age         float64 // CompareAgeIfUsenet's bucket
	hasAge      bool
	size        float64 // size in 200 MB steps
	reliability float64 // learned indexer grab success
}

// compareCandidates reports whether a ranks before b.
func compareCandidates(a, b candidate) bool {
	// Shared names never outrank evidence identifying the requested show.
	if a.identityWeak != b.identityWeak {
		return !a.identityWeak
	}
	// Gates.
	if a.policy.Fallback != b.policy.Fallback {
		return a.policy.Fallback < b.policy.Fallback
	}
	if a.langOK != b.langOK {
		return a.langOK
	}
	if a.bandwidth != b.bandwidth {
		return a.bandwidth > b.bandwidth
	}
	// Radarr/Sonarr.
	x, y := a.keys, b.keys
	if x.position != y.position {
		return x.position > y.position
	}
	if x.revision != y.revision {
		return x.revision > y.revision
	}
	if a.cf != b.cf {
		return a.cf > b.cf
	}
	if x.protocol != y.protocol {
		return x.protocol
	}
	if x.priority != y.priority {
		return x.priority < y.priority
	}
	if x.torrent && y.torrent {
		if x.seeders != y.seeders {
			return x.seeders > y.seeders
		}
		if x.peers != y.peers {
			return x.peers > y.peers
		}
	}
	if x.usenet && y.usenet && x.hasAge && y.hasAge && x.age != y.age {
		return x.age > y.age
	}
	// A pack's size is the whole season's, so it compares with no
	// single-episode release.
	if x.pack == y.pack && x.size != y.size {
		return x.size > y.size
	}
	// Tie-breaks.
	if a.identityConflict != b.identityConflict {
		return !a.identityConflict
	}
	if a.verified != b.verified {
		return a.verified
	}
	if x.reliability != y.reliability {
		return x.reliability > y.reliability
	}
	// With nothing else to tell them apart, the single-episode release
	// goes first: it is the episode's own file, not a season to open.
	return !x.pack && y.pack
}

// rankContext is what the keys read besides the release.
type rankContext struct {
	profile        core.QualityProfile
	title          *core.Title
	now            time.Time
	preferSource   string // the owner's source preference: PreferUsenet, PreferDebrid or "" for none
	preferRevision bool
	priority       map[string]int // indexer id → priority
	reliability    func(indexer string) float64
}

// keysFor computes a candidate's comparer keys.
func keysFor(rel core.Release, starved bool, rc rankContext) rankKeys {
	info := rel.Info
	k := rankKeys{height: gradedHeight(info.Height, starved)}
	k.position = qualityPosition(rc.profile.QualityOrder, info, k.height)
	if rc.preferRevision {
		k.revision = revisionRank(info)
	}
	if rc.preferSource != "" {
		k.protocol = sourceName(protocolOf(rel)) == rc.preferSource
	}
	if rc.title != nil && (rc.title.Type == core.TypeEpisode || rc.title.Key.Kind() == core.KindEpisode) {
		k.pack = info.FullSeason
	}
	if p, ok := rc.priority[rel.IndexerID]; ok {
		k.priority = p
	}
	switch rel.Protocol {
	case core.ProtocolTorrent:
		k.torrent = true
		k.seeders = log10Rounded(rel.Seeders)
		k.peers = log10Rounded(rel.Peers)
	case core.ProtocolUsenet:
		k.usenet = true
		if !rel.PubDate.IsZero() {
			k.hasAge = true
			k.age = usenetAgeScore(rel.PubDate, rc.now)
		}
	}
	const step = 200 << 20
	k.size = math.Round(float64(rel.Size) / step)
	if rc.reliability != nil {
		k.reliability = rc.reliability(rel.Indexer)
	}
	return k
}

// gradedHeight is the height a release ranks at: one resolution step down
// for a release below its bitrate floor or a presumed upscale.
func gradedHeight(h int, starved bool) int {
	if !starved {
		return h
	}
	switch {
	case h >= 2160:
		return 1080
	case h >= 1080:
		return 720
	case h >= 720:
		return 480
	}
	return h
}

// releaseQualityName is a release's quality in a quality order's terms
// ("webdl-1080", "unknown-2160") at height h; "" when it names no height.
// A DVD rip that names none is a 480p DVD, as Radarr and Sonarr grade it.
func releaseQualityName(info core.ReleaseInfo, h int) string {
	source := string(info.Source)
	if info.Source == core.SourceUnknown {
		source = "unknown"
	}
	if h == 0 {
		if info.Source != core.SourceDVD {
			return ""
		}
		h = 480
	}
	return fmt.Sprintf("%s-%d", source, h)
}

// qualityPosition is the release's place in the quality order (Radarr and
// Sonarr's QualityProfile.GetIndex): higher is better, qualities in one item
// tie, and a quality no item lists ranks 0.
func qualityPosition(order []core.QualityItem, info core.ReleaseInfo, height int) int {
	q := releaseQualityName(info, height)
	if q == "" {
		return 0
	}
	for i, item := range order {
		if slices.Contains(item.Qualities, q) {
			return len(order) - i
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

// preferredSource is the owner's explicit source preference, which stands
// in for a Radarr/Sonarr delay profile's preferred protocol: PreferUsenet,
// PreferDebrid, or "" for automatic.
func preferredSource(mode string) string {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case PreferUsenet, PreferDebrid:
		return m
	}
	return ""
}
