package resolver

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// The ranking is Radarr's and Sonarr's DownloadDecisionComparer with Ether's
// streaming context in front of it and a few tie-breaks after it
// (docs/quality-ranking.md):
//
//	gates       Only-play fallback tier, accepted audio language, fits the connection
//	standard    quality position, revision, custom format score, preferred
//	            protocol, soft source-delivery hint, indexer priority, seeders
//	            and peers (torrents) or age (Usenet), size
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
	sourceBand  int     // priority band when any Stremio add-on keeps its own order: lower first
	addonOrder  int     // 1-based position in the add-on's response when UseAddonOrder is on; 0 otherwise
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
	// Releases with confirmed required audio languages always precede releases
	// whose audio tracks still require media-probe verification.
	if a.audioUnverified != b.audioUnverified {
		return !a.audioUnverified
	}
	// A file the owner already has plays before any release Ether would
	// fetch, whatever their qualities (Resolver.PreferLocalFiles).
	if a.localFirst != b.localFirst {
		return a.localFirst
	}
	// Soft profile preferences precede the ordinary quality comparer.
	if a.belowScore != b.belowScore {
		return !a.belowScore
	}
	if a.belowScore && a.counted != b.counted {
		return a.counted > b.counted
	}
	if a.fallback != b.fallback {
		return !a.fallback
	}
	// Unlisted lower qualities still prefer the closest resolution.
	if a.fallback && b.fallback && a.keys.height != b.keys.height {
		return a.keys.height > b.keys.height
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
	// Prefer wanted audio/subtitle tracks only within the same gate tier so a
	// track match cannot override local-first, quality, policy, or bandwidth gates.
	if a.trackPreference != b.trackPreference {
		return a.trackPreference > b.trackPreference
	}
	// Source bands and add-on order (when any Stremio add-on keeps its own
	// result order, each such add-on forms its own priority band and keeps its
	// response order inside that band).
	x, y := a.keys, b.keys
	if x.sourceBand != y.sourceBand {
		return x.sourceBand < y.sourceBand
	}
	if x.addonOrder > 0 && y.addonOrder > 0 {
		if x.addonOrder != y.addonOrder {
			return x.addonOrder < y.addonOrder
		}
		return !x.pack && y.pack
	}
	// Radarr/Sonarr.
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
	if a.langRank == b.langRank && a.delivery != b.delivery {
		return a.delivery > b.delivery
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
	profile           core.QualityProfile
	title             *core.Title
	now               time.Time
	preferSource      string // the owner's source preference: PreferUsenet, PreferDebrid or "" for none
	preferRevision    bool
	preferLocal       bool            // the owner plays granted local files first (Resolver.PreferLocalFiles)
	priority          map[string]int  // indexer id → priority
	addonOrder        map[string]bool // indexer id → keep add-on result order
	sourceBand        map[string]int  // indexer id → priority band (nil when no source uses add-on order)
	defaultSourceBand int             // band for sources outside cfg.Indexers (e.g. plugin add-ons)
	reliability       func(indexer string) float64
}

// computeSourceBands partitions enabled indexers into priority bands whenever
// at least one enabled Stremio add-on keeps its own result order.
//
// Walking enabled indexers in ascending Priority order (ties keep config slice
// order): each Stremio add-on with UseAddonOrder gets its own band, while
// contiguous profile-ranked sources share a band so they still compete by
// quality profile. When no enabled Stremio add-on uses UseAddonOrder, every
// band is 0 and ranking is unchanged.
func computeSourceBands(indexers []config.Indexer) (addonOrder map[string]bool, bands map[string]int, defaultBand int) {
	type entry struct {
		id         string
		priority   int
		idx        int
		addonOrder bool
	}
	var enabled []entry
	anyAddonOrder := false
	for i, ix := range indexers {
		if !ix.Enabled || ix.ID == "" {
			continue
		}
		ao := ix.IsStremio() && ix.UseAddonOrder
		if ao {
			anyAddonOrder = true
		}
		enabled = append(enabled, entry{
			id:         ix.ID,
			priority:   ix.Priority,
			idx:        i,
			addonOrder: ao,
		})
	}
	if !anyAddonOrder {
		return nil, nil, 0
	}
	slices.SortFunc(enabled, func(a, b entry) int {
		if c := cmp.Compare(a.priority, b.priority); c != 0 {
			return c
		}
		return cmp.Compare(a.idx, b.idx)
	})
	addonOrder = make(map[string]bool)
	bands = make(map[string]int, len(enabled))
	band := -1
	lastWasProfile := false
	for _, e := range enabled {
		if e.addonOrder {
			band++
			addonOrder[e.id] = true
			bands[e.id] = band
			lastWasProfile = false
		} else {
			if !lastWasProfile {
				band++
				lastWasProfile = true
			}
			bands[e.id] = band
		}
	}
	// Sources not in cfg.Indexers (such as plugin-contributed add-on rows
	// appended at priority 1<<20) join the trailing profile band, or sit in a
	// band after the last add-on-ordered source.
	if lastWasProfile {
		defaultBand = band
	} else {
		defaultBand = band + 1
	}
	return addonOrder, bands, defaultBand
}

// keysFor computes a candidate's comparer keys.
func keysFor(rel core.Release, starved bool, rc rankContext) rankKeys {
	info := rel.Info
	k := rankKeys{height: gradedHeight(info.Height, starved)}
	if rc.sourceBand != nil {
		if b, ok := rc.sourceBand[rel.IndexerID]; ok {
			k.sourceBand = b
		} else {
			k.sourceBand = rc.defaultSourceBand
		}
	}
	if rc.addonOrder[rel.IndexerID] {
		k.addonOrder = rel.SourceOrder
	}
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

// prioritizeOwnerTracks restores track preference within each quality and
// delivery gate tier, keeping identity, required-audio, local-first, quality,
// policy, language, and bandwidth gates first.
func prioritizeOwnerTracks(cands []candidate) {
	if len(cands) < 2 || !slices.ContainsFunc(cands[1:], func(c candidate) bool {
		return c.trackPreference != cands[0].trackPreference
	}) {
		return
	}
	cmpBoolTrueFirst := func(a, b bool) int {
		if a == b {
			return 0
		}
		if a {
			return -1
		}
		return 1
	}
	slices.SortStableFunc(cands, func(a, b candidate) int {
		if c := cmpBoolTrueFirst(!a.identityWeak, !b.identityWeak); c != 0 {
			return c
		}
		if c := cmpBoolTrueFirst(!a.audioUnverified, !b.audioUnverified); c != 0 {
			return c
		}
		if c := cmpBoolTrueFirst(a.localFirst, b.localFirst); c != 0 {
			return c
		}
		if c := cmpBoolTrueFirst(!a.belowScore, !b.belowScore); c != 0 {
			return c
		}
		if a.belowScore {
			if c := cmp.Compare(b.counted, a.counted); c != 0 {
				return c
			}
		}
		if c := cmpBoolTrueFirst(!a.fallback, !b.fallback); c != 0 {
			return c
		}
		if a.fallback {
			if c := cmp.Compare(b.keys.height, a.keys.height); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(a.policy.Fallback, b.policy.Fallback); c != 0 {
			return c
		}
		if c := cmpBoolTrueFirst(a.langOK, b.langOK); c != 0 {
			return c
		}
		if c := cmp.Compare(b.bandwidth, a.bandwidth); c != 0 {
			return c
		}
		return cmp.Compare(b.trackPreference, a.trackPreference)
	})
}

func samePreferenceTier(a, b candidate) bool {
	return a.trackPreference == b.trackPreference && a.audioUnverified == b.audioUnverified && a.localFirst == b.localFirst && a.belowScore == b.belowScore && a.fallback == b.fallback &&
		(!a.belowScore || a.counted == b.counted) && (!a.fallback || a.keys.height == b.keys.height)
}

// grantedLocalFile reports a release a source plugin serves from a folder the
// owner granted it (the plugin:<id>:file:<path> GUID), as opposed to a
// stream or download it found elsewhere.
func grantedLocalFile(rel core.Release) bool {
	rest, ok := strings.CutPrefix(rel.GUID, "plugin:")
	return ok && rel.Protocol == "direct" && strings.Contains(rest, ":file:")
}

// localFirst reports a candidate PreferLocalFiles plays first: a granted local
// file outside every Never-play fallback tier, in an accepted audio language.
func localFirst(rc rankContext, rel core.Release, policyFallback int, langOK bool) bool {
	return rc.preferLocal && grantedLocalFile(rel) && policyFallback == 0 && langOK
}
