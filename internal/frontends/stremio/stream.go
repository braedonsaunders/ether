package stremio

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) resolveID(r *http.Request, viewer *frontend.Viewer, device frontend.Device, typ string, id ids.StremioID) (frontend.RatingKey, error) {
	if id.Source == "tmdb" {
		return ids.StremioTMDBKey(typ, id)
	}
	mediaType := frontend.MediaType("movie")
	if typ == "series" {
		mediaType = "show"
	} else if typ != "movie" {
		return 0, ids.ErrInvalid
	}
	req := frontend.ExternalRequest{Source: "imdb", ID: id.ID, Type: mediaType}
	if id.HasEpisode {
		if typ != "series" {
			return 0, ids.ErrInvalid
		}
		req.Season = &id.Season
		req.Episode = &id.Episode
	}
	return i.host.Experience.ResolveExternal(r.Context(), viewer, device, req)
}

func (i *instance) stream(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	i.observeClient(credential)
	if i.host.LazyStreams == nil || i.host.Capabilities == nil || i.host.ClientIP == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	typ, raw := r.PathValue("type"), r.PathValue("id")
	if !strings.HasSuffix(raw, ".json") {
		http.NotFound(w, r)
		return
	}
	id, err := ids.ParseStremio(strings.TrimSuffix(raw, ".json"))
	if err != nil || (typ == "series" && !id.HasEpisode) {
		http.NotFound(w, r)
		return
	}
	device := deviceFor(credential)
	key, err := i.resolveID(r, viewer, device, typ, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ip, ok := i.host.ClientIP(r)
	if !ok {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := frontend.WithClientIP(r.Context(), ip)
	profile, err := i.host.Capabilities.Build(ctx, frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: "stremio", ClientID: device.ID, Product: device.Product}, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	base := i.imageBase(r)
	if base == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	plan := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "stremio", Item: key, Caps: profile.Client, Intent: frontend.PlayIntent{Options: frontend.DecideOptions{DirectPlay: true}}}
	lazy, err := i.host.LazyStreams.Issue(ctx, credential, plan)
	if err != nil || !strings.HasPrefix(lazy, "/sessions/lazy/") {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	base.Path = strings.TrimSuffix(base.Path, "/image/") + lazy
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"streams": []any{map[string]any{"name": "Ether", "url": base.String()}}})
}

func (i *instance) serveLazy(w http.ResponseWriter, r *http.Request) {
	if i.host.LazyStreams == nil {
		http.NotFound(w, r)
		return
	}
	i.host.LazyStreams.ServeHTTP(w, r)
}
