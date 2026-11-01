package playback

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

const debridPayloadPrefix = "ETHER-DEBRID/1\n"

// payloadMatchesTitle verifies that a cached or resumed debrid payload's
// chosen file path matches the requested episode.
func payloadMatchesTitle(nzb []byte, title *core.Title) bool {
	if title == nil || title.Type != core.TypeEpisode || !bytes.HasPrefix(nzb, []byte(debridPayloadPrefix)) {
		return true
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(nzb[len(debridPayloadPrefix):], &p); err != nil || p.Path == "" {
		return true
	}
	season, episode := title.Season, title.Episode
	if title.Key.Kind() == core.KindEpisode {
		if season == 0 {
			season = title.Key.Season()
		}
		if episode == 0 {
			episode = title.Key.Episode()
		}
	}
	if episode <= 0 {
		return true
	}
	normPath := strings.ReplaceAll(p.Path, "\\", "/")
	baseInfo := releaseparse.Parse(path.Base(normPath))
	fullInfo := releaseparse.Parse(strings.ReplaceAll(normPath, "/", " "))
	if len(baseInfo.Seasons) == 0 && len(fullInfo.Seasons) > 0 {
		baseInfo.Seasons = fullInfo.Seasons
	}
	if len(baseInfo.Episodes) > 0 {
		return releaseparse.EpisodeMatches(baseInfo, season, episode)
	}
	if len(fullInfo.Episodes) > 0 {
		return releaseparse.EpisodeMatches(fullInfo, season, episode)
	}
	return true
}

// releaseMatchesTitle applies the same episode identity constraints to fresh,
// warmed, remembered and failover choices. Unknown filename metadata is not
// proof of identity; it only permits the resolver/file selector to continue.
func releaseMatchesTitle(rel core.Release, title *core.Title) bool {
	if title == nil {
		return true
	}
	expected := title.IDs
	if title.Type == core.TypeEpisode {
		expected = title.ShowIDs
		if expected.TMDB == 0 && title.Key.Kind() == core.KindEpisode {
			expected.TMDB = title.Key.TMDB()
		}
	}
	idMatch, idConflict := releaseparse.CompareTitleIDs(rel.TitleIDs, expected)
	if idConflict {
		return false
	}
	if title.Type != core.TypeEpisode {
		return true
	}
	season, episode := title.Season, title.Episode
	if title.Key.Kind() == core.KindEpisode {
		if season == 0 {
			season = title.Key.Season()
		}
		if episode == 0 {
			episode = title.Key.Episode()
		}
	}
	info := rel.Info
	if len(info.Episodes) == 0 && !info.FullSeason {
		info = releaseparse.Parse(rel.Name)
	}
	if episode > 0 && (len(info.Episodes) > 0 || info.FullSeason) {
		if info.FullSeason {
			if len(info.Seasons) > 0 {
				found := false
				for _, n := range info.Seasons {
					found = found || n == season
				}
				if !found {
					return false
				}
			}
		} else if !releaseparse.EpisodeMatches(info, season, episode) {
			return false
		}
	}
	if rel.Target != nil && episode > 0 &&
		(rel.Target.Episode != episode || rel.Target.Season != season) {
		return false
	}
	year := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
	if !releaseparse.SeriesYearMatches(year, title.ShowYear) ||
		(year == 0 && !idMatch && !releaseparse.EpisodeTitleMatches(rel.Name, title.Title)) {
		return false
	}
	// A torrent descriptor embeds the expected identity for file selection.
	// Older cached descriptors must be resolved again to acquire that target.
	return rel.Protocol != core.ProtocolTorrent || title.ShowYear <= 0 ||
		(rel.Target != nil && rel.Target.ShowYear == title.ShowYear && rel.Target.EpisodeTitle == title.Title)
}
