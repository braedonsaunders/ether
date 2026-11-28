package playback

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"path"
	"slices"
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
	if episode <= 0 {
		return true
	}
	origSeason, origEpisode := season, episode
	absolute := playbackAbsoluteEpisode(title, season, episode)
	var valid bool
	season, episode, valid = targetEpisodeNumbers(p.Target, season, episode)
	if !valid {
		return false
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
	// The file's own season, else the one its nearest season folder
	// names: in a range pack "Show.S01-S05/S02/..." the whole path reads
	// as seasons 1 to 5 and would accept another season's file.
	folderSeasons := releaseparse.FolderSeasons(normPath)
	if len(baseInfo.Seasons) == 0 && len(folderSeasons) > 0 {
		baseInfo.Seasons = folderSeasons
		fullInfo.Seasons = folderSeasons
	}
	matchesEpisode := func(info core.ReleaseInfo) bool {
		if releaseparse.AbsoluteConflict(info, absolute) {
			return false
		}
		if releaseparse.EpisodeMatchesNumbering(info, season, episode, absolute) {
			return true
		}
		if p.Target != nil && p.Target.CanonicalSeason == origSeason && p.Target.CanonicalEpisode == origEpisode {
			if releaseparse.EpisodeMatches(info, origSeason, origEpisode) {
				return true
			}
			for _, n := range p.Target.Aliases {
				if releaseparse.EpisodeMatches(info, n.Season, n.Episode) {
					return true
				}
			}
		}
		if season > 0 && !info.FullSeason && releaseparse.EpisodeMatchesAirDate(info, cmp.Or(title.EpisodeAirDate, title.Released)) {
			if len(info.Episodes) == 0 && len(info.AbsoluteEpisodes) == 0 {
				return true
			}
			if season > 1 && absolute <= 0 && len(info.Seasons) == 0 && len(info.AbsoluteEpisodes) == 0 && len(info.Episodes) == 1 && info.Episodes[0] > episode {
				return true
			}
		}
		return false
	}
	if len(baseInfo.Episodes) > 0 || len(baseInfo.AbsoluteEpisodes) > 0 || baseInfo.AirDate != "" {
		return matchesEpisode(baseInfo)
	}
	if len(fullInfo.Episodes) > 0 || len(fullInfo.AbsoluteEpisodes) > 0 || fullInfo.AirDate != "" {
		return matchesEpisode(fullInfo)
	}
	// A classic "Season 1/05 - Title.mkv" carries no episode Parse reads,
	// but its season folder still has to be the wanted season.
	if len(baseInfo.Seasons) > 0 {
		return slices.Contains(baseInfo.Seasons, season) || slices.Contains(baseInfo.Seasons, origSeason) ||
			p.Target != nil && slices.ContainsFunc(p.Target.Aliases, func(n core.EpisodeNumber) bool { return slices.Contains(baseInfo.Seasons, n.Season) })
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
	}
	idMatch, idConflict := releaseparse.CompareTitleIDs(rel.ReportedTitleIDs(), expected)
	if idConflict {
		return false
	}
	if title.Type != core.TypeEpisode {
		return true
	}
	season, episode := title.Season, title.Episode
	origSeason, origEpisode := season, episode
	absolute := playbackAbsoluteEpisode(title, season, episode)
	var valid bool
	season, episode, valid = targetEpisodeNumbers(rel.Target, season, episode)
	if !valid {
		return false
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
	if len(info.Episodes) == 0 && !info.FullSeason && info.AirDate == "" {
		info = releaseparse.Parse(rel.Name)
	}
	if episode > 0 && (len(info.Episodes) > 0 || len(info.AbsoluteEpisodes) > 0 || info.FullSeason || info.AirDate != "") {
		if info.FullSeason {
			if len(info.Seasons) > 0 {
				found := false
				for _, n := range info.Seasons {
					found = found || n == season
				}
				if !found && rel.Target != nil && rel.Target.CanonicalSeason == origSeason && rel.Target.CanonicalEpisode == origEpisode {
					for _, alias := range rel.Target.Aliases {
						for _, n := range info.Seasons {
							found = found || n == alias.Season
						}
					}
				}
				if !found {
					return false
				}
			}
		} else {
			matched := releaseparse.EpisodeMatchesNumbering(info, season, episode, absolute)
			if !matched && !releaseparse.AbsoluteConflict(info, absolute) && rel.Target != nil && rel.Target.CanonicalSeason == origSeason && rel.Target.CanonicalEpisode == origEpisode {
				for _, alias := range rel.Target.Aliases {
					if releaseparse.EpisodeMatches(info, alias.Season, alias.Episode) {
						matched = true
						break
					}
				}
			}
			if !matched && season > 0 && !releaseparse.AbsoluteConflict(info, absolute) && releaseparse.EpisodeMatchesAirDate(info, cmp.Or(title.EpisodeAirDate, title.Released)) {
				if len(info.Episodes) == 0 && len(info.AbsoluteEpisodes) == 0 {
					matched = true
				} else if season > 1 && absolute <= 0 && len(info.Seasons) == 0 && len(info.AbsoluteEpisodes) == 0 && len(info.Episodes) == 1 && info.Episodes[0] > episode {
					matched = true
				}
			}
			if !matched {
				return false
			}
		}
	}
	if rel.Target != nil && episode > 0 &&
		(rel.Target.Episode != episode || rel.Target.Season != season) {
		return false
	}
	year := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
	// A dated TVDB release can identify itself with a series name and
	// broadcast date even when its filename omits an episode title.
	daily := title.EpisodeOrder.Source == core.MetadataTVDB && info.AirDate != "" &&
		len(info.Episodes) == 0 && len(info.AbsoluteEpisodes) == 0 && !info.FullSeason
	dailyMatch := daily && releaseparse.EpisodeMatchesAirDate(info, cmp.Or(title.EpisodeAirDate, title.Released)) &&
		(releaseparse.TitleMatchesAny(info, title.ShowTitle, title.ShowAlternativeTitles) ||
			releaseparse.TitleMatchesAny(info, title.ShowOriginalTitle, []string{title.ShowEnglishTitle}))
	if (daily && !idMatch && !dailyMatch) || !releaseparse.SeriesYearMatches(year, title.ShowYear) ||
		((season != 0 || episode <= 0) && year == 0 && !idMatch && !dailyMatch && !releaseparse.EpisodeTitleMatches(rel.Name, title.Title)) {
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

// An owner pin is checked at use time so warm entries, persisted resume picks
// and old alternates cannot bypass a pin installed after they were created.
func (m *Manager) ownerPinAllows(s *session, releaseID string) bool {
	if pins, ok := m.deps.Resolver.(interface {
		PinnedReleaseID(core.ResolveRequest) string
	}); ok {
		id := pins.PinnedReleaseID(core.ResolveRequest{Title: s.title, Client: s.caps, User: &s.user, Prefer: s.prefer})
		return id == "" || id == releaseID
	}
	return true
}

// preferredRelease is the owner's preferred release (a pin with fallback)
// that applies to this session, and whether it may be tried now. Unlike a
// strict pin it never forbids another release; it only orders the first try.
func (m *Manager) preferredRelease(s *session) (string, bool) {
	if pins, ok := m.deps.Resolver.(interface {
		PreferredRelease(core.ResolveRequest) (string, bool)
	}); ok && s != nil && s.title != nil {
		return pins.PreferredRelease(core.ResolveRequest{Title: s.title, Client: s.caps, User: &s.user, Prefer: s.preferenceSelector()})
	}
	return "", false
}

// preferenceSelector is the selector the owner's preference is looked up with:
// an Auto start that opened its release by id (bound as Auto) is still Auto,
// not an explicit pick of that release.
func (s *session) preferenceSelector() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding == core.SourceBindingAuto {
		return ""
	}
	return s.prefer
}

// notePreferenceLocked records the session's preference and, when the
// session plays another release, that it fell back from it for good.
// Callers hold s.mu.
func (s *session) notePreferenceLocked(pref, playing string) {
	if s.preference != pref {
		s.preference, s.preferenceFailed = pref, false
	}
	if s.preference != "" && playing != "" && playing != s.preference {
		s.preferenceFailed = true
		if !slices.Contains(s.tried, s.preference) {
			s.tried = append(s.tried, s.preference)
		}
	}
}

// untriedPreference is the eligible preferred release this session has not
// tried yet, or "". While it is set, warm and remembered sources and cached
// alternates give way so a Resolve tries the preference first. A preference
// the session tried, or fell back from, stays tried for the whole session.
func (m *Manager) untriedPreference(s *session, tried []string) string {
	id, eligible := m.preferredRelease(s)
	if id == "" || !eligible || slices.Contains(tried, id) {
		return ""
	}
	s.mu.Lock()
	done := slices.Contains(s.tried, id) || (s.preferenceFailed && s.preference == id)
	s.mu.Unlock()
	if done {
		return ""
	}
	return id
}

// preferenceGate orders stored sources around the preferred release: while it
// is untried only it may be used, and while it is inside its failure window it
// is not reused from warm, resume or cached alternates.
type preferenceGate struct{ untried, failed string }

func (g preferenceGate) blocks(releaseID string) bool {
	return (g.untried != "" && releaseID != g.untried) || (g.failed != "" && releaseID == g.failed)
}

func (m *Manager) preferenceGate(s *session, tried []string) preferenceGate {
	if id, eligible := m.preferredRelease(s); id != "" && !eligible {
		return preferenceGate{failed: id}
	}
	return preferenceGate{untried: m.untriedPreference(s, tried)}
}

// preferenceAllows reports whether releaseID may be used without first
// trying an eligible, untried preferred release, and is not a preference
// inside its failure window.
func (m *Manager) preferenceAllows(s *session, releaseID string, tried []string) bool {
	return !m.preferenceGate(s, tried).blocks(releaseID)
}

// ownerChoiceCovers reports whether releaseID plays under an owner pin's
// exemptions: any release while a strict pin applies (nothing else can open),
// or the preferred release itself when it is the one being used.
func (m *Manager) ownerChoiceCovers(s *session, releaseID string) bool {
	if !m.ownerPinAllows(s, "") {
		return true
	}
	id, _ := m.preferredRelease(s)
	return id != "" && id == releaseID
}

// Alternate file numbers are valid only for the canonical episode the resolver
// explicitly attached them to. Plain targets retain the existing checks.
func targetEpisodeNumbers(target *core.ReleaseTarget, season, episode int) (int, int, bool) {
	if target == nil || target.CanonicalEpisode <= 0 {
		return season, episode, true
	}
	if target.CanonicalSeason != season || target.CanonicalEpisode != episode || target.Episode <= 0 {
		return season, episode, false
	}
	return target.Season, target.Episode, true
}
