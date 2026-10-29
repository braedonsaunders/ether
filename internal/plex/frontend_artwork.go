package plex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// FrontendArtwork adapts the existing Plex artwork selection and rendering
// paths for other surfaces. Title selections use the locally mirrored data
// on a cold cache; alternate variants are filled in the background.
type FrontendArtwork struct {
	Server *Server
	// Variants must be cache-only; it may schedule a bounded background fill.
	Variants func(*core.Title, string) (*tmdb.Images, bool)
}

type frontendArtworkEntry struct {
	image                         frontend.Image
	element, personKind, personID string
	discoveryKind                 string
	discovery                     bool
	remotePath                    string
}

type artworkCacheOnlyKey struct{}

// Artwork enumeration is called from item enrichment and must not start a
// catalogue network fetch. Plex requests retain their existing Get behavior.
func (s *Server) artworkTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if cacheOnly, _ := ctx.Value(artworkCacheOnlyKey{}).(bool); cacheOnly {
		stored, ok := s.d.Catalog.(core.StoredTitleLookup)
		if !ok {
			return nil, core.ErrNotAvailable
		}
		return stored.StoredTitle(ctx, key)
	}
	return s.d.Catalog.Get(ctx, key)
}

func artworkReference(req frontend.ArtworkRequest, kind, source string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s\x00%s", req.Viewer.ID, req.Item, req.Device.Language, kind, source, "artwork-v1")))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a FrontendArtwork) entries(ctx context.Context, req frontend.ArtworkRequest) ([]frontendArtworkEntry, bool, error) {
	if a.Server == nil || req.Viewer == nil || req.Item == 0 {
		return nil, false, core.ErrUnauthorized
	}
	s := a.Server
	ctx = context.WithValue(ctx, artworkCacheOnlyKey{}, true)
	ctx = withRequest(ctx, &Request{User: req.Viewer, Language: req.Device.Language, Product: req.Device.Product, Platform: req.Device.Platform, DeviceName: req.Device.DeviceName, ClientID: req.Device.ID})
	var title *core.Title
	var collectionVersion string
	switch req.Item.Kind() {
	case core.KindCollection, core.KindSmart:
		collection, err := s.collection(ctx, req.Viewer, req.Item)
		if err != nil {
			return nil, false, err
		}
		collectionVersion = collectionStamp(collection)
	case core.KindPlaylist:
		if s.d.Playlists == nil {
			return nil, false, core.ErrNotFound
		}
		if _, err := s.d.Playlists.Get(ctx, req.Viewer.ID, req.Item); err != nil {
			return nil, false, err
		}
	default:
		if s.d.Catalog == nil {
			return nil, false, core.ErrNotFound
		}
		var err error
		title, err = s.artworkTitle(ctx, req.Item)
		if err != nil || title == nil || title.Key != req.Item || !title.IsReleased(s.now()) || !s.visibleTitle(ctx, req.Viewer, title) {
			return nil, false, core.ErrNotFound
		}
	}
	var entries []frontendArtworkEntry
	add := func(kind, source string, entry frontendArtworkEntry) {
		ref := artworkReference(req, kind, source)
		entry.image = frontend.Image{Kind: kind, Path: ref, Tag: ref}
		entries = append(entries, entry)
	}
	var decorated model.Metadata
	if title != nil && s.d.Discovery != nil {
		decorated.Thumb, decorated.Art = "plain", "plain"
		decorated.Image = []model.Image{{Type: "clearLogo", URL: "plain"}}
		s.decorateDiscoveryMetadata(ctx, FromContext(ctx), title, &decorated)
	}
	for _, item := range []struct{ kind, element string }{{frontend.ImagePoster, "poster"}, {frontend.ImageThumb, "thumb"}, {frontend.ImageBackdrop, "art"}, {frontend.ImageLogo, "logo"}, {frontend.ImageBanner, "banner"}} {
		path, _, err := s.artPath(ctx, req.Item, item.element)
		generated := req.Item.Kind() == core.KindSmart && (item.element == "thumb" || item.element == "poster")
		selected := ""
		if title != nil {
			switch item.kind {
			case frontend.ImagePoster, frontend.ImageThumb:
				selected = decorated.Thumb
			case frontend.ImageBackdrop:
				selected = decorated.Art
			case frontend.ImageLogo:
				if len(decorated.Image) > 0 {
					selected = decorated.Image[0].URL
				}
			}
		}
		if strings.Contains(selected, "/discoveryPoster/") {
			u, e := url.Parse(selected)
			if e == nil {
				add(item.kind, selected, frontendArtworkEntry{discovery: true, discoveryKind: u.Query().Get("kind")})
				continue
			}
		}
		if (err != nil || path == "") && !generated {
			continue
		}
		add(item.kind, item.element+"\x00"+path+"\x00"+collectionVersion, frontendArtworkEntry{element: item.element})
	}
	complete := true
	if title != nil {
		if artwork.ValidPath(title.StillPath) {
			add(frontend.ImageStill, "still\x00"+title.StillPath, frontendArtworkEntry{remotePath: title.StillPath})
		}
		if a.Variants != nil {
			variants, ready := a.Variants(title, req.Device.Language)
			if ready && variants != nil {
				for _, group := range []struct {
					kind  string
					files []tmdb.ImageFile
				}{{frontend.ImagePoster, variants.Posters}, {frontend.ImageBackdrop, variants.Backdrops}, {frontend.ImageLogo, variants.Logos}, {frontend.ImageStill, variants.Still}} {
					for _, file := range group.files {
						if artwork.ValidPath(file.FilePath) {
							add(group.kind, "variant\x00"+file.FilePath, frontendArtworkEntry{remotePath: file.FilePath})
						}
					}
				}
			}
		}
		for _, group := range []struct {
			kind   string
			people []core.Person
		}{{"actor", title.Cast}, {"director", title.Directors}, {"writer", title.Writers}} {
			for _, person := range group.people {
				if person.ThumbPath == "" {
					continue
				}
				id := base64.RawURLEncoding.EncodeToString([]byte(person.ThumbPath))
				add(frontend.ImagePerson, group.kind+"\x00"+person.ThumbPath, frontendArtworkEntry{personKind: group.kind, personID: id})
			}
		}
	}
	return entries, complete, nil
}

