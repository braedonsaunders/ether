package plex

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"strconv"
)

// DiscoveryFeatures is the shared discovery service adapted by Plex.
type DiscoveryFeatures = discovery.Features

func (s *Server) discoveryHubs(ctx context.Context, pr *Request, limit int) []hubSpec {
	d := s.d.Discovery
	if d == nil || pr.User == nil {
		return nil
	}
	var out []hubSpec
	add := func(purpose core.RowPurpose, load func(context.Context, *core.User, int) ([]core.Hub, error)) {
		var hubs []core.Hub
		selector := experience.DiscoverySelection{Visibility: s.visibility(), Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) }}
		rows := selector.Load(ctx, pr.User, limit, func(ctx context.Context, fetch int) []experience.DiscoveryCandidate {
			hubs, _ = load(ctx, pr.User, fetch)
			candidates := make([]experience.DiscoveryCandidate, len(hubs))
			for i, hub := range hubs {
				candidates[i] = experience.DiscoveryCandidate{Index: i, Purpose: purpose, Items: hub.Items}
			}
			return candidates
		})
		for _, row := range rows {
			h := hubs[row.Index]
			h.Items = row.Items
			out = append(out, hubSpec{Hub: h})
		}
	}
	if d.Watchlist != nil {
		add(core.RowPurpose("watchlist"), d.Watchlist.Hubs)
	}
	if d.Personal != nil {
		add(core.RowPurpose("recommendation"), d.Personal.Hubs)
	}
	return out
}
func (s *Server) discoveryRemoveContinue(ctx context.Context, pr *Request, key core.RatingKey) error {
	if s.d.Discovery == nil || s.d.Discovery.Personal == nil || pr.User == nil {
		return nil
	}
	t, e := s.d.Catalog.Get(ctx, key)
	if e != nil {
		return e
	}
	if !s.visibleTitle(ctx, pr.User, t) {
		return core.ErrNotFound
	}
	return s.d.Discovery.Personal.RemoveContinue(ctx, pr.User.ID, key)
}

