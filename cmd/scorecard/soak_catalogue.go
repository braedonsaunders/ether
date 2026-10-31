package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// catalogFile is the on-disk scorecard manifest row.
type catalogFile struct {
	TMDB    int64  `json:"tmdb"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

type catalogItem struct {
	Id                string            `json:"Id"`
	SeriesId          string            `json:"SeriesId,omitempty"`
	Name              string            `json:"Name"`
	Type              string            `json:"Type"`
	Path              string            `json:"Path,omitempty"`
	ProviderIds       map[string]string `json:"ProviderIds,omitempty"`
	IndexNumber       int               `json:"IndexNumber,omitempty"`
	ParentIndexNumber int               `json:"ParentIndexNumber,omitempty"`
	ProductionYear    int               `json:"ProductionYear,omitempty"`
	RunTimeTicks      int64             `json:"RunTimeTicks,omitempty"`
}

var yearInPath = regexp.MustCompile(`\((19|20)\d{2}\)`)

// synthesizeCatalogue builds the scorecard item listing from verified files.
func synthesizeCatalogue(files []catalogFile) ([]byte, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no catalogue files")
	}
	const runtimeTicks int64 = 45 * 60 * 10000000
	seenShow := map[int64]bool{}
	seenSeason := map[string]bool{}
	var items []catalogItem
	for _, file := range files {
		if file.TMDB <= 0 || file.Path == "" || len(file.SHA256) != 64 {
			return nil, fmt.Errorf("catalogue file is incomplete")
		}
		year := 0
		if match := yearInPath.FindString(file.Path); match != "" {
			year, _ = strconv.Atoi(match[1 : len(match)-1])
		}
		tmdb := strconv.FormatInt(file.TMDB, 10)
		if file.Episode == 0 {
			items = append(items, catalogItem{
				Id: "movie-" + tmdb, Name: strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path)),
				Type: "Movie", Path: file.Path, ProviderIds: map[string]string{"Tmdb": tmdb},
				ProductionYear: year, RunTimeTicks: runtimeTicks,
			})
			continue
		}
		showName := filepath.Base(filepath.Dir(file.Path))
		if strings.HasPrefix(strings.ToLower(showName), "season") {
			showName = filepath.Base(filepath.Dir(filepath.Dir(file.Path)))
		}
		showName = strings.TrimSpace(yearInPath.ReplaceAllString(showName, ""))
		showID := "show-" + tmdb
		if !seenShow[file.TMDB] {
			seenShow[file.TMDB] = true
			items = append(items, catalogItem{
				Id: showID, Name: showName, Type: "Series", ProviderIds: map[string]string{"Tmdb": tmdb},
				ProductionYear: year,
			})
		}
		seasonID := fmt.Sprintf("%s-s%d", showID, file.Season)
		if !seenSeason[seasonID] {
			seenSeason[seasonID] = true
			items = append(items, catalogItem{
				Id: seasonID, SeriesId: showID, Name: fmt.Sprintf("Season %d", file.Season), Type: "Season",
				IndexNumber: file.Season, ProviderIds: map[string]string{"Tmdb": tmdb}, ProductionYear: year,
			})
		}
		items = append(items, catalogItem{
			Id: fmt.Sprintf("ep-%d-%d-%d", file.TMDB, file.Season, file.Episode), SeriesId: showID,
			Name: strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path)), Type: "Episode",
			Path: file.Path, ProviderIds: map[string]string{"Tmdb": tmdb},
			IndexNumber: file.Episode, ParentIndexNumber: file.Season, ProductionYear: year, RunTimeTicks: runtimeTicks,
		})
	}
	return json.Marshal(items)
}
