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

// acceptedAudio lists the audio languages a release may declare and still
// rank with the rest: the profile's languages when the owner set them,
// otherwise the viewer's own audio languages, otherwise the server's
// language; the title's original language is always accepted. An empty
// result accepts everything (nothing is known to ask for).
func acceptedAudio(profile core.QualityProfile, user *core.User, serverLang string, t *core.Title) []string {
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
// original language as far as anyone knows, and MULTI carries more than
// one track, so both are acceptable; so is a regional variant of an
// accepted language (Castilian for a Latino preference).
func audioAcceptable(info core.ReleaseInfo, accepted []string) bool {
	if len(accepted) == 0 || len(info.Languages) == 0 {
		return true
	}
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti {
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
