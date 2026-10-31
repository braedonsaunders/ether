// Package service owns shared artwork authorization, selection and rendering.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/discovery"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"net/url"
	"sync"
	"time"
)

// Source has no protocol server or protocol wire model dependencies.
type Source struct {
	Catalog          core.StoredTitleLookup
	CollectionDevice string
	Collections      core.Collections
	Playlists        core.Playlists
	Visibility       experience.Visibility
	LiveTV           interface {
		AllowedChannels(context.Context, *core.User) ([]livetv.Channel, error)
	}
	Discovery     *discovery.Features
	Images        *RemoteCache
	ImageURL      core.ImageURLFunc
	Palettes      *PaletteCache
	Now           func() time.Time
	Variants      func(*core.Title, string) (*tmdb.Images, bool)
	Metadata      func(context.Context, frontend.ArtworkRequest, frontend.Image) frontend.Image
	meta          *MetadataQueue
	VirtualSeason func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	URLImages     interface {
		GetUntrustedURL(context.Context, string) ([]byte, string, error)
	}
	Raster   *artwork.SVGRasterizer
	Collages *LibraryCollageCache
}
type artworkVirtualSeasonKey struct{}

func (a Source) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a Source) title(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if t, ok := ctx.Value(artworkVirtualSeasonKey{}).(*core.Title); ok && t != nil && t.Key == key {
		return t, nil
	}
	if a.Catalog == nil {
		return nil, core.ErrNotAvailable
	}
	return a.Catalog.StoredTitle(ctx, key)
}

type visMemoKey struct{}

type visMemo struct {
	mu   sync.Mutex
	seen map[core.RatingKey]bool
	val  map[core.RatingKey]bool
}

func withVisMemo(ctx context.Context) context.Context {
	if ctx.Value(visMemoKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, visMemoKey{}, &visMemo{})
}

func (a Source) visible(ctx context.Context, v *core.User, t *core.Title) bool {
	if t == nil {
		return false
	}
	memo, _ := ctx.Value(visMemoKey{}).(*visMemo)
	if memo != nil {
		memo.mu.Lock()
		if memo.seen[t.Key] {
			ok := memo.val[t.Key]
			memo.mu.Unlock()
			return ok
		}
		memo.mu.Unlock()
	}
	ok := a.judgeVisible(ctx, v, t)
	if memo != nil {
		memo.mu.Lock()
		if memo.seen == nil {
			memo.seen = map[core.RatingKey]bool{}
			memo.val = map[core.RatingKey]bool{}
		}
		memo.seen[t.Key] = true
		memo.val[t.Key] = ok
		memo.mu.Unlock()
	}
	return ok
}

func (a Source) judgeVisible(ctx context.Context, v *core.User, t *core.Title) bool {
	vis := a.Visibility
	vis.GetTitle = a.title
	return vis.Visible(ctx, v, t)
}
func ultraBlurType(t core.MediaType) bool { return t == core.TypeMovie || t == core.TypeShow }

// discoveryIdentity preserves artwork-v1 hash material. It is never exposed as
// a route or credential; protocol image URLs remain adapter-owned.
func discoveryIdentity(key core.RatingKey, version, kind, lang string) string {
	suffix := "?lang=" + url.QueryEscape(lang)
	if kind != "" {
		suffix = "?kind=" + kind + "&lang=" + url.QueryEscape(lang)
	}
	return fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s%s", key.String(), version, suffix)
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
	ultra                         *Palette
	virtualSeason                 *core.Title
}

