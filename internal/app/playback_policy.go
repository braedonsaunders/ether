package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/retryafter"
	"github.com/braedonsaunders/ether/internal/transcode"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// reconcileProwlarrIndexers upserts discovered indexers into existing and,
// only after a discovery treated as authoritative, removes previously
// imported indexers from the SAME Prowlarr instance/base (IDs with prefix,
// e.g. "prowlarr-<scope>-") that are absent from discovered — i.e. removed
// or disabled at the source, or still present but no longer importable
// (unknown protocol: sync removes its local copy; discovery's warning
// explains the skip). "Treated as authoritative" means HTTP 200 with a
// valid JSON body: the Prowlarr API offers no proof of completeness (no
// total/count to check against), so the sync trusts that list, like
// Prowlarr's own FullSync does — it is not a proven-complete discovery.
// Safety rules, mirroring Prowlarr upstream where removal is an explicit
// sync action, never an aggressive side effect:
//   - a discovery with neither discovered nor presentIDs, or an empty
//     prefix, never removes anything (a failed/empty response is not
//     proof that the source is empty; the handler also aborts before
//     this on any discovery error); presentIDs alone is likewise only
//     treated as authoritative (not proven complete), so the guard is
//     the composite len(discovered)>0 || len(presentIDs)>0;
//   - the "prowlarr-*" ID namespace is managed by this sync: an entry
//     hand-written into the config with a forged "prowlarr-<scope>-" ID
//     is indistinguishable from an imported one and enters the sync's
//     scope. Indexers outside that namespace (ordinary manual entries,
//     other origins) and prowlarr-* IDs from other Prowlarr
//     instances/bases are kept, except pre-normalisation orphans proven
//     to be this same instance (below);
//   - presentIDs (ProwlarrDiscovery.PresentIDs) does NOT protect an ID:
//     it only records what the treated-as-authoritative discovery
//     reported and supplies numeric suffixes for the legacy check.
//     Unknown-protocol copies are therefore removed.
//
// Legacy migration: an indexer minted before base-URL normalisation has
// an old-scope ID, so the exact-prefix rule above would keep it forever
// next to its re-imported twin. It is removed only when (i) its numeric
// suffix (after the last "-") equals a discovered/present suffix of this
// import and (ii) ProwlarrStoredIndexerMatchesPrefix proves its stored
// endpoint normalises to this instance. If (i) holds but the endpoint
// cannot be proven (unparseable/mismatched URL), it is kept and counted
// in staleLegacy so the handler can warn; a provably different endpoint
// is another instance and is kept silently.
//
// It returns the merged list, how many indexers were removed and how
// many unproven legacy indexers were kept. The result never aliases
// existing: the caller's slice is left untouched.
func reconcileProwlarrIndexers(existing, discovered []config.Indexer, prefix string, presentIDs []string) ([]config.Indexer, int, int) {
	removed, staleLegacy := 0, 0
	out := slices.Clone(existing)
	if prefix != "" && (len(discovered) > 0 || len(presentIDs) > 0) {
		found := make(map[string]bool, len(discovered))
		for _, x := range discovered {
			found[x.ID] = true
		}
		suffixes := make(map[string]bool, len(discovered)+len(presentIDs))
		for _, x := range discovered {
			if s, ok := prowlarrNumericSuffix(x.ID); ok {
				suffixes[s] = true
			}
		}
		for _, id := range presentIDs {
			if s, ok := prowlarrNumericSuffix(id); ok {
				suffixes[s] = true
			}
		}
		kept := make([]config.Indexer, 0, len(existing))
		for _, x := range existing {
			switch {
			case strings.HasPrefix(x.ID, prefix):
				if !found[x.ID] {
					removed++
					continue
				}
			case strings.HasPrefix(x.ID, "prowlarr-"):
				suffix, ok := prowlarrNumericSuffix(x.ID)
				if ok && suffixes[suffix] {
					if matches, determinable := newznab.ProwlarrStoredIndexerMatchesPrefix(x, prefix); matches {
						removed++
						continue
					} else if !determinable {
						staleLegacy++
					}
				}
			}
			kept = append(kept, x)
		}
		out = kept
	}
	byID := map[string]int{}
	for i, x := range out {
		byID[x.ID] = i
	}
	for _, x := range discovered {
		if i, ok := byID[x.ID]; ok {
			out[i] = x
		} else {
			byID[x.ID] = len(out)
			out = append(out, x)
		}
	}
	return out, removed, staleLegacy
}

