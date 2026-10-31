package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// ConsoleCatalog is the catalogue surface the release console needs
// (satisfied by core.Catalog).
type ConsoleCatalog interface {
	Get(ctx context.Context, key core.RatingKey) (*core.Title, error)
	Children(ctx context.Context, key core.RatingKey) ([]core.Title, error)
}

// ConsoleDeps wires the release console's owner API.
type ConsoleDeps struct {
	Catalog ConsoleCatalog
	// Invalidate drops anything prepared for a title (playback's
	// pre-resolved warm entries) so the next play honours a change.
	Invalidate func(core.RatingKey)
	Logger     *slog.Logger
	// Guides reads TRaSH Guides for quality profile imports; nil uses
	// GitHub with default options.
	Guides *TrashGuides
	// Dictionarry reads Profilarr (Dictionarry) quality profiles for
	// imports; nil uses GitHub with default options.
	Dictionarry *Dictionarry
	// SaveConfig saves an import through the owner's validated config
	// path: it applies mutate to the current settings (again if they changed
	// meanwhile), validates and saves them. nil disables imports.
	SaveConfig func(mutate func(*config.Config) error) error
}

// ConsoleHandler serves the release console under the owner API (mounted
// at /ether/api/releases behind owner authentication):
//
//	GET    /profiles                     profiles interactive search can rank with
//	GET    /search?key=&profile=&fresh=1
//	GET    /titles/{key}/children        seasons of a show, episodes of a season
//	POST   /health   {key, releaseId}
//	POST   /forget   {key}
//	GET    /pins
//	POST   /pins     {key, releaseId, note}
//	DELETE /pins/{key}
//	GET    /blocklist?key=&source=&q=&limit=&offset=
//	POST   /blocklist {releaseId, key, reason, name, indexer}
//	DELETE /blocklist/{releaseId}
//	GET    /history?key=&limit=&offset=
//	GET    /history/{id}
//	GET    /trash/{app}/profiles         TRaSH Guides quality profiles (radarr, sonarr)
//	GET    /dictionarry/{app}/profiles   Profilarr (Dictionarry) quality profiles (radarr, sonarr)
//	POST   /quality-import/preview       {app, source, profile | json}
//	POST   /quality-import               {app, source, profile | json, name}
func (s *Service) ConsoleHandler(deps ConsoleDeps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = s.log
	}
	if deps.Guides == nil {
		deps.Guides = NewTrashGuides(TrashOptions{})
	}
	if deps.Dictionarry == nil {
		deps.Dictionarry = NewDictionarry(DictionarryOptions{})
	}
	h := &consoleAPI{s: s, deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /profiles", h.profiles)
	mux.HandleFunc("POST /rules/preview", h.previewRules)
	mux.HandleFunc("GET /search", h.search)
	mux.HandleFunc("GET /titles/{key}/children", h.children)
	mux.HandleFunc("POST /health", h.health)
	mux.HandleFunc("POST /forget", h.forget)
	mux.HandleFunc("GET /pins", h.listPins)
	mux.HandleFunc("POST /pins", h.pin)
	mux.HandleFunc("DELETE /pins/{key}", h.unpin)
	mux.HandleFunc("GET /blocklist", h.blocklist)
	mux.HandleFunc("POST /blocklist", h.block)
	mux.HandleFunc("DELETE /blocklist/{id}", h.unblock)
	mux.HandleFunc("GET /history", h.history)
	mux.HandleFunc("GET /history/{id}", h.historyEntry)
	mux.HandleFunc("GET /trash/{app}/profiles", h.trashProfiles)
	mux.HandleFunc("GET /dictionarry/{app}/profiles", h.dictionarryProfiles)
	mux.HandleFunc("POST /quality-import/preview", h.previewImport)
	mux.HandleFunc("POST /quality-import", h.applyImport)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		consoleError(w, http.StatusNotFound, "not_found", "no such release console endpoint")
	})
	return mux
}

type consoleAPI struct {
	s    *Service
	deps ConsoleDeps
}

func consoleJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// consoleError writes the owner API's uniform error shape.
func consoleError(w http.ResponseWriter, status int, code, msg string) {
	consoleJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func (h *consoleAPI) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		consoleError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrUnknownRelease):
		consoleError(w, http.StatusNotFound, "unknown_release", err.Error())
	case errors.Is(err, ErrUnknownProfile), errors.Is(err, ErrNotPinnable):
		consoleError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, ErrPinBlocked):
		consoleError(w, http.StatusConflict, "blocked", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		consoleError(w, http.StatusGatewayTimeout, "timeout", "the indexers or providers did not answer in time")
	default:
		h.deps.Logger.Warn("release console request failed", "err", err)
		consoleError(w, http.StatusBadGateway, "upstream", err.Error())
	}
}

