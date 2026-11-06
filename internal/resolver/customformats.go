package resolver

import (
	"encoding/json"
	"errors"
	"fmt"

	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/dlclark/regexp2"
)

// errFormatTimeout reports a custom format regex that exceeded its time
// budget on one release name.
var errFormatTimeout = errors.New("regex timed out")

type FormatMatch struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Score int    `json:"score"`
	// revision marks a format that recognises Repack/Proper release titles;
	// the ranking then skips Ether's own built-in revision bonus.
	revision bool
}

type compiledSpec struct {
	def core.FormatSpecification
	re  *regexp2.Regexp
	// sonarr reads SourceSpecification values in Sonarr's enum.
	sonarr bool
}
type compiledFormat struct {
	def      core.CustomFormat
	specs    []compiledSpec
	revision bool
}
type FormatScorer struct {
	formats []compiledFormat
	scores  map[string]int
}

// maxFormatSpecs bounds one format's specifications.
const maxFormatSpecs = 500

// NewFormatScorer validates every rule before publishing an immutable scorer.
// .NET lookarounds used by TRaSH are supported; regex execution is time bounded.
func NewFormatScorer(defs []core.CustomFormat, scores map[string]int) (*FormatScorer, error) {
	if len(defs) > 1000 {
		return nil, fmt.Errorf("custom formats: maximum 1000 definitions")
	}
	s := &FormatScorer{scores: map[string]int{}}
	for k, v := range scores {
		s.scores[k] = v
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if d.ID == "" {
			d.ID = d.Name
		}
		switch {
		case d.Name == "":
			return nil, fmt.Errorf("custom format %q: missing name", d.ID)
		case seen[d.ID]:
			return nil, fmt.Errorf("custom format %q: duplicate id %q", d.Name, d.ID)
		case len(d.Specifications) == 0:
			return nil, fmt.Errorf("custom format %q: no specifications", d.Name)
		case len(d.Specifications) > maxFormatSpecs:
			// TRaSH's largest (Anime LQ Groups) has 137.
			return nil, fmt.Errorf("custom format %q: more than %d specifications", d.Name, maxFormatSpecs)
		}
		seen[d.ID] = true
		if d.App != "" && d.App != AppRadarr && d.App != AppSonarr {
			return nil, fmt.Errorf("custom format %q: app must be radarr or sonarr", d.Name)
		}
		maxSource := 9 // Radarr's BLURAY
		if d.App == AppSonarr {
			maxSource = 7 // Sonarr's BlurayRaw
		}
		f := compiledFormat{def: d}
		for _, spec := range d.Specifications {
			c := compiledSpec{def: spec, sonarr: d.App == AppSonarr}
			switch spec.Implementation {
			case "ReleaseTitleSpecification", "ReleaseGroupSpecification", "EditionSpecification":
				var pattern string
				if json.Unmarshal(spec.Fields["value"], &pattern) != nil || pattern == "" || len(pattern) > 16384 {
					return nil, fmt.Errorf("custom format %q: invalid regex", d.Name)
				}
				var err error
				c.re, err = regexp2.Compile(pattern, regexp2.IgnoreCase)
				if err != nil {
					return nil, fmt.Errorf("custom format %q: %w", d.Name, err)
				}
				c.re.MatchTimeout = 25 * time.Millisecond
			case "ResolutionSpecification", "SourceSpecification", "QualityModifierSpecification", "SizeSpecification", "LanguageSpecification":
				if spec.Implementation == "SizeSpecification" {
					for _, key := range []string{"min", "max"} {
						if raw, ok := spec.Fields[key]; ok {
							var value float64
							if json.Unmarshal(raw, &value) != nil || value < 0 {
								return nil, fmt.Errorf("custom format %q: invalid size bound", d.Name)
							}
						}
					}
				} else {
					var value int
					if json.Unmarshal(spec.Fields["value"], &value) != nil {
						return nil, fmt.Errorf("custom format %q: numeric native specification value required", d.Name)
					}
					if (spec.Implementation == "LanguageSpecification" && !knownLanguageValue(value)) || (spec.Implementation == "SourceSpecification" && (value < 0 || value > maxSource)) || (spec.Implementation == "QualityModifierSpecification" && (value < 0 || value > 5)) {
						return nil, fmt.Errorf("custom format %q: unsupported native enum value %d", d.Name, value)
					}
				}
			default:
				return nil, fmt.Errorf("custom format %q: unsupported specification %q", d.Name, spec.Implementation)
			}
			f.specs = append(f.specs, c)
		}
		f.revision = recognisesRevision(f.specs)
		s.formats = append(s.formats, f)
	}
	return s, nil
}

// assumedLanguage names a title's original language (ISO 639-1) the way
// release language tags are named; English when unknown.
func assumedLanguage(t *core.Title) string {
	if name := originalLanguage(t); name != "" {
		return name
	}
	return "english"
}

// originalLanguage names a title's known original language the way release
// language tags are named, or "" when unknown.
func originalLanguage(t *core.Title) string {
	if t == nil || t.Language == "" {
		return ""
	}
	return releaseparse.LanguageName(t.Language)
}

