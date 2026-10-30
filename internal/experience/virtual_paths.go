package experience

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libpath"
)

const (
	virtualPathLibraryPage  = 250
	virtualPathChildrenPage = 500
	virtualPathScanLimit    = 100000
	virtualPathMaxInput     = 256
	virtualPathMaxDistinct  = 50
)

// VirtualPathView is one viewer-authorized library section. It carries no
// protocol or wire context.
type VirtualPathView struct {
	ID   string
	Type core.MediaType
}

// VirtualPathPage is one authorized page of catalogue titles.
type VirtualPathPage struct {
	Items []core.Title
	Total int
}

// VirtualPathCatalog is the authorized browse port for virtual library paths.
// Views, LibraryItems and Children must already enforce viewer policy. A nil
// function fails closed. This service does not resolve a source, release or
// playback detail; names come from libpath, which depends only on core and is
// shared with WebDAV rather than reimplemented here.
type VirtualPathCatalog struct {
	Views        func(context.Context, *core.Viewer) ([]VirtualPathView, error)
	LibraryItems func(context.Context, *core.Viewer, core.Device, string, int, int) (VirtualPathPage, error)
	Children     func(context.Context, *core.Viewer, core.Device, core.RatingKey, int, int) (VirtualPathPage, error)
}

// VirtualPaths resolves at most 50 distinct keys from at most 256 inputs.
// Hidden or unsupported keys are omitted. One requested set shares each
// library scan. A malformed or truncated page, or any port error, returns no
// paths.
func VirtualPaths(ctx context.Context, catalog VirtualPathCatalog, viewer *core.Viewer, device core.Device, keys []core.RatingKey) (map[core.RatingKey]string, error) {
	if len(keys) > virtualPathMaxInput {
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
	if len(requested) > virtualPathMaxDistinct {
		return nil, core.ErrNotAvailable
	}
	result := make(map[core.RatingKey]string)
	if needMovies {
		titles, err := virtualPathLibrary(ctx, catalog, viewer, device, core.TypeMovie)
		if err != nil {
			return nil, err
		}
		names := virtualPathSiblingNames(titles, libpath.MovieName)
		for _, title := range titles {
			if requested[title.Key] {
				name := names[title.Key]
				result[title.Key] = path.Join("Movies", name, name+".mkv")
			}
		}
	}
	if needShows {
		shows, err := virtualPathLibrary(ctx, catalog, viewer, device, core.TypeShow)
		if err != nil {
			return nil, err
		}
		names := virtualPathSiblingNames(shows, func(t core.Title) string { return libpath.Safe(t.Title) })
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
			seasonItems, err := virtualPathChildren(ctx, catalog, viewer, device, show.Key)
			if err != nil {
				return nil, err
			}
			filtered := seasonItems[:0]
			for _, season := range seasonItems {
				if season.Type == core.TypeSeason {
					filtered = append(filtered, season)
				}
			}
			seasonNames := virtualPathSiblingNames(filtered, func(t core.Title) string { return fmt.Sprintf("Season %02d", t.Season) })
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
				episodeItems, err := virtualPathChildren(ctx, catalog, viewer, device, season.Key)
				if err != nil {
					return nil, err
				}
				filteredEpisodes := episodeItems[:0]
				for _, episode := range episodeItems {
					if episode.Type == core.TypeEpisode {
						filteredEpisodes = append(filteredEpisodes, episode)
					}
				}
				episodeNames := virtualPathSiblingNames(filteredEpisodes, func(t core.Title) string { return strings.TrimSuffix(path.Base(libpath.Episode(t)), ".mkv") })
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

// VirtualPath builds the relative path for one already identified title.
// The caller supplies the title; this function does not look up or enrich it.
// Sibling names still come from the authorized library and children. A title
// absent from that tree is not found. A port or page error returns no path.
func VirtualPath(ctx context.Context, catalog VirtualPathCatalog, viewer *core.Viewer, device core.Device, title core.Title) (string, error) {
	if title.Type == core.TypeMovie {
		titles, err := virtualPathLibrary(ctx, catalog, viewer, device, core.TypeMovie)
		if err != nil {
			return "", err
		}
		name := virtualPathSiblingNames(titles, libpath.MovieName)[title.Key]
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
	shows, err := virtualPathLibrary(ctx, catalog, viewer, device, core.TypeShow)
	if err != nil {
		return "", err
	}
	showName := virtualPathSiblingNames(shows, func(t core.Title) string { return libpath.Safe(t.Title) })[showKey]
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
	seasonItems, err := virtualPathChildren(ctx, catalog, viewer, device, showKey)
	if err != nil {
		return "", err
	}
	filteredSeasons := seasonItems[:0]
	for _, season := range seasonItems {
		if season.Type == core.TypeSeason {
			filteredSeasons = append(filteredSeasons, season)
		}
	}
	seasonNames := virtualPathSiblingNames(filteredSeasons, func(t core.Title) string { return fmt.Sprintf("Season %02d", t.Season) })
	for _, season := range filteredSeasons {
		seasonName := seasonNames[season.Key]
		if seasonName == "" {
			continue
		}
		if title.Type == core.TypeSeason && season.Key == title.Key {
			return path.Join(base, seasonName), nil
		}
		if title.Type != core.TypeEpisode {
			continue
		}
		items, err := virtualPathChildren(ctx, catalog, viewer, device, season.Key)
		if err != nil {
			return "", err
		}
		episodes := items[:0]
		for _, episode := range items {
			if episode.Type == core.TypeEpisode {
				episodes = append(episodes, episode)
			}
		}
		names := virtualPathSiblingNames(episodes, func(t core.Title) string { return strings.TrimSuffix(path.Base(libpath.Episode(t)), ".mkv") })
		if name := names[title.Key]; name != "" {
			return path.Join(base, seasonName, name+".mkv"), nil
		}
	}
	return "", core.ErrNotFound
}

func virtualPathLibrary(ctx context.Context, catalog VirtualPathCatalog, viewer *core.Viewer, device core.Device, typ core.MediaType) ([]core.Title, error) {
	if catalog.Views == nil || catalog.LibraryItems == nil {
		return nil, core.ErrNotAvailable
	}
	views, err := catalog.Views(ctx, viewer)
	if err != nil {
		return nil, err
	}
	var titles []core.Title
	for _, view := range views {
		if view.Type != typ {
			continue
		}
		for offset := 0; offset < virtualPathScanLimit; {
			page, err := catalog.LibraryItems(ctx, viewer, device, view.ID, offset, virtualPathLibraryPage)
			if err != nil {
				return nil, err
			}
			for _, title := range page.Items {
				if title.Type == typ {
					titles = append(titles, title)
				}
			}
			offset += len(page.Items)
			if offset >= page.Total {
				break
			}
			if len(page.Items) == 0 || offset >= virtualPathScanLimit {
				return nil, core.ErrNotAvailable
			}
		}
	}
	return titles, nil
}

func virtualPathChildren(ctx context.Context, catalog VirtualPathCatalog, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]core.Title, error) {
	if catalog.Children == nil {
		return nil, core.ErrNotAvailable
	}
	var titles []core.Title
	for offset := 0; offset < virtualPathScanLimit; {
		page, err := catalog.Children(ctx, viewer, device, key, offset, virtualPathChildrenPage)
		if err != nil {
			return nil, err
		}
		titles = append(titles, page.Items...)
		offset += len(page.Items)
		if offset >= page.Total {
			return titles, nil
		}
		if len(page.Items) == 0 {
			return nil, core.ErrNotAvailable
		}
	}
	return nil, core.ErrNotAvailable
}

func virtualPathSiblingNames(titles []core.Title, name func(core.Title) string) map[core.RatingKey]string {
	items := make([]libpath.NamedKey, 0, len(titles))
	for _, title := range titles {
		items = append(items, libpath.NamedKey{Name: name(title), Key: title.Key})
	}
	return libpath.Disambiguate(items)
}