func (h *consoleAPI) invalidate(key core.RatingKey) {
	if key != 0 && h.deps.Invalidate != nil {
		h.deps.Invalidate(key)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		consoleError(w, http.StatusBadRequest, "invalid", "invalid request body")
		return false
	}
	return true
}

// parseKey parses a rating key; when playable is set it must be a movie
// or an episode.
func parseKey(w http.ResponseWriter, raw string, playable bool) (core.RatingKey, bool) {
	k, err := core.ParseRatingKey(strings.TrimSpace(raw))
	if err != nil || (playable && !pinnableKey(k)) {
		consoleError(w, http.StatusBadRequest, "invalid", "key must be a movie or episode rating key")
		return 0, false
	}
	return k, true
}

// title loads a playable title from the catalogue.
func (h *consoleAPI) title(w http.ResponseWriter, ctx context.Context, key core.RatingKey) (*core.Title, bool) {
	if h.deps.Catalog == nil {
		consoleError(w, http.StatusNotImplemented, "not_implemented", "the catalogue is not available")
		return nil, false
	}
	t, err := h.deps.Catalog.Get(ctx, key)
	if err != nil || t == nil {
		if err == nil || errors.Is(err, core.ErrNotFound) {
			consoleError(w, http.StatusNotFound, "not_found", "no such title")
		} else {
			h.fail(w, err)
		}
		return nil, false
	}
	return t, true
}

func pageParams(r *http.Request) (limit, offset int) {
	q := r.URL.Query()
	limit, _ = strconv.Atoi(q.Get("limit"))
	offset, _ = strconv.Atoi(q.Get("offset"))
	return limit, offset
}

func (h *consoleAPI) profiles(w http.ResponseWriter, r *http.Request) {
	def, names := h.s.ConsoleProfiles()
	if names == nil {
		names = []string{}
	}
	consoleJSON(w, http.StatusOK, map[string]any{"default": def, "profiles": names,
		"special": []string{ProfileNone, ProfileDownload}})
}

func (h *consoleAPI) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key, ok := parseKey(w, q.Get("key"), true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	t, ok := h.title(w, ctx, key)
	if !ok {
		return
	}
	fresh := q.Get("fresh") == "1" || q.Get("fresh") == "true"
	res, err := h.s.SearchReleases(ctx, t, SearchOptions{Profile: q.Get("profile"), Fresh: fresh})
	if err != nil {
		h.fail(w, err)
		return
	}
	consoleJSON(w, http.StatusOK, res)
}

