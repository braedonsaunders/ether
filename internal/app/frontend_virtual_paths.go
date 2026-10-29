package app

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libpath"
)

type surfaceVirtualPaths struct{ experience frontend.Experience }

func (s surfaceVirtualPaths) library(ctx context.Context, viewer *core.Viewer, device core.Device, typ core.MediaType) ([]core.Title, error) {
	views, err := s.experience.Views(ctx, viewer)
	if err != nil {
		return nil, err
	}
	var titles []core.Title
	for _, view := range views {
		if view.Type != typ {
			continue
		}
		for offset := 0; offset < 100000; offset += 250 {
			page, err := s.experience.LibraryItems(ctx, viewer, device, view.ID, frontend.ItemQuery{Offset: offset, Limit: 250})
			if err != nil {
				return nil, err
			}
			for _, title := range page.Items {
				if title.Type == typ {
					titles = append(titles, title)
				}
			}
			if len(page.Items) < 250 {
				break
			}
			if offset == 99750 {
				return nil, core.ErrNotAvailable
			}
		}
	}
	return titles, nil
}

func siblingNames(titles []core.Title, name func(core.Title) string) map[core.RatingKey]string {
	items := make([]libpath.NamedKey, 0, len(titles))
	for _, title := range titles {
		items = append(items, libpath.NamedKey{Name: name(title), Key: title.Key})
	}
	return libpath.Disambiguate(items)
}

func (s surfaceVirtualPaths) Path(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (string, error) {
	if s.experience == nil {
		return "", core.ErrNotAvailable
	}
	detail, err := s.experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
	if err != nil {
		return "", err
	}
	title := detail.Title
	if title.Key != key {
		return "", core.ErrNotFound
	}
	if title.Type == core.TypeMovie {
		titles, err := s.library(ctx, viewer, device, core.TypeMovie)
		if err != nil {
			return "", err
		}
		name := siblingNames(titles, libpath.MovieName)[key]
		if name == "" {
			return "", core.ErrNotFound
		}
		return path.Join("Movies", name, name+".mkv"), nil
	}
	showKey := title.Key
	if title.Type == core.TypeEpisode || title.Type == core.TypeSeason {
		showKey = title.ShowKey
	}
	if showKey == 0 {
		return "", core.ErrNotFound
	}
	shows, err := s.library(ctx, viewer, device, core.TypeShow)
	if err != nil {
		return "", err
	}
	showName := siblingNames(shows, func(t core.Title) string { return libpath.Safe(t.Title) })[showKey]
	if showName == "" {
		return "", core.ErrNotFound
	}
	base := path.Join("TV", showName)
	if title.Type == core.TypeShow {
		return base, nil
	}
	if title.Type != core.TypeSeason && title.Type != core.TypeEpisode {
		return "", core.ErrNotFound
	}
	seasons, err := s.experience.Children(ctx, viewer, device, showKey, frontend.ItemQuery{Limit: 10000})
	if err != nil {
		return "", err
	}
	filteredSeasons := seasons.Items[:0]
	for _, season := range seasons.Items {
		if season.Type == core.TypeSeason {
			filteredSeasons = append(filteredSeasons, season)
		}
	}
	seasons.Items = filteredSeasons
	seasonNames := siblingNames(seasons.Items, func(t core.Title) string { return fmt.Sprintf("Season %02d", t.Season) })
	for _, season := range seasons.Items {
		seasonName := seasonNames[season.Key]
		if seasonName == "" {
			continue
		}
		if title.Type == core.TypeSeason && season.Key == key {
			return path.Join(base, seasonName), nil
		}
		if title.Type != core.TypeEpisode {
			continue
		}
		page, err := s.experience.Children(ctx, viewer, device, season.Key, frontend.ItemQuery{Limit: 10000})
		if err != nil {
			return "", err
		}
		episodes := page.Items[:0]
		for _, episode := range page.Items {
			if episode.Type == core.TypeEpisode {
				episodes = append(episodes, episode)
			}
		}
		page.Items = episodes
		names := siblingNames(page.Items, func(t core.Title) string { return strings.TrimSuffix(path.Base(libpath.Episode(t)), ".mkv") })
		if name := names[key]; name != "" {
			return path.Join(base, seasonName, name+".mkv"), nil
		}
	}
	return "", core.ErrNotFound
}
