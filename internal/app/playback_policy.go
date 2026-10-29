package app

import (
	"crypto/sha256"
	"encoding/hex"
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
			// ApprovedRemovals and Plan bind a sync apply to the exact
			// preview the owner confirmed: the removal IDs approved and
			// that preview's plan fingerprint. Both are required on a
			// sync apply; the server recomputes discovery + local state
			// and refuses (409, no mutation) when either diverged.
			ApprovedRemovals []string `json:"approvedRemovals,omitempty"`
			Plan             string   `json:"plan,omitempty"`
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
		if in.Sync {
			switch {
			case len(report.PresentIDs) == 0:
				warnings = append(warnings, "source reported no enabled indexers; nothing removed")
			case report.SkippedUnknownProtocol > 0:
				warnings = append(warnings, "sync removed nothing: discovery is incomplete (unsupported-protocol indexers were skipped); nothing removed")
			}
		}
		var plan prowlarrSyncPlan
		removed := 0
		if in.Import && in.Sync {
			// Sync apply is bound to its preview: without the approved
			// removal IDs and the plan fingerprint there is nothing to
			// verify against, so refuse before touching anything.
			if in.Plan == "" || in.ApprovedRemovals == nil {
				http.Error(w, "prowlarr sync apply requires a preview plan (approvedRemovals + plan); review again", http.StatusConflict)
				return
			}
			err = a.cfg.Update(func(c *config.Config) error {
				// Recompute the plan against the exact state being
				// mutated (fresh discovery above + c.Indexers here):
				// any source or local change since the preview changes
				// the fingerprint or the removal set and aborts with
				// no mutation at all (not even upserts).
				recomputed := computeProwlarrSyncPlan(c.Indexers, in.Config, report)
				if recomputed.fingerprint != in.Plan || !slices.Equal(recomputed.approvedRemovals, sortedStrings(in.ApprovedRemovals)) {
					return errProwlarrPlanStale
				}
				plan = recomputed
				kept := make(map[string]bool, len(plan.keptEdited))
				for _, x := range plan.keptEdited {
					kept[x.ID] = true
				}
				byID := map[string]int{}
				for i, x := range c.Indexers {
					byID[x.ID] = i
				}
				for _, x := range indexers {
					if i, ok := byID[x.ID]; ok {
						// Local-edit preservation also covers the
						// upsert: a stored entry the owner edited
						// (vs the generated one) is never overwritten
						// by a sync; it is reported in keptEdited.
						if kept[x.ID] {
							continue
						}
						c.Indexers[i] = x
					} else {
						byID[x.ID] = len(c.Indexers)
						c.Indexers = append(c.Indexers, x)
					}
				}
				if plan.canRemove {
					drop := make(map[string]bool, len(plan.removals))
					for _, x := range plan.removals {
						drop[x.ID] = true
					}
					c.Indexers = slices.DeleteFunc(c.Indexers, func(x config.Indexer) bool { return drop[x.ID] })
					removed = len(plan.removals)
				}
				return nil
			})
			if errors.Is(err, errProwlarrPlanStale) {
				http.Error(w, "prowlarr sync plan is stale: source or local indexers changed since preview; review again", http.StatusConflict)
				return
			}
			if err != nil {
				http.Error(w, "could not save indexers", 500)
				return
			}
		} else if in.Import {
			// Additive only: upsert the discovered indexers. Indexers
			// absent from this discovery are NEVER removed here —
			// disappearance at the source may be transient, so deletion
			// is the owner's explicit action, not an import side effect.
			// Unlike sync, this also upserts locally edited entries, as
			// before.
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
				return nil
			})
			if err != nil {
				http.Error(w, "could not save indexers", 500)
				return
			}
		} else if in.Sync {
			// Sync preview: compute the plan and list removals, change
			// nothing. The response carries the approved removal IDs
			// and the plan fingerprint the apply must echo back.
			plan = computeProwlarrSyncPlan(a.cfg.Get().Indexers, in.Config, report)
		}
		for i := range indexers {
			indexers[i].APIKey = admin.RedactedSecret
		}
		resp := map[string]any{"indexers": indexers, "imported": in.Import, "skippedUnknownProtocol": report.SkippedUnknownProtocol, "warnings": warnings}
		if in.Sync {
			// Only an explicit sync response carries removal/plan
			// fields, so the plain preview/import contract is unchanged.
			resp["removals"] = redactedIndexers(plan.removals)
			resp["removed"] = removed
			resp["keptEdited"] = redactedIndexers(plan.keptEdited)
			resp["approvedRemovals"] = plan.approvedRemovals
			resp["plan"] = plan.fingerprint
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return m
}

// errProwlarrPlanStale aborts a sync apply (409, no mutation) when the
// recomputed plan no longer matches the preview the owner approved.
var errProwlarrPlanStale = errors.New("prowlarr sync plan stale")

// prowlarrSyncPlan is the full reconciliation plan a sync preview
// returns and a sync apply must echo back: the removals it would apply,
// the locally edited entries it keeps, the approved removal IDs and a
// deterministic fingerprint over everything the plan depends on.
type prowlarrSyncPlan struct {
	removals         []config.Indexer
	keptEdited       []config.Indexer
	approvedRemovals []string // sorted removal IDs (empty when canRemove is false)
	fingerprint      string
	canRemove        bool
}