// childTitle is a season or episode for the title picker.
type childTitle struct {
	Key     string `json:"key"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
	Aired   string `json:"aired,omitempty"`
	Pinned  bool   `json:"pinned,omitempty"`
}

func (h *consoleAPI) children(w http.ResponseWriter, r *http.Request) {
	key, ok := parseKey(w, r.PathValue("key"), false)
	if !ok {
		return
	}
	if key.Kind() != core.KindShow && key.Kind() != core.KindSeason {
		consoleError(w, http.StatusBadRequest, "invalid", "key must be a show or season rating key")
		return
	}
	if h.deps.Catalog == nil {
		consoleError(w, http.StatusNotImplemented, "not_implemented", "the catalogue is not available")
		return
	}
	kids, err := h.deps.Catalog.Children(r.Context(), key)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]childTitle, 0, len(kids))
	for _, t := range kids {
		c := childTitle{Key: t.Key.String(), Type: string(t.Type), Title: t.Title, Season: t.Season, Episode: t.Episode}
		if c.Season == 0 && t.Key.Kind() != core.KindShow {
			c.Season = t.Key.Season()
		}
		if c.Episode == 0 {
			c.Episode = t.Key.Episode()
		}
		if !t.Released.IsZero() {
			c.Aired = t.Released.Format("2006-01-02")
		}
		_, c.Pinned = h.s.PinFor(t.Key)
		out = append(out, c)
	}
	consoleJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *consoleAPI) health(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key       string `json:"key"`
		ReleaseID string `json:"releaseId"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.ReleaseID == "" {
		consoleError(w, http.StatusBadRequest, "invalid", "releaseId is required")
		return
	}
	var key core.RatingKey
	if in.Key != "" {
		var ok bool
		if key, ok = parseKey(w, in.Key, true); !ok {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := h.s.CheckRelease(ctx, in.ReleaseID, key)
	if err != nil {
		h.fail(w, err)
		return
	}
	if res.Blocked {
		h.invalidate(key)
	}
	consoleJSON(w, http.StatusOK, res)
}

func (h *consoleAPI) forget(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	key, ok := parseKey(w, in.Key, true)
	if !ok {
		return
	}
	t, ok := h.title(w, r.Context(), key)
	if !ok {
		return
	}
	if err := h.s.Forget(r.Context(), t); err != nil {
		h.fail(w, err)
		return
	}
	h.invalidate(key)
	consoleJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *consoleAPI) listPins(w http.ResponseWriter, r *http.Request) {
	pins := h.s.Pins()
	for i := range pins {
		pins[i] = browserPin(pins[i])
	}
	consoleJSON(w, http.StatusOK, map[string]any{"pins": pins})
}

func (h *consoleAPI) pin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key       string `json:"key"`
		ReleaseID string `json:"releaseId"`
		Note      string `json:"note"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	key, ok := parseKey(w, in.Key, true)
	if !ok {
		return
	}
	if in.ReleaseID == "" {
		consoleError(w, http.StatusBadRequest, "invalid", "releaseId is required")
		return
	}
	if len(in.Note) > 500 {
		consoleError(w, http.StatusBadRequest, "invalid", "note is too long")
		return
	}
	// The release comes from the server's own search results, never from
	// the request, so a pin cannot point the server at an arbitrary URL.
	rel, found := h.s.LookupRelease(in.ReleaseID)
	if !found {
		h.fail(w, ErrUnknownRelease)
		return
	}
	name := ""
	if h.deps.Catalog != nil {
		if t, err := h.deps.Catalog.Get(r.Context(), key); err == nil && t != nil {
			name = consoleTitleName(t)
		}
	}
	p, err := h.s.Pin(r.Context(), key, name, rel, in.Note)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.invalidate(key)
	consoleJSON(w, http.StatusOK, browserPin(p))
}

func (h *consoleAPI) unpin(w http.ResponseWriter, r *http.Request) {
	key, ok := parseKey(w, r.PathValue("key"), true)
	if !ok {
		return
	}
	had, err := h.s.Unpin(r.Context(), key)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !had {
		consoleError(w, http.StatusNotFound, "not_found", "the title has no pin")
		return
	}
	h.invalidate(key)
	w.WriteHeader(http.StatusNoContent)
}

func (h *consoleAPI) blocklist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := BlockFilter{Query: q.Get("q"), Source: q.Get("source")}
	if f.Source != "" && f.Source != BlockAuto && f.Source != BlockOwner {
		consoleError(w, http.StatusBadRequest, "invalid", "source must be auto or owner")
		return
	}
	if raw := q.Get("key"); raw != "" {
		k, ok := parseKey(w, raw, true)
		if !ok {
			return
		}
		f.TitleKey = k
	}
	f.Limit, f.Offset = pageParams(r)
	items, total, err := h.s.Blocklist(r.Context(), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	consoleJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (h *consoleAPI) block(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ReleaseID string `json:"releaseId"`
		Key       string `json:"key"`
		Reason    string `json:"reason"`
		Name      string `json:"name"`
		Indexer   string `json:"indexer"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.ReleaseID == "" || len(in.ReleaseID) > 200 {
		consoleError(w, http.StatusBadRequest, "invalid", "releaseId is required")
		return
	}
	if len(in.Reason) > 500 || len(in.Name) > 500 || len(in.Indexer) > 200 {
		consoleError(w, http.StatusBadRequest, "invalid", "reason, name or indexer is too long")
		return
	}
	req := BlockRequest{ReleaseID: in.ReleaseID, Name: in.Name, Indexer: in.Indexer}
	if in.Key != "" {
		k, ok := parseKey(w, in.Key, true)
		if !ok {
			return
		}
		req.TitleKey = k
		if h.deps.Catalog != nil {
			if t, err := h.deps.Catalog.Get(r.Context(), k); err == nil && t != nil {
				req.TitleName = consoleTitleName(t)
			}
		}
	}
	e, err := h.s.Block(r.Context(), req, in.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.invalidate(e.TitleKey)
	consoleJSON(w, http.StatusOK, e)
}

func (h *consoleAPI) unblock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		consoleError(w, http.StatusBadRequest, "invalid", "release id is required")
		return
	}
	e, err := h.s.Unblock(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.invalidate(e.TitleKey)
	w.WriteHeader(http.StatusNoContent)
}

func (h *consoleAPI) history(w http.ResponseWriter, r *http.Request) {
	var f HistoryFilter
	if raw := r.URL.Query().Get("key"); raw != "" {
		k, ok := parseKey(w, raw, false)
		if !ok {
			return
		}
		f.TitleKey = k
	}
	f.Limit, f.Offset = pageParams(r)
	items, total, err := h.s.History(r.Context(), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	consoleJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (h *consoleAPI) historyEntry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		consoleError(w, http.StatusBadRequest, "invalid", "invalid history id")
		return
	}
	e, err := h.s.HistoryEntry(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	consoleJSON(w, http.StatusOK, e)
}
