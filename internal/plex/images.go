package plex

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/flight"
	"github.com/braedonsaunders/ether/internal/lru"
	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
)

// setArtworkHeaders keeps remote SVG (or an upstream HTML error page) from
// executing on Ether's origin when opened directly. Plex and the owner UI share
// an origin, and the latter stores its owner credential in localStorage. Image
// requests can carry a viewer's query token, so viewer authentication alone does
// not make an active artwork document safe for the owner's browser.
func setArtworkHeaders(h http.Header, contentType string) {
	h.Set("Content-Type", contentType)
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
}

// ---------------------------------------------------------------------------
// Artwork cache: an in-memory LRU of fetched TMDB images, with request
// coalescing so a grid of posters is fetched once.

const (
	maxImageBytes = 15 << 20
	imageTTL      = 24 * time.Hour
)

type cachedImage struct {
	url         string
	data        []byte
	contentType string
	fetchedAt   time.Time
}

type imageCache struct {
	client *http.Client
	budget int64

	mu       sync.Mutex
	items    *lru.Cache[string, *cachedImage]
	inflight flight.Group[string, *cachedImage]
}

func newImageCache(budget int64, client *http.Client) *imageCache {
	return &imageCache{
		client: client,
		budget: budget,
		items: lru.New(lru.Options[string, *cachedImage]{MaxCost: budget, Cost: func(_ string, img *cachedImage) int64 {
			return int64(len(img.data))
		}}),
	}
}

var errImageNotFound = errors.New("image not found")

// get returns the image at u, fetching it once for concurrent callers.
func (c *imageCache) get(ctx context.Context, u string, now time.Time) (*cachedImage, error) {
	c.mu.Lock()
	if img, ok := c.items.Get(u); ok {
		if now.Sub(img.fetchedAt) < imageTTL {
			c.mu.Unlock()
			return img, nil
		}
		c.items.Remove(u)
	}
	c.mu.Unlock()
	// The fetch is shared, so it must not die with the first requester.
	return c.inflight.DoDetached(ctx, u, func(ctx context.Context) (*cachedImage, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		img, err := c.fetch(ctx, u, now)
		if err == nil {
			c.mu.Lock()
			c.addLocked(img)
			c.mu.Unlock()
		}
		return img, err
	})
}

func (c *imageCache) fetch(ctx context.Context, u string, now time.Time) (*cachedImage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errImageNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("image fetch %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image %s too large", u)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = http.DetectContentType(data)
	}
	return &cachedImage{url: u, data: data, contentType: ct, fetchedAt: now}, nil
}

func (c *imageCache) addLocked(img *cachedImage) {
	if int64(len(img.data)) > c.budget {
		return
	}
	c.items.Add(img.url, img)
}

// ---------------------------------------------------------------------------
// TMDB size selection

// tmdbWidths are the width variants the TMDB image CDN serves.
var tmdbWidths = []int{92, 154, 185, 300, 342, 500, 780, 1280}

// tmdbSize picks the smallest variant at least w wide, or "original".
func tmdbSize(w int) string {
	if w <= 0 {
		return "original"
	}
	for _, s := range tmdbWidths {
		if s >= w {
			return "w" + strconv.Itoa(s)
		}
	}
	return "original"
}

var tmdbSizeRe = regexp.MustCompile(`/t/p/(w\d+|h\d+|original)/`)

// resizeTMDBURL rewrites the size segment of a TMDB image URL.
func resizeTMDBURL(u string, width int) string {
	if width <= 0 || !tmdbSizeRe.MatchString(u) {
		return u
	}
	return tmdbSizeRe.ReplaceAllString(u, "/t/p/"+tmdbSize(width)+"/")
}

// ---------------------------------------------------------------------------
// Handlers

