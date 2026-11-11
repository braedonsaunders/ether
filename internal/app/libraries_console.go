package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// Owner console API for Libraries (spec §11, slice H). Mounted as the
// "libraries" admin extension, so every route sits behind the owner
// API's LAN, token and CSRF boundaries like the cache extension.
// Shares the long-lived pluginitems/blobcache services built by
// buildPlugins when present, falling back to lazy open in unit tests
// that construct App without pluginsRT.

type librariesConsole struct {
	app *App

	once  sync.Once
	items *pluginitems.Service
	blobs *blobcache.Cache
	err   error
}

// libraryHandlers registers the /api/libraries and /api/plugins-setup
// extensions (called from extensionHandlers).
func (a *App) libraryHandlers(m map[string]http.Handler) {
	c := &librariesConsole{app: a}
	m["libraries"] = c.handler()
	m["plugins-setup"] = c.setupHandler()
}

func (c *librariesConsole) services() (*pluginitems.Service, *blobcache.Cache, error) {
	if c.app.pluginsRT != nil && c.app.pluginsRT.pluginitems != nil && c.app.pluginsRT.blobcache != nil {
		return c.app.pluginsRT.pluginitems, c.app.pluginsRT.blobcache, nil
	}
	c.once.Do(func() {
		ctx := context.Background()
		opts := pluginitems.Options{Logger: c.app.log}
		if c.app.pluginsRT != nil && c.app.pluginsRT.mgr != nil {
			opts.Caller = c.app.pluginsRT.mgr
			opts.Health = c.app.pluginsRT.mgr
		}
		if c.items, c.err = pluginitems.New(ctx, c.app.db, opts); c.err != nil {
			return
		}
		c.blobs, c.err = blobcache.Open(filepath.Join(c.app.opts.DataDir, "blobcache"),
			blobcache.Options{Logger: c.app.log, DiskUsage: c.app.opts.diskUsage})
	})
	return c.items, c.blobs, c.err
}

func libJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func libError(w http.ResponseWriter, status int, msg string) {
	libJSON(w, status, map[string]string{"error": msg})
}

func libErrStatus(err error) int {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, libraries.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, libraries.ErrBuiltinLibrary):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func decodeLibBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		libError(w, http.StatusBadRequest, "Invalid request body")
		return false
	}
	return true
}

// kindItemDTO puts Ref and Key back on the wire: core.KindItem hides
// both from plugins (json:"-"), but the owner console needs them.
type kindItemDTO struct {
	core.KindItem
	Ref string `json:"ref"`
	Key int64  `json:"key"`
}

func kindDTOs(items []core.KindItem) []kindItemDTO {
	out := make([]kindItemDTO, len(items))
	for i, it := range items {
		out[i] = kindItemDTO{KindItem: it, Ref: string(it.Ref), Key: int64(it.Key)}
	}
	return out
}

type libraryDTO struct {
	core.Library
	ItemCount  *int                   `json:"itemCount,omitempty"`
	Sync       *pluginitems.SyncState `json:"sync,omitempty"`
	Kind       *plugins.MediaKindMeta `json:"kind,omitempty"`
	PluginName string                 `json:"pluginName,omitempty"`
}

func (c *librariesConsole) decorate(ctx context.Context, lib core.Library) libraryDTO {
	dto := libraryDTO{Library: lib}
	switch lib.Source {
	case core.LibraryPlugin:
		if items, _, err := c.services(); err == nil && items != nil {
			if rec, err := items.GetKindByLibrary(ctx, lib.ID); err == nil {
				decl := rec.Decl
				dto.Kind = &decl
				if tally, err := items.LibraryTally(ctx, lib.ID); err == nil {
					n := 0
					if len(decl.Levels) > 0 {
						n = tally[decl.Levels[0].Key]
					} else {
						for _, v := range tally {
							n += v
						}
					}
					dto.ItemCount = &n
				}
				if st, err := items.SyncStateOf(ctx, lib.PluginID, lib.KindKey); err == nil {
					dto.Sync = &st
				}
			}
		}
		if c.app.pluginsRT != nil && c.app.pluginsRT.mgr != nil {
			if rec, err := c.app.pluginsRT.mgr.Get(ctx, lib.PluginID); err == nil {
				dto.PluginName = rec.Name
			}
		}
	case core.LibraryBuiltin:
		if c.app.catalog != nil {
			if st, err := c.app.catalog.Stats(ctx); err == nil {
				n := st.BrowsableMovies
				if lib.MediaType == core.TypeShow {
					n = st.BrowsableShows
				}
				dto.ItemCount = &n
			}
		}
	case core.LibraryFilter:
		if page, err := c.browseTitles(ctx, lib, core.Filter{}, core.BrowseQuery{Limit: 1}); err == nil {
			dto.ItemCount = &page.Total
		}
	}
	return dto
}

