package plex

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// DiscoveryFeatures is optional. Viewers use these features through ordinary
// Plex metadata, Home, search and continue-watching endpoints.
type DiscoveryFeatures struct {
	Watchlist *watchlist.Service
	Ratings   *ratings.Service
	Overlays  *overlays.Service
	Personal  *personalize.Discovery
	LLM       *llm.Service
	Localize  func(context.Context, *core.Title, string) (*core.Title, error)
	once      sync.Once
	mu        sync.Mutex
	queue     chan discoveryJob
	cache     map[string]discoveryEntry
	pending   map[string]bool
}
type discoveryJob struct {
	key   string
	title core.Title
	user  core.User
	lang  string
	recap bool
	state core.State
}
type discoveryEntry struct {
	title *core.Title
	recap string
	at    time.Time
}

func (d *DiscoveryFeatures) init() {
	d.once.Do(func() {
		d.queue = make(chan discoveryJob, 128)
		d.cache = map[string]discoveryEntry{}
		d.pending = map[string]bool{}
	})
}
func (d *DiscoveryFeatures) cached(job discoveryJob) discoveryEntry {
	d.init()
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.cache[job.key]
	if time.Since(v.at) < 6*time.Hour {
		return v
	}
	if !d.pending[job.key] {
		select {
		case d.queue <- job:
			d.pending[job.key] = true
		default:
		}
	}
	return v
}

// Run is the sole metadata background worker, bounded and stopped with the app.
func (d *DiscoveryFeatures) Run(ctx context.Context) {
	if d == nil {
		return
	}
	d.init()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.queue:
			bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
			var v discoveryEntry
			var e error
			if job.recap {
				v.recap, e = d.LLM.SeasonRecap(bounded, &job.user, job.title.Key, job.state)
			} else {
				v.title, e = d.Localize(bounded, &job.title, job.lang)
			}
			cancel()
			d.mu.Lock()
			delete(d.pending, job.key)
			v.at = time.Now()
			if e != nil {
				v.title = nil
				v.recap = ""
				v.at = time.Now().Add(-6*time.Hour + 5*time.Minute)
			}
			if len(d.cache) >= 256 {
				for k := range d.cache {
					delete(d.cache, k)
					break
				}
			}
			d.cache[job.key] = v
			d.mu.Unlock()
		}
	}
}