// artPath picks the TMDB path for an item's element.
func (s *Server) artPath(ctx context.Context, key core.RatingKey, element string) (path string, defWidth int, err error) {
	switch key.Kind() {
	case core.KindCollection, core.KindSmart:
		if s.d.Collections == nil {
			return "", 0, core.ErrNotFound
		}
		c, err := s.collection(ctx, FromContext(ctx).User, key)
		if err != nil {
			return "", 0, err
		}
		if element == "art" {
			return c.BackdropPath, 1280, nil
		}
		if c.PosterPath == "" {
			// Fall back to the first item's poster the viewer may see:
			// collection items are not filtered by the viewer's policy.
			u := FromContext(ctx).User
			if p, err := s.d.Collections.Items(ctx, u, key, 0, 20); err == nil {
				for i := range p.Items {
					if p.Items[i].PosterPath != "" && s.visibleTitle(ctx, u, &p.Items[i]) {
						return p.Items[i].PosterPath, 500, nil
					}
				}
			}
		}
		return c.PosterPath, 500, nil
	case core.KindPlaylist:
		u := FromContext(ctx).User
		if s.d.Playlists == nil || u == nil {
			return "", 0, core.ErrNotFound
		}
		pl, err := s.d.Playlists.Get(ctx, u.ID, key)
		if err != nil {
			return "", 0, err
		}
		for _, k := range pl.Items {
			if t, err := s.artworkTitle(ctx, k); err == nil && s.visibleTitle(ctx, u, t) {
				if element == "art" && t.BackdropPath != "" {
					return t.BackdropPath, 1280, nil
				}
				if t.PosterPath != "" {
					return t.PosterPath, 500, nil
				}
			}
		}
		return "", 0, core.ErrNotFound
	}
	t, err := s.artworkTitle(ctx, key)
	if err != nil {
		return "", 0, err
	}
	if (!t.Released.IsZero() && t.Released.After(s.now())) || !s.visibleTitle(ctx, FromContext(ctx).User, t) {
		return "", 0, core.ErrNotFound
	}
	parent := func() *core.Title {
		pk := t.ShowKey
		if pk == 0 {
			pk = key.Parent()
			if t.Type == core.TypeEpisode {
				pk = pk.Parent()
			}
		}
		if pk == 0 {
			return nil
		}
		p, err := s.artworkTitle(ctx, pk)
		if err != nil {
			return nil
		}
		return p
	}
	switch element {
	case "art", "background":
		if t.BackdropPath != "" {
			return t.BackdropPath, 1280, nil
		}
		if p := parent(); p != nil && p.BackdropPath != "" {
			return p.BackdropPath, 1280, nil
		}
	case "clearLogo", "logo":
		if t.LogoPath != "" {
			return t.LogoPath, 500, nil
		}
		if p := parent(); p != nil && p.LogoPath != "" {
			return p.LogoPath, 500, nil
		}
	case "banner":
		if t.BackdropPath != "" {
			return t.BackdropPath, 1280, nil
		}
	default: // thumb, poster
		if t.Type == core.TypeEpisode && t.StillPath != "" {
			return t.StillPath, 0, nil
		}
		if t.PosterPath != "" {
			return t.PosterPath, 500, nil
		}
		if p := parent(); p != nil && p.PosterPath != "" {
			return p.PosterPath, 500, nil
		}
	}
	return "", 0, core.ErrNotFound
}

// handleImage serves /library/metadata/{key}/{element}[/{ts}] and the
// collection/playlist equivalents.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	key, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	element := chi.URLParam(r, "element")
	switch element {
	case "thumb", "art", "poster", "banner", "clearLogo", "logo", "background", "composite", "squareArt":
	default:
		s.handleUnknown(w, r)
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("width"))
	s.serveItemImage(w, r, key, element, width)
}

