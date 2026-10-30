package plex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/artwork"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"github.com/go-chi/chi/v5"
)

// FrontendArtwork adapts the existing Plex artwork selection and rendering
// paths for other surfaces. Title selections use the locally mirrored data
// on a cold cache; alternate variants are filled in the background.
type FrontendArtwork struct {
	Server *Server
	// Variants must be cache-only; it may schedule a bounded background fill.
	Variants func(*core.Title, string) (*tmdb.Images, bool)
	// Metadata reads cached facts and may enqueue a bounded background fill.
	Metadata func(context.Context, frontend.ArtworkRequest, frontend.Image) frontend.Image
	// VirtualSeason reads only the viewer's existing episode-order layout.
	VirtualSeason func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	URLImages     interface {
		GetUntrustedURL(context.Context, string) ([]byte, string, error)
	}
	Raster *artwork.SVGRasterizer
}

type frontendArtworkEntry struct {
	image                         frontend.Image
	element, personKind, personID string
	subject                       string
	discoveryKind                 string
	discovery                     bool
	remotePath                    string
	channel                       bool
	channelLogo                   string
	channelNumber, channelName    string
	ultra                         *model.UltraBlurColors
	virtualSeason                 *core.Title
}

type artworkCacheOnlyKey struct{}
type artworkVirtualSeasonKey struct{}

// Artwork enumeration is called from item enrichment and must not start a
// catalogue network fetch. Plex requests retain their existing Get behavior.
func (s *Server) artworkTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if cacheOnly, _ := ctx.Value(artworkCacheOnlyKey{}).(bool); cacheOnly {
		if title, ok := ctx.Value(artworkVirtualSeasonKey{}).(*core.Title); ok && title != nil && title.Key == key {
			return title, nil
		}
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

// ChannelImage describes a channel already filtered through AllowedChannels.
// The reference is not an authorization credential; List rechecks policy.
func (a FrontendArtwork) ChannelImage(viewer *core.Viewer, channel livetv.Channel) frontend.Image {
	if viewer == nil {
		return frontend.Image{}
	}
	req := frontend.ArtworkRequest{Viewer: viewer, Channel: channel.ID}
	ref := artworkReference(req, frontend.ImageChannel, channel.ID+"\x00"+channel.Logo+"\x00"+channel.Name+"\x00"+channel.Number)
	return frontend.Image{Kind: frontend.ImageChannel, Path: ref, Tag: ref}
}

func (a FrontendArtwork) entries(ctx context.Context, req frontend.ArtworkRequest) ([]frontendArtworkEntry, bool, error) {
	if a.Server == nil || req.Viewer == nil || ((req.Item == 0) == (req.Channel == "")) {
		return nil, false, core.ErrUnauthorized
	}
	s := a.Server
	if policy, ok := s.d.AccessPolicy.(core.UserPolicySource); ok {
		viewer := policy.UserPolicy(req.Viewer)
		if viewer == nil || viewer.ID != req.Viewer.ID {
			return nil, false, core.ErrUnauthorized
		}
		req.Viewer = viewer
	}
	ctx = context.WithValue(ctx, artworkCacheOnlyKey{}, true)
	ctx = withRequest(ctx, &Request{User: req.Viewer, Language: req.Device.Language, Product: req.Device.Product, Platform: req.Device.Platform, DeviceName: req.Device.DeviceName, ClientID: req.Device.ID})
	if req.Channel != "" {
		if s.d.LiveTV == nil {
			return nil, false, core.ErrNotFound
		}
		channels, err := s.d.LiveTV.AllowedChannels(ctx, req.Viewer)
		if err != nil {
			return nil, false, err
		}
		for _, channel := range channels {
			if channel.ID != req.Channel {
				continue
			}
			return []frontendArtworkEntry{{image: a.ChannelImage(req.Viewer, channel), channel: true, channelLogo: channel.Logo, channelNumber: channel.Number, channelName: channel.Name}}, true, nil
		}
		return nil, false, core.ErrNotFound
	}
	var title *core.Title
	var virtualSeason *core.Title
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
		if errors.Is(err, core.ErrNotFound) && a.VirtualSeason != nil {
			title, err = a.VirtualSeason(ctx, req.Viewer, req.Item)
			if err == nil && title != nil {
				virtualSeason = title
				ctx = context.WithValue(ctx, artworkVirtualSeasonKey{}, title)
			}
		}
		if err != nil || title == nil || title.Key != req.Item || (virtualSeason == nil && !title.IsReleased(s.now())) || !s.visibleTitle(ctx, req.Viewer, title) {
			return nil, false, core.ErrNotFound
		}
	}
	var entries []frontendArtworkEntry
	add := func(kind, source string, entry frontendArtworkEntry) {
		ref := artworkReference(req, kind, source)
		entry.image = frontend.Image{Kind: kind, Path: ref, Tag: ref, Subject: entry.subject}
		entry.virtualSeason = virtualSeason
		entries = append(entries, entry)
	}
	var decorated model.Metadata
	if title != nil && virtualSeason == nil && s.d.Discovery != nil {
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
		if ultraBlurType(title.Type) && title.BackdropPath != "" {
			if colors := s.ultraBlurFor(title.Key, title.BackdropPath); colors != nil {
				stamp := colors.TopLeft + colors.TopRight + colors.BottomLeft + colors.BottomRight
				add(frontend.ImageBackdrop, "ultrablur\x00"+title.BackdropPath+"\x00"+stamp, frontendArtworkEntry{ultra: colors})
			}
		}
		if artwork.ValidPath(title.StillPath) {
			add(frontend.ImageStill, "still\x00"+title.StillPath, frontendArtworkEntry{remotePath: title.StillPath})
		}
		if virtualSeason == nil && a.Variants != nil {
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
				if person.ThumbPath == "" || person.TMDB <= 0 {
					continue
				}
				id := base64.RawURLEncoding.EncodeToString([]byte(person.ThumbPath))
				subject := "tmdb:" + strconv.FormatInt(person.TMDB, 10)
				add(frontend.ImagePerson, group.kind+"\x00"+subject+"\x00"+person.ThumbPath, frontendArtworkEntry{personKind: group.kind, personID: id, subject: subject})
			}
		}
	}
	return entries, complete, nil
}

