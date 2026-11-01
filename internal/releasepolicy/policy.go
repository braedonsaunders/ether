// Package releasepolicy evaluates the same bounded, declarative rules for
// playback, release search and unsaved previews. No client or UI dependencies.
package releasepolicy

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

type State string

const (
	Match   State = "match"
	Miss    State = "miss"
	Unknown State = "unknown"
)

type Explanation struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Action  string `json:"action"`
	State   State  `json:"state"`
	Outcome string `json:"outcome"` // pass, exclude, prefer, fallback, skip, unknown
}
type Result struct {
	Rejected     bool          `json:"rejected"`
	Unknown      bool          `json:"unknown"`
	Fallback     int           `json:"fallback"`
	Preferences  []int         `json:"preferences"`
	Explanations []Explanation `json:"explanations"`
}

// PreferRule returns 1 when the prefer rule with id matched, 0 otherwise.
func (r Result) PreferRule(id string) int {
	for i, e := range r.Explanations {
		if e.ID == id {
			if i < len(r.Preferences) {
				return r.Preferences[i]
			}
			if e.Action == "prefer" && e.Outcome == "prefer" {
				return 1
			}
			return 0
		}
	}
	return 0
}

// Context is evidence that belongs to the title, not the release.
// OriginalLanguage is the catalogue title's original language
// (Title.Language) when the caller has that title. An empty value means
// the comparison is unavailable. It is never a guessed language, and it
// is not part of the profile selection hash or the search cache key.
type Context struct {
	OriginalLanguage string
}
type condition struct {
	core.ReleaseCondition
	re *regexp.Regexp
}
type rule struct {
	source                       core.ReleaseRule
	conditions, unless, fallback []condition
}
type Policy struct{ rules []rule }

func Compile(rules []core.ReleaseRule) (*Policy, error) {
	if len(rules) > 32 {
		return nil, fmt.Errorf("at most 32 release preferences are allowed")
	}
	p := &Policy{}
	ids := map[string]bool{}
	for i, r := range rules {
		if r.ID == "" || len(r.ID) > 80 || ids[r.ID] {
			return nil, fmt.Errorf("preference %d needs a unique ID", i+1)
		}
		ids[r.ID] = true
		if strings.TrimSpace(r.Name) == "" || len(r.Name) > 160 {
			return nil, fmt.Errorf("preference %d needs a name of at most 160 characters", i+1)
		}
		if r.Action != "require" && r.Action != "prefer" && r.Action != "avoid" {
			return nil, fmt.Errorf("%s: unsupported action", r.Name)
		}
		if r.Match != "" && r.Match != "all" && r.Match != "any" {
			return nil, fmt.Errorf("%s: choose all or any conditions", r.Name)
		}
		if len(r.Conditions) == 0 {
			return nil, fmt.Errorf("%s: add a condition", r.Name)
		}
		if len(r.Fallback) > 0 && r.Action != "require" {
			return nil, fmt.Errorf("%s: only required preferences can have fallbacks", r.Name)
		}
		cr := rule{source: r}
		var err error
		for _, part := range []struct {
			in  []core.ReleaseCondition
			out *[]condition
		}{{r.Conditions, &cr.conditions}, {r.Unless, &cr.unless}, {r.Fallback, &cr.fallback}} {
			if len(part.in) > 16 {
				return nil, fmt.Errorf("%s: at most 16 conditions per group", r.Name)
			}
			for _, c := range part.in {
				cc, e := compileCondition(c)
				if e != nil {
					err = fmt.Errorf("%s: %w", r.Name, e)
					break
				}
				*part.out = append(*part.out, cc)
			}
			if err != nil {
				return nil, err
			}
		}
		p.rules = append(p.rules, cr)
	}
	return p, nil
}
func compileCondition(c core.ReleaseCondition) (condition, error) {
	out := condition{ReleaseCondition: c}
	numeric := c.Field == "height" || c.Field == "size_gib"
	switch c.Field {
	case "audio_matches_original":
		if c.Operator != "" && c.Operator != "equals" {
			return out, fmt.Errorf("unsupported comparison %q", c.Operator)
		}
		if strings.TrimSpace(c.Value) != "" {
			return out, fmt.Errorf("original-language audio takes no value")
		}
		return out, nil
	case "language", "subtitles", "name", "group", "source", "video", "audio", "height", "size_gib", "indexer", "protocol", "edition", "hdr":
	default:
		return out, fmt.Errorf("unsupported field %q", c.Field)
	}
	if numeric {
		if c.Operator != "range" || math.IsNaN(c.Min) || math.IsNaN(c.Max) || math.IsInf(c.Min, 0) || math.IsInf(c.Max, 0) || c.Min < 0 || c.Max < 0 || (c.Max > 0 && c.Max < c.Min) {
			return out, fmt.Errorf("invalid numeric range")
		}
		return out, nil
	}
	if strings.TrimSpace(c.Value) == "" || len(c.Value) > 512 {
		return out, fmt.Errorf("condition needs a value of at most 512 characters")
	}
	switch c.Operator {
	case "equals", "contains":
	case "regex":
		re, err := regexp.Compile("(?i)" + c.Value)
		if err != nil {
			return out, fmt.Errorf("invalid expression: %w", err)
		}
		out.re = re
	default:
		return out, fmt.Errorf("unsupported comparison %q", c.Operator)
	}
	if (c.Field == "language" || c.Field == "subtitles") && c.Operator != "equals" {
		return out, fmt.Errorf("languages use exact matching; use filename for expressions")
	}
	return out, nil
}
func (p *Policy) Evaluate(rel core.Release, ctx Context) Result {
	out := Result{Preferences: []int{}, Explanations: []Explanation{}}
	for _, r := range p.rules {
		if r.source.Disabled {
			continue
		}
		e := Explanation{ID: r.source.ID, Name: r.source.Name, Action: r.source.Action, State: group(r.conditions, r.source.Match, rel, ctx), Outcome: "pass"}
		if len(r.unless) > 0 && group(r.unless, "any", rel, ctx) == Match {
			e.Outcome = "skip"
			out.Preferences = append(out.Preferences, 0)
			out.Explanations = append(out.Explanations, e)
			continue
		}
		pref := 0
		switch r.source.Action {
		case "require":
			if e.State != Match {
				if len(r.fallback) > 0 && group(r.fallback, "any", rel, ctx) == Match {
					out.Fallback++
					e.Outcome = "fallback"
				} else {
					out.Rejected = true
					e.Outcome = "exclude"
					if e.State == Unknown {
						out.Unknown = true
						e.Outcome = "unknown"
					}
				}
			}
		case "avoid":
			if e.State == Match {
				out.Rejected = true
				e.Outcome = "exclude"
			} else if e.State == Unknown {
				out.Rejected = true
				out.Unknown = true
				e.Outcome = "unknown"
			}
		case "prefer":
			if e.State == Match {
				pref = 1
				e.Outcome = "prefer"
			} else if e.State == Unknown {
				e.Outcome = "unknown"
			}
		}
		out.Preferences = append(out.Preferences, pref)
		out.Explanations = append(out.Explanations, e)
	}
	return out
}

