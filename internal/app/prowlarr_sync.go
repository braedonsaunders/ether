package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/newznab"
)

// A saved Prowlarr connection with tags is refreshed on save and, lightly,
// on this interval. The first wait keeps startup from calling Prowlarr in
// the same burst as every other leader job. Discovery only reads the
// indexer list; it never searches or downloads.
const (
	prowlarrSyncFirstWait = time.Minute
	prowlarrSyncInterval  = 15 * time.Minute
)

func (a *App) runProwlarrSync(ctx context.Context) {
	timer := time.NewTimer(prowlarrSyncFirstWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			a.syncSavedProwlarr(ctx)
			timer.Reset(prowlarrSyncInterval)
		}
	}
}

// syncSavedProwlarr refreshes the saved connection when the owner has chosen
// tags. An empty tag list is today's manual import: nothing runs in the
// background, and a save stays additive.
func (a *App) syncSavedProwlarr(ctx context.Context) {
	if a == nil || a.cfg == nil {
		return
	}
	cfg := a.cfg.Get().Prowlarr
	cfg.Tags = config.NormalizeProwlarrTags(cfg.Tags)
	if strings.TrimSpace(cfg.URL) == "" || cfg.APIKey == "" || len(cfg.Tags) == 0 {
		return
	}
	_, _, _, _ = a.syncProwlarr(ctx, cfg, true)
}

func (a *App) handleProwlarrStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfg.Get().Prowlarr
	if cfg.APIKey != "" {
		cfg.APIKey = admin.RedactedSecret
	}
	state, kept := a.prowlarrStatus()
	writeProwlarrJSON(w, http.StatusOK, map[string]any{
		"config": cfg,
		"status": state,
		"kept":   kept,
	})
}

func (a *App) handleProwlarrTags(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Config config.Prowlarr `json:"config"`
	}
	if !decodeProwlarr(w, r, &in) {
		return
	}
	cfg := a.resolveProwlarr(in.Config)
	tags, err := newznab.ListProwlarrTags(r.Context(), cfg, newznab.Options{Logger: a.log})
	if err != nil {
		a.writeProwlarrDiscoveryError(w, err)
		return
	}
	if tags == nil {
		tags = []newznab.ProwlarrTag{}
	}
	writeProwlarrJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

func (a *App) handleProwlarrSync(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Config config.Prowlarr `json:"config"`
		Import bool            `json:"import"`
	}
	if !decodeProwlarr(w, r, &in) {
		return
	}
	cfg := a.resolveProwlarr(in.Config)
	report, stopped, kept, err := a.syncProwlarr(r.Context(), cfg, in.Import)
	if err != nil {
		if kept {
			var rl *newznab.RateLimitError
			if errors.As(err, &rl) {
				if h := prowlarrRetryAfterHeader(rl.RetryAfter); h != "" {
					w.Header().Set("Retry-After", h)
				}
			}
			writeProwlarrJSON(w, http.StatusOK, map[string]any{
				"indexers": a.scopedProwlarrIndexers(cfg),
				"imported": false,
				"status":   "unreachable",
				"kept":     true,
			})
			return
		}
		if !errors.Is(err, errProwlarrSave) {
			a.writeProwlarrDiscoveryError(w, err)
			return
		}
		http.Error(w, "could not save indexers", http.StatusInternalServerError)
		return
	}
	if !in.Import && a.savedProwlarrMatches(cfg) {
		a.noteProwlarrStatus("ok", false)
	}
	indexers := report.Indexers
	if indexers == nil {
		indexers = []config.Indexer{}
	}
	for i := range indexers {
		if indexers[i].APIKey != "" {
			indexers[i].APIKey = admin.RedactedSecret
		}
	}
	body := map[string]any{
		"indexers":               indexers,
		"imported":               in.Import,
		"skippedUnknownProtocol": report.SkippedUnknownProtocol,
		"warnings":               report.Warnings,
		"status":                 "ok",
	}
	if stopped > 0 {
		body["stopped"] = stopped
	}
	writeProwlarrJSON(w, http.StatusOK, body)
}

var errProwlarrSave = errors.New("prowlarr: save failed")

// syncProwlarr reads Prowlarr and, when commit is set, upserts the indexers.
// With tags chosen, indexers from this Prowlarr that no longer carry one
// are disabled (they stop being used) but are not deleted. With no tags the
// update is additive, matching import before tag sync existed.
// A failed read leaves the stored indexers alone. When a previous set exists,
// kept is true and status is "unreachable".
func (a *App) syncProwlarr(ctx context.Context, cfg config.Prowlarr, commit bool) (newznab.ProwlarrDiscovery, int, bool, error) {
	report, err := newznab.DiscoverProwlarrReport(ctx, cfg, newznab.Options{Logger: a.log})
	if err != nil {
		if commit && a.hasLastProwlarrSet(cfg) {
			a.noteProwlarrStatus("unreachable", true)
			if a.log != nil {
				a.log.Warn("prowlarr sync failed; keeping the last indexers", "err", err)
			}
			return report, 0, true, err
		}
		if commit {
			a.noteProwlarrStatus("unreachable", false)
		}
		return report, 0, false, err
	}
	if !commit {
		return report, 0, false, nil
	}
	stopped := 0
	err = a.cfg.Update(func(c *config.Config) error {
		stopped = applyProwlarrSync(c, cfg, report.Indexers)
		return nil
	})
	if err != nil {
		return report, 0, false, errProwlarrSave
	}
	a.noteProwlarrStatus("ok", false)
	return report, stopped, false, nil
}