// Warm schedules the cache-only variant source for a visible row or grid item.
func (a FrontendArtwork) Warm(title core.Title, device core.Device) {
	if a.Server != nil && ultraBlurType(title.Type) && title.BackdropPath != "" {
		_ = a.Server.ultraBlurFor(title.Key, title.BackdropPath)
	}
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
		img := e.image
		if a.Metadata != nil {
			img = a.Metadata(ctx, req, img)
		}
		out = append(out, img)
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
		if !frontend.SameImage(entry.image, img) {
			continue
		}
		if policy, ok := a.Server.d.AccessPolicy.(core.UserPolicySource); ok {
			viewer := policy.UserPolicy(req.Viewer)
			if viewer == nil || viewer.ID != req.Viewer.ID {
				return nil, "", core.ErrUnauthorized
			}
			req.Viewer = viewer
		}
		if entry.ultra != nil {
			return renderUltraBlur(*entry.ultra, transform)
		}
		opts := artwork.RasterOptions{Width: transform.Width, Height: transform.Height, Format: transform.Format, Blur: transform.Blur}
		if entry.channel && entry.channelLogo == "" && (transform.Format != "" || transform.Blur) {
			return a.Raster.ChannelLogo(ctx, img.Path, entry.channelNumber, entry.channelName, opts)
		}
		if entry.channel && entry.channelLogo != "" {
			if a.URLImages == nil {
				return nil, "", core.ErrNotAvailable
			}
			data, ct, err := a.URLImages.GetUntrustedURL(ctx, entry.channelLogo)
			if err != nil {
				return nil, "", err
			}
			return a.transformArtwork(ctx, img.Path, data, ct, transform)
		}
		ctx = context.WithValue(ctx, artworkCacheOnlyKey{}, true)
		if entry.virtualSeason != nil {
			ctx = context.WithValue(ctx, artworkVirtualSeasonKey{}, entry.virtualSeason)
		}
		ctx = withRequest(ctx, &Request{User: req.Viewer, Language: req.Device.Language, Product: req.Device.Product, Platform: req.Device.Platform, DeviceName: req.Device.DeviceName, ClientID: req.Device.ID})
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/?width="+strconv.Itoa(transform.Width), nil)
		if err != nil {
			return nil, "", err
		}
		w := &artworkResponse{header: make(http.Header)}
		switch {
		case entry.channel:
			route := chi.NewRouteContext()
			route.URLParams.Add("channel", req.Channel)
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, route))
			a.Server.handleLiveLogo(w, r)
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
		return a.transformArtwork(ctx, img.Path, w.body.Bytes(), w.header.Get("Content-Type"), transform)
	}
	return nil, "", core.ErrUnauthorized
}

func (a FrontendArtwork) transformArtwork(ctx context.Context, reference string, data []byte, contentType string, transform frontend.ArtworkTransform) ([]byte, string, error) {
	if strings.HasPrefix(contentType, "image/svg+xml") {
		if transform.Format == "" && !transform.Blur {
			return data, contentType, nil
		}
		return a.Raster.Rasterize(ctx, reference, data, artwork.RasterOptions{Width: transform.Width, Height: transform.Height, Format: transform.Format, Blur: transform.Blur})
	}
	return artworkservice.Transform(data, contentType, transform)
}

// renderUltraBlur converts the Plex wire palette at the adapter boundary.
func renderUltraBlur(colors model.UltraBlurColors, transform frontend.ArtworkTransform) ([]byte, string, error) {
	return artworkservice.RenderUltraBlur(artworkservice.Palette{TopLeft: colors.TopLeft, TopRight: colors.TopRight, BottomLeft: colors.BottomLeft, BottomRight: colors.BottomRight}, transform)
}

var _ frontend.Artwork = FrontendArtwork{}