// Better gives explicit fallback tiers precedence, then ordered preferences.
// Equal policy results leave TRaSH, health and source routing order untouched.
func Better(a, b Result) bool {
	if a.Fallback != b.Fallback {
		return a.Fallback < b.Fallback
	}
	for i := 0; i < len(a.Preferences) && i < len(b.Preferences); i++ {
		if a.Preferences[i] != b.Preferences[i] {
			return a.Preferences[i] > b.Preferences[i]
		}
	}
	return false
}

// BetterByRanking compares policy results using the rule:<id> order in ranking,
// falling back to Better when ranking carries no rule entries.
func BetterByRanking(a, b Result, ranking []string) bool {
	if a.Fallback != b.Fallback {
		return a.Fallback < b.Fallback
	}
	hasRules := false
	for _, step := range ranking {
		if id, ok := strings.CutPrefix(step, core.RankRulePrefix); ok && id != "" {
			hasRules = true
			if pa, pb := a.PreferRule(id), b.PreferRule(id); pa != pb {
				return pa > pb
			}
		}
	}
	if !hasRules {
		return Better(a, b)
	}
	return false
}

// Equivalent reports the same fallback tier and the same ordered
// preference vector. Health racing may overtake only an equivalent
// candidate; a fallback must not pass a still-valid primary.
func Equivalent(a, b Result) bool {
	if a.Fallback != b.Fallback || len(a.Preferences) != len(b.Preferences) {
		return false
	}
	for i := range a.Preferences {
		if a.Preferences[i] != b.Preferences[i] {
			return false
		}
	}
	return true
}

// Conflict describes required rules that cannot be true of one release.
// Empty means no such conflict. Disabled rules, exceptions and fallbacks
// are left alone: those can still be satisfied.
func Conflict(rules []core.ReleaseRule) string {
	type need struct{ name, field, value string }
	var needs []need
	for _, r := range rules {
		if r.Disabled || r.Action != "require" || r.Match == "any" || len(r.Unless) > 0 || len(r.Fallback) > 0 {
			continue
		}
		seen := map[string]string{}
		var fields []string
		for _, c := range r.Conditions {
			if !singleValueField(c.Field) || c.Operator != "equals" {
				continue
			}
			if prev, ok := seen[c.Field]; ok && !strings.EqualFold(prev, c.Value) {
				return fmt.Sprintf("required preference %q cannot match: %s would have to equal both %q and %q", r.Name, c.Field, prev, c.Value)
			}
			if _, ok := seen[c.Field]; !ok {
				fields = append(fields, c.Field)
			}
			seen[c.Field] = c.Value
		}
		for _, field := range fields {
			needs = append(needs, need{r.Name, field, seen[field]})
		}
	}
	for i := range needs {
		for j := i + 1; j < len(needs); j++ {
			if needs[i].field == needs[j].field && !strings.EqualFold(needs[i].value, needs[j].value) {
				return fmt.Sprintf("required preferences %q and %q cannot match the same release: %s would have to equal both %q and %q", needs[i].name, needs[j].name, needs[i].field, needs[i].value, needs[j].value)
			}
		}
	}
	return ""
}