func artworkReference(req frontend.ArtworkRequest, kind, source string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s\x00%s", req.Viewer.ID, req.Item, req.Device.Language, kind, source, "artwork-v1")))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// oneChannel authorizes a single guide row. Home asks for a logo per
// channel; scanning every channel for each logo held the request for seconds.
func (a Source) oneChannel(ctx context.Context, viewer *core.User, id string) (livetv.Channel, error) {
	if a.LiveTV == nil || viewer == nil || id == "" {
		return livetv.Channel{}, core.ErrNotFound
	}
	if permit, ok := a.LiveTV.(interface {
		ChannelPermitted(context.Context, *core.User, string) (bool, error)
		Channel(context.Context, string) (livetv.Channel, error)
	}); ok {
		allowed, err := permit.ChannelPermitted(ctx, viewer, id)
		if err != nil {
			return livetv.Channel{}, err
		}
		if !allowed {
			return livetv.Channel{}, core.ErrNotFound
		}
		channel, err := permit.Channel(ctx, id)
		if err != nil {
			return livetv.Channel{}, err
		}
		channel.StreamURL = ""
		channel.StreamSources = nil
		return channel, nil
	}
	channels, err := a.LiveTV.AllowedChannels(ctx, viewer)
	if err != nil {
		return livetv.Channel{}, err
	}
	for _, channel := range channels {
		if channel.ID == id {
			return channel, nil
		}
	}
	return livetv.Channel{}, core.ErrNotFound
}

// ChannelImage describes a channel already filtered through AllowedChannels.
// The reference is not an authorization credential; List rechecks policy.
func (a Source) ChannelImage(viewer *core.Viewer, channel livetv.Channel) frontend.Image {
	if viewer == nil {
		return frontend.Image{}
	}
	req := frontend.ArtworkRequest{Viewer: viewer, Channel: channel.ID}
	ref := artworkReference(req, frontend.ImageChannel, channel.ID+"\x00"+channel.Logo+"\x00"+channel.Name+"\x00"+channel.Number)
	return frontend.Image{Kind: frontend.ImageChannel, Path: ref, Tag: ref}
}

func (a Source) entries(ctx context.Context, req frontend.ArtworkRequest) ([]frontendArtworkEntry, bool, error) {
	ctx = withVisMemo(ctx)
	if req.Viewer == nil || ((req.Item == 0) == (req.Channel == "")) {
		return nil, false, core.ErrUnauthorized
	}
	s := a
	if policy, ok := s.Visibility.Policy.(core.UserPolicySource); ok {
		viewer := policy.UserPolicy(req.Viewer)
		if viewer == nil || viewer.ID != req.Viewer.ID {
			return nil, false, core.ErrUnauthorized
		}
		req.Viewer = viewer
	}

	if req.Channel != "" {
		channel, err := s.oneChannel(ctx, req.Viewer, req.Channel)
		if err != nil {
			return nil, false, err
		}
		return []frontendArtworkEntry{{image: a.ChannelImage(req.Viewer, channel), channel: true, channelLogo: channel.Logo, channelNumber: channel.Number, channelName: channel.Name}}, true, nil
	}
	var title *core.Title
	var virtualSeason *core.Title
	var collectionVersion string
	switch req.Item.Kind() {
	case core.KindCollection, core.KindSmart:
		collection, err := s.collection(ctx, req)
		if err != nil {
			return nil, false, err
		}
		collectionVersion = collectionStamp(collection)
	case core.KindPlaylist:
		if s.Playlists == nil {
			return nil, false, core.ErrNotFound
		}
		if _, err := s.Playlists.Get(ctx, req.Viewer.ID, req.Item); err != nil {
			return nil, false, err
		}
	default:
		if s.Catalog == nil {
			return nil, false, core.ErrNotFound
		}
		var err error
		title, err = s.title(ctx, req.Item)
		if errors.Is(err, core.ErrNotFound) && a.VirtualSeason != nil {
			title, err = a.VirtualSeason(ctx, req.Viewer, req.Item)
			if err == nil && title != nil {
				virtualSeason = title
				ctx = context.WithValue(ctx, artworkVirtualSeasonKey{}, title)
			}
		}
		if err != nil || title == nil || title.Key != req.Item || (virtualSeason == nil && !title.IsReleased(s.now())) || !s.visible(ctx, req.Viewer, title) {
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
	var selected discovery.ArtworkSelection
	if title != nil && virtualSeason == nil && s.Discovery != nil && experience.TitleAllowed(s.Visibility.Policy, req.Viewer, title) {
		selected = s.Discovery.SelectArtwork(ctx, req.Viewer, title, req.Device.Language)
	}
	for _, item := range []struct{ kind, element string }{{frontend.ImagePoster, "poster"}, {frontend.ImageThumb, "thumb"}, {frontend.ImageBackdrop, "art"}, {frontend.ImageLogo, "logo"}, {frontend.ImageBanner, "banner"}} {
		path, _, err := s.ArtPath(ctx, req, item.element)
		generated := req.Item.Kind() == core.KindSmart && (item.element == "thumb" || item.element == "poster")
		version, kind := "", ""
		switch item.kind {
		case frontend.ImagePoster, frontend.ImageThumb:
			version = selected.PosterVersion
		case frontend.ImageBackdrop:
			version = selected.BackdropVersion
			kind = "backdrop"
		case frontend.ImageLogo:
			version = selected.LogoVersion
			kind = "logo"
		}
		if version != "" {
			add(item.kind, discoveryIdentity(req.Item, version, kind, req.Device.Language), frontendArtworkEntry{discovery: true, discoveryKind: kind})
			continue
		}
		if (err != nil || path == "") && !generated {
			continue
		}
		add(item.kind, item.element+"\x00"+path+"\x00"+collectionVersion, frontendArtworkEntry{element: item.element})
	}
	complete := true
	if title != nil {
		if s.Palettes != nil && ultraBlurType(title.Type) && title.BackdropPath != "" {
			if colors := s.Palettes.Lookup(title.Key, title.BackdropPath); colors != nil {
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
		for _, portrait := range creditedPortraits(*title) {
			add(frontend.ImagePerson, portrait.source, frontendArtworkEntry{
				personKind: portrait.role,
				personID:   base64.RawURLEncoding.EncodeToString([]byte(portrait.thumb)),
				subject:    portrait.subject,
			})
		}
	}
	return entries, complete, nil
}

// Warm schedules the cache-only variant source for a visible row or grid item.
func (a Source) Warm(title core.Title, device core.Device) {
	if a.Palettes != nil && ultraBlurType(title.Type) && title.BackdropPath != "" {
		_ = a.Palettes.Lookup(title.Key, title.BackdropPath)
	}
	if a.Variants != nil {
		_, _ = a.Variants(&title, device.Language)
	}
}

// BindMetadata keeps the queue so fixture ingest can store hashes before a page is served.
func (a *Source) BindMetadata(q *MetadataQueue) {
	if a != nil {
		a.meta = q
	}
}

// StoreMetadata hashes each title's images and waits until those rows are stored.
// A later page joins the rows and does not wait on the hash.
func (a *Source) StoreMetadata(ctx context.Context, viewer *core.User, titles []core.Title) error {
	if a == nil || a.meta == nil || viewer == nil {
		return nil
	}
	// The queue holds a bounded number of jobs. Repeat until each title's
	// images have been stored; a stored row returns without new work.
	for pass := 0; pass < 4; pass++ {
		for _, title := range titles {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_, _, _ = a.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Item: title.Key})
		}
		if err := a.meta.WaitIdle(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a Source) List(ctx context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
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

var _ frontend.Artwork = Source{}