func fieldNumber(s core.FormatSpecification, k string) float64 {
	var v float64
	_ = json.Unmarshal(s.Fields[k], &v)
	return v
}
func specMatch(s compiledSpec, r core.Release, original string) (bool, error) {
	value := fieldNumber(s.def, "value")
	switch s.def.Implementation {
	case "ReleaseTitleSpecification":
		return s.re.MatchString(r.Name)
	case "ReleaseGroupSpecification":
		return s.re.MatchString(r.Info.Group)
	case "EditionSpecification":
		return s.re.MatchString(r.Info.Edition)
	case "ResolutionSpecification":
		return r.Info.Height == int(value), nil
	case "SourceSpecification":
		if s.sonarr {
			// Sonarr's source enum: Unknown=0, Television=1, TelevisionRaw=2,
			// Web=3, WebRip=4, DVD=5, Bluray=6, BlurayRaw=7 (remuxes).
			source := map[core.Source]int{core.SourceHDTV: 1, core.SourceWEBDL: 3, core.SourceWEBRip: 4, core.SourceDVD: 5, core.SourceBluRay: 6, core.SourceRemux: 7}[r.Info.Source]
			return source == int(value), nil
		}
		// Radarr's source enum: Unknown=0, CAM=1, TS=2, TC=3, WORKPRINT=4, DVD=5,
		// TV=6, WEB-DL=7, WEBRip=8, BluRay=9.
		source := map[core.Source]int{core.SourceCam: 1, core.SourceScreener: 5, core.SourceDVD: 5, core.SourceHDTV: 6, core.SourceWEBDL: 7, core.SourceWEBRip: 8, core.SourceBluRay: 9, core.SourceRemux: 9}[r.Info.Source]
		return source == int(value), nil
	case "QualityModifierSpecification":
		// Radarr's modifier enum: NONE=0, REGIONAL=1, SCREENER=2, RAWHD=3,
		// BRDISK=4, REMUX=5 (TRaSH's remux formats use 5).
		modifier := 0
		switch r.Info.Source {
		case core.SourceRemux:
			modifier = 5
		case core.SourceScreener:
			modifier = 2
		}
		return modifier == int(value), nil
	case "SizeSpecification":
		if r.Size <= 0 {
			return false, nil
		}
		gib := float64(r.Size) / (1 << 30)
		min, max := fieldNumber(s.def, "min"), fieldNumber(s.def, "max")
		return gib >= min && (max == 0 || gib <= max), nil
	case "LanguageSpecification":
		var name string
		_ = json.Unmarshal(s.def.Fields["value"], &name)
		if name == "" {
			name = starrLanguages[int(value)]
			if int(value) == languageOriginal {
				// Like Radarr, Original compares against the title's original
				// language when known; otherwise it matches no language.
				name = original
			}
		}
		var except bool
		_ = json.Unmarshal(s.def.Fields["exceptLanguage"], &except)
		name = formatLanguageName(name)
		for _, l := range r.Info.Languages {
			same := name != "" && strings.EqualFold(formatLanguageName(l), name)
			if same != except {
				return true, nil
			}
		}
		return false, nil
	}
	return false, nil
}

// Starr uses Spanish/Portuguese for both an unspecified region and the
// European variant, with separate enums for Latino/Brazilian. Keep those
// imported rules compatible while quality-profile preferences distinguish
// explicit es-ES and pt-PT from their region-neutral es and pt counterparts.
func formatLanguageName(name string) string {
	name = releaseparse.LanguageName(name)
	switch name {
	case releaseparse.LanguageSpanishSpain:
		return "spanish"
	case releaseparse.LanguagePortuguesePortugal:
		return "portuguese"
	}
	return name
}

// Radarr's language enum (NzbDrone.Core.Languages.Language), named as the
// release parser names languages. Like Radarr, Portuguese (Brazil) and
// Spanish (Latino) are languages of their own, distinct from Portuguese and
// Spanish. Unknown (0) and Any (-1) are valid values that name no language.
const (
	languageAny      = -1
	languageOriginal = -2
)

var starrLanguages = map[int]string{
	1: "english", 2: "french", 3: "spanish", 4: "german", 5: "italian", 6: "danish", 7: "dutch", 8: "japanese",
	9: "icelandic", 10: "chinese", 11: "russian", 12: "polish", 13: "vietnamese", 14: "swedish", 15: "norwegian",
	16: "finnish", 17: "turkish", 18: "portuguese", 19: "flemish", 20: "greek", 21: "korean", 22: "hungarian",
	23: "hebrew", 24: "lithuanian", 25: "czech", 26: "hindi", 27: "romanian", 28: "thai", 29: "bulgarian",
	30: releaseparse.LanguagePortugueseBrazil, 31: "arabic", 32: "ukrainian", 33: "persian", 34: "bengali", 35: "slovak", 36: "latvian",
	37: releaseparse.LanguageSpanishLatino, 38: "catalan", 39: "croatian", 40: "serbian", 41: "bosnian", 42: "estonian", 43: "tamil",
	44: "indonesian", 45: "telugu", 46: "macedonian", 47: "slovenian", 48: "malayalam", 49: "kannada",
	50: "albanian", 51: "afrikaans", 52: "marathi", 53: "tagalog", 54: "urdu", 55: "romansh", 56: "mongolian",
	57: "georgian", 58: "irish",
}