func (s *Server) discoveryHubs(ctx context.Context, pr *Request, limit int) []hubSpec {
	if !s.viewerRestricted(pr.User) {
		return s.legacyDiscoveryHubs(ctx, pr, limit)
	}
	d := s.d.Discovery
	if d == nil || pr.User == nil {
		return nil
	}
	var out []hubSpec
	add := func(purpose core.RowPurpose, load func(context.Context, *core.User, int) ([]core.Hub, error)) {
		var hubs []core.Hub
		rows := experience.LoadDiscovery(ctx, pr.User, s.visibility(), limit, func(ctx context.Context, fetch int) []experience.DiscoveryCandidate {
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
func (s *Server) discoveryContinue(ctx context.Context, pr *Request, items []core.Title) []core.Title {
	if s.d.Discovery == nil || s.d.Discovery.Personal == nil {
		return items
	}
	return s.d.Discovery.Personal.FilterContinue(ctx, pr.User, items)
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

// discoverySearch's bool is false when the owner has left NL search disabled.
func (s *Server) discoverySearch(ctx context.Context, pr *Request, query string, limit int) ([]core.Title, bool) {
	d := s.d.Discovery
	if d == nil || d.LLM == nil || d.Watchlist == nil {
		return nil, false
	}
	cfg, e := d.Watchlist.Settings(ctx)
	if e != nil || !cfg.NaturalSearch {
		return nil, false
	}
	items, e := d.LLM.NaturalSearch(ctx, pr.User, query, limit)
	if e != nil {
		return nil, false
	}
	return items, true
}
func (s *Server) decorateDiscoveryMetadata(ctx context.Context, pr *Request, t *core.Title, m *model.Metadata) {
	d := s.d.Discovery
	if d == nil || t == nil || pr.User == nil || !s.titleAllowed(pr.User, t) {
		return
	}
	localized := t
	if d.Overlays != nil {
		cfg, _ := d.Overlays.Settings(ctx)
		if cfg.Enabled && cfg.Localize && d.Localize != nil && pr.Language != "" {
			entry := d.cached(discoveryJob{key: fmt.Sprintf("lang:%s:%s:%d", t.Key.String(), pr.Language, t.UpdatedAt.Unix()), title: *t, lang: pr.Language})
			if entry.title != nil {
				localized = entry.title
				m.Title = localized.Title
				m.Summary = localized.Summary
				m.Tagline = localized.Tagline
			}

		}
		if (cfg.Enabled && localized.PosterPath != "") || (d.Overlays.CustomPostersEnabled() && (localized.Type == core.TypeMovie || localized.Type == core.TypeShow)) {
			version := d.Overlays.Version(ctx, pr.User, localized, pr.Language)
			m.Thumb = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?lang=%s", t.Key.String(), version, url.QueryEscape(pr.Language))
		}
		if cfg.Enabled {
			_, backdrop := d.Overlays.Paths(ctx, pr.User, localized, pr.Language)
			if backdrop != "" && backdrop != t.BackdropPath {
				m.Art = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=backdrop&lang=%s", t.Key.String(), d.Overlays.Version(ctx, pr.User, localized, pr.Language), url.QueryEscape(pr.Language))
			}
		}
		if cfg.Enabled {
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
					if localized.LogoPath != "" {
						m.Image[i].URL = fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=logo&lang=%s", t.Key.String(), d.Overlays.Version(ctx, pr.User, localized, pr.Language), url.QueryEscape(pr.Language))
					}
				}
			}
			hasLogo := false
			for _, img := range m.Image {
				hasLogo = hasLogo || img.Type == "clearLogo"
			}
			if !hasLogo && localized.LogoPath != "" {
				m.Image = append(m.Image, model.Image{Alt: m.Title, Type: "clearLogo", URL: fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s?kind=logo&lang=%s", t.Key.String(), d.Overlays.Version(ctx, pr.User, localized, pr.Language), url.QueryEscape(pr.Language))})
			}
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
			entry := d.cached(discoveryJob{key: fmt.Sprintf("recap:%d:%d:%d", pr.User.ID, t.Key, s.now().Unix()/3600), title: *t, user: *pr.User, recap: true, state: s.d.State})
			if entry.recap != "" {
				m.Summary = "Previously watched: " + entry.recap + "\n\n" + m.Summary
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
	t, e := s.d.Catalog.Get(ctx, k)
	if e != nil || pr.User == nil || !s.visibleTitle(ctx, pr.User, t) {
		writeHTTPError(w, 404)
		return
	}
	if lang == "" {
		lang = pr.Language
	}
	cfg, _ := d.Overlays.Settings(ctx)
	if cfg.Enabled && cfg.Localize && d.Localize != nil {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		v, e := d.Localize(bounded, t, lang)
		cancel()
		if e == nil && v != nil {
			t = v
		}
	}
	version := d.Overlays.Version(ctx, pr.User, t, lang)
	etag := `"` + version + `"`
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Vary", "X-Plex-Token, X-Plex-Language")
	w.Header().Set("ETag", etag)
	if !d.Overlays.CustomPostersEnabled() && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var data []byte
	var ct string
	switch kind {
	case "backdrop":
		data, ct, e = d.Overlays.Backdrop(ctx, pr.User, t, lang)
	case "logo":
		data, ct, e = d.Overlays.Logo(ctx, t)
	default:
		data, ct, e = d.Overlays.Poster(ctx, pr.User, t, lang)
	}
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
