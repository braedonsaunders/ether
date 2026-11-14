package stremio

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

var specialNumberTag = regexp.MustCompile(`(?i)\bS0{1,3}[ ._-]*E(\d{1,4})\b`)

// specialRelease validates the add-on's own filename before translating the
// identity to the catalogue order. The original IMDb number is retained for
// debrid's independent file validation.
func specialRelease(rel core.Release, req core.SearchRequest, file, torrent string, asked int) (core.Release, bool) {
	shows := append([]string{req.Title, req.OriginalTitle}, req.AlternativeTitles...)
	actual := file
	if actual == "" {
		actual = torrent
	}
	if actual == "" {
		actual = rel.Name
	}
	info := releaseparse.Parse(actual)
	packInfo := releaseparse.Parse(torrent)
	if len(packInfo.Seasons) > 0 && !releaseparse.NamesSpecial(torrent) {
		return rel, false
	}
	if len(info.Seasons) > 0 && (!slices.Contains(info.Seasons, 0) || len(info.Seasons) != 1) {
		return rel, false
	}
	if !releaseparse.SpecialMatches(actual, req.EpisodeTitle, req.SpecialTitles, shows) {
		return rel, false
	}
	expected := req.Episode
	if asked > 0 {
		expected = asked
	}
	if len(info.Seasons) > 0 {
		if !releaseparse.EpisodeMatches(info, 0, expected) {
			return rel, false
		}
	} else {
		// Bare regular numbering never becomes season zero on the add-on's word.
		if len(info.Episodes) > 0 || len(info.AbsoluteEpisodes) > 0 || !releaseparse.SpecialTitleEvidence(actual, req.EpisodeTitle, shows...) {
			return rel, false
		}
	}
	// Use the validated special's name, retaining technical facts from the pack.
	rel.Name = actual
	rel.Name = specialNumberTag.ReplaceAllString(rel.Name, fmt.Sprintf("S00E%02d", req.Episode))
	rel.Info = describe(rel.Name, file, torrent)
	if prefix := releaseparse.SpecialSeriesTitle(actual); prefix != "" && releaseparse.TitleMatchesAny(core.ReleaseInfo{Title: prefix}, req.Title, shows) {
		rel.Info.Title = prefix
	}
	rel.Info.Seasons, rel.Info.Episodes, rel.Info.FullSeason = []int{0}, []int{req.Episode}, false
	rel.Info.AbsoluteEpisodes = nil
	rel.Target = &core.ReleaseTarget{Season: 0, Episode: req.Episode, EpisodeTitle: req.EpisodeTitle, ShowTitles: shows, SpecialTitles: req.SpecialTitles, AddonSpecialEpisode: asked}
	return rel, true
}
