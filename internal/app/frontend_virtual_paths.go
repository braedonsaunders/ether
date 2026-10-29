package app

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libpath"
)

type surfaceVirtualPaths struct {
	experience frontend.Experience
	base       func(context.Context, *http.Request) (string, error)
	enabled    func() bool
}

func (s surfaceVirtualPaths) Paths(ctx context.Context, viewer *core.Viewer, device core.Device, keys []core.RatingKey) (map[core.RatingKey]string, error) {
	if s.experience == nil || (s.enabled != nil && !s.enabled()) {
		return nil, core.ErrNotAvailable
	}
	if len(keys) > 256 {
		return nil, core.ErrNotAvailable
	}
	requested := make(map[core.RatingKey]bool, len(keys))
	var needMovies, needShows bool
	wantedShows := map[core.RatingKey]bool{}
	for _, key := range keys {
		requested[key] = true
		switch key.Kind() {
		case core.KindMovie:
			needMovies = true
		case core.KindShow:
			needShows = true
			wantedShows[key] = true
		case core.KindSeason, core.KindEpisode:
			needShows = true
			wantedShows[core.ShowKey(key.TMDB())] = true
		}
	}
	if len(requested) > 50 {
		return nil, core.ErrNotAvailable
	}
	result := make(map[core.RatingKey]string)
	if needMovies {
		titles, err := s.library(ctx, viewer, device, core.TypeMovie)
		if err != nil {
			return nil, err
		}
		names := siblingNames(titles, libpath.MovieName)
		for _, title := range titles {
			if requested[title.Key] {
				name := names[title.Key]
				result[title.Key] = path.Join("Movies", name, name+".mkv")
			}
		}
	}
	if needShows {
		shows, err := s.library(ctx, viewer, device, core.TypeShow)
		if err != nil {
			return nil, err
		}
		names := siblingNames(shows, func(t core.Title) string { return libpath.Safe(t.Title) })
		for _, show := range shows {
			if !wantedShows[show.Key] {
				continue
			}
			base := path.Join("TV", names[show.Key])
			if requested[show.Key] {
				result[show.Key] = base
			}
			var wantsChildren bool
			for key := range requested {
				if key.Kind() != core.KindShow && key.TMDB() == show.Key.TMDB() && (key.Kind() == core.KindSeason || key.Kind() == core.KindEpisode) {
					wantsChildren = true
					break
				}
			}
			if !wantsChildren {
				continue
			}
			seasons, err := s.experience.Children(ctx, viewer, device, show.Key, frontend.ItemQuery{Limit: 10000})
			if err != nil {
				return nil, err
			}
			filtered := seasons.Items[:0]
			for _, season := range seasons.Items {
				if season.Type == core.TypeSeason {
					filtered = append(filtered, season)
				}
			}
			seasonNames := siblingNames(filtered, func(t core.Title) string { return fmt.Sprintf("Season %02d", t.Season) })
			for _, season := range filtered {
				seasonBase := path.Join(base, seasonNames[season.Key])
				if requested[season.Key] {
					result[season.Key] = seasonBase
				}
				var wantsEpisodes bool
				for key := range requested {
					if key.Kind() == core.KindEpisode && key.TMDB() == show.Key.TMDB() {
						wantsEpisodes = true
						break
					}
				}
				if !wantsEpisodes {
					continue
				}
				episodes, err := s.experience.Children(ctx, viewer, device, season.Key, frontend.ItemQuery{Limit: 10000})
				if err != nil {
					return nil, err
				}
				filteredEpisodes := episodes.Items[:0]
				for _, episode := range episodes.Items {
					if episode.Type == core.TypeEpisode {
						filteredEpisodes = append(filteredEpisodes, episode)
					}
				}
				episodeNames := siblingNames(filteredEpisodes, func(t core.Title) string { return strings.TrimSuffix(path.Base(libpath.Episode(t)), ".mkv") })
				for _, episode := range filteredEpisodes {
					if requested[episode.Key] {
						result[episode.Key] = path.Join(seasonBase, episodeNames[episode.Key]+".mkv")
					}
				}
			}
		}
	}
	return result, nil
}

func (s surfaceVirtualPaths) Base(ctx context.Context, req *http.Request) (string, error) {
	if s.enabled == nil || !s.enabled() || s.base == nil {
		return "", core.ErrNotAvailable
	}
	return s.base(ctx, req)
}

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
	if s.enabled != nil && !s.enabled() {
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