// prowlarrNumericSuffix returns the numeric suffix after the last "-"
// of a scoped indexer ID ("prowlarr-<scope>-42" -> "42").
func prowlarrNumericSuffix(id string) (string, bool) {
	idx := strings.LastIndex(id, "-")
	if idx < 0 {
		return "", false
	}
	s := id[idx+1:]
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return s, true
}

func (a *App) playbackPolicyHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /policy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"policies": transcode.ClientPolicies()})
	})
	m.HandleFunc("POST /prowlarr", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Config config.Prowlarr `json:"config"`
			Import bool            `json:"import"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil {
			http.Error(w, "invalid Prowlarr settings", 400)
			return
		}
		report, err := newznab.DiscoverProwlarrReport(r.Context(), in.Config, newznab.Options{Logger: a.log})
		if err != nil {
			// A failed discovery changes nothing (no import, no removal);
			// a 429 keeps a validated Retry-After for the caller (never a
			// raw/malformed/past value from upstream).
			var rl *newznab.RateLimitError
			if errors.As(err, &rl) {
				if h := prowlarrRetryAfterHeader(rl.RetryAfter); h != "" {
					w.Header().Set("Retry-After", h)
				}
			}
			http.Error(w, "Prowlarr discovery failed", 502)
			return
		}
		indexers := report.Indexers
		removed, staleLegacy := 0, 0
		if in.Import {
			// Reaching here means discovery returned HTTP 200 with a valid
			// body and is treated as authoritative (the API offers no proof
			// of completeness; sync trusts it, like Prowlarr's FullSync);
			// only then may stale same-scope imports be removed.
			err = a.cfg.Update(func(c *config.Config) error {
				c.Indexers, removed, staleLegacy = reconcileProwlarrIndexers(c.Indexers, indexers, newznab.ProwlarrIDPrefix(in.Config), report.PresentIDs)
				return nil
			})
			if err != nil {
				http.Error(w, "could not save indexers", 500)
				return
			}
		}
		warnings := slices.Clone(report.Warnings)
		if staleLegacy > 0 {
			warnings = append(warnings, fmt.Sprintf("stale prowlarr indexer from previous import spelling: %d kept because its stored endpoint could not be proven to be this instance; remove it manually if it duplicates an imported indexer", staleLegacy))
		}
		for i := range indexers {
			indexers[i].APIKey = admin.RedactedSecret
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"indexers": indexers, "imported": in.Import, "removed": removed, "staleProwlarrIndexers": staleLegacy, "skippedUnknownProtocol": report.SkippedUnknownProtocol, "warnings": warnings})
	})
	return m
}

// prowlarrRetryAfterHeader validates a Retry-After value before it is
// propagated to the caller: retryafter.Parse rejects empty, malformed and
// past values, and the delay is capped at newznab.MaxRateLimitPause (the
// Newznab client's shared bound) so a bogus upstream value is never
// forwarded verbatim. Returns "" when there is nothing valid to send.
func prowlarrRetryAfterHeader(v string) string {
	d, ok := retryafter.Parse(v, time.Now(), newznab.MaxRateLimitPause)
	if !ok {
		return ""
	}
	return strconv.FormatInt(int64(d/time.Second), 10)
}

func validatePlaybackConfig(c config.Config) []admin.FieldError {
	var errs []admin.FieldError
	if _, err := resolver.NewFormatScorer(c.Resolver.CustomFormats, c.Resolver.CustomFormatScores); err != nil {
		errs = append(errs, admin.FieldError{Field: "resolver.customFormats", Message: err.Error()})
	}
	if c.Resolver.HealthRace < 0 || c.Resolver.HealthRace > 4 {
		errs = append(errs, admin.FieldError{Field: "resolver.healthRace", Message: "must be 0 through 4"})
	}
	if c.Plex.CompatibilityVersion != "" && !plextv.ValidPMSVersion(c.Plex.CompatibilityVersion) {
		errs = append(errs, admin.FieldError{Field: "plex.compatibilityVersion", Message: "must be a valid public PMS version"})
	}
	return errs
}