// computeProwlarrSyncPlan lists what an explicit sync would reconcile:
// same-scope stored indexers (ID under this instance's prefix, in its
// saved spelling — #55) that the source no longer reports as enabled are
// removals, unless locally edited. Local-edit preservation covers BOTH
// directions: an absent entry is kept when its identity no longer
// matches the generated pattern (newznab.ProwlarrLocallyEdited), and a
// present entry is kept — and skipped by the sync upsert — when it
// differs from the entry discovery just generated for it
// (newznab.ProwlarrDiffersFromGenerated). Entries outside the scope are
// never touched. An empty/all-disabled source or an incomplete
// discovery (skippedUnknownProtocol > 0) can never remove.
func computeProwlarrSyncPlan(stored []config.Indexer, cfg config.Prowlarr, report newznab.ProwlarrDiscovery) prowlarrSyncPlan {
	plan := prowlarrSyncPlan{removals: []config.Indexer{}, keptEdited: []config.Indexer{}, approvedRemovals: []string{}}
	plan.canRemove = len(report.PresentIDs) > 0 && report.SkippedUnknownProtocol == 0
	prefix := newznab.ProwlarrIDPrefix(cfg)
	if prefix == "" {
		plan.fingerprint = prowlarrPlanFingerprint(cfg, stored, report, plan)
		return plan
	}
	present := make(map[string]bool, len(report.PresentIDs))
	for _, id := range report.PresentIDs {
		present[id] = true
	}
	generated := make(map[string]config.Indexer, len(report.Indexers))
	for _, x := range report.Indexers {
		generated[x.ID] = x
	}
	for _, x := range stored {
		if !strings.HasPrefix(x.ID, prefix) {
			continue
		}
		if present[x.ID] {
			if gen, ok := generated[x.ID]; ok {
				if newznab.ProwlarrDiffersFromGenerated(x, gen) {
					plan.keptEdited = append(plan.keptEdited, x)
				}
			} else if newznab.ProwlarrLocallyEdited(cfg, x) {
				// Present but unimportable (skipped protocol): no
				// generated entry to diff, fall back to the pattern.
				plan.keptEdited = append(plan.keptEdited, x)
			}
			continue
		}
		if newznab.ProwlarrLocallyEdited(cfg, x) {
			plan.keptEdited = append(plan.keptEdited, x)
		} else if plan.canRemove {
			plan.removals = append(plan.removals, x)
		}
	}
	for _, x := range plan.removals {
		plan.approvedRemovals = append(plan.approvedRemovals, x.ID)
	}
	slices.Sort(plan.approvedRemovals)
	plan.fingerprint = prowlarrPlanFingerprint(cfg, stored, report, plan)
	return plan
}

func sortedStrings(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// prowlarrPlanFingerprint is a deterministic hash (SHA-256) over
// everything a sync plan depends on: the scope prefix, the removal
// guard (canRemove), the skip count, the present IDs (sorted), every
// same-scope stored indexer's ID + relevant fields and every discovered
// indexer's ID + same fields (name, URL, limits, grabs, priority,
// enabled, protocol, categories — never the API key), plus the approved
// removal IDs. Any source or local change between preview and apply
// changes the fingerprint; field order and sorting are fixed so equal
// states always hash equally.
func prowlarrPlanFingerprint(cfg config.Prowlarr, stored []config.Indexer, report newznab.ProwlarrDiscovery, plan prowlarrSyncPlan) string {
	prefix := newznab.ProwlarrIDPrefix(cfg)
	h := sha256.New()
	fmt.Fprintf(h, "prowlarr-sync-v2\nprefix=%q canRemove=%t skipped=%d\n", prefix, plan.canRemove, report.SkippedUnknownProtocol)
	writeCanon := func(x config.Indexer) {
		fmt.Fprintf(h, "ix id=%q name=%q url=%q dailyLimit=%d dailyGrabs=%d priority=%d enabled=%t protocol=%q movie=%v tv=%v\n",
			x.ID, x.Name, x.URL, x.DailyLimit, x.DailyGrabs, x.Priority, x.Enabled, x.Protocol, x.MovieCategories, x.TVCategories)
	}
	fmt.Fprint(h, "present\n")
	for _, id := range sortedStrings(report.PresentIDs) {
		fmt.Fprintf(h, "id=%q\n", id)
	}
	fmt.Fprint(h, "local\n")
	local := make([]config.Indexer, 0, len(stored))
	for _, x := range stored {
		if prefix != "" && strings.HasPrefix(x.ID, prefix) {
			local = append(local, x)
		}
	}
	slices.SortFunc(local, func(a, b config.Indexer) int { return strings.Compare(a.ID, b.ID) })
	for _, x := range local {
		writeCanon(x)
	}
	fmt.Fprint(h, "discovered\n")
	discovered := slices.Clone(report.Indexers)
	slices.SortFunc(discovered, func(a, b config.Indexer) int { return strings.Compare(a.ID, b.ID) })
	for _, x := range discovered {
		writeCanon(x)
	}
	fmt.Fprint(h, "approved\n")
	for _, id := range plan.approvedRemovals {
		fmt.Fprintf(h, "id=%q\n", id)
	}
	return hex.EncodeToString(h.Sum(nil))
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