func singleValueField(field string) bool {
	switch field {
	case "group", "source", "video", "audio", "indexer", "protocol", "edition":
		return true
	default:
		return false
	}
}

// UsesOriginalAudio reports whether evaluation of these rules needs
// the catalogue original language. Disabled rules do not.
func UsesOriginalAudio(rules []core.ReleaseRule) bool {
	for _, r := range rules {
		if r.Disabled {
			continue
		}
		for _, group := range [][]core.ReleaseCondition{r.Conditions, r.Unless, r.Fallback} {
			for _, c := range group {
				if c.Field == "audio_matches_original" {
					return true
				}
			}
		}
	}
	return false
}

func group(cs []condition, mode string, r core.Release, ctx Context) State {
	unknown := false
	for _, c := range cs {
		s := c.evaluate(r, ctx)
		if mode == "any" && s == Match {
			return Match
		}
		if mode != "any" && s == Miss {
			return Miss
		}
		unknown = unknown || s == Unknown
	}
	if unknown {
		return Unknown
	}
	if mode == "any" {
		return Miss
	}
	return Match
}
func (c condition) evaluate(r core.Release, ctx Context) State {
	if c.Field == "audio_matches_original" {
		return c.originalAudioState(r, ctx)
	}
	if c.Operator == "range" {
		n := float64(r.Info.Height)
		if c.Field == "size_gib" {
			n = float64(r.Size) / (1 << 30)
			if r.Info.FullSeason || r.Target != nil {
				return Unknown
			}
		}
		if n <= 0 {
			return Unknown
		}
		if n >= c.Min && (c.Max == 0 || n <= c.Max) {
			return Match
		}
		return Miss
	}
	if c.Field == "language" || c.Field == "subtitles" {
		return c.languageState(r)
	}
	var values []string
	switch c.Field {
	case "name":
		values = []string{r.Name}
	case "group":
		values = []string{r.Info.Group}
	case "source":
		values = []string{string(r.Info.Source)}
	case "video":
		values = []string{r.Info.VideoCodec}
	case "audio":
		values = []string{r.Info.AudioCodec}
	case "indexer":
		values = []string{r.Indexer}
	case "protocol":
		// Live indexer results leave Protocol empty for Usenet.
		// Filename previews set ProtocolUnstated when the name
		// supplies no protocol, and that absence is not Usenet.
		if r.Protocol == core.ProtocolUnstated {
			return Unknown
		}
		v := r.Protocol
		if v == "" {
			v = "usenet"
		}
		values = []string{v}
	case "edition":
		values = []string{r.Info.Edition}
	case "hdr":
		if r.Info.DolbyVision {
			values = append(values, "dv")
		}
		if r.Info.HDR10 {
			values = append(values, "hdr10")
		}
	}
	known := false
	for _, v := range values {
		if v == "" || ((c.Field == "language" || c.Field == "subtitles") && (v == "multi" || v == "dual" || v == "und")) {
			continue
		}
		known = true
		switch c.Operator {
		case "equals":
			if strings.EqualFold(v, c.Value) {
				return Match
			}
		case "contains":
			if strings.Contains(strings.ToLower(v), strings.ToLower(c.Value)) {
				return Match
			}
		case "regex":
			if c.re.MatchString(v) {
				return Match
			}
		}
	}
	if !known {
		return Unknown
	}
	return Miss
}

