package config

import (
	"encoding/json"
)

// Scope says where a setting lives when nodes form a cluster.
//
// A standalone server keeps every setting in config.json. In a cluster the
// shared part lives in the database (one document every node reads) and the
// node-local part stays in each node's config.json. The classification is
// expressed over the JSON keys of Config so that it can split and merge the
// documents without reflection; TestScopeClassifiesEveryField fails when a
// field is added without being classified here.
type Scope int

const (
	// Shared settings are identical on every node of a cluster.
	Shared Scope = iota
	// Local settings belong to one node: its listener, storage paths and
	// sizes, hardware, logging and database connection.
	Local
)

func (s Scope) String() string {
	if s == Local {
		return "local"
	}
	return "shared"
}

// sectionScopes classifies every top-level key of Config whose whole value
// shares one scope.
var sectionScopes = map[string]Scope{
	"frontends":     Shared,
	"frontendPorts": Local,
	"posters":       Shared,
	"sharedCache":   Shared,
	"availability":  Shared,
	"usenet":        Shared,
	"indexers":      Shared,
	"catalog":       Shared,
	"resolver":      Shared,
	"quality":       Shared,
	"trakt":         Shared,
	"simkl":         Shared,
	"openSubtitles": Shared,
	"llm":           Shared,
	"updates":       Shared, // incl. maxDrain: every node drains the same way
	"markers":       Shared,
	"personalize":   Shared,
	"setupComplete": Shared,
	// Spike serves files from this node's disk.
	"spike": Local,
	// Each node holds its own connection settings; the URL is sealed.
	"database": Local,
}

// mixedSections classifies, key by key, the sections that hold both shared
// and node-local settings.
var mixedSections = map[string]map[string]Scope{
	"server": {
		"trustedProxyCIDRs":  Local,
		"port":               Local,
		"bindAddress":        Local,
		"logLevel":           Local,
		"friendlyName":       Shared,
		"webFrameOrigins":    Shared,
		"machineIdentifier":  Shared, // one server identity for the cluster
		"defaultStreamLimit": Shared,
		"maxStreams":         Shared,
		// This node's part of account-wide budgets in a cluster.
		"clusterWeight": Local,
	},
	"plex": {
		"compatibilityVersion":       Shared,
		"latestCompatibilityVersion": Shared,
		"trackLatestVersion":         Shared,
		"disableVersionTracking":     Shared,
		"token":                      Shared,
		"clientIdentifier":           Shared,
		"ownerPlexId":                Shared,
		"ownerUsername":              Shared,
		"claimed":                    Shared,
		"remoteAccess":               Shared,
		"customUrls":                 Shared,
		"certHash":                   Shared,
		"secureConnections":          Shared,
		// Each node is reached through its own port forward.
		"useUpnp":    Local,
		"publicPort": Local,
		"publicHost": Local,
		// Paths on this node's disk (the certificate itself is shared
		// through the database when clustered).
		"certFile": Local,
		"keyFile":  Local,
	},
	"cache": {
		"storage":                Local,
		"dir":                    Local,
		"budgetBytes":            Local,
		"mode":                   Local,
		"preloadEnabled":         Local,
		"preloadBudgetBytes":     Local,
		"preloadMaxTitles":       Local,
		"preloadRateMiB":         Local,
		"preloadDuringPlayback":  Local,
		"preloadMinFreeBytes":    Local,
		"preloadSource":          Local,
		"preloadContent":         Local,
		"preloadHouseholdWeight": Local,
		"preloadHouseholdScope":  Local,
		"preloadEpisodesAhead":   Local,
		"preloadHistoryDays":     Local,
		"retentionDays":          Local,
		"replayBoostHours":       Local,
		"disableSpeculation":     Local,
		"detailWarmup":           Local,
		"detailBytes":            Local,
		"prefetchNextEpisode":    Shared,
		"prefetchBytes":          Shared,
		"keepInProgressWarm":     Shared,
		"nextEpisodeAt":          Shared,
		"inProgressPinMaxAge":    Shared,
		"inProgressPinPercent":   Shared,
	},
	"transcode": {
		"rtxVideo":             Local, // executable, GPU and enhancement resource limits are node-specific
		"ffmpegPath":           Local,
		"ffprobePath":          Local,
		"hwAccel":              Local,
		"vaapiDevice":          Local,
		"tempDir":              Local,
		"maxTranscodes":        Local, // sized to this node's hardware
		"remoteMaxBitrateKbps": Shared,
		"fastStart":            Shared,
		"fastStartSegments":    Shared,
		"fastStartSegment":     Shared,
		"fastStartMaxHeight":   Shared,
		"abrLadder":            Shared,
		"vbvReservoir":         Shared,
		"vbvBufferSeconds":     Shared,
		"spikeGovernor":        Shared,
		"spikeMarginPercent":   Shared,
	},
	"downloads": {
		"enabled":              Shared,
		"allowFriends":         Shared,
		"maxConcurrentPerUser": Shared,
		"expiry":               Shared,
		"maxHeight":            Shared,
		// Remuxes run on and are stored by the node serving them.
		"maxConcurrent":   Local,
		"diskBudgetBytes": Local,
	},
	"indexerSites": {
		// One trusted definitions source for the cluster; FlareSolverr
		// usually runs beside each node (http://localhost:8191).
		"definitionsUrl":  Shared,
		"flareSolverrUrl": Local,
	},
	"extras": {
		"enabled":      Shared,
		"prewarmBytes": Shared,
		// The resolver tool is a binary on this node's disk.
		"resolverCommand": Local,
		"resolverArgs":    Local,
		"resolverTimeout": Local,
	},
	"debrid": {
		// The owner's accounts and acceptance policy are one decision for
		// the cluster.
		"providers":      Shared,
		"cachedOnly":     Shared,
		"removeUncached": Shared,
		// Range-request tuning follows this node's network.
		"chunkBytes":      Local,
		"readAheadChunks": Local,
		"maxConnections":  Local,
	},
}

