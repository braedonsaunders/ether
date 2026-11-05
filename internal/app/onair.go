package app

import (
	"bytes"
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/transcode"
	"image"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *App) buildOnAir() error {
	o := onair.Options{DB: a.db, Dir: filepath.Join(a.opts.DataDir, "cache", "onair"), Logger: a.log.With("component", "onair"), Bindings: a.onairBindings}
	o.StyleChannels = func(ctx context.Context) (map[string][]string, error) {
		if a.livetv == nil {
			return map[string][]string{}, nil
		}
		return a.livetv.StyleChannelIDs(ctx, a.cfg.Read().LiveTV.DefaultStyleID)
	}
	o.ReelChannels = func(ctx context.Context) (map[string][]string, error) {
		if a.livetv == nil {
			return map[string][]string{}, nil
		}
		return a.livetv.ReelChannelIDs(ctx)
	}
	o.Changed = func(ctx context.Context) error {
		if a.livetv != nil {
			return a.livetv.RefreshGuide(ctx)
		}
		return nil
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
			b, _, e := a.images.Get(ctx, ref, 1920)
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
		v["backdrop"] = p.Item.Art
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