// Warm schedules the cache-only variant source for a visible row or grid item.
func (a FrontendArtwork) Warm(title core.Title, device core.Device) {
	if a.Variants != nil {
		_, _ = a.Variants(&title, device.Language)
	}
}

func (a FrontendArtwork) List(ctx context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	entries, complete, err := a.entries(ctx, req)
	if err != nil {
		return nil, false, err
	}
	out := make([]frontend.Image, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.image)
	}
	return out, complete, nil
}

type artworkResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	failed bool
}

func (w *artworkResponse) Header() http.Header { return w.header }
func (w *artworkResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *artworkResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(p) > maxImageBytes {
		w.failed = true
		return 0, core.ErrNotAvailable
	}
	return w.body.Write(p)
}

func (a FrontendArtwork) Read(ctx context.Context, req frontend.ArtworkRequest, img frontend.Image, transform frontend.ArtworkTransform) ([]byte, string, error) {
	if transform.Width < 0 || transform.Width > 4000 || transform.Height < 0 || transform.Height > 4000 || (transform.Format != "" && transform.Format != "jpeg" && transform.Format != "png") {
		return nil, "", core.ErrNotAvailable
	}
	entries, _, err := a.entries(ctx, req)
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if entry.image != img {
			continue
		}
		ctx = context.WithValue(ctx, artworkCacheOnlyKey{}, true)
		ctx = withRequest(ctx, &Request{User: req.Viewer, Language: req.Device.Language, Product: req.Device.Product, Platform: req.Device.Platform, DeviceName: req.Device.DeviceName, ClientID: req.Device.ID})
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/?width="+strconv.Itoa(transform.Width), nil)
		if err != nil {
			return nil, "", err
		}
		w := &artworkResponse{header: make(http.Header)}
		switch {
		case entry.remotePath != "":
			a.Server.serveRemoteImage(w, r, a.Server.d.ImageURL(entry.remotePath, tmdbSize(transform.Width)))
		case entry.discovery:
			a.Server.serveDiscoveryPoster(w, r, req.Item, entry.discoveryKind, req.Device.Language)
		case entry.personKind != "":
			a.Server.servePersonImage(w, r, req.Item, entry.personKind, entry.personID)
		default:
			a.Server.serveItemImage(w, r, req.Item, entry.element, transform.Width)
		}
		if w.failed || (w.status != 0 && w.status != http.StatusOK) {
			return nil, "", core.ErrNotAvailable
		}
		if w.body.Len() == 0 {
			return nil, "", core.ErrNotFound
		}
		return transformArtwork(w.body.Bytes(), w.header.Get("Content-Type"), transform)
	}
	return nil, "", core.ErrUnauthorized
}

func transformArtwork(data []byte, contentType string, transform frontend.ArtworkTransform) ([]byte, string, error) {
	// Width alone is handled by the existing image sources, including TMDB's
	// width buckets and the poster provider's resize path.
	if transform.Height == 0 && transform.Format == "" && !transform.Blur {
		return data, contentType, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4000 || cfg.Height > 4000 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return nil, "", core.ErrNotAvailable
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	width, height := transform.Width, transform.Height
	if width == 0 && height > 0 {
		width = max(1, cfg.Width*height/cfg.Height)
	}
	if height == 0 && width > 0 {
		height = max(1, cfg.Height*width/cfg.Width)
	}
	if width > 4000 || height > 4000 {
		return nil, "", core.ErrNotAvailable
	}
	if width > 0 && height > 0 && (width != cfg.Width || height != cfg.Height) {
		src, err = kitimages.Resize(src, width, height)
		if err != nil {
			return nil, "", err
		}
	}
	if transform.Blur {
		src, err = kitimages.Blur(src)
		if err != nil {
			return nil, "", err
		}
	}
	var out bytes.Buffer
	switch transform.Format {
	case "png":
		err = png.Encode(&out, src)
		contentType = "image/png"
	default:
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: 90})
		contentType = "image/jpeg"
	}
	if err != nil {
		return nil, "", err
	}
	return out.Bytes(), contentType, nil
}

var _ frontend.Artwork = FrontendArtwork{}
