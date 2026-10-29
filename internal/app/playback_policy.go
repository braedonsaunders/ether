package app

import (
	"encoding/json"
	"errors"
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
			// Sync is the explicit reconciliation opt-in: only with
			// sync=true does a preview compute removals, and only with
			// import=true + sync=true are they applied. Without it the
			// import stays additive, exactly as before.
			Sync bool `json:"sync"`
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
		warnings := slices.Clone(report.Warnings)
		// Sync reconciliation is guarded: an empty/all-disabled source
		// (no enabled indexers at all) or an incomplete discovery (some
		// enabled indexers skipped for an unknown protocol) never
		// removes anything — disappearance may be transient or partial.
		canRemove := false
		if in.Sync {
			switch {
			case len(report.PresentIDs) == 0:
				warnings = append(warnings, "source reported no enabled indexers; nothing removed")
			case report.SkippedUnknownProtocol > 0:
				warnings = append(warnings, "sync removed nothing: discovery is incomplete (unsupported-protocol indexers were skipped); nothing removed")
			default:
				canRemove = true
			}
		}
		var removals, keptEdited []config.Indexer
		removed := 0
		if in.Import {
			// Without sync: additive only — upsert the discovered
			// indexers and NEVER remove absent ones. With sync=true the
			// removals computed by prowlarrSyncPlan are applied in the
			// same update, so upserts and removals land together.
			err = a.cfg.Update(func(c *config.Config) error {
				byID := map[string]int{}
				for i, x := range c.Indexers {
					byID[x.ID] = i
				}
				for _, x := range indexers {
					if i, ok := byID[x.ID]; ok {
						c.Indexers[i] = x
					} else {
						byID[x.ID] = len(c.Indexers)
						c.Indexers = append(c.Indexers, x)
					}
				}
				if in.Sync {
					var planned []config.Indexer
					planned, keptEdited = prowlarrSyncPlan(c.Indexers, in.Config, report)
					if canRemove {
						removals = planned
						drop := make(map[string]bool, len(removals))
						for _, x := range removals {
							drop[x.ID] = true
						}
						c.Indexers = slices.DeleteFunc(c.Indexers, func(x config.Indexer) bool { return drop[x.ID] })
						removed = len(removals)
					}
				}
				return nil
			})
			if err != nil {
				http.Error(w, "could not save indexers", 500)
				return
			}
		} else if in.Sync {
			// Sync preview: compute and list removals, change nothing.
			var planned []config.Indexer
			planned, keptEdited = prowlarrSyncPlan(a.cfg.Get().Indexers, in.Config, report)
			if canRemove {
				removals = planned
			}
		}
		for i := range indexers {
			indexers[i].APIKey = admin.RedactedSecret
		}
		resp := map[string]any{"indexers": indexers, "imported": in.Import, "skippedUnknownProtocol": report.SkippedUnknownProtocol, "warnings": warnings}
		if in.Sync {
			// Only an explicit sync response carries removal fields, so
			// the plain preview/import contract is unchanged.
			resp["removals"] = redactedIndexers(removals)
			resp["removed"] = removed
			resp["keptEdited"] = redactedIndexers(keptEdited)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return m
}

// prowlarrSyncPlan lists what an explicit sync would reconcile away:
// same-scope stored indexers (ID under this instance's normalised
// prefix) that the source no longer reports as enabled — absent from
// the discovery or disabled there, both of which keep them out of
// PresentIDs. Same-scope entries the owner edited locally (see
// newznab.ProwlarrLocallyEdited for the exact pattern rule) are never
// planned for removal; they are returned as keptEdited instead. Entries
// outside the scope (manual or from another Prowlarr instance) are
// never touched. The caller decides whether the plan may be applied:
// an empty/all-disabled or incomplete discovery must not remove.
func prowlarrSyncPlan(stored []config.Indexer, cfg config.Prowlarr, report newznab.ProwlarrDiscovery) (removals, keptEdited []config.Indexer) {
	removals = []config.Indexer{}
	keptEdited = []config.Indexer{}
	prefix := newznab.ProwlarrIDPrefix(cfg)
	if prefix == "" {
		return removals, keptEdited
	}
	present := make(map[string]bool, len(report.PresentIDs))
	for _, id := range report.PresentIDs {
		present[id] = true
	}
	for _, x := range stored {
		if !strings.HasPrefix(x.ID, prefix) || present[x.ID] {
			continue
		}
		if newznab.ProwlarrLocallyEdited(cfg, x) {
			keptEdited = append(keptEdited, x)
		} else {
			removals = append(removals, x)
		}
	}
	return removals, keptEdited
}

// redactedIndexers copies indexers for a response with secrets hidden,
// returning an empty (non-nil) slice so sync fields encode as [].
func redactedIndexers(in []config.Indexer) []config.Indexer {
	out := make([]config.Indexer, 0, len(in))
	for _, x := range in {
		x.APIKey = admin.RedactedSecret
		out = append(out, x)
	}
	return out
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
