package app

import (
	"bytes"
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/transcode"
	"image"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *App) buildOnAir() error {
	o := onair.Options{Changed: func(ctx context.Context) error {
		if a.livetv != nil {
			return a.livetv.RefreshGuide(ctx)
		}
		return nil
	}, DB: a.db, Dir: filepath.Join(a.opts.DataDir, "cache", "onair"), Logger: a.log.With("component", "onair"), Bindings: a.onairBindings}
	o.StyleChannels = func(ctx context.Context) (map[string][]string, error) {
		if a.livetv == nil {
			return map[string][]string{}, nil
		}
		return a.livetv.StyleChannelIDs(ctx)
	}
	o.ReelChannels = func(ctx context.Context) (map[string][]string, error) {
		if a.livetv == nil {
			return map[string][]string{}, nil
		}
		return a.livetv.ReelChannelIDs(ctx)
	}
	a.pluginContent = onair.NewPluginContentCache()
	o.PluginSegment = a.pluginContent.Lookup
	chat := llm.NewClient(llm.ClientOptions{Config: a.cfg.Read().LLM, Logger: a.log.With("component", "onair-llm")})
	a.cfg.Subscribe(func(c config.Config) { chat.UpdateConfig(c.LLM) })
	o.Chat = chat
	o.StaticRequests = func(ctx context.Context, id string) []core.ClipRequest { return a.livetv.GraphicsRequests(ctx, id) }
	o.Upcoming = func(ctx context.Context) []core.ClipRequest { return a.livetv.GraphicsRequests(ctx, "") }
	if a.images != nil {
		o.Artwork = func(ctx context.Context, ref string) (image.Image, error) {
			b, e := a.onairArtwork(ctx, ref)
			if e != nil {
				return nil, e
			}
			im, _, e := image.Decode(bytes.NewReader(b))
			return im, e
		}
	}
	if a.transcoder != nil {
		o.Acquire = func(ctx context.Context) (context.Context, func(), error) {
			priority := transcode.SlotBackground
			if deadline, imminent := onair.BoundaryDeadline(ctx); imminent && deadline.After(time.Now()) {
				priority = transcode.SlotLivePrep
			}
			return a.transcoder.AcquireTranscodeSlot(ctx, priority)
		}
		o.FFmpeg = a.transcoder.FFmpegPath
		o.Probe = a.transcoder.Probe
		o.WorkingSize = func() (int, int) {
			hw := a.transcoder.HWAccel()
			if hw.Name == "" || hw.Name == "none" {
				return 1280, 720
			}
			return 1920, 1080
		}
	}
	svc, e := onair.New(o)
	if e != nil {
		return e
	}
	a.onair = svc
	a.closers = append(a.closers, svc.Close)
	return nil
}
func (a *App) onairBindings(ctx context.Context, channel, viewer string, at time.Time) (any, error) {
	empty := func() map[string]string {
		return map[string]string{"title": "", "time": "", "reason": "", "meta": "", "logline": "", "backdrop": ""}
	}
	out := map[string]any{"channel": map[string]string{"name": ""}, "viewer": map[string]string{"name": "", "greeting": ""}, "now": empty(), "next": empty(), "list": []map[string]string{empty(), empty(), empty(), empty()}, "offair": map[string]string{"message": "Off air"}}
	if a.livetv == nil {
		return out, nil
	}
	if channel == "" {
		cs, e := a.livetv.ChannelSummaries(ctx)
		if e != nil {
			return nil, e
		}
		if len(cs) == 0 {
			return out, nil
		}
		channel = cs[0].ID
	}
	c, e := a.livetv.Channel(ctx, channel)
	if e != nil {
		return nil, e
	}
	u, e := a.users.Owner(ctx)
	if e != nil || u == nil {
		u = &core.User{IsOwner: true}
	}
	if viewer != "" {
		id, e := strconv.ParseInt(viewer, 10, 64)
		if e != nil {
			return nil, e
		}
		u, e = a.users.ActiveUser(ctx, id)
		if e != nil {
			return nil, e
		}
	}
	name := u.Title
	if name == "" {
		name = u.Username
	}
	greeting := "Good evening"
	if at.Hour() < 12 {
		greeting = "Good morning"
	} else if at.Hour() < 18 {
		greeting = "Good afternoon"
	}
	out["viewer"] = map[string]string{"name": name, "greeting": greeting}
	out["channel"] = map[string]string{"name": c.Name, "logo": c.Logo}
	ps, e := a.livetv.GuideForChannels(ctx, u, at, at.Add(24*time.Hour), []string{channel})
	if e != nil {
		return nil, e
	}
	list := []map[string]string{}
	for _, p := range ps {
		if p.Item.Key == 0 || p.Item.Slate != "" {
			continue
		}
		v := empty()
		v["title"] = p.Item.Title
		v["time"] = p.Start.Format("15:04")
		v["meta"] = fmt.Sprintf("%d min", p.Item.DurationMS/60000)
		v["logline"] = strings.SplitN(p.Item.Summary, ". ", 2)[0]
		v["backdrop"] = livetv.ArtworkRef(p.Item.Key, p.Item.Art, "art")
		list = append(list, v)
		if len(list) == 4 {
			break
		}
	}
	for len(list) < 4 {
		list = append(list, empty())
	}
	out["list"] = list
	if len(list) > 0 {
		out["now"] = list[0]
	}
	if len(list) > 1 {
		out["next"] = list[1]
	}
	return out, nil
}

var _ core.ChannelGraphics = (*onair.Service)(nil)

// onairArtwork loads an On Air artwork reference: a TMDB image path, or a
// title's poster/backdrop by key, which resolves Plex-sourced art through the
// verified Plex metadata cache the Plex apps use for the same title.
func (a *App) onairArtwork(ctx context.Context, ref string) ([]byte, error) {
	key, element, ok := livetv.ParseArtworkRef(ref)
	if !ok {
		b, _, e := a.images.Get(ctx, ref, 1920)
		return b, e
	}
	if a.catalog == nil {
		return nil, artwork.ErrNotFound
	}
	t, e := a.catalog.Get(ctx, key)
	if e != nil {
		return nil, e
	}
	path := t.PosterPath
	if element == "art" {
		path = t.BackdropPath
	} else if path == "" {
		path = t.StillPath
	}
	if artwork.ValidPath(path) {
		b, _, e := a.images.Get(ctx, path, 1920)
		return b, e
	}
	if path != plexmeta.RemoteArt || a.plexMeta == nil || a.plexGUID == nil {
		return nil, artwork.ErrNotFound
	}
	row, ok := a.plexMeta.Cached(ctx, key)
	if !ok || row.Empty() || row.GUID == "" || row.GUID != a.plexGUID.Cached(ctx, key) {
		return nil, artwork.ErrNotFound
	}
	remote := row.ThumbURL
	if element == "art" {
		remote = row.ArtURL
	}
	u, e := url.Parse(remote)
	if e != nil || !u.IsAbs() || !plexmeta.ImageHost(u.Host) {
		return nil, artwork.ErrNotFound
	}
	b, _, e := a.images.GetUntrustedURL(ctx, remote)
	return b, e
}
