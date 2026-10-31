package admin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Catalogue helpers for the Collection Studio and the TMDB image proxy.

// CatalogHelper backs /ether/api/catalog/*. Implemented by
// internal/collections.
type CatalogHelper interface {
	SearchTitles(ctx context.Context, q string, t core.MediaType, limit int) ([]core.Title, error)
	// Facets: genre, decade, contentRating, studio, country, language.
	Facets(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error)
	// Lookup kinds: person, keyword, company, network, collection, watchProvider.
	Lookup(ctx context.Context, kind, q, region string) ([]core.LookupResult, error)
	TitleDetail(ctx context.Context, key core.RatingKey) (*core.Title, error)
}

// CatalogBrowser exposes paginated local browsing without a remote TMDB search.
type CatalogBrowser interface {
	BrowseTitles(context.Context, core.BrowseQuery) (core.Page, error)
}

func (h *Handler) handleCatalogBrowse(w http.ResponseWriter, r *http.Request) {
	browser, ok := h.deps.CatalogHelper.(CatalogBrowser)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Catalog browsing is unavailable")
		return
	}
	p := r.URL.Query()
	q := core.BrowseQuery{Limit: 48, Sort: core.SortPopularity, Desc: true}
	q.Filter.Type = mediaTypeParam(p.Get("type"))
	q.Filter.Query = strings.TrimSpace(p.Get("q"))
	if genre := p.Get("genre"); genre != "" {
		q.Filter.Genres = []string{genre}
	}
	if decade := p.Get("decade"); decade != "" {
		year, err := strconv.Atoi(decade)
		if err != nil || year < 1800 || year > 2200 || year%10 != 0 {
			writeValidationError(w, []FieldError{{Field: "decade", Message: "Choose a valid decade"}})
			return
		}
		q.Filter.YearFrom, q.Filter.YearTo = year, year+9
	}
	if sort := p.Get("sort"); sort != "" {
		switch core.SortOrder(sort) {
		case core.SortPopularity, core.SortRating, core.SortReleased, core.SortAdded, core.SortTitle:
			q.Sort = core.SortOrder(sort)
			q.Desc = q.Sort != core.SortTitle
		default:
			writeValidationError(w, []FieldError{{Field: "sort", Message: "Unknown catalog sort"}})
			return
		}
	}
	for _, field := range []string{"offset", "limit"} {
		if raw := p.Get(field); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || (field == "limit" && (n == 0 || n > 100)) {
				writeValidationError(w, []FieldError{{Field: field, Message: "Invalid page bounds"}})
				return
			}
			if field == "offset" {
				q.Offset = n
			} else {
				q.Limit = n
			}
		}
	}
	page, err := browser.BrowseTitles(r.Context(), q)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": cardsFor(page.Items), "total": page.Total})
}

// ImageProxy serves TMDB artwork by path at the TMDB size covering w.
// Implemented by internal/artwork. It must reject anything that is not a
// TMDB image path.
type ImageProxy interface {
	Get(ctx context.Context, path string, w int) ([]byte, string, error)
}

