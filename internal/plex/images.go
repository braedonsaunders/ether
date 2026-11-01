package plex

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plexmeta"
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

const maxImageBytes = artworkservice.MaxImageBytes

var errImageNotFound = artworkservice.ErrRemoteImageNotFound

func tmdbSize(w int) string { return artworkservice.TMDBSize(w) }

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
type refreshingArtworkCatalog struct{ core.Catalog }

func (c refreshingArtworkCatalog) StoredTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	return c.Get(ctx, key)
}
func (s *Server) artPath(ctx context.Context, key core.RatingKey, element string) (string, int, error) {
	source := (FrontendArtwork{Server: s}).source()
	source.Catalog = refreshingArtworkCatalog{s.d.Catalog}
	pr := FromContext(ctx)
	source.CollectionDevice = pr.Device
	return source.ArtPath(ctx, frontend.ArtworkRequest{Viewer: pr.User, Item: key, Device: core.Device{Product: pr.Product, Platform: pr.Platform}}, element)
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
	if raw == "" && key.Kind() != core.KindSmart {
		t, err := s.artworkTitle(r.Context(), key)
		if err != nil {
			t, err = s.scheduledEpisode(r.Context(), FromContext(r.Context()), key)
		}
		if r.Context().Err() != nil || err != nil || t == nil || !t.IsReleased(s.now()) || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
	}
	if raw == "" && s.serveCollectionArtwork(w, r, key, element, width) {
		return
	}
	path, defWidth, err := s.artPath(r.Context(), key, element)
	if raw == "" && err == nil && path != "" {
		if width <= 0 {
			width = defWidth
		}
		s.serveRemoteImage(w, r, s.d.ImageURL(path, tmdbSize(width)))
		return
	}
	if raw != "" || err != nil || path == "" {
		t, _, e := s.lookupPlaceholder(r.Context(), FromContext(r.Context()), key, raw)
		if e != nil && raw == "" {
			t, e = s.scheduledEpisode(r.Context(), FromContext(r.Context()), key)
		}
		if e == nil && t != nil {
			path := t.PosterPath
			if t.Type == core.TypeEpisode {
				path = t.StillPath
			}
			if element == "art" || element == "background" {
				path = t.BackdropPath
			}
			if element != "thumb" && element != "poster" && element != "art" && element != "background" {
				path = ""
			}
			if path != "" && path != "plex" {
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
		}
	}
	if remote := s.plexMetaRemoteImage(r.Context(), key, element); remote != "" {
		if err := s.writeRemoteImage(w, r, remote); err == nil {
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	writeHTTPError(w, http.StatusNotFound)
}

func (s *Server) serveRemoteImage(w http.ResponseWriter, r *http.Request, u string) {
	if err := s.writeRemoteImage(w, r, u); err != nil {
		if errors.Is(err, errImageNotFound) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if r.Context().Err() == nil {
			s.log.Debug("image fetch failed", "url", u, "err", err)
		}
		writeHTTPError(w, http.StatusBadGateway)
	}
}

func (s *Server) writeRemoteImage(w http.ResponseWriter, r *http.Request, u string) error {
	img, err := s.images.Get(r.Context(), u, s.now())
	if err != nil {
		return err
	}
	h := w.Header()
	setArtworkHeaders(h, img.ContentType)
	h.Set("Cache-Control", "max-age=604800, private")
	h.Add("Vary", "X-Plex-Token")
	h.Set("Content-Length", strconv.Itoa(len(img.Data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(img.Data)
	}
	return nil
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
		if ov, ok := s.plexMetaCached(r.Context(), key); ok {
			for _, person := range overlayPeople(ov, kind) {
				if person.Thumb == string(raw) {
					imagePath = person.Thumb
					break
				}
			}
		}
	}
	if imagePath == "" {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if plexImageURL(imagePath) {
		s.serveRemoteImage(w, r, imagePath)
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
	if plexmeta.ImageHost(host) {
		return true
	}
	ref, err := url.Parse(s.d.ImageURL("/x.jpg", "original"))
	if err != nil {
		return false
	}
	return strings.EqualFold(ref.Host, host)
}