func knownLanguageValue(v int) bool {
	_, ok := starrLanguages[v]
	return ok || v == 0 || v == languageAny || v == languageOriginal
}

// Score mirrors Starr grouping: every specification type must match; within
// a type all required rules and at least one rule must match.
func (s *FormatScorer) Score(r core.Release) (int, []FormatMatch, error) {
	return s.ScoreWithScores(r, nil)
}

func (s *FormatScorer) ScoreWithScores(r core.Release, overrides map[string]int) (int, []FormatMatch, error) {
	return s.ScoreForTitle(r, overrides, "")
}

// ScoreForTitle scores r for a title whose original language is original
// (a release-parser language name, "" when unknown), which "Original"
// language specifications compare against.
func (s *FormatScorer) ScoreForTitle(r core.Release, overrides map[string]int, original string) (int, []FormatMatch, error) {
	return s.ScoreForProfile(r, overrides, original, false)
}

// ScoreForProfile is ScoreForTitle with Servarr's profile semantics when
// strict: a format the profile does not list contributes 0 (Radarr/Sonarr's
// QualityProfile.CalculateCustomFormatScore sums FormatItems only), instead of
// inheriting the global or guide default score. Imported profiles are strict;
// hand-made sparse maps keep inheriting.
func (s *FormatScorer) ScoreForProfile(r core.Release, overrides map[string]int, original string, strict bool) (int, []FormatMatch, error) {
	return s.scoreFormats(r, overrides, original, strict)
}

// scoreFor scores a release for a title in the profile's evaluation mode.
// Ether mode keeps Ether's behaviour exactly (an untagged release is in the
// title's original language, sparse profile maps inherit global scores, the
// raw release name is matched). Servarr mode builds the language list like
// AggregateLanguages, scores unlisted formats 0, and matches each app's own
// release-title form.
func (s *FormatScorer) scoreFor(rel core.Release, t *core.Title, overrides map[string]int, servarr bool) (int, []FormatMatch, error) {
	if servarr {
		rel.Info.Languages = formatLanguages(rel, t)
	} else if len(rel.Info.Languages) == 0 {
		rel.Info.Languages = []string{assumedLanguage(t)}
	}
	return s.scoreFormats(rel, overrides, originalLanguage(t), servarr)
}

// scoreFormats is the scoring loop; servarr selects Servarr's unlisted-format
// and release-title rules.
func (s *FormatScorer) scoreFormats(r core.Release, overrides map[string]int, original string, servarr bool) (int, []FormatMatch, error) {
	if s == nil {
		return 0, nil, nil
	}
	total := 0
	var matches []FormatMatch
	byApp := map[string]core.Release{}
	for _, f := range s.formats {
		in, cached := byApp[f.def.App]
		if !cached {
			in = r
			if servarr {
				in = titleInput(f.def.App, r)
			}
			byApp[f.def.App] = in
		}
		type group struct {
			requiredOK bool
			anyOK      bool
		}
		groups := map[string]*group{}
		for _, spec := range f.specs {
			g := groups[spec.def.Implementation]
			if g == nil {
				g = &group{requiredOK: true}
				groups[spec.def.Implementation] = g
			}
			match, err := specMatch(spec, in, original)
			if err != nil {
				return 0, nil, fmt.Errorf("custom format %q: %w", f.def.Name, errFormatTimeout)
			}
			if spec.def.Negate {
				match = !match
			}
			g.anyOK = g.anyOK || match
			if spec.def.Required {
				g.requiredOK = g.requiredOK && match
			}
		}
		ok := true
		for _, g := range groups {
			if !g.requiredOK || !g.anyOK {
				ok = false
			}
		}
		if !ok {
			continue
		}
		// Sparse owner maps inherit global scores for formats they do not override.
		score := f.def.Scores["default"]
		if v, ok := s.scores[f.def.ID]; ok {
			score = v
		}
		if servarr {
			// Radarr/Sonarr sum the profile's own FormatItems only.
			score = 0
		}
		if v, ok := overrides[f.def.ID]; ok {
			score = v
		}
		total += score
		matches = append(matches, FormatMatch{ID: f.def.ID, Name: f.def.Name, Score: score, revision: f.revision})
	}
	return total, matches, nil
}

// recognisesRevision reports whether a format has a non-negated release-title
// rule that matches a Repack or Proper tag (TRaSH's "Repack/Proper",
// "Repack2", "Repack3").
func recognisesRevision(specs []compiledSpec) bool {
	for _, c := range specs {
		if c.def.Implementation != "ReleaseTitleSpecification" || c.def.Negate || c.re == nil {
			continue
		}
		for _, probe := range []string{"Movie.2020.REPACK.1080p", "Movie.2020.PROPER.1080p", "Movie.2020.REPACK2.1080p", "Movie.2020.REPACK3.1080p"} {
			if ok, _ := c.re.MatchString(probe); ok {
				return true
			}
		}
	}
	return false
}
