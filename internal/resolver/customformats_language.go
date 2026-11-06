package resolver

import (
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/dlclark/regexp2"
)

// originalToken is the pseudo-language Servarr's parser adds for dual
// language releases before the title's original language replaces it.
const originalToken = "original"

var (
	// Radarr/Sonarr LanguageParser: GermanDualLanguageRegex and
	// GermanMultiLanguageRegex.
	germanDualRe  = regexp2.MustCompile(`(?<!WEB[-_. ]?)\bDL\b`, regexp2.IgnoreCase)
	germanMultiRe = regexp2.MustCompile(`\bML\b`, regexp2.IgnoreCase)
)

// formatLanguages is the language list custom-format language rules see,
// built the way Radarr and Sonarr's AggregateLanguages builds
// RemoteMovie/RemoteEpisode.Languages (Download/Aggregation/Aggregators/
// AggregateLanguages.cs and Parser/LanguageParser.cs):
//
//  1. "multi" and "dual" are release tags, not languages.
//  2. A release whose only language is German gets the original language
//     added for ".DL", and the original language plus English for ".ML".
//  3. Languages that appear only inside the title's own name are dropped.
//  4. With no language left, the title's original language is assumed.
//  5. "Original" is replaced by the title's original language.
//
// The original language is English when the title's is unknown.
func formatLanguages(rel core.Release, t *core.Title) []string {
	original := assumedLanguage(t)
	var langs []string
	for _, l := range rel.Info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual || l == "" {
			continue
		}
		if !slices.Contains(langs, l) {
			langs = append(langs, l)
		}
	}
	if len(langs) == 1 && langs[0] == "german" {
		if ok, _ := germanDualRe.MatchString(rel.Name); ok {
			langs = append(langs, originalToken)
		} else if ok, _ := germanMultiRe.MatchString(rel.Name); ok {
			langs = append(langs, originalToken, "english")
		}
	}
	if t != nil && t.Title != "" {
		langs = dropTitleLanguages(langs, rel.Name, t.Title)
	}
	if len(langs) == 0 {
		langs = []string{original}
	}
	if i := slices.Index(langs, originalToken); i >= 0 {
		langs = slices.Delete(langs, i, i+1)
		if !slices.Contains(langs, original) {
			langs = append(langs, original)
		}
	}
	return langs
}

// dropTitleLanguages removes languages named only by the title's own words
// ("The German Doctor"), as Servarr does when the normalized title occurs in
// the release name.
func dropTitleLanguages(langs []string, name, title string) []string {
	titleLangs := releaseparse.Parse(title).Languages
	if len(titleLangs) == 0 {
		return langs
	}
	nName, nTitle := releaseparse.NormalizeTitle(name), releaseparse.NormalizeTitle(title)
	i := strings.Index(nName, nTitle)
	if nTitle == "" || i < 0 {
		return langs
	}
	rest := releaseparse.Parse(nName[:i] + " " + nName[i+len(nTitle):]).Languages
	var out []string
	for _, l := range langs {
		if slices.Contains(titleLangs, l) && !slices.Contains(rest, l) {
			continue
		}
		out = append(out, l)
	}
	return out
}