// ScopeOf reports the scope of a setting by its JSON path: a top-level key
// ("usenet") or a section and key ("server.port"). Unknown paths are
// shared, the safe default for a cluster (one value everywhere).
func ScopeOf(path string) Scope {
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			if sub, ok := mixedSections[path[:i]]; ok {
				return sub[path[i+1:]]
			}
			return sectionScopes[path[:i]]
		}
	}
	return sectionScopes[path]
}

// splitDoc divides a decoded config document into its shared and local
// parts. Keys the classification does not know (from a newer or older
// version) are kept on the shared side.
func splitDoc(doc map[string]any) (shared, local map[string]any) {
	shared, local = map[string]any{}, map[string]any{}
	for k, v := range doc {
		sub, mixed := mixedSections[k]
		if !mixed {
			if sectionScopes[k] == Local {
				local[k] = v
			} else {
				shared[k] = v
			}
			continue
		}
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		sh, lo := map[string]any{}, map[string]any{}
		for sk, sv := range obj {
			if sub[sk] == Local {
				lo[sk] = sv
			} else {
				sh[sk] = sv
			}
		}
		shared[k], local[k] = sh, lo
	}
	return shared, local
}

// overlayShared replaces every shared setting of dst with shared's: shared
// sections are replaced whole and, in mixed sections, every shared key is
// removed before shared's are copied, so a setting absent from shared (an
// omitempty zero value) is cleared rather than left stale. Node-local keys
// of dst are untouched.
func overlayShared(dst, shared map[string]any) {
	for k := range dst {
		if _, mixed := mixedSections[k]; !mixed && sectionScopes[k] != Local {
			delete(dst, k)
		}
	}
	for k, sub := range mixedSections {
		target, _ := dst[k].(map[string]any)
		if target == nil {
			target = map[string]any{}
			dst[k] = target
		}
		for sk := range target {
			if sub[sk] != Local {
				delete(target, sk)
			}
		}
	}
	for k, v := range shared {
		sub, mixed := mixedSections[k]
		if !mixed {
			if sectionScopes[k] != Local {
				dst[k] = v
			}
			continue
		}
		obj, _ := v.(map[string]any)
		target := dst[k].(map[string]any)
		for sk, sv := range obj {
			if sub[sk] != Local {
				target[sk] = sv
			}
		}
	}
}

// overlayDeep copies src over dst, merging objects one level deep (section
// keys) and replacing everything else.
func overlayDeep(dst, src map[string]any) {
	for k, v := range src {
		if obj, ok := v.(map[string]any); ok {
			if target, ok := dst[k].(map[string]any); ok {
				for sk, sv := range obj {
					target[sk] = sv
				}
				continue
			}
		}
		dst[k] = v
	}
}

func toDoc(c Config) map[string]any {
	b, _ := json.Marshal(c)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	return doc
}

func fromDoc(doc map[string]any) (Config, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return Config{}, err
	}
	var c Config
	err = json.Unmarshal(b, &c)
	return c, err
}

// SharedJSON is the cluster-shared part of c as JSON (unsealed).
func SharedJSON(c Config) []byte {
	shared, _ := splitDoc(toDoc(c))
	b, _ := json.Marshal(shared)
	return b
}

// LocalJSON is the node-local part of c as JSON (unsealed).
func LocalJSON(c Config) []byte {
	_, local := splitDoc(toDoc(c))
	b, _ := json.Marshal(local)
	return b
}

// MergeShared returns local with every shared setting taken from the
// shared JSON document (as produced by SharedJSON). Shared settings the
// document lacks (written by an older version) take their defaults;
// node-local keys in the document are ignored.
func MergeShared(local Config, sharedJSON []byte) (Config, error) {
	var shared map[string]any
	if err := json.Unmarshal(sharedJSON, &shared); err != nil {
		return Config{}, err
	}
	base, _ := splitDoc(toDoc(Defaults("")))
	overlayDeep(base, shared)
	doc := toDoc(local)
	overlayShared(doc, base)
	return fromDoc(doc)
}
