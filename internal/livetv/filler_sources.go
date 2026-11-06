package livetv

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func validFillerItem(it Item) bool {
	if it.Key > 0 {
		return it.Slate == "" && it.AssetDigest == "" && it.ReelClipID == ""
	}
	if it.Key != 0 {
		return false
	}
	if it.AssetDigest != "" {
		b, e := hex.DecodeString(it.AssetDigest)
		return e == nil && len(b) == 32 && it.Slate == "asset" && it.ReelID == "" && it.ReelClipID == ""
	}
	if it.ReelClipID != "" {
		return it.Slate == "reel" && it.ReelID != ""
	}
	return fillerMoment(it.Slate) || strings.HasPrefix(it.Slate, "plugin:")
}

// Resolve only owner-selected short-form references while saving. The scheduler
// consumes the resulting immutable item metadata, without asset/provider I/O.
func (s *Service) expandFillers(ctx context.Context, c *Channel) error {
	reels := map[string]ReelSnapshot{}
	resolve := func(items []Item, look *ChannelLook) ([]Item, error) {
		if len(items) > 1000 {
			return nil, fmt.Errorf("too many filler items")
		}
		view := *c
		view.Look = look
		view = s.graphicsChannel(view)
		out := append([]Item(nil), items...)
		for i, it := range out {
			if !validFillerItem(it) {
				return nil, fmt.Errorf("invalid filler source")
			}
			switch {
			case it.Key > 0:
				// Catalogue sources keep their saved, owner-selected duration, as ordinary
				// programme snapshots do. Viewer policy still checks the catalogue key.
			case it.AssetDigest != "":
				if s.o.FillerAsset == nil {
					return nil, fmt.Errorf("video filler metadata unavailable")
				}
				a, e := s.o.FillerAsset(ctx, it.AssetDigest)
				if e != nil || a.Duration <= 0 {
					return nil, fmt.Errorf("video filler is not ready")
				}
				it.Title = a.Name
				it.DurationMS = a.Duration.Milliseconds()
			case it.ReelClipID != "":
				reel, ok := reels[it.ReelID]
				if !ok {
					if s.o.Reels == nil {
						return nil, fmt.Errorf("filler collection unavailable")
					}
					var e error
					reel, e = s.o.Reels(ctx, it.ReelID)
					if e != nil {
						return nil, e
					}
					reels[it.ReelID] = reel
				}
				found := false
				for _, clip := range reel.Clips {
					if clip.ID == it.ReelClipID && clip.Duration > 0 && clip.Path != "" {
						it.Title = clip.Name
						it.DurationMS = clip.Duration.Milliseconds()
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("filler clip is not ready")
				}
			case strings.HasPrefix(it.Slate, "plugin:"):
				selected, ok := view.pluginItem(it.Slate)
				if !ok {
					return nil, fmt.Errorf("filler plugin unavailable")
				}
				it = selected
			case fillerMoment(it.Slate):
				d, ok := view.momentDuration(it.Slate)
				if !ok || d <= 0 {
					return nil, fmt.Errorf("filler promo unavailable")
				}
				it.DurationMS = d.Milliseconds()
			}
			if it.DurationMS < 1000 || it.DurationMS > int64((24*time.Hour)/time.Millisecond) {
				return nil, fmt.Errorf("invalid filler duration")
			}
			out[i] = it
		}
		return out, nil
	}
	var err error
	c.Filler, err = resolve(c.Filler, c.Look)
	if err != nil {
		return err
	}
	for i := range c.Blocks {
		b := &c.Blocks[i]
		b.Filler, err = resolve(b.Filler, effectiveBlockLook(c.Look, b.Breaks))
		if err != nil {
			return err
		}
	}
	for i := range c.Pins {
		p := &c.Pins[i]
		p.Filler, err = resolve(p.Filler, effectiveBlockLook(c.Look, p.Breaks))
		if err != nil {
			return err
		}
	}
	return nil
}

func (c Channel) fillerReelIDs() []string {
	ids := map[string]bool{}
	add := func(items []Item) {
		for _, it := range items {
			if it.ReelID != "" {
				ids[it.ReelID] = true
			}
		}
	}
	add(c.Filler)
	for _, b := range c.Blocks {
		add(b.Filler)
	}
	for _, p := range c.Pins {
		add(p.Filler)
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out
}