// languageState matches declared languages, not probed tracks.
//
// A want with neither script nor region accepts every declaration of
// that language. Each dimension the want names is judged on its own.
// The release must evidence that dimension: the same value is a match,
// a different value is a miss, and a declaration that omits it is
// unknown. An extra dimension the want did not name does not reject.
// Unknown fails a requirement closed and does not claim a match.
//
// The release parser keeps a region only for Spanish and Portuguese.
// Other BCP 47 regions and scripts collapse to the base name, so those
// wants are read from the tag. A Chinese region-only tag can satisfy a
// script want, because legacy parser output uses zh-TW for Traditional
// and zh-CN for Simplified. A script never supplies a region: zh-Hant
// is not Taiwan, and zh-Hans is not mainland China. A tag that states
// both keeps both. No other language gets the region-to-script alias.
// originalAudioState matches positively declared audio to the
// catalogue original language. Subtitles, filename words, multi,
// and an assumed language are not evidence. A missing catalogue
// language or a missing audio declaration is unknown, not a miss
// and not English.
func (c condition) originalAudioState(r core.Release, ctx Context) State {
	original := strings.TrimSpace(ctx.OriginalLanguage)
	if original == "" || declaredLanguage(original).name == "" {
		return Unknown
	}
	want := declaredLanguage(original)
	known := false
	unspecified := false
	for _, v := range r.Info.Languages {
		if v == "" || v == "multi" || v == "dual" || v == "und" {
			continue
		}
		known = true
		switch compareLanguage(declaredLanguage(v), want) {
		case langExact:
			return Match
		case langUnspecified:
			unspecified = true
		}
	}
	if !known || unspecified {
		return Unknown
	}
	return Miss
}

func (c condition) languageState(r core.Release) State {
	values := r.Info.Languages
	if c.Field == "subtitles" {
		values = r.Info.SubtitleLanguages
	}
	want := declaredLanguage(c.Value)
	known := false
	unspecified := false
	for _, v := range values {
		if v == "" || v == "multi" || v == "dual" || v == "und" {
			continue
		}
		known = true
		switch compareLanguage(declaredLanguage(v), want) {
		case langExact:
			return Match
		case langUnspecified:
			unspecified = true
		}
	}
	if !known || unspecified {
		return Unknown
	}
	return Miss
}

type langRelation int

const (
	langNone langRelation = iota
	langUnspecified
	langExact
)

// declaredLanguageFact is one declared language. script and region are
// the BCP 47 subtags the input actually named. Parser names that
// already keep a variant (portuguese-brazil) leave both empty and are
// compared by LanguageMatch.
type declaredLanguageFact struct {
	name    string
	primary string
	script  string
	region  string
}

func declaredLanguage(v string) declaredLanguageFact {
	name := releaseparse.LanguageName(v)
	raw := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(v, "_", "-")))
	// A folded parser name such as portuguese-brazil already carries
	// the variant. A raw tag LanguageName could not fold still has
	// its script and region in the tag.
	if strings.Contains(name, "-") && name != raw {
		return declaredLanguageFact{name: name}
	}
	tag, ok := langtag.ParseBCP47(v)
	if !ok {
		return declaredLanguageFact{name: name}
	}
	return declaredLanguageFact{name: name, primary: tag.Primary, script: tag.Script, region: tag.Region}
}

// evidencesScript reports whether this declaration decides a script.
// A stated script is used as itself. A Chinese region-only tag may
// also decide it, for legacy parser output: cn is Hans and tw is Hant.
// That alias does not run in reverse. Any other omitted script is unknown.
func (f declaredLanguageFact) evidencesScript(script string) (known, match bool) {
	if f.script != "" {
		return true, f.script == script
	}
	if f.primary == "zh" {
		switch f.region {
		case "cn":
			return true, script == "hans"
		case "tw":
			return true, script == "hant"
		}
	}
	return false, false
}

// evidencesRegion reports whether this declaration states a region.
// Script is not a region, including for Chinese: Traditional Chinese
// is not evidence of Taiwan and Simplified Chinese is not evidence of
// mainland China. An omitted region is unknown.
func (f declaredLanguageFact) evidencesRegion(region string) (known, match bool) {
	if f.region == "" {
		return false, false
	}
	return true, f.region == region
}

func judgeDimension(known, match bool) langRelation {
	switch {
	case !known:
		return langUnspecified
	case match:
		return langExact
	default:
		return langNone
	}
}

func combineLanguage(a, b langRelation) langRelation {
	if a == langNone || b == langNone {
		return langNone
	}
	if a == langUnspecified || b == langUnspecified {
		return langUnspecified
	}
	return langExact
}

func compareLanguage(got, want declaredLanguageFact) langRelation {
	if got.name == "" || want.name == "" || releaseparse.LanguageBase(got.name) != releaseparse.LanguageBase(want.name) {
		return langNone
	}
	if want.script == "" && want.region == "" {
		switch releaseparse.LanguageMatch(got.name, want.name) {
		case releaseparse.LanguageExactMatch:
			return langExact
		case releaseparse.LanguageBaseMatch:
			if !strings.Contains(got.name, "-") {
				return langUnspecified
			}
			return langNone
		default:
			return langNone
		}
	}
	rel := langExact
	if want.script != "" {
		rel = combineLanguage(rel, judgeDimension(got.evidencesScript(want.script)))
	}
	if want.region != "" && rel != langNone {
		rel = combineLanguage(rel, judgeDimension(got.evidencesRegion(want.region)))
	}
	return rel
}
