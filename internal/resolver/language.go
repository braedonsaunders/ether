package resolver

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// catalogLanguage is the server's language: the catalogue metadata language.
func (s *Service) catalogLanguage() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.catalogLang
}

// wantedAudio lists the viewer's own preferred audio languages: the profile's
// languages when the owner set them, otherwise the viewer's own audio
// languages, otherwise the server's catalogue language.
func wantedAudio(profile core.QualityProfile, user *core.User, serverLang string) []string {
	var out []string
	switch {
	case len(profile.PreferredLanguages) > 0:
		out = append(out, profile.PreferredLanguages...)
	case user != nil && len(user.AudioLanguages) > 0:
		out = append(out, user.AudioLanguages...)
	case strings.TrimSpace(serverLang) != "":
		base, _, _ := strings.Cut(strings.TrimSpace(serverLang), "-")
		out = append(out, base)
	}
	return out
}

// acceptedAudio lists the audio languages a release may declare and still
// rank with the rest: the profile's languages when the owner set them,
// otherwise the viewer's own audio languages, otherwise the server's
// language; the title's original language is always accepted. An empty
// result accepts everything (nothing is known to ask for).
func acceptedAudio(profile core.QualityProfile, user *core.User, serverLang string, t *core.Title) []string {
	out := wantedAudio(profile, user, serverLang)
	if len(out) == 0 {
		return nil
	}
	if lang := titleOriginalLanguage(t); lang != "" {
		out = append(out, lang)
	}
	return out
}

// audioAcceptable reports whether a release's declared audio fits the
// accepted languages. A release that declares nothing is in the title's
// original language as far as anyone knows, and MULTI / DUAL carry more
// than one track (including the original language), so all three are
// acceptable; so is a regional variant of an accepted language (Castilian
// for a Latino preference).
func audioAcceptable(info core.ReleaseInfo, accepted []string) bool {
	if len(accepted) == 0 || len(info.Languages) == 0 {
		return true
	}
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			return true
		}
		for _, want := range accepted {
			if releaseparse.LanguageMatch(l, want) != releaseparse.LanguageNoMatch {
				return true
			}
		}
	}
	return false
}

// foreignTitleWantedAudio reports whether a release of a non-wanted-language
// title explicitly declares the viewer's wanted audio language (or MULTI).
// When the title's original language is already one of the viewer's wanted
// languages (or unknown), untagged releases are already in the viewer's
// language and this returns false for all candidates so their relative order
// is unchanged. On a foreign-language title (e.g. a Spanish film for an
// English viewer), releases that declare English or MULTI are ranked ahead of
// untagged original-language or bare .DUAL releases within the same resolution
// tier.
func foreignTitleWantedAudio(info core.ReleaseInfo, wanted []string, t *core.Title) bool {
	orig := titleOriginalLanguage(t)
	if orig == "" || len(wanted) == 0 {
		return false
	}
	for _, w := range wanted {
		if releaseparse.LanguageMatch(orig, w) != releaseparse.LanguageNoMatch {
			return false
		}
	}
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti {
			return true
		}
		if l == releaseparse.LanguageDual {
			continue
		}
		for _, w := range wanted {
			if releaseparse.LanguageMatch(l, w) != releaseparse.LanguageNoMatch {
				return true
			}
		}
	}
	return false
}

// localizedEpisodeTitle reports a release dubbed into a language that is
// neither the catalogue's nor the title's original: its name carries the
// episode title in that language ("S06E06.La.resa.dei.conti.ITA.ENG"), so
// a different title there is a translation, not another episode.
func localizedEpisodeTitle(info core.ReleaseInfo, t *core.Title, catalogLang string) bool {
	base, _, _ := strings.Cut(strings.TrimSpace(catalogLang), "-")
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			continue
		}
		if base != "" && releaseparse.LanguageMatch(l, base) != releaseparse.LanguageNoMatch {
			continue
		}
		if orig := titleOriginalLanguage(t); orig != "" && releaseparse.LanguageMatch(l, orig) != releaseparse.LanguageNoMatch {
			continue
		}
		return true
	}
	return false
}

// matchesAlternativeTitle reports a release named by another of the title's
// names: a romanized original ("Sousou.no.Frieren"), a regional or working
// title. The year, episode and identifier checks still apply.
func matchesAlternativeTitle(info core.ReleaseInfo, alts []string) bool {
	for _, alt := range alts {
		if releaseparse.TitleMatches(info, alt) {
			return true
		}
	}
	return false
}
