package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Read re-enumerates under current policy and descriptors before touching bytes.
func (a Source) Read(ctx context.Context, req frontend.ArtworkRequest, img frontend.Image, transform frontend.ArtworkTransform) ([]byte, string, error) {
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
		if policy, ok := a.Visibility.Policy.(core.UserPolicySource); ok {
			viewer := policy.UserPolicy(req.Viewer)
			if viewer == nil || viewer.ID != req.Viewer.ID {
				return nil, "", core.ErrUnauthorized
			}
			req.Viewer = viewer
		}
		if entry.ultra != nil {
			return RenderUltraBlur(*entry.ultra, transform)
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
			return a.transform(ctx, img.Path, data, ct, transform)
		}
		if entry.virtualSeason != nil {
			ctx = context.WithValue(ctx, artworkVirtualSeasonKey{}, entry.virtualSeason)
		}
		data, ct, err := a.readEntry(ctx, req, entry, transform.Width)
		if err != nil || len(data) > MaxImageBytes {
			return nil, "", core.ErrNotAvailable
		}
		if len(data) == 0 {
			return nil, "", core.ErrNotFound
		}
		return a.transform(ctx, img.Path, data, ct, transform)
	}
	if img.Kind == frontend.ImagePerson {
		if data, ct, err := a.readFreshPortrait(ctx, req, img, transform); err == nil || !errors.Is(err, core.ErrNotFound) {
			return data, ct, err
		}
	}
	return nil, "", core.ErrUnauthorized
}

// readFreshPortrait loads the stored title again when the minute cache does
// not yet list a credit that was just written, then fetches that thumb the
// same way other remote artwork does.
func (a Source) readFreshPortrait(ctx context.Context, req frontend.ArtworkRequest, img frontend.Image, transform frontend.ArtworkTransform) ([]byte, string, error) {
	fresh, ok := a.Catalog.(interface {
		RefreshStoredTitle(context.Context, core.RatingKey) (*core.Title, error)
	})
	if !ok {
		return nil, "", core.ErrNotFound
	}
	title, err := fresh.RefreshStoredTitle(ctx, req.Item)
	if err != nil || title == nil || !title.IsReleased(a.now()) || !a.visible(ctx, req.Viewer, title) {
		if err == nil {
			err = core.ErrNotFound
		}
		return nil, "", err
	}
	for _, portrait := range creditedPortraits(*title) {
		ref := artworkReference(req, frontend.ImagePerson, portrait.source)
		candidate := frontend.Image{Kind: frontend.ImagePerson, Path: ref, Tag: ref, Subject: portrait.subject}
		if !frontend.SameImage(candidate, img) {
			continue
		}
		data, ct, err := a.remote(ctx, portrait.thumb, 185)
		if err != nil || len(data) > MaxImageBytes {
			return nil, "", core.ErrNotAvailable
		}
		if len(data) == 0 {
			return nil, "", core.ErrNotFound
		}
		return a.transform(ctx, img.Path, data, ct, transform)
	}
	return nil, "", core.ErrNotFound
}

func (a Source) remote(ctx context.Context, path string, width int) ([]byte, string, error) {
	if a.Images == nil || a.ImageURL == nil {
		return nil, "", core.ErrNotAvailable
	}
	image, err := a.Images.Get(ctx, a.ImageURL(path, TMDBSize(width)), a.now())
	if err != nil {
		return nil, "", err
	}
	return image.Data, image.ContentType, nil
}
func (a Source) readEntry(ctx context.Context, req frontend.ArtworkRequest, entry frontendArtworkEntry, width int) ([]byte, string, error) {
	switch {
	case entry.channel:
		// Recheck channel membership, matching the former authenticated handler.
		channels, err := a.LiveTV.AllowedChannels(ctx, req.Viewer)
		if err != nil {
			return nil, "", err
		}
		for _, c := range channels {
			if c.ID == req.Channel {
				return []byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360"><rect width="640" height="360" rx="24" fill="#182d45"/><text x="40" y="125" fill="#64dfc7" font-size="68" font-family="sans-serif">%s</text><text x="40" y="220" fill="white" font-size="30" font-family="sans-serif">%s</text></svg>`, html.EscapeString(c.Number), html.EscapeString(c.Name))), "image/svg+xml", nil
			}
		}
		return nil, "", core.ErrNotFound
	case entry.remotePath != "":
		return a.remote(ctx, entry.remotePath, width)
	case entry.discovery:
		t, err := a.title(ctx, req.Item)
		if err != nil || req.Viewer == nil || !a.visible(ctx, req.Viewer, t) {
			return nil, "", core.ErrNotFound
		}
		t, _, err = a.Discovery.PrepareArtwork(ctx, req.Viewer, t, req.Device.Language)
		if err != nil {
			return nil, "", err
		}
		data, ct, err := a.Discovery.RenderArtwork(ctx, req.Viewer, t, entry.discoveryKind, req.Device.Language)
		if err == nil && a.Discovery.Overlays.CustomPostersEnabled() && width > 0 && (entry.discoveryKind == "" || entry.discoveryKind == "poster") {
			if resized, e := artwork.ResizePoster(data, width); e == nil {
				data = resized
				ct = "image/jpeg"
			}
		}
		return data, ct, err
	case entry.personKind != "":
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		t, err := a.title(ctx, req.Item)
		if err != nil || t == nil || !t.IsReleased(a.now()) || !a.visible(ctx, req.Viewer, t) {
			return nil, "", core.ErrNotFound
		}
		var people []core.Person
		switch entry.personKind {
		case "actor":
			people = t.Cast
		case "director":
			people = t.Directors
		case "writer":
			people = t.Writers
		default:
			return nil, "", core.ErrNotFound
		}
		raw, err := base64.RawURLEncoding.DecodeString(entry.personID)
		if err != nil || len(raw) == 0 {
			return nil, "", core.ErrNotFound
		}
		for _, person := range people {
			if person.ThumbPath == string(raw) {
				return a.remote(ctx, person.ThumbPath, 185)
			}
		}
		return nil, "", core.ErrNotFound
	default:
		if req.Item.Kind() == core.KindSmart {
			if art, ok := a.Collections.(interface {
				CollectionArtwork(context.Context, *core.User, core.RatingKey, string, int) ([]byte, string, bool, error)
			}); ok {
				if _, err := a.collection(ctx, req); err == nil {
					data, ct, ok, err := art.CollectionArtwork(ctx, req.Viewer, req.Item, entry.element, width)
					if err == nil && ok && len(data) > 0 {
						return data, ct, nil
					}
				}
			}
		}
		path, defaultWidth, err := a.ArtPath(ctx, req, entry.element)
		if err != nil || path == "" {
			return nil, "", core.ErrNotFound
		}
		if width <= 0 {
			width = defaultWidth
		}
		return a.remote(ctx, path, width)
	}
}
func (a Source) transform(ctx context.Context, reference string, data []byte, contentType string, transform frontend.ArtworkTransform) ([]byte, string, error) {
	if strings.HasPrefix(contentType, "image/svg+xml") {
		if transform.Format == "" && !transform.Blur {
			return data, contentType, nil
		}
		return a.Raster.Rasterize(ctx, reference, data, artwork.RasterOptions{Width: transform.Width, Height: transform.Height, Format: transform.Format, Blur: transform.Blur})
	}
	return Transform(data, contentType, transform)
}
