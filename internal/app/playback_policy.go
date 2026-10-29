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
	"net/http"
	"strconv"
	"time"
)

func (a *App) playbackPolicyHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /policy", func(w http.ResponseWriter, r *http.Request) {
		policies, err := a.clientPolicies(r.Context())
		if err != nil {
			http.Error(w, "device policy unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"policies": policies})
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
		if in.Import {
			// Additive only: upsert the discovered indexers. Indexers
			// absent from this discovery are NEVER removed here —
			// disappearance at the source may be transient, so deletion
			// is the owner's explicit action, not an import side effect.
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
		}
		for i := range indexers {
			indexers[i].APIKey = admin.RedactedSecret
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"indexers": indexers, "imported": in.Import, "skippedUnknownProtocol": report.SkippedUnknownProtocol, "warnings": report.Warnings})
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