func (s *Server) decorateDiscoveryMetadata(ctx context.Context, pr *Request, t *core.Title, m *model.Metadata, detail bool) {
	d := s.d.Discovery
	if d == nil || t == nil || pr.User == nil || !s.titleAllowed(pr.User, t) {
		return
	}
	selected := d.SelectArtwork(ctx, pr.User, t, pr.Language)
	localized := selected.Title
	if localized != t {
		m.Title = localized.Title
		m.Summary = localized.Summary
		m.Tagline = localized.Tagline
	}
	if d.OverviewTranslate != nil && pr.Language != "" {
		if summary, tagline, ok := d.OverviewTranslate(ctx, t, pr.Language, detail); ok {
			if summary != "" {
				m.Summary = summary
			}
			if tagline != "" {
				m.Tagline = tagline
			}
		}
	}
	if selected.PosterVersion != "" {
		m.Thumb = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?lang=%s", t.Key.String(), selected.PosterVersion, url.QueryEscape(pr.Language))
	}
	if selected.BackdropVersion != "" {
		m.Art = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=backdrop&lang=%s", t.Key.String(), selected.BackdropVersion, url.QueryEscape(pr.Language))
	}
	if selected.Enabled {
		hasLogo := false
		for i := range m.Image {
			m.Image[i].Alt = m.Title
			switch m.Image[i].Type {
			case "coverPoster":
				if t.Type != core.TypeEpisode && m.Thumb != "" {
					m.Image[i].URL = m.Thumb
				}
			case "background":
				if m.Art != "" {
					m.Image[i].URL = m.Art
				}
			case "clearLogo":
				hasLogo = true
				if selected.LogoVersion != "" {
					m.Image[i].URL = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=logo&lang=%s", t.Key.String(), selected.LogoVersion, url.QueryEscape(pr.Language))
				}
			}
		}
		if !hasLogo && selected.LogoVersion != "" {
			m.Image = append(m.Image, model.Image{Alt: m.Title, Type: "clearLogo", URL: fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=logo&lang=%s", t.Key.String(), selected.LogoVersion, url.QueryEscape(pr.Language))})
		}
	}
	if d.Ratings != nil && (t.Type == core.TypeMovie || t.Type == core.TypeShow) {
		bs, e := d.Ratings.Cached(ctx, t)
		cfg, ce := d.Ratings.Settings(ctx)
		if e == nil && ce == nil && cfg.Enabled {
			d.Ratings.Queue(*t)
			for _, b := range bs {
				if b.Source == cfg.Critic && b.PlexImage() != "" {
					m.Rating = b.PlexValue()
					m.RatingImage = b.PlexImage()
				}
				if b.Source == cfg.Audience && b.PlexImage() != "" {
					m.AudienceRating = b.PlexValue()
					m.AudienceRatingImage = b.PlexImage()
				}
			}
		}
		if len(bs) > 0 && len(m.Rating_) > 0 {
			m.Rating_ = nil
			if m.RatingImage != "" {
				m.Rating_ = append(m.Rating_, model.RatingTag{Type: "critic", Image: m.RatingImage, Value: m.Rating})
			}
			if m.AudienceRatingImage != "" {
				m.Rating_ = append(m.Rating_, model.RatingTag{Type: "audience", Image: m.AudienceRatingImage, Value: m.AudienceRating})
			}
		}

	}
	if d.LLM != nil && d.Watchlist != nil && (t.Type == core.TypeSeason || t.Type == core.TypeEpisode) {
		cfg, _ := d.Watchlist.Settings(ctx)
		if cfg.Recaps {
			entry := d.CachedRecap(pr.User, t, s.d.State, s.now())
			if entry != "" {
				m.Summary = "Previously watched: " + entry + "\n\n" + m.Summary
			}

		}
	}
}
func (s *Server) handleDiscoveryPoster(w http.ResponseWriter, r *http.Request) {
	k, e := core.ParseRatingKey(chi.URLParam(r, "id"))
	if e != nil {
		writeHTTPError(w, 400)
		return
	}
	q := r.URL.Query()
	s.serveDiscoveryPoster(w, r, k, q.Get("kind"), q.Get("lang"))
}

// serveDiscoveryPoster also serves Plex's photo transcode wrapper. Keep the
// authenticated outer request: credentials inside an artwork URL grant nothing.
func (s *Server) serveDiscoveryPoster(w http.ResponseWriter, r *http.Request, k core.RatingKey, kind, lang string) {
	d := s.d.Discovery
	if d == nil || d.Overlays == nil {
		writeHTTPError(w, 404)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	t, e := s.artworkTitle(ctx, k)
	if e != nil || pr.User == nil || !s.visibleTitle(ctx, pr.User, t) {
		writeHTTPError(w, 404)
		return
	}
	if lang == "" {
		lang = pr.Language
	}
	t, version, e := d.PrepareArtwork(ctx, pr.User, t, lang)
	if e != nil {
		writeHTTPError(w, 404)
		return
	}
	etag := `"` + version + `"`
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Vary", "X-Plex-Token, X-Plex-Language")
	w.Header().Set("ETag", etag)
	if !d.Overlays.CustomPostersEnabled() && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, ct, e := d.RenderArtwork(ctx, pr.User, t, kind, lang)
	if e != nil {
		writeHTTPError(w, 404)
		return
	}
	if d.Overlays.CustomPostersEnabled() {
		if width, _ := strconv.Atoi(r.URL.Query().Get("width")); width > 0 && (kind == "" || kind == "poster") {
			if resized, err := artwork.ResizePoster(data, width); err == nil {
				data = resized
				ct = "image/jpeg"
			}
		}
		etag = fmt.Sprintf("\"%x\"", sha256.Sum256(data))
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	setArtworkHeaders(w.Header(), ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
