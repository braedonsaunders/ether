package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// These controls are mounted only on the authenticated owner cache extension.
// Reads touch only the bounded pin list and cached metadata, never the library.
func (a *App) registerWarmingControls(mux *http.ServeMux) {
	mux.HandleFunc("GET /warming", func(w http.ResponseWriter, r *http.Request) {
		cfg := a.runningCacheConfig()
		type title struct {
			Key   core.RatingKey `json:"key"`
			Title string         `json:"title"`
		}
		titles := make([]title, 0, len(cfg.KeepWarmTitles))
		for _, k := range cfg.KeepWarmTitles {
			name := k.String()
			if t, err := a.catalog.CachedTitle(r.Context(), k); err == nil && t != nil {
				name = t.Title
			}
			titles = append(titles, title{k, name})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"titles": titles, "speculative": cfg.SpeculativeWarming != "off"})
	})
	mux.HandleFunc("POST /warming", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key         core.RatingKey `json:"key"`
			Pinned      *bool          `json:"pinned"`
			Speculative *bool          `json:"speculative"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || (body.Pinned == nil && body.Speculative == nil) {
			http.Error(w, "Invalid request", 400)
			return
		}
		if body.Pinned != nil {
			if body.Key.Kind() != core.KindMovie && body.Key.Kind() != core.KindShow {
				http.Error(w, "Choose a movie or show", 400)
				return
			}
			if *body.Pinned {
				if t, err := a.catalog.CachedTitle(r.Context(), body.Key); err != nil || t == nil {
					http.Error(w, "Title not found", 404)
					return
				}
			}
		}
		err := a.cfg.Update(func(c *config.Config) error {
			cfg := c.EffectiveCache()
			cfg.KeepWarmTitles = slices.Clone(cfg.KeepWarmTitles)
			if body.Pinned != nil {
				index := slices.Index(cfg.KeepWarmTitles, body.Key)
				if *body.Pinned && index < 0 {
					if len(cfg.KeepWarmTitles) >= 200 {
						return errors.New("keep warm list is full")
					}
					cfg.KeepWarmTitles = append(cfg.KeepWarmTitles, body.Key)
				} else if !*body.Pinned && index >= 0 {
					cfg.KeepWarmTitles = slices.Delete(cfg.KeepWarmTitles, index, index+1)
				}
			}
			if body.Speculative != nil {
				cfg.SpeculativeWarming = "off"
				if *body.Speculative {
					cfg.SpeculativeWarming = "on"
				}
			}
			c.SetCachePolicy(cfg)
			return nil
		})
		if err != nil {
			http.Error(w, "Could not save warming preferences", 500)
			return
		}
		a.cachefill.Wake()
		w.WriteHeader(http.StatusNoContent)
	})
}
