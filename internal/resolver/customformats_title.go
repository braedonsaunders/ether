package resolver

import (
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Release-title input for ReleaseTitleSpecification, per Servarr app:
//
//   - Radarr matches ParsedMovieInfo.SimpleReleaseTitle (Parser.cs
//     ParseMovieTitle): the title without its file extension, with dashes and
//     underscores trimmed from the ends, 【】 turned into [], "<>?*|" removed,
//     and the parsed movie title replaced by "A.Movie" / "A Movie".
//   - Sonarr matches ParsedEpisodeInfo.ReleaseTitle: the title without its
//     file extension with 【】 turned into [].
//
// Both also OR the file name, which is empty for a search result.
var (
	servarrExtensionRe = regexp.MustCompile(`(?i)\.(mkv|mp4|m4v|avi|ts|m2ts|wmv|mov|iso|nzb|torrent|rar|zip|7z|par2|nfo|srt)$`)
	radarrJunkRe       = regexp.MustCompile(`\s*[<>?*|]`)
)

func servarrReleaseTitle(name string) string {
	name = servarrExtensionRe.ReplaceAllString(name, "")
	return strings.NewReplacer("【", "[", "】", "]").Replace(name)
}

// radarrSimpleReleaseTitle mirrors Radarr's SimpleReleaseTitle for a release
// whose parsed movie title is movie.
func radarrSimpleReleaseTitle(name, movie string) string {
	s := strings.Trim(servarrExtensionRe.ReplaceAllString(name, ""), "-_")
	s = strings.NewReplacer("【", "[", "】", "]").Replace(s)
	s = radarrJunkRe.ReplaceAllString(s, "")
	if words := strings.FieldsFunc(movie, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127)
	}); len(words) > 0 {
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = regexp.QuoteMeta(w)
		}
		re, err := regexp.Compile(`(?i)` + strings.Join(quoted, `[^a-zA-Z0-9]+`))
		if err == nil {
			if loc := re.FindStringIndex(s); loc != nil {
				repl := "A Movie"
				if strings.Contains(s[loc[0]:loc[1]], ".") {
					repl = "A.Movie"
				}
				s = s[:loc[0]] + repl + s[loc[1]:]
			}
		}
	}
	return s
}

// titleInputs gives, per app, the release a title rule sees.
func titleInput(app string, r core.Release) core.Release {
	switch app {
	case AppRadarr:
		r.Name = radarrSimpleReleaseTitle(r.Name, r.Info.Title)
	case AppSonarr:
		r.Name = servarrReleaseTitle(r.Name)
	}
	return r
}