// browseTitles browses the TMDB catalogue scoped to a builtin or
// filter library: the library's saved filter is ANDed with the
// request filter through a FilterGroup (the same composition
// experience/library_index.go uses for viewer state).
// libraryScopeFilter merges a library's media type, saved filter and extra
// rules onto req. When req.RuleVisibility != "" (rule preview browsing),
// saved ExtraRules on a filter library are omitted so draft PreviewRules
// evaluate cleanly over the canonical+filter candidate pool.
func libraryScopeFilter(lib core.Library, req core.Filter) core.Filter {
	f := req
	f.LocalOnly = true
	if lib.MediaType != "" {
		f.Type = lib.MediaType
	}
	if lib.Filter != nil && lib.Filter.Type != "" {
		f.Type = lib.Filter.Type
	}
	if lib.Source == core.LibraryFilter {
		if lib.Filter != nil {
			saved := *lib.Filter
			saved.Type = "" // already pinned above; keep the group type-free
			child := core.FilterGroup{Operator: "and", Filter: &saved}
			if f.Group != nil {
				f.Group = &core.FilterGroup{Operator: "and", Children: []core.FilterGroup{*f.Group, child}}
			} else {
				f.Group = &core.FilterGroup{Operator: "and", Children: []core.FilterGroup{child}}
			}
		}
		if f.RuleVisibility != "" {
			f.ApplyCanonicalInPreview = true
		} else if len(lib.Rules) > 0 || lib.VisibilityDefault != "" {
			f.ExtraRules = lib.Rules
			f.ExtraFallback = lib.VisibilityDefault
		}
	}
	return f
}

func (c *librariesConsole) browseTitles(ctx context.Context, lib core.Library, req core.Filter, q core.BrowseQuery) (core.Page, error) {
	q.Filter = libraryScopeFilter(lib, req)
	if q.Sort == "" {
		q.Sort = core.SortPopularity
		q.Desc = true
	}
	if q.Limit <= 0 {
		q.Limit = 48
	}
	return c.app.collections.BrowseTitles(ctx, q)
}

func (c *librariesConsole) library(w http.ResponseWriter, r *http.Request) (core.Library, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		libError(w, http.StatusBadRequest, "Invalid library id")
		return core.Library{}, false
	}
	lib, err := c.app.libraries.Get(r.Context(), core.LibraryID(id))
	if err != nil {
		libError(w, libErrStatus(err), "Library not found")
		return core.Library{}, false
	}
	return lib, true
}