// TitleCard is the shared title shape of the owner API.
type TitleCard struct {
	Key           string  `json:"key"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	Year          int     `json:"year,omitempty"`
	Summary       string  `json:"summary,omitempty"`
	Poster        string  `json:"poster,omitempty"`
	Backdrop      string  `json:"backdrop,omitempty"`
	Rating        float64 `json:"rating,omitempty"`
	ContentRating string  `json:"contentRating,omitempty"`
	// ContentRatingSource is where ContentRating came from ("tmdb:US:6",
	// "omdb"); see core.Title.
	ContentRatingSource string   `json:"contentRatingSource,omitempty"`
	RuntimeMinutes      int      `json:"runtimeMinutes,omitempty"`
	Genres              []string `json:"genres,omitempty"`
	ShowTitle           string   `json:"showTitle,omitempty"`
	Season              int      `json:"season,omitempty"`
	Episode             int      `json:"episode,omitempty"`
}

// ImageURL is the browser-loadable proxy URL of a TMDB image path.
func ImageURL(path string, w int) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return "/ether/api/image?path=" + url.QueryEscape(path) + "&w=" + strconv.Itoa(w)
}

// CardFor converts a catalogue title to a TitleCard.
func CardFor(t core.Title) TitleCard {
	c := TitleCard{
		Key: t.Key.String(), Type: string(t.Type), Title: t.Title, Year: t.Year, Summary: t.Summary,
		Rating: roundRating(t.Rating), ContentRating: t.ContentRating, ContentRatingSource: t.ContentRatingSource, RuntimeMinutes: int(t.Runtime / time.Minute),
		Genres: t.Genres, ShowTitle: t.ShowTitle, Season: t.Season, Episode: t.Episode,
	}
	poster := t.PosterPath
	if t.Type == core.TypeEpisode && t.StillPath != "" {
		poster = t.StillPath
	}
	c.Poster = ImageURL(poster, 342)
	c.Backdrop = ImageURL(t.BackdropPath, 780)
	return c
}

func cardsFor(ts []core.Title) []TitleCard {
	out := make([]TitleCard, 0, len(ts))
	for _, t := range ts {
		out = append(out, CardFor(t))
	}
	return out
}

func roundRating(r float64) float64 { return float64(int(r*10+0.5)) / 10 }

func mediaTypeParam(v string) core.MediaType {
	switch strings.ToLower(v) {
	case "movie", "movies":
		return core.TypeMovie
	case "show", "shows", "tv":
		return core.TypeShow
	}
	return ""
}

func (h *Handler) handleCatalogSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	items, err := h.deps.CatalogHelper.SearchTitles(r.Context(), q.Get("q"), mediaTypeParam(q.Get("type")), limit)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": cardsFor(items)})
}

func (h *Handler) handleCatalogFacets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	facet := q.Get("facet")
	if facet == "" {
		writeValidationError(w, []FieldError{{Field: "facet", Message: "facet is required"}})
		return
	}
	vals, err := h.deps.CatalogHelper.Facets(r.Context(), mediaTypeParam(q.Get("type")), facet)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	if vals == nil {
		vals = []core.FacetValue{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": vals})
}

// lookupResult is a lookup hit with a browser-loadable image.
type lookupResult struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Subtitle string `json:"subtitle,omitempty"`
	Image    string `json:"image,omitempty"`
}

func (h *Handler) handleCatalogLookup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	switch kind {
	case "person", "keyword", "company", "network", "collection", "watchProvider":
	default:
		writeValidationError(w, []FieldError{{Field: "kind", Message: "kind is person, keyword, company, network, collection or watchProvider"}})
		return
	}
	res, err := h.deps.CatalogHelper.Lookup(r.Context(), kind, q.Get("q"), strings.ToUpper(q.Get("region")))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	out := make([]lookupResult, 0, len(res))
	for _, x := range res {
		lr := lookupResult{ID: x.ID, Name: x.Name, Subtitle: x.Subtitle}
		if x.ImagePath != "" {
			lr.Image = ImageURL(x.ImagePath, 185)
		}
		out = append(out, lr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

type personCard struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`
	Image string `json:"image,omitempty"`
}