func (s *Server) serveItemImage(w http.ResponseWriter, r *http.Request, key core.RatingKey, element string, width int) {
	if element == "composite" || element == "squareArt" {
		element = "thumb"
	}
	raw := r.URL.Query().Get("collection")
	if raw == "" && s.serveCollectionArtwork(w, r, key, element, width) {
		return
	}
	path, defWidth, err := s.artPath(r.Context(), key, element)
	if raw != "" || err != nil || path == "" {
		t, _, e := s.lookupPlaceholder(r.Context(), FromContext(r.Context()), key, raw)
		if e != nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		path := t.PosterPath
		if element == "art" || element == "background" {
			path = t.BackdropPath
		}
		if element != "thumb" && element != "poster" && element != "art" && element != "background" {
			path = ""
		}
		if path == "" {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if width <= 0 {
			if element == "art" || element == "background" {
				width = 1280
			} else {
				width = 500
			}
		}
		s.serveRemoteImage(w, r, s.d.ImageURL(path, tmdbSize(width)))
		return
	}
	if width <= 0 {
		width = defWidth
	}
	s.serveRemoteImage(w, r, s.d.ImageURL(path, tmdbSize(width)))
}

func (s *Server) serveRemoteImage(w http.ResponseWriter, r *http.Request, u string) {
	img, err := s.images.get(r.Context(), u, s.now())
	if err != nil {
		if errors.Is(err, errImageNotFound) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if r.Context().Err() == nil {
			s.log.Debug("image fetch failed", "url", u, "err", err)
		}
		writeHTTPError(w, http.StatusBadGateway)
		return
	}
	h := w.Header()
	setArtworkHeaders(h, img.contentType)
	h.Set("Cache-Control", "max-age=604800, private")
	h.Add("Vary", "X-Plex-Token")
	h.Set("Content-Length", strconv.Itoa(len(img.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(img.data)
	}
}

// Person artwork is scoped to one title so authorization requires one catalog lookup.
func (s *Server) servePersonImage(w http.ResponseWriter, r *http.Request, key core.RatingKey, kind, identity string) {
	if r.Context().Err() != nil {
		return
	}
	t, err := s.artworkTitle(r.Context(), key)
	if r.Context().Err() != nil || err != nil || t == nil || !t.IsReleased(s.now()) || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var people []core.Person
	switch kind {
	case "actor":
		people = t.Cast
	case "director":
		people = t.Directors
	case "writer":
		people = t.Writers
	default:
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(identity)
	if err != nil || len(raw) == 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var imagePath string
	for _, person := range people {
		if person.ThumbPath == string(raw) {
			imagePath = person.ThumbPath
			break
		}
	}
	if imagePath == "" {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	// Keep the same w185 profile URL emitted by metadata before proxying; poster widths are not profile configuration. Width and height are intentionally ignored.
	s.serveRemoteImage(w, r, s.d.ImageURL(imagePath, "w185"))
}
func (s *Server) handlePersonImage(w http.ResponseWriter, r *http.Request) {
	key, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.servePersonImage(w, r, key, chi.URLParam(r, "kind"), chi.URLParam(r, "identity"))
}

var personImageRe = regexp.MustCompile(`^/library/metadata/(\d+)/person/(actor|director|writer)/([A-Za-z0-9_-]+)$`)

var metadataImageRe = regexp.MustCompile(`^/library/(?:metadata|collections)/(\d+)/(thumb|art|poster|banner|clearLogo|composite|squareArt)(?:/\d+)?$`)
var playlistImageRe = regexp.MustCompile(`^/playlists/(\d+)/(composite|thumb|art)(?:/\d+)?$`)
var discoveryImageRe = regexp.MustCompile(`^/library/metadata/(\d+)/discoveryPoster/[^/]+$`)

// handlePhotoTranscode serves /photo/:/transcode. The url parameter may be
// one of our own artwork paths (relative, or absolute on this server) or an
// absolute TMDB image URL. Ether does no pixel work: it serves the TMDB
// variant closest to the requested width.
func (s *Server) handlePhotoTranscode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	raw := q.Get("url")
	if raw == "" {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	width, _ := strconv.Atoi(q.Get("width"))
	height, _ := strconv.Atoi(q.Get("height"))
	if width <= 0 && height > 0 {
		width = height * 2 / 3 // poster aspect
	} else if q.Get("minSize") == "1" && height > 0 && height*2/3 > width {
		width = height * 2 / 3
	}
	u, err := url.Parse(raw)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if u.IsAbs() && s.allowedImageHost(u.Host) {
		if !s.remoteArtworkAllowed(r, u) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		s.serveRemoteImage(w, r, resizeTMDBURL(u.String(), width))
		return
	}
	if !u.IsAbs() || s.isSelfHost(r, u.Host) {
		path := u.Path
		if m := discoveryImageRe.FindStringSubmatch(path); m != nil {
			key, err := core.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			params := u.Query()
			r = r.Clone(r.Context())
			effective := r.URL.Query()
			effective.Set("width", strconv.Itoa(width))
			r.URL.RawQuery = effective.Encode()
			s.serveDiscoveryPoster(w, r, key, params.Get("kind"), params.Get("lang"))
			return
		}
		if m := personImageRe.FindStringSubmatch(path); m != nil {
			key, err := core.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			s.servePersonImage(w, r, key, m[2], m[3])
			return
		}
		if m := metadataImageRe.FindStringSubmatch(path); m != nil {
			key, err := core.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			if collection := u.Query().Get("collection"); collection != "" {
				r = r.Clone(r.Context())
				q := r.URL.Query()
				q.Set("collection", collection)
				r.URL.RawQuery = q.Encode()
			}
			s.serveItemImage(w, r, key, m[2], width)
			return
		}
		if m := playlistImageRe.FindStringSubmatch(path); m != nil {
			key, err := core.ParseRatingKey(m[1])
			if err != nil {
				writeHTTPError(w, http.StatusNotFound)
				return
			}
			s.serveItemImage(w, r, key, m[2], width)
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if !s.allowedImageHost(u.Host) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	s.serveRemoteImage(w, r, resizeTMDBURL(u.String(), width))
}

// isSelfHost reports whether host refers to this server.
func (s *Server) isSelfHost(r *http.Request, host string) bool {
	h := strings.ToLower(host)
	if h == "" || strings.EqualFold(h, r.Host) {
		return true
	}
	name := h
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.HasSuffix(h, "]") {
		name = h[:i]
	}
	name = strings.Trim(name, "[]")
	return name == "localhost" || name == "127.0.0.1" || name == "::1" || strings.HasSuffix(name, ".plex.direct")
}

// allowedImageHost limits the proxy to the artwork CDN, so it is not an
// open proxy.
func (s *Server) allowedImageHost(host string) bool {
	ref, err := url.Parse(s.d.ImageURL("/x.jpg", "original"))
	if err != nil {
		return false
	}
	return strings.EqualFold(ref.Host, host)
}