func (c *librariesConsole) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		libs, err := c.app.libraries.List(r.Context())
		if err != nil {
			libError(w, libErrStatus(err), "Could not list libraries")
			return
		}
		out := make([]libraryDTO, len(libs))
		for i, lib := range libs {
			out[i] = c.decorate(r.Context(), lib)
		}
		libJSON(w, http.StatusOK, map[string]any{"libraries": out})
	})
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name    string              `json:"name"`
			Filter  core.Filter         `json:"filter"`
			Present *core.Presentations `json:"present"`
		}
		if !decodeLibBody(w, r, &body) {
			return
		}
		// Console-only by default (§15.3): the owner opts in per
		// protocol afterwards from the library's Settings tab.
		var present core.Presentations
		if body.Present != nil {
			present = *body.Present
		}
		lib, err := c.app.libraries.CreateFilter(r.Context(), body.Name, body.Filter, present)
		if err != nil {
			libError(w, libErrStatus(err), "Could not create the library")
			return
		}
		libJSON(w, http.StatusCreated, c.decorate(r.Context(), lib))
	})
	mux.HandleFunc("GET /{id}", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		libJSON(w, http.StatusOK, c.decorate(r.Context(), lib))
	})
	mux.HandleFunc("PATCH /{id}", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		var body struct {
			Name              *string                  `json:"name"`
			Hidden            *bool                    `json:"hidden"`
			Sort              *int                     `json:"sort"`
			Present           *core.Presentations      `json:"present"`
			Filter            *core.Filter             `json:"filter"`
			Rules             *[]core.LibraryExclusion `json:"rules"`
			VisibilityDefault *string                  `json:"visibilityDefault"`
			QualityProfile    *string                  `json:"qualityProfile"`
		}
		if !decodeLibBody(w, r, &body) {
			return
		}
		if body.Filter != nil {
			// libraries.Service has no SetFilter yet (slice A gap):
			// a saved filter is immutable in v0.3.8 rather than
			// silently replaced under a stable library id.
			libError(w, http.StatusBadRequest, "A saved filter cannot be changed yet; delete and recreate the library")
			return
		}
		ctx := r.Context()
		svc := c.app.libraries
		if body.Name != nil {
			if err := svc.Rename(ctx, lib.ID, *body.Name); err != nil {
				libError(w, libErrStatus(err), "Could not rename the library")
				return
			}
		}
		if body.Hidden != nil {
			if err := svc.SetHidden(ctx, lib.ID, *body.Hidden); err != nil {
				libError(w, libErrStatus(err), "Could not update the library")
				return
			}
		}
		if body.Sort != nil {
			if err := svc.SetSort(ctx, lib.ID, *body.Sort); err != nil {
				libError(w, libErrStatus(err), "Could not reorder the library")
				return
			}
		}
		if body.Present != nil {
			if err := svc.SetPresent(ctx, lib.ID, *body.Present); err != nil {
				libError(w, libErrStatus(err), "Could not update where the library appears")
				return
			}
		}
		if body.QualityProfile != nil {
			if err := svc.SetQualityProfile(ctx, lib.ID, *body.QualityProfile); err != nil {
				libError(w, libErrStatus(err), "Could not update the quality profile")
				return
			}
		}
		if body.Rules != nil || body.VisibilityDefault != nil {
			if lib.Source == core.LibraryPlugin {
				libError(w, http.StatusBadRequest, "Plugin libraries do not use TMDB visibility rules")
				return
			}
			rules := lib.Rules
			if body.Rules != nil {
				rules = *body.Rules
			}
			visDefault := lib.VisibilityDefault
			if body.VisibilityDefault != nil {
				visDefault = *body.VisibilityDefault
			}
			if _, err := libraryfilter.CompilePolicy(rules, visDefault); err != nil {
				libError(w, http.StatusBadRequest, "Invalid visibility rules")
				return
			}
			if err := svc.SetRules(ctx, lib.ID, rules, visDefault); err != nil {
				libError(w, libErrStatus(err), "Could not save visibility rules")
				return
			}
			if lib.ID == core.LibraryMovies || lib.ID == core.LibraryShows {
				if err := c.app.refreshLibraryPolicies(ctx); err != nil {
					libError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
		fresh, err := svc.Get(ctx, lib.ID)
		if err != nil {
			libError(w, libErrStatus(err), "Library not found")
			return
		}
		libJSON(w, http.StatusOK, c.decorate(ctx, fresh))
	})
	mux.HandleFunc("DELETE /{id}", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		if lib.Source == core.LibraryPlugin {
			libError(w, http.StatusConflict, "Plugin libraries are removed with their plugin")
			return
		}
		if err := c.app.libraries.Delete(r.Context(), lib.ID); err != nil {
			libError(w, libErrStatus(err), "Could not delete the library")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /{id}/items", c.handleItems)
	mux.HandleFunc("GET /{id}/facets/{field...}", c.handleFacets)
	mux.HandleFunc("GET /{id}/item/{ref}/match", c.handleMatchGet)
	mux.HandleFunc("PUT /{id}/item/{ref}/match", c.handleMatchPin)
	mux.HandleFunc("DELETE /{id}/item/{ref}/match", c.handleMatchClear)
	mux.HandleFunc("POST /{id}/item/{ref}/match", c.handleMatchRun)
	mux.HandleFunc("GET /{id}/item/{ref}", c.handleItem)
	mux.HandleFunc("POST /{id}/sync", c.handleSync)
	mux.HandleFunc("GET /{id}/cache", c.handleCacheGet)
	mux.HandleFunc("PUT /{id}/cache", c.handleCachePut)
	mux.HandleFunc("POST /{id}/cache/sweep", c.handleCacheSweep)
	mux.HandleFunc("POST /{id}/cache/purge", c.handleCachePurge)
	mux.HandleFunc("GET /{id}/setup", func(w http.ResponseWriter, r *http.Request) {
		lib, ok := c.library(w, r)
		if !ok {
			return
		}
		c.writeSetup(w, r, lib)
	})
	return mux
}

func parseFacets(r *http.Request) map[string][]string {
	out := map[string][]string{}
	for k, vs := range r.URL.Query() {
		if f, ok := strings.CutPrefix(k, "facet."); ok && f != "" {
			for _, v := range vs {
				out[f] = append(out[f], strings.Split(v, ",")...)
			}
		}
	}
	return out
}

func (c *librariesConsole) handleItems(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	p := r.URL.Query()
	limit, _ := strconv.Atoi(p.Get("limit"))
	if lib.Source == core.LibraryPlugin {
		items, _, err := c.services()
		if err != nil || items == nil {
			libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
			return
		}
		facets := parseFacets(r)
		if q := strings.TrimSpace(p.Get("q")); q != "" {
			page, err := items.Search(r.Context(), pluginitems.SearchQuery{
				LibraryID: lib.ID, Query: q, Level: p.Get("level"), Facets: facets, Limit: limit,
			})
			if err != nil {
				libError(w, http.StatusInternalServerError, "Search failed")
				return
			}
			libJSON(w, http.StatusOK, map[string]any{
				"items": kindDTOs(page.Items), "total": page.Total, "nextCursor": page.NextCursor,
			})
			return
		}
		page, err := items.Browse(r.Context(), pluginitems.BrowseQuery{
			LibraryID: lib.ID,
			ParentRef: core.ItemRef(p.Get("parent")),
			Level:     p.Get("level"),
			Sort:      p.Get("sort"),
			Desc:      p.Get("desc") == "1",
			Facets:    facets,
			Cursor:    p.Get("cursor"),
			Limit:     limit,
		})
		if err != nil {
			libError(w, http.StatusBadRequest, "Could not browse this library")
			return
		}
		libJSON(w, http.StatusOK, map[string]any{
			"items": kindDTOs(page.Items), "total": page.Total, "nextCursor": page.NextCursor,
		})
		return
	}
	// Builtin/filter: TMDB titles through the catalogue, scoped.
	var reqFilter core.Filter
	if raw := p.Get("filter"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &reqFilter); err != nil {
			libError(w, http.StatusBadRequest, "Invalid filter")
			return
		}
	}
	if q := strings.TrimSpace(p.Get("q")); q != "" {
		reqFilter.Query = q
	}
	offset, _ := strconv.Atoi(p.Get("offset"))
	sort := core.SortOrder(p.Get("sort"))
	page, err := c.browseTitles(r.Context(), lib, reqFilter, core.BrowseQuery{
		Sort: sort, Desc: p.Get("desc") != "0", Offset: offset, Limit: limit,
	})
	if err != nil {
		libError(w, http.StatusInternalServerError, "Could not browse this library")
		return
	}
	if page.Items == nil {
		page.Items = []core.Title{}
	}
	libJSON(w, http.StatusOK, map[string]any{"items": page.Items, "total": page.Total})
}

func (c *librariesConsole) handleFacets(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	field := r.PathValue("field")
	p := r.URL.Query()
	limit, _ := strconv.Atoi(p.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if lib.Source == core.LibraryPlugin {
		items, _, err := c.services()
		if err != nil || items == nil {
			libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
			return
		}
		values, err := items.Facets(r.Context(), lib.ID, p.Get("level"), field, limit)
		if err != nil {
			libError(w, http.StatusInternalServerError, "Could not list filter values")
			return
		}
		if values == nil {
			values = []pluginitems.FacetCount{}
		}
		libJSON(w, http.StatusOK, map[string]any{"field": field, "values": values})
		return
	}
	t := lib.MediaType
	if lib.Filter != nil && lib.Filter.Type != "" {
		t = lib.Filter.Type
	}
	values, err := c.app.collections.Facets(r.Context(), t, field)
	if err != nil {
		libError(w, http.StatusBadRequest, "Unknown filter")
		return
	}
	if values == nil {
		values = []core.FacetValue{}
	}
	libJSON(w, http.StatusOK, map[string]any{"field": field, "values": values})
}

func (c *librariesConsole) itemRef(w http.ResponseWriter, r *http.Request, lib core.Library) (core.ItemRef, *pluginitems.Service, bool) {
	if lib.Source != core.LibraryPlugin {
		libError(w, http.StatusNotFound, "Only plugin libraries have plugin items")
		return "", nil, false
	}
	ref, err := core.ParseItemRef(r.PathValue("ref"))
	if err != nil {
		libError(w, http.StatusBadRequest, "Invalid item reference")
		return "", nil, false
	}
	items, _, err := c.services()
	if err != nil || items == nil {
		libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
		return "", nil, false
	}
	if _, libID, err := items.GetByRef(r.Context(), ref); err != nil {
		libError(w, http.StatusNotFound, "Item not found")
		return "", nil, false
	} else if libID != lib.ID {
		libError(w, http.StatusNotFound, "Item not found")
		return "", nil, false
	}
	return ref, items, true
}

func (c *librariesConsole) handleItem(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	ctx := r.Context()
	item, _, _ := items.GetByRef(ctx, ref)
	children, _ := items.Children(ctx, ref, r.URL.Query().Get("cursor"), 48)
	matches, _ := items.ListMatches(ctx, ref)
	if matches == nil {
		matches = []pluginitems.MatchRecord{}
	}
	out := map[string]any{
		"item":     kindItemDTO{KindItem: item, Ref: string(item.Ref), Key: int64(item.Key)},
		"children": kindDTOs(children.Items),
		"matches":  matches,
	}
	if rec, err := items.GetKindByLibrary(ctx, lib.ID); err == nil {
		out["kind"] = rec.Decl
	}
	if src, found, err := items.GetResolution(ctx, ref); err == nil && found {
		out["resolution"] = src
	}
	libJSON(w, http.StatusOK, out)
}

func (c *librariesConsole) handleMatchGet(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	matches, err := items.ListMatches(r.Context(), ref)
	if err != nil {
		libError(w, http.StatusInternalServerError, "Could not list matches")
		return
	}
	if matches == nil {
		matches = []pluginitems.MatchRecord{}
	}
	libJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

func (c *librariesConsole) handleMatchPin(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	var body struct {
		CandidateRef string   `json:"candidateRef"`
		Reasons      []string `json:"reasons"`
	}
	if !decodeLibBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.CandidateRef) == "" {
		libError(w, http.StatusBadRequest, "Pick a match first")
		return
	}
	if err := items.PinMatch(r.Context(), ref, body.CandidateRef, body.Reasons); err != nil {
		libError(w, http.StatusInternalServerError, "Could not save the match")
		return
	}
	c.handleMatchGet(w, r)
}

func (c *librariesConsole) handleMatchClear(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	if err := items.ClearManualMatch(r.Context(), ref); err != nil {
		libError(w, http.StatusInternalServerError, "Could not clear the match")
		return
	}
	c.handleMatchGet(w, r)
}

func (c *librariesConsole) handleMatchRun(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ref, items, ok := c.itemRef(w, r, lib)
	if !ok {
		return
	}
	if c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
		libError(w, http.StatusServiceUnavailable, "Matching needs the plugin")
		return
	}
	ctx := r.Context()
	item, _, err := items.GetByRef(ctx, ref)
	if err != nil {
		libError(w, http.StatusNotFound, "Item not found")
		return
	}
	threshold := pluginitems.DefaultMatchThreshold
	if rec, err := items.GetKindByLibrary(ctx, lib.ID); err == nil && rec.Decl.Match.ThresholdDefault > 0 {
		threshold = rec.Decl.Match.ThresholdDefault
	}
	scores, err := items.MatchItem(ctx, c.app.pluginsRT.mgr, lib.PluginID, lib.KindKey, item, nil, nil, threshold)
	if err != nil {
		libError(w, http.StatusBadGateway, "The plugin could not match this item")
		return
	}
	if err := items.SaveMatches(ctx, ref, "auto", scores, threshold); err != nil {
		libError(w, http.StatusInternalServerError, "Could not save matches")
		return
	}
	c.handleMatchGet(w, r)
}

func (c *librariesConsole) handleSync(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	if lib.Source != core.LibraryPlugin {
		libError(w, http.StatusConflict, "Only plugin libraries sync from a plugin")
		return
	}
	items, _, err := c.services()
	if err != nil || items == nil {
		libError(w, http.StatusServiceUnavailable, "Plugin items are unavailable")
		return
	}
	var body struct {
		Force    bool `json:"force"`
		FullWalk bool `json:"fullWalk"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeLibBody(w, r, &body) {
		return
	}
	res, err := items.SyncKind(r.Context(), lib.PluginID, lib.KindKey, pluginitems.SyncOptions{
		Force: body.Force, FullWalk: body.FullWalk,
	})
	if err != nil {
		libError(w, http.StatusBadGateway, "Sync failed")
		return
	}
	libJSON(w, http.StatusOK, map[string]any{
		"upserted": res.Upserted, "deleted": res.Deleted, "skipped": res.Skipped,
	})
}

// --- per-library blob cache (spec §11 Cache row, §15.5) ---

func (c *librariesConsole) namespace(w http.ResponseWriter, r *http.Request, lib core.Library) (*blobcache.Namespace, bool) {
	if lib.Source != core.LibraryPlugin {
		libError(w, http.StatusNotFound, "Only plugin libraries have a file cache")
		return nil, false
	}
	items, blobs, err := c.services()
	if err != nil || blobs == nil {
		libError(w, http.StatusServiceUnavailable, "The file cache is unavailable")
		return nil, false
	}
	quota := blobcache.Quota{}
	if items != nil {
		if rec, err := items.GetKindByLibrary(r.Context(), lib.ID); err == nil {
			quota.MaxBytes = rec.Decl.Cache.MaxBytes
			if ttl, err := time.ParseDuration(rec.Decl.Cache.TTL); err == nil {
				quota.TTL = ttl
			}
		}
	}
	blobs.BindPluginLibrary(lib.PluginID, lib.Key)
	return blobs.Namespace(lib.Key, quota), true
}

func cacheDTO(lib core.Library, ns *blobcache.Namespace) map[string]any {
	used, blobs := ns.Stats()
	q := ns.Quota()
	return map[string]any{
		"libraryId": int64(lib.ID), "used": used, "blobs": blobs,
		"maxBytes": q.EffectiveMaxBytes(), "ttlSeconds": int64(q.TTL.Seconds()),
	}
}

func (c *librariesConsole) handleCacheGet(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	_, blobs, _ := c.services()
	out := cacheDTO(lib, ns)
	totalUsed, totalBlobs := blobs.TotalStats()
	out["sharedUsed"] = totalUsed
	out["sharedBlobs"] = totalBlobs
	out["sharedCap"] = blobs.EffectiveSharedCap()
	libJSON(w, http.StatusOK, out)
}

func (c *librariesConsole) handleCachePut(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	var body struct {
		MaxBytes   int64 `json:"maxBytes"`
		TTLSeconds int64 `json:"ttlSeconds"`
	}
	if !decodeLibBody(w, r, &body) {
		return
	}
	if body.MaxBytes < 0 || body.TTLSeconds < 0 {
		libError(w, http.StatusBadRequest, "Cache limits cannot be negative")
		return
	}
	// Owner quota overrides are session-lived; the kind's declared cap (or
	// the 2 GiB default) applies again at restart.
	ns.SetQuota(blobcache.Quota{MaxBytes: body.MaxBytes, TTL: time.Duration(body.TTLSeconds) * time.Second})
	ns.Sweep()
	libJSON(w, http.StatusOK, cacheDTO(lib, ns))
}

func (c *librariesConsole) handleCacheSweep(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	ns.Sweep()
	libJSON(w, http.StatusOK, cacheDTO(lib, ns))
}

func (c *librariesConsole) handleCachePurge(w http.ResponseWriter, r *http.Request) {
	lib, ok := c.library(w, r)
	if !ok {
		return
	}
	ns, ok := c.namespace(w, r, lib)
	if !ok {
		return
	}
	ns.Purge()
	libJSON(w, http.StatusOK, cacheDTO(lib, ns))
}

// --- guided setup (spec §8): library-scoped view + plugin-scoped API ---

type setupStepDTO struct {
	plugins.SetupStep
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func (c *librariesConsole) setupFor(ctx context.Context, pluginID string) (map[string]any, error) {
	mgr := c.app.pluginsRT.mgr
	rec, err := mgr.Get(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	state, err := mgr.SetupState(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	byStep := map[string]plugins.SetupStepState{}
	for _, st := range state {
		byStep[st.Step] = st
	}
	// The setup view shows the plugin's own "$t:" text resolved, never the
	// raw keys.
	resolveText := func(v string) string {
		key, ok := strings.CutPrefix(strings.TrimSpace(v), "$t:")
		if !ok {
			return v
		}
		if got, ok := mgr.ResolveText(ctx, pluginID, key, "en"); ok && strings.TrimSpace(got) != "" {
			return got
		}
		return key
	}
	steps := []setupStepDTO{}
	blockers := []string{}
	if e := rec.Manifest.EffectiveEther(); e != nil && e.Contributes.Setup != nil {
		for _, s := range e.Contributes.Setup.Steps {
			st := byStep[s.Key]
			status := st.Status
			if status == "" {
				status = plugins.SetupPending
			}
			s.Label, s.Hint = resolveText(s.Label), resolveText(s.Hint)
			steps = append(steps, setupStepDTO{SetupStep: s, Status: status, Detail: st.Detail})
			if s.Required && status != plugins.SetupOK && status != plugins.SetupSkipped {
				blockers = append(blockers, s.Key)
			}
		}
	}
	settings, schema, err := mgr.GetSettings(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	kinds := []plugins.MediaKindMeta{}
	libraries := []libraryDTO{}
	if e := rec.Manifest.EffectiveEther(); e != nil {
		kinds = append(kinds, e.Contributes.MediaKinds...)
	}
	if libs, err := c.app.libraries.List(ctx); err == nil {
		for _, lib := range libs {
			if lib.Source == core.LibraryPlugin && lib.PluginID == pluginID {
				libraries = append(libraries, c.decorate(ctx, lib))
			}
		}
	}
	if len(schema) > 0 {
		var doc any
		if json.Unmarshal(schema, &doc) == nil {
			doc = resolveSchemaText(doc, resolveText)
			if b, err := json.Marshal(doc); err == nil {
				schema = b
			}
		}
	}
	return map[string]any{
		"pluginId": pluginID, "pluginName": rec.Name, "steps": steps,
		"blockers": blockers, "settings": settings, "schema": json.RawMessage(schema),
		"kinds": kinds, "libraries": libraries,
	}, nil
}

func (c *librariesConsole) writeSetup(w http.ResponseWriter, r *http.Request, lib core.Library) {
	if lib.Source != core.LibraryPlugin || c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
		libError(w, http.StatusNotFound, "This library has no plugin setup")
		return
	}
	out, err := c.setupFor(r.Context(), lib.PluginID)
	if err != nil {
		libError(w, http.StatusNotFound, "Plugin not found")
		return
	}
	libJSON(w, http.StatusOK, out)
}

func (c *librariesConsole) setupHandler() http.Handler {
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter) bool {
		if c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
			libError(w, http.StatusServiceUnavailable, "Plugins are unavailable")
			return false
		}
		return true
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if c.app.pluginsRT == nil || c.app.pluginsRT.mgr == nil {
			libJSON(w, http.StatusOK, map[string]any{"plugins": []any{}})
			return
		}
		recs, err := c.app.pluginsRT.mgr.List(r.Context())
		if err != nil {
			libError(w, http.StatusInternalServerError, "Could not list plugins")
			return
		}
		var libs []core.Library
		if c.app.libraries != nil {
			libs, _ = c.app.libraries.List(r.Context())
		}
		out := []map[string]any{}
		for _, rec := range recs {
			e := rec.Manifest.EffectiveEther()
			if e == nil || len(e.Contributes.MediaKinds) == 0 {
				continue
			}
			pluginLibs := []libraryDTO{}
			for _, lib := range libs {
				if lib.Source == core.LibraryPlugin && lib.PluginID == rec.ID {
					pluginLibs = append(pluginLibs, c.decorate(r.Context(), lib))
				}
			}
			out = append(out, map[string]any{
				"pluginId":   rec.ID,
				"pluginName": rec.Name,
				"summary":    rec.Manifest.Description,
				"kinds":      e.Contributes.MediaKinds,
				"libraries":  pluginLibs,
			})
		}
		libJSON(w, http.StatusOK, map[string]any{"plugins": out})
	})
	mux.HandleFunc("POST /{plugin}/ensure", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		ctx := r.Context()
		pluginID := r.PathValue("plugin")
		rec, err := c.app.pluginsRT.mgr.Get(ctx, pluginID)
		if err != nil {
			libError(w, http.StatusNotFound, "Plugin not found")
			return
		}
		e := rec.Manifest.EffectiveEther()
		if e == nil || len(e.Contributes.MediaKinds) == 0 {
			libError(w, http.StatusBadRequest, "This plugin does not contribute a library kind")
			return
		}
		items, _, err := c.services()
		if err != nil {
			libError(w, http.StatusInternalServerError, "Plugin items are unavailable")
			return
		}
		for _, decl := range e.Contributes.MediaKinds {
			name := strings.TrimSpace(decl.Library.Name)
			if name == "" {
				name = strings.TrimSpace(decl.Label)
			}
			if name == "" {
				name = rec.Name
			}
			lib, err := c.app.libraries.UpsertPluginLibrary(ctx, pluginID, decl.Key, libraries.PluginLibraryDecl{
				Name:    name,
				Present: decl.Present,
			})
			if err != nil {
				libError(w, libErrStatus(err), "Could not create plugin library")
				return
			}
			if _, err := items.UpsertKind(ctx, pluginID, lib.ID, decl, false); err != nil {
				libError(w, http.StatusBadRequest, "Could not initialize plugin library kind")
				return
			}
		}
		out, err := c.setupFor(ctx, pluginID)
		if err != nil {
			libError(w, http.StatusNotFound, "Plugin not found")
			return
		}
		libJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /{plugin}", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		out, err := c.setupFor(r.Context(), r.PathValue("plugin"))
		if err != nil {
			libError(w, http.StatusNotFound, "Plugin not found")
			return
		}
		libJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("PUT /{plugin}/settings", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		var values map[string]any
		if !decodeLibBody(w, r, &values) {
			return
		}
		fieldErrors, err := c.app.pluginsRT.mgr.Configure(r.Context(), r.PathValue("plugin"), values)
		if err != nil {
			libError(w, http.StatusBadRequest, "Could not save these settings")
			return
		}
		libJSON(w, http.StatusOK, map[string]any{"fieldErrors": fieldErrors})
	})
	mux.HandleFunc("POST /{plugin}/{step}/test", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		res, err := c.app.pluginsRT.mgr.TestSetupStep(r.Context(), r.PathValue("plugin"), r.PathValue("step"))
		if err != nil {
			libError(w, http.StatusNotFound, "Setup step not found")
			return
		}
		libJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /{plugin}/{step}/skip", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		if err := c.app.pluginsRT.mgr.SkipSetupStep(r.Context(), r.PathValue("plugin"), r.PathValue("step")); err != nil {
			libError(w, http.StatusNotFound, "Setup step not found")
			return
		}
		libJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return mux
}

// resolveSchemaText resolves "$t:" references in a plugin settings schema's
// human text (labels, hints, help, placeholders, option labels).
func resolveSchemaText(v any, resolve func(string) string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			switch k {
			case "label", "hint", "help", "placeholder", "title", "description":
				if str, ok := child.(string); ok {
					t[k] = resolve(str)
					continue
				}
			}
			t[k] = resolveSchemaText(child, resolve)
		}
		return t
	case []any:
		for i := range t {
			t[i] = resolveSchemaText(t[i], resolve)
		}
		return t
	default:
		return v
	}
}