type titleDetail struct {
	TitleCard
	Cast       []personCard `json:"cast"`
	Directors  []personCard `json:"directors"`
	Keywords   []string     `json:"keywords"`
	Collection *struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"collection"`
}

func people(ps []core.Person, max int) []personCard {
	out := []personCard{}
	for _, p := range ps {
		if len(out) >= max {
			break
		}
		out = append(out, personCard{ID: p.TMDB, Name: p.Name, Role: p.Role, Image: ImageURL(p.ThumbPath, 185)})
	}
	return out
}

func (h *Handler) handleCatalogTitle(w http.ResponseWriter, r *http.Request) {
	key, err := core.ParseRatingKey(r.PathValue("key"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such title")
		return
	}
	t, err := h.deps.CatalogHelper.TitleDetail(r.Context(), key)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	d := titleDetail{TitleCard: CardFor(*t), Cast: people(t.Cast, 20), Directors: people(t.Directors, 5), Keywords: t.Keywords}
	if d.Keywords == nil {
		d.Keywords = []string{}
	}
	if t.CollectionID > 0 {
		d.Collection = &struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}{t.CollectionID, t.CollectionName}
	}
	writeJSON(w, http.StatusOK, d)
}

// errImageNotFound and errInvalidImage are matched by message-agnostic
// interfaces so the proxy can live in another package.
var errBadImagePath = errors.New("path must be a TMDB image path like /abc123.jpg")

// handleImage proxies TMDB artwork for the owner UI. It is public (images
// are loaded by <img> tags, which cannot send the owner token) but only
// ever serves TMDB image paths, so it is not an open proxy.
func (h *Handler) handleImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if !validImagePath(path) {
		writeError(w, http.StatusBadRequest, "bad_path", errBadImagePath.Error())
		return
	}
	width, _ := strconv.Atoi(q.Get("w"))
	if width <= 0 || width > 4000 {
		width = 500 // never pull TMDB originals (often several MB) by default
	}
	etag := `"` + strconv.Itoa(width) + path + `"`
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, ct, err := h.deps.Images.Get(r.Context(), path, width)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "image not found")
			return
		}
		if r.Context().Err() == nil {
			h.log.Debug("image proxy", "path", path, "err", err)
		}
		writeError(w, http.StatusBadGateway, "upstream", "could not fetch the image")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ct)
	// Images (SVG logos, owner-supplied custom art) must never run script on
	// the owner UI origin.
	hdr.Set("Content-Security-Policy", imageCSP)
	// TMDB image paths are content-addressed: a path never changes.
	hdr.Set("Cache-Control", "public, max-age=2592000, immutable")
	hdr.Set("ETag", etag)
	hdr.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// imageCSP sandboxes proxied images: an SVG opened directly cannot run
// script or load anything.
const imageCSP = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox"

// validImagePath accepts a single TMDB file name: "/abc_123-x.jpg".
func validImagePath(p string) bool {
	if len(p) < 6 || len(p) > 140 || p[0] != '/' {
		return false
	}
	dot := strings.LastIndexByte(p, '.')
	if dot < 2 {
		return false
	}
	switch strings.ToLower(p[dot+1:]) {
	case "jpg", "jpeg", "png", "webp", "svg":
	default:
		return false
	}
	for _, c := range p[1:dot] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func isNotFound(err error) bool {
	if errors.Is(err, core.ErrNotFound) {
		return true
	}
	var nf interface{ NotFound() bool }
	if errors.As(err, &nf) && nf.NotFound() {
		return true
	}
	return strings.Contains(err.Error(), "not found")
}

func (h *Handler) handleVisibilityPreview(w http.ResponseWriter, r *http.Request) {
	if h.deps.CatalogVisibility == nil {
		writeError(w, 503, "unavailable", "Catalog preview unavailable")
		return
	}
	var body struct {
		Exclusions        []core.LibraryExclusion `json:"exclusions"`
		VisibilityDefault string                  `json:"visibilityDefault"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body); err != nil {
		writeError(w, 400, "bad_json", "Invalid filter")
		return
	}
	if _, err := libraryfilter.CompilePolicy(body.Exclusions, body.VisibilityDefault); err != nil {
		writeError(w, 400, "invalid_filter", "Could not preview catalogue")
		return
	}
	p, err := h.deps.CatalogVisibility(r.Context(), body.Exclusions, body.VisibilityDefault)
	if err != nil {
		writeError(w, 500, "preview_failed", "Could not preview catalogue")
		return
	}
	writeJSON(w, 200, p)
}
