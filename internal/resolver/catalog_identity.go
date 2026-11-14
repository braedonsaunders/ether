package resolver

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

type localIdentity struct{ core.EpisodeIdentity }

// Resolve only names actually used by this result set. The catalogue callback
// must be local and indexed; missing data preserves the existing identity gates.
func (s *Service) localEpisodeIdentity(ctx context.Context, t *core.Title, req core.SearchRequest, results []core.Release) localIdentity {
	if s.episodeIdentity == nil || t == nil || t.ShowKey == 0 || req.Type != core.TypeEpisode {
		return localIdentity{}
	}
	names := []string{req.Title}
	if t.ShowOriginalTitle != "" && t.ShowOriginalTitle != req.Title {
		names = append(names, t.ShowOriginalTitle)
	}
	for _, rel := range results {
		if len(names) >= 8 {
			break
		}
		if !matchesAlternativeTitle(rel.Info, req.AlternativeTitles) && !matchesAlternativeTitle(rel.Info, t.ShowAlternativeTitles) {
			continue
		}
		duplicate := false
		for _, name := range names {
			if releaseparse.TitlesMatch(rel.Info.Title, name) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			names = append(names, rel.Info.Title)
		}
	}
	facts, err := s.episodeIdentity(ctx, t.ShowKey, req.Season, req.Episode, names)
	if err != nil {
		s.log.Debug("local episode identity unavailable", "show", t.ShowKey, "err", err)
		return localIdentity{}
	}
	return localIdentity{facts}
}

func (f localIdentity) shared(req core.SearchRequest) bool {
	return len(f.OtherShows[req.Title]) > 0 || (req.OriginalTitle != "" && len(f.OtherShows[req.OriginalTitle]) > 0)
}

// Phrase matching keeps word boundaries and ignores release punctuation. Short
// or generic episode names cannot establish a competing show's identity.
func identityWords(s string) string {
	s = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(s))
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
}
func identityPhrase(name, title string) bool {
	phrase := identityWords(title)
	return phrase != "" && strings.Contains(" "+identityWords(name)+" ", " "+phrase+" ")
}
func distinctiveEpisode(name, title string) bool {
	words := identityWords(title)
	if len([]rune(words)) < 5 || strings.HasPrefix(words, "episode ") || strings.HasPrefix(words, "chapter ") {
		return false
	}
	return identityPhrase(name, title)
}

var identityYear = regexp.MustCompile(`\b(?:19|20)[0-9]{2}\b`)
var genericSeasonName = regexp.MustCompile(`(?i)^(season|series|part|book|volume|vol|specials?|extras?)(\s+[0-9ivxlcdm]+)?$`)

func (f localIdentity) reject(rel core.Release, t *core.Title, req core.SearchRequest) string {
	if req.Type != core.TypeEpisode {
		return ""
	}
	if reason := f.otherSeason(rel, t, req); reason != "" {
		return reason
	}
	idMatch, _ := releaseparse.CompareTitleIDs(rel.ReportedTitleIDs(), req.IDs)
	year := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
	primary := releaseparse.TitleMatches(rel.Info, req.Title) || (t.ShowOriginalTitle != "" && releaseparse.TitleMatches(rel.Info, t.ShowOriginalTitle))
	for name, others := range f.OtherShows {
		if !releaseparse.TitleMatches(rel.Info, name) {
			continue
		}
		if !primary && year == 0 && !idMatch && len(others) > 0 {
			return "alternative title names another show"
		}
		for _, other := range others {
			if other.Year > 0 && !releaseparse.SeriesYearMatches(other.Year, req.Year) {
				for _, raw := range identityYear.FindAllString(rel.Name, -1) {
					y, _ := strconv.Atoi(raw)
					// An episode's air year is not necessarily the show's premiere year.
					if y == other.Year && (req.Released.IsZero() || (y < req.Released.Year()-1 || y > req.Released.Year()+1)) {
						return "release names another show's premiere year"
					}
				}
			}
			if distinctiveEpisode(rel.Name, other.EpisodeTitle) && releaseparse.EpisodeTitleEvidence(rel.Name, other.EpisodeTitle) && !distinctiveEpisode(rel.Name, req.EpisodeTitle) && identityWords(other.EpisodeTitle) != identityWords(req.EpisodeTitle) {
				return "release names another show's episode"
			}
		}
	}
	return ""
}

func (f localIdentity) otherSeason(rel core.Release, t *core.Title, req core.SearchRequest) string {
	if req.Season <= 0 {
		return ""
	}
	meaningful := func(name string) bool {
		norm := identityWords(name)
		if len(strings.Fields(norm)) < 2 || genericSeasonName.MatchString(norm) {
			return false
		}
		for _, show := range append([]string{req.Title, t.ShowOriginalTitle}, t.ShowAlternativeTitles...) {
			if releaseparse.NormalizeTitle(name) == releaseparse.NormalizeTitle(show) {
				return false
			}
		}
		return true
	}
	own := f.Seasons[req.Season]
	for season, name := range f.Seasons {
		if season <= 0 || season == req.Season || !meaningful(name) {
			continue
		}
		// A longer own-season name can contain a shorter sibling name.
		if meaningful(own) && identityPhrase(own, name) && identityPhrase(rel.Info.Title, own) {
			continue
		}
		if identityPhrase(rel.Info.Title, name) {
			return "release names another season"
		}
	}
	return ""
}

func (f localIdentity) ambiguous(rel core.Release, req core.SearchRequest) bool {
	if !f.shared(req) || !(releaseparse.TitleMatches(rel.Info, req.Title) || (req.OriginalTitle != "" && releaseparse.TitleMatches(rel.Info, req.OriginalTitle))) {
		return false
	}
	// IDs, a matching series year, or the actual episode title identify the
	// show. A fansub tag or bare absolute number alone does not prove identity.
	return !episodeIdentityEvidence(rel, req)
}

func clearIdentityVariants(cands []candidate) []candidate {
	clear := false
	for _, c := range cands {
		if !c.identityWeak {
			clear = true
			break
		}
	}
	if !clear {
		return cands
	}
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if !c.identityWeak {
			out = append(out, c)
		}
	}
	return out
}
