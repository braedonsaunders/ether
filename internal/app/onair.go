package app

import (
	"bytes"
	"context"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/onair"
	plexmeta "github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/transcode"
	"image"
	"net/url"
	"path/filepath"
	"strconv"
	"time"
)

func (a *App) buildOnAir() error {
	o := onair.Options{Enabled: func() bool { return a.livetv != nil && a.livetv.Enabled() }, Showcase: parityShowcase(), Changed: func(ctx context.Context) error {
		if a.livetv != nil {
			return a.livetv.RefreshGuide(ctx)
		}
		return nil
	}, ReelsChanged: func(ctx context.Context) {
		if a.livetv != nil {
			a.livetv.RefreshGraphics(ctx)
		}
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
			return a.transcoder.AcquireTranscodeSlot(ctx, transcode.SlotBackground)
		}
		o.FFmpeg = a.transcoder.FFmpegPath
		o.Probe = a.transcoder.Probe
		o.EncoderOptions = a.transcoder.OnAirEncoderOptions
		o.WorkingSize = func() (int, int) { return 1280, 720 }
	}
	o.Downloader = extras.NewDownloader(extras.DownloaderOptions{
		ToolsDir: filepath.Join(a.opts.DataDir, "tools"),
		Config:   func() config.Extras { return a.cfg.Read().Extras },
		FFmpeg:   o.FFmpeg,
	})
	o.AssetDir = filepath.Join(a.opts.DataDir, backup.OnAirAssetDir)
	if a.cluster != nil {
		o.NodeID = a.cluster.Self().ID
		if a.cluster.Mode() != cluster.ModeSolo {
			o.Members = a.onairMembers
			o.AssetsChanged = func(ctx context.Context) { _ = a.cluster.Publish(ctx, onairAssetsTopic, nil) }
		}
	}
	svc, e := onair.New(o)
	if e != nil {
		return e
	}
	a.onair = svc
	a.closers = append(a.closers, svc.Close)
	if o.Members != nil {
		// Another node asked for bytes, or membership changed: run a pass.
		a.closers = append(a.closers, a.cluster.Subscribe(onairAssetsTopic, func([]byte) { svc.PokeAssets() }), a.cluster.Subscribe(cluster.TopicMembers, func([]byte) { svc.PokeAssets() }))
	}
	return nil
}

// onairAssetsTopic asks every node to run an On Air asset pass.
const onairAssetsTopic = "onair:assets"

// onairMembers are the nodes that keep On Air assets: every recorded member
// (dead ones stay recorded for a day) that runs On Air or may be an older
// full server reading asset bytes from the database.
func (a *App) onairMembers(ctx context.Context) ([]onair.Member, error) {
	nodes, err := a.cluster.AllNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]onair.Member, 0, len(nodes))
	for _, n := range nodes {
		if n.Caps.OnAirAssets > 0 || n.Caps.Roles.FullServer() {
			out = append(out, onair.Member{ID: n.ID, Files: n.Caps.OnAirAssets >= onair.AssetFilesVersion})
		}
	}
	return out, nil
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
		v["meta"] = livetv.ProgrammeMeta(p.Item)
		v["logline"] = livetv.Logline(p.Item.Summary)
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