// applyProwlarrSync upserts found into c and records the connection.
// Tags empty: leave every other indexer as it was.
// Tags set: disable enabled indexers from this Prowlarr that are not in found.
func applyProwlarrSync(c *config.Config, cfg config.Prowlarr, found []config.Indexer) (stopped int) {
	cfg.Tags = config.NormalizeProwlarrTags(cfg.Tags)
	prefix := newznab.ProwlarrIDPrefix(cfg)
	byID := map[string]int{}
	for i, x := range c.Indexers {
		byID[x.ID] = i
	}
	kept := map[string]bool{}
	for _, x := range found {
		kept[x.ID] = true
		if i, ok := byID[x.ID]; ok {
			c.Indexers[i] = x
		} else {
			byID[x.ID] = len(c.Indexers)
			c.Indexers = append(c.Indexers, x)
		}
	}
	c.Prowlarr = cfg
	if len(cfg.Tags) == 0 || prefix == "" {
		return 0
	}
	for i, x := range c.Indexers {
		if !strings.HasPrefix(x.ID, prefix) || kept[x.ID] || !x.Enabled {
			continue
		}
		c.Indexers[i].Enabled = false
		stopped++
	}
	return stopped
}

func (a *App) resolveProwlarr(in config.Prowlarr) config.Prowlarr {
	in.URL = strings.TrimSpace(in.URL)
	in.Tags = config.NormalizeProwlarrTags(in.Tags)
	if in.APIKey == "" || in.APIKey == admin.RedactedSecret {
		stored := a.cfg.Get().Prowlarr
		if stored.APIKey != "" && sameProwlarrBase(stored.URL, in.URL) {
			in.APIKey = stored.APIKey
		} else {
			in.APIKey = ""
		}
	}
	return in
}

func (a *App) savedProwlarrMatches(cfg config.Prowlarr) bool {
	if a.cfg == nil {
		return false
	}
	stored := a.cfg.Get().Prowlarr
	return stored.URL != "" && sameProwlarrBase(stored.URL, cfg.URL)
}

func (a *App) hasLastProwlarrSet(cfg config.Prowlarr) bool {
	prefix := newznab.ProwlarrIDPrefix(cfg)
	if prefix == "" || a.cfg == nil {
		return false
	}
	for _, x := range a.cfg.Get().Indexers {
		if strings.HasPrefix(x.ID, prefix) {
			return true
		}
	}
	stored := a.cfg.Get().Prowlarr
	return stored.APIKey != "" && sameProwlarrBase(stored.URL, cfg.URL)
}

func (a *App) scopedProwlarrIndexers(cfg config.Prowlarr) []config.Indexer {
	prefix := newznab.ProwlarrIDPrefix(cfg)
	out := []config.Indexer{}
	if prefix == "" || a.cfg == nil {
		return out
	}
	for _, x := range a.cfg.Get().Indexers {
		if !strings.HasPrefix(x.ID, prefix) {
			continue
		}
		if x.APIKey != "" {
			x.APIKey = admin.RedactedSecret
		}
		out = append(out, x)
	}
	return out
}

func sameProwlarrBase(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

func (a *App) noteProwlarrStatus(state string, kept bool) {
	a.prowlarrMu.Lock()
	a.prowlarrState = state
	a.prowlarrKept = kept
	a.prowlarrMu.Unlock()
}

func (a *App) prowlarrStatus() (state string, kept bool) {
	a.prowlarrMu.Lock()
	defer a.prowlarrMu.Unlock()
	return a.prowlarrState, a.prowlarrKept
}

func (a *App) writeProwlarrDiscoveryError(w http.ResponseWriter, err error) {
	var rl *newznab.RateLimitError
	if errors.As(err, &rl) {
		if h := prowlarrRetryAfterHeader(rl.RetryAfter); h != "" {
			w.Header().Set("Retry-After", h)
		}
	}
	http.Error(w, "Prowlarr discovery failed", http.StatusBadGateway)
}

func decodeProwlarr(w http.ResponseWriter, r *http.Request, dst any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil {
		http.Error(w, "invalid Prowlarr settings", http.StatusBadRequest)
		return false
	}
	return true
}

func writeProwlarrJSON(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
