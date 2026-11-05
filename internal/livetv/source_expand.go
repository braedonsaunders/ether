package livetv

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// expandProgramSource runs only while saving or rebuilding background snapshots.
// Persisted Items are the sole input to the deterministic playout engine.
func (s *Service) expandProgramSource(ctx context.Context, src ProgramSource, depth int) ([]Item, error) {
	if src.StartSeason < 0 || src.StartEpisode < 0 {
		return nil, fmt.Errorf("show starting season and episode must not be negative")
	}
	if src.Order != "" && src.Order != "order" && src.Order != "shuffle" {
		return nil, fmt.Errorf("source order must be order or shuffle")
	}
	if depth > 8 {
		return nil, fmt.Errorf("programming sources exceed eight levels")
	}
	var items []Item
	var err error
	switch src.Kind {
	case "show":
		if s.o.Catalog == nil || src.ShowKey == 0 {
			return nil, fmt.Errorf("show source needs a catalogue show")
		}
		var titles []core.Title
		titles, err = s.o.Catalog.Grandchildren(ctx, src.ShowKey)
		for _, title := range titles {
			if title.Type == core.TypeEpisode && !title.Adult && title.Runtime > 0 && (title.Season > src.StartSeason || title.Season == src.StartSeason && title.Episode >= src.StartEpisode) {
				items = append(items, itemFromTitle(title))
			}
		}
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].Season != items[j].Season {
				return items[i].Season < items[j].Season
			}
			return items[i].Episode < items[j].Episode
		})
	case "movies":
		f := core.Filter{Type: core.TypeMovie}
		if src.Filter != nil {
			f = *src.Filter
			f.Type = core.TypeMovie
		}
		if len(src.Items) > 0 {
			for _, it := range src.Items {
				if it.ShowKey == 0 && it.MediaType != "episode" {
					items = append(items, it)
				}
			}
		} else {
			items, err = s.expandQuery(ctx, src.Collection, &f)
		}
		movies := items[:0]
		for _, it := range items {
			if it.ShowKey == 0 && it.MediaType != "episode" {
				movies = append(movies, it)
			}
		}
		items = movies
	case "personal":
		if s.o.Picker == nil || s.o.InputUser == nil {
			return nil, fmt.Errorf("personal picks unavailable")
		}
		user, e := s.o.InputUser(ctx)
		if e != nil {
			return nil, e
		}
		if user == nil {
			return nil, fmt.Errorf("personal picks need a viewer")
		}
		weights := map[string]float64{}
		if src.Personal != nil {
			for _, part := range src.Personal.Ingredients {
				if part.Enabled {
					weights[part.Kind] = part.Share
				}
			}
		}
		picks, e := s.o.Picker.Picks(ctx, user, weights, 200)
		err = e
		for _, pick := range picks {
			if pick.Title != nil && !pick.Title.Adult && pick.Title.Runtime > 0 {
				items = append(items, itemFromTitle(*pick.Title))
			}
		}
	case "plugin":
		if !strings.HasPrefix(src.SegmentID, "plugin:") || s.o.Graphics == nil {
			return nil, fmt.Errorf("plugin segment unavailable")
		}
		c := s.graphicsChannel(Channel{})
		it, ok := c.pluginItem(src.SegmentID)
		if !ok {
			return nil, fmt.Errorf("plugin segment unavailable")
		}
		items = []Item{it}
	case "mix":
		parts := make([][]Item, len(src.Sources))
		weights := make([]int, len(src.Sources))
		for i, child := range src.Sources {
			parts[i], err = s.expandProgramSource(ctx, child, depth+1)
			src.Sources[i].Items = append([]Item(nil), parts[i]...)
			if err != nil {
				return nil, err
			}
			weights[i] = max(1, min(20, child.Weight))
		}
		items = interleave(parts, weights)
	case "", "items", "collection", "filter":
		items, err = s.resolveProgramming(ctx, src.Items, src.Collection, src.Filter, nil, src.Watched)
	default:
		return nil, fmt.Errorf("unknown programming source %q", src.Kind)
	}
	if err != nil {
		return nil, err
	}
	if src.Kind != "" && src.Kind != "items" && src.Kind != "collection" && src.Kind != "filter" && src.Watched != "" {
		items, err = s.filterWatched(ctx, items, src.Watched)
	}
	for i := range items {
		if src.ID != "" {
			items[i].SourceID = src.ID
		}
	}
	return items, err
}

