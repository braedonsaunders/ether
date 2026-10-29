package plex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// FrontendArtwork adapts the existing Plex artwork selection and rendering
// paths for other surfaces. Its list remains non-authoritative until all
// supported artwork sources have been enumerated.
type FrontendArtwork struct{ Server *Server }

type frontendArtworkEntry struct {
	image                         frontend.Image
	element, personKind, personID string
	discoveryKind                 string
	discovery                     bool
}

func artworkReference(req frontend.ArtworkRequest, kind, source string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s\x00%s", req.Viewer.ID, req.Item, req.Device.Language, kind, source, "artwork-v1")))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a FrontendArtwork) entries(ctx context.Context, req frontend.ArtworkRequest) ([]frontendArtworkEntry, error) {
	if a.Server == nil || req.Viewer == nil || req.Item == 0 {
		return nil, core.ErrUnauthorized
	}
	s := a.Server
	ctx = withRequest(ctx, &Request{User: req.Viewer, Language: req.Device.Language, Product: req.Device.Product, Platform: req.Device.Platform, DeviceName: req.Device.DeviceName, ClientID: req.Device.ID})
	var title *core.Title
	var collectionVersion string
	switch req.Item.Kind() {
	case core.KindCollection, core.KindSmart:
		collection, err := s.collection(ctx, req.Viewer, req.Item)
		if err != nil {
			return nil, err
		}
		collectionVersion = collectionStamp(collection)
	case core.KindPlaylist:
		if s.d.Playlists == nil {
			return nil, core.ErrNotFound
		}
		if _, err := s.d.Playlists.Get(ctx, req.Viewer.ID, req.Item); err != nil {
			return nil, err
		}
	default:
		if s.d.Catalog == nil {
			return nil, core.ErrNotFound
		}
		var err error
		title, err = s.d.Catalog.Get(ctx, req.Item)
		if err != nil || title == nil || !title.IsReleased(s.now()) || !s.visibleTitle(ctx, req.Viewer, title) {
			return nil, core.ErrNotFound
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
	if title != nil {
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
	return entries, nil
}

func (a FrontendArtwork) List(ctx context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	entries, err := a.entries(ctx, req)
	if err != nil {
		return nil, false, err
	}
	out := make([]frontend.Image, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.image)
	}
	// Alternates, channel art and all generated sources are not yet complete.
	return out, false, nil
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
	if transform.Width < 0 || transform.Width > 4000 || transform.Height != 0 || transform.Format != "" || transform.Blur {
		return nil, "", core.ErrNotAvailable
	}
	entries, err := a.entries(ctx, req)
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if entry.image != img {
			continue
		}
		ctx = withRequest(ctx, &Request{User: req.Viewer, Language: req.Device.Language, Product: req.Device.Product, Platform: req.Device.Platform, DeviceName: req.Device.DeviceName, ClientID: req.Device.ID})
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/?width="+strconv.Itoa(transform.Width), nil)
		if err != nil {
			return nil, "", err
		}
		w := &artworkResponse{header: make(http.Header)}
		switch {
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
		return w.body.Bytes(), w.header.Get("Content-Type"), nil
	}
	return nil, "", core.ErrUnauthorized
}

var _ frontend.Artwork = FrontendArtwork{}
