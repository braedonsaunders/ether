package playback

import (
	"bytes"
	"context"
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
		Path   string              `json:"path"`
		Target *core.ReleaseTarget `json:"target"`
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
	if season == 0 {
		if p.Target == nil || p.Target.Season != 0 || p.Target.Episode != episode {
			return false
		}
		if !releaseparse.SpecialMatches(p.Path, title.Title, p.Target.SpecialTitles, p.Target.ShowTitles) {
			return false
		}
		actual := episode
		if p.Target.AddonSpecialEpisode > 0 {
			actual = p.Target.AddonSpecialEpisode
		}
		info := releaseparse.Parse(path.Base(strings.ReplaceAll(p.Path, "\\", "/")))
		return releaseparse.EpisodeMatches(info, 0, actual) || (len(info.Seasons) == 0 && len(info.Episodes) == 0 && len(info.AbsoluteEpisodes) == 0 && releaseparse.SpecialTitleEvidence(path.Base(strings.ReplaceAll(p.Path, "\\", "/")), title.Title, p.Target.ShowTitles...))
	}
	normPath := strings.ReplaceAll(p.Path, "\\", "/")
	baseInfo := releaseparse.Parse(path.Base(normPath))
	fullInfo := releaseparse.Parse(strings.ReplaceAll(normPath, "/", " "))
	if len(baseInfo.Seasons) == 0 && len(fullInfo.Seasons) > 0 {
		baseInfo.Seasons = fullInfo.Seasons
	}
	if len(baseInfo.Episodes) > 0 || len(baseInfo.AbsoluteEpisodes) > 0 {
		return releaseparse.EpisodeMatchesNumbering(baseInfo, season, episode, playbackAbsoluteEpisode(title, season, episode))
	}
	if len(fullInfo.Episodes) > 0 || len(fullInfo.AbsoluteEpisodes) > 0 {
		return releaseparse.EpisodeMatchesNumbering(fullInfo, season, episode, playbackAbsoluteEpisode(title, season, episode))
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
	idMatch, idConflict := releaseparse.CompareTitleIDs(rel.ReportedTitleIDs(), expected)
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
	// Zero season denotes a special only when an episode number is known.
	// Partially hydrated titles still carry usable series/year identity.
	if season == 0 && episode > 0 {
		shows := append([]string{title.ShowTitle, title.ShowOriginalTitle}, title.ShowAlternativeTitles...)
		var siblings []string
		if rel.Target != nil {
			siblings = rel.Target.SpecialTitles
		}
		if !releaseparse.SpecialMatches(rel.Name, title.Title, siblings, shows) {
			return false
		}
	}
	info := rel.Info
	if len(info.Episodes) == 0 && !info.FullSeason {
		info = releaseparse.Parse(rel.Name)
	}
	if episode > 0 && (len(info.Episodes) > 0 || len(info.AbsoluteEpisodes) > 0 || info.FullSeason) {
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
		} else if !releaseparse.EpisodeMatchesNumbering(info, season, episode, playbackAbsoluteEpisode(title, season, episode)) {
			return false
		}
	}
	if rel.Target != nil && episode > 0 &&
		(rel.Target.Episode != episode || rel.Target.Season != season) {
		return false
	}
	year := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
	if !releaseparse.SeriesYearMatches(year, title.ShowYear) ||
		((season != 0 || episode <= 0) && year == 0 && !idMatch && !releaseparse.EpisodeTitleMatches(rel.Name, title.Title)) {
		return false
	}
	// A torrent descriptor embeds the expected identity for file selection.
	// Older cached descriptors must be resolved again to acquire that target.
	return rel.Protocol != core.ProtocolTorrent || title.ShowYear <= 0 ||
		(rel.Target != nil && rel.Target.ShowYear == title.ShowYear && rel.Target.EpisodeTitle == title.Title)
}

// rememberedReleaseMatchesTitle delegates to current resolver identity rules
// when supported, including its owner-pin exemption.
func (m *Manager) rememberedReleaseMatchesTitle(ctx context.Context, rel core.Release, title *core.Title) bool {
	if checker, ok := m.deps.Resolver.(interface {
		RememberedReleaseAllowed(context.Context, *core.Title, core.Release) bool
	}); ok {
		return checker.RememberedReleaseAllowed(ctx, title, rel)
	}
	return releaseMatchesTitle(rel, title)
}

// Use the same known absolute number as the resolver, including season one's
// natural count. Unknown later-season counts provide no fallback evidence.
func playbackAbsoluteEpisode(title *core.Title, season, episode int) int {
	if title.AbsoluteEpisode > 0 {
		return title.AbsoluteEpisode
	}
	if season == 1 {
		return episode
	}
	return 0
}