func validateProgramming(c *Channel, check func([]Item) error) error {
	var sources func([]ProgramSource, int) error
	sources = func(list []ProgramSource, depth int) error {
		if depth > 8 || len(list) > 100 {
			return fmt.Errorf("too many programming sources")
		}
		for _, src := range list {
			if src.StartDate != "" {
				if _, err := time.Parse("2006-01-02", src.StartDate); err != nil {
					return fmt.Errorf("invalid strip start date")
				}
			}
			if src.StartSeason < 0 || src.StartEpisode < 0 || src.Weight < 0 || src.Weight > 100 {
				return fmt.Errorf("invalid source start or weight")
			}
			if src.Order != "" && src.Order != "order" && src.Order != "shuffle" {
				return fmt.Errorf("invalid source order")
			}
			if src.Strip && src.Kind != "show" {
				return fmt.Errorf("strip requires a show source")
			}
			if err := sources(src.Sources, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := sources(c.Sources, 0); err != nil {
		return err
	}
	for _, b := range c.Blocks {
		if _, _, err := DaypartPreset(b.Preset); err != nil {
			return err
		}
		if err := sources(b.Sources, 0); err != nil {
			return err
		}
	}
	for _, p := range c.Pins {
		if err := sources(p.Sources, 0); err != nil {
			return err
		}
	}

	look := func(l *ChannelLook) error {
		if l == nil {
			return nil
		}
		switch l.BreakPlacement {
		case "", "natural", "even", "between":
		default:
			return fmt.Errorf("unknown break placement")
		}
		for moment, digest := range l.MomentAssets {
			switch moment {
			case "ident", "upnext", "tonight", "break", "back":
			default:
				return fmt.Errorf("invalid creative moment")
			}
			if digest != "" {
				if len(digest) != 64 {
					return fmt.Errorf("invalid creative asset digest")
				}
				for _, c := range digest {
					if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
						return fmt.Errorf("invalid creative asset digest")
					}
				}
			}
		}
		if l.BreakPattern != nil {
			if err := l.BreakPattern.Validate(); err != nil {
				return err
			}
			l.BreakContent = "reel"
			l.Placement = "insert"
			if l.ReelID == "" {
				return fmt.Errorf("commercial pods require a reel")
			}
		}
		return nil
	}
	if err := look(c.Look); err != nil {
		return err
	}
	for _, b := range c.Blocks {
		if err := look(b.Breaks); err != nil {
			return err
		}
	}
	for i, p := range c.Pins {
		if p.Start < 0 || p.End <= p.Start || p.End > 1440 {
			return fmt.Errorf("pinned slots must be within 00:00–24:00")
		}
		if p.Date != "" {
			if _, err := time.Parse("2006-01-02", p.Date); err != nil {
				return fmt.Errorf("special needs a date in YYYY-MM-DD format")
			}
		}
		for _, d := range p.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("pin weekday must be 0–6")
			}
		}
		if len(p.Items) == 0 {
			return fmt.Errorf("pin needs programming")
		}
		if err := check(p.Items); err != nil {
			return err
		}
		if err := look(p.Breaks); err != nil {
			return err
		}
		if err := checkPlay(p.Mode, "", "cut", p.RepeatDays, ""); err != nil {
			return err
		}
		for _, other := range c.Pins[i+1:] {
			if p.Date != other.Date {
				continue
			}
			if blocksOverlap(Block{Start: p.Start, End: p.End, Days: p.Days}, Block{Start: other.Start, End: other.End, Days: other.Days}) {
				return fmt.Errorf("pinned slots must not overlap")
			}
		}
	}
	return nil
}
