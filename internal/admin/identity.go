package admin

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/newznab"
)

// IdentityView is the owner-facing client-identity summary: the effective
// global values and every indexer's inherited effective values. It carries
// preset names only, never keys or custom header values.
type IdentityView struct {
	TVSearch    string                `json:"tvSearch"`
	MovieSearch string                `json:"movieSearch"`
	TVGrab      string                `json:"tvGrab"`
	MovieGrab   string                `json:"movieGrab"`
	Indexers    []IndexerIdentityView `json:"indexers"`
	// UserAgents is the literal user agent each preset presents, so the UI
	// can show what a selection actually sends. Custom has no fixed value.
	UserAgents map[string]string `json:"userAgents"`
}

// IndexerIdentityView reports one indexer's overrides and the values it
// resolves to. Empty overrides mean inherit.
type IndexerIdentityView struct {
	ID                   string `json:"id"`
	SearchOverride       string `json:"searchOverride,omitempty"`
	GrabOverride         string `json:"grabOverride,omitempty"`
	EffectiveTVSearch    string `json:"effectiveTvSearch"`
	EffectiveMovieSearch string `json:"effectiveMovieSearch"`
	EffectiveTVGrab      string `json:"effectiveTvGrab"`
	EffectiveMovieGrab   string `json:"effectiveMovieGrab"`
}

func identityView(cfg config.Config, version string) IdentityView {
	policy := cfg.ClientIdentity
	view := IdentityView{
		TVSearch:    newznab.EffectiveSearchIdentity(policy, config.Indexer{}, false),
		MovieSearch: newznab.EffectiveSearchIdentity(policy, config.Indexer{}, true),
		TVGrab:      newznab.EffectiveGrabIdentity(policy, config.Indexer{}, false),
		MovieGrab:   newznab.EffectiveGrabIdentity(policy, config.Indexer{}, true),
		Indexers:    make([]IndexerIdentityView, 0, len(cfg.Indexers)),
		UserAgents:  make(map[string]string, len(newznab.IdentityPresets())),
	}
	for _, name := range newznab.IdentityPresets() {
		if ua := newznab.PresetUserAgent(name, version); ua != "" {
			view.UserAgents[name] = ua
		}
	}
	for _, ix := range cfg.Indexers {
		view.Indexers = append(view.Indexers, IndexerIdentityView{
			ID:                   ix.ID,
			SearchOverride:       ix.SearchIdentity,
			GrabOverride:         ix.GrabIdentity,
			EffectiveTVSearch:    newznab.EffectiveSearchIdentity(policy, ix, false),
			EffectiveMovieSearch: newznab.EffectiveSearchIdentity(policy, ix, true),
			EffectiveTVGrab:      newznab.EffectiveGrabIdentity(policy, ix, false),
			EffectiveMovieGrab:   newznab.EffectiveGrabIdentity(policy, ix, true),
		})
	}
	return view
}

func (h *Handler) handleGetIdentity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, identityView(h.deps.Config.Get(), h.deps.Version))
}
