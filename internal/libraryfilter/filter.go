// Package libraryfilter evaluates owner catalogue visibility rules without
// network access. The same compiled conditions drive local previews and every
// client. Rules run top to bottom and the first match wins. An empty action
// is hide, and an empty default is show, so a saved exclusion list hides the
// same titles as before.
package libraryfilter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/store"
	"reflect"
	"strings"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

type condition struct {
	predicateMatch func(*core.Title) bool

	children      []condition
	predicate     *core.Filter
	predicateSQL  string
	predicateArgs []any

	field, op    string
	allCountries bool
	values       []string
	nums         []float64
	// levels are a content-rating rule's age levels. Unrated is the explicit
	// "Not rated" choice. gte and lte hold exactly one rated level.
	levels []contentrating.Level
}
type rule struct {
	name               string
	action, typ, match string
	discovery          bool // hide from discovery only; search ignores it
	rules              []condition
}
type Filter struct {
	rules         []rule
	movieFallback string // show or hide
	showFallback  string // show or hide
}

// CompilePolicy compiles ordered rules. fallback is "show" or "hide" for
// titles that match nothing; empty means show.
func CompilePolicy(specs []core.LibraryExclusion, fallback string) (*Filter, error) {
	switch fallback {
	case "", "show":
		fallback = "show"
	case "hide":
	default:
		return nil, fmt.Errorf("visibility default must be show or hide")
	}
	if len(specs) > 34 {
		return nil, fmt.Errorf("at most 34 visibility rules")
	}
	f := &Filter{movieFallback: fallback, showFallback: fallback}
	for i, s := range specs {
		// A name is optional. The owner UI labels a rule from its conditions.
		name := strings.TrimSpace(s.Name)
		if len(name) > 120 {
			return nil, fmt.Errorf("group %d name must be at most 120 characters", i+1)
		}
		action := s.Action
		if action == "" {
			action = "hide"
		}
		if action != "show" && action != "hide" {
			return nil, fmt.Errorf("group %d: action must be show or hide", i+1)
		}
		if s.MediaType != "movie" && s.MediaType != "show" && s.MediaType != "mixed" {
			return nil, fmt.Errorf("group %d: choose movies, shows or both", i+1)
		}
		if s.Match != "all" && s.Match != "any" {
			return nil, fmt.Errorf("group %d: match must be all or any", i+1)
		}
		if len(s.Rules) == 0 || len(s.Rules) > 64 {
			return nil, fmt.Errorf("group %d needs 1–64 conditions", i+1)
		}
		g := rule{name: name, action: action, typ: s.MediaType, match: s.Match}
		switch s.Scope {
		case "", core.VisibilityEverywhere:
		case core.VisibilityDiscovery:
			if action != "hide" {
				return nil, fmt.Errorf("group %d: only hide rules can be limited to browsing", i+1)
			}
			g.discovery = true
		default:
			return nil, fmt.Errorf("group %d: scope must be everywhere or discovery", i+1)
		}
		for _, r := range s.Rules {
			c, err := compileCondition(r)
			if err != nil {
				return nil, err
			}
			c.allCountries = action == "hide" && c.field == "country" && (c.op == "in" || c.op == "is")
			g.rules = append(g.rules, c)
		}
		f.rules = append(f.rules, g)
	}
	return f, nil
}

// CompileLibraryPolicies compiles the canonical Movies (library 1) and TV Shows
// (library 2) policies into a single Filter used across all client protocols
// and the console. Each library may independently hold up to 32 rules and its
// own default visibility ("show" or "hide").
func CompileLibraryPolicies(movies []core.LibraryExclusion, movieFallback string, shows []core.LibraryExclusion, showFallback string) (*Filter, error) {
	mSpecs := make([]core.LibraryExclusion, len(movies))
	for i, r := range movies {
		r.MediaType = string(core.TypeMovie)
		mSpecs[i] = r
	}
	mf, err := CompilePolicy(mSpecs, movieFallback)
	if err != nil {
		return nil, fmt.Errorf("movies rules: %w", err)
	}
	sSpecs := make([]core.LibraryExclusion, len(shows))
	for i, r := range shows {
		r.MediaType = string(core.TypeShow)
		sSpecs[i] = r
	}
	sf, err := CompilePolicy(sSpecs, showFallback)
	if err != nil {
		return nil, fmt.Errorf("shows rules: %w", err)
	}
	f := &Filter{
		movieFallback: mf.movieFallback,
		showFallback:  sf.showFallback,
		rules:         make([]rule, 0, len(mf.rules)+len(sf.rules)),
	}
	f.rules = append(f.rules, mf.rules...)
	f.rules = append(f.rules, sf.rules...)
	return f, nil
}

// RuleName returns the optional owner name of compiled rule index, or "".
func (f *Filter) RuleName(index int) string {
	if f == nil || index < 0 || index >= len(f.rules) {
		return ""
	}
	return f.rules[index].name
}

func compileCondition(r core.CollectionRule) (condition, error) { return compileConditionDepth(r, 0) }
func compileConditionDepth(r core.CollectionRule, depth int) (condition, error) {
	if depth > 8 {
		return condition{}, fmt.Errorf("rule groups may nest at most eight levels")
	}
	if r.Field == "group" {
		if r.Op != "is" && r.Op != "in" && r.Op != "isNot" {
			return condition{}, fmt.Errorf("invalid rule group")
		}
		var specs []core.CollectionRule
		if err := json.Unmarshal(r.Value, &specs); err != nil {
			return condition{}, err
		}
		if len(specs) > 64 {
			return condition{}, fmt.Errorf("too many conditions")
		}
		c := condition{field: r.Field, op: r.Op}
		for _, s := range specs {
			child, err := compileConditionDepth(s, depth+1)
			if err != nil {
				return condition{}, err
			}
			c.children = append(c.children, child)
		}
		return c, nil
	}
	if strings.HasPrefix(r.Field, "browse:") || r.Field == "viewer" {
		if r.Op != "is" && r.Op != "isNot" {
			return condition{}, fmt.Errorf("invalid browse comparison")
		}
		var f core.Filter
		if r.Field == "viewer" {
			var g core.FilterGroup
			if err := json.Unmarshal(r.Value, &g); err != nil {
				return condition{}, err
			}
			if g.Filter != nil || len(g.Children) > 0 {
				return condition{}, fmt.Errorf("viewer conditions only describe activity")
			}
			g.Operator = "and"
			f.Group = &g
		} else {
			key := strings.TrimPrefix(r.Field, "browse:")
			valid := false
			typ := reflect.TypeOf(f)
			for i := 0; i < typ.NumField(); i++ {
				tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
				if tag != "-" && tag != "" && tag != "group" && tag != "catalogueSources" && tag == key {
					valid = true
				}
			}
			if !valid {
				return condition{}, fmt.Errorf("unknown browse condition %q", key)
			}
			data, _ := json.Marshal(map[string]json.RawMessage{key: r.Value})
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&f); err != nil {
				return condition{}, err
			}
		}
		return condition{field: r.Field, op: r.Op, predicate: &f}, nil
	}
	field := r.Field
	if field == "network" {
		field = "studio" // networks are stored with studios
	}
	c := condition{field: field, op: r.Op}
	if r.Op == "unknown" || r.Op == "known" {
		switch field {
		case "country", "language", "genre", "contentRating", "year", "rating", "votes", "popularity", "runtime", "studio", "keyword":
			return c, nil
		default:
			return condition{}, fmt.Errorf("unsupported catalogue field %q", r.Field)
		}
	}
	switch field {
	case "contentRating":
		levels, err := compileRatings(r.Op, r.Value)
		if err != nil {
			return condition{}, err
		}
		c.levels = levels
	case "country", "language", "genre", "studio", "keyword":
		if r.Op != "in" && r.Op != "notIn" && r.Op != "is" && r.Op != "isNot" {
			return condition{}, fmt.Errorf("unsupported %s comparison", r.Field)
		}
		vals, err := textValues(r.Value)
		if err != nil {
			return condition{}, fmt.Errorf("%s needs text values", r.Field)
		}
		if len(vals) == 0 || len(vals) > 100 {
			return condition{}, fmt.Errorf("%s needs 1–100 values", r.Field)
		}
		var out []string
		for _, v := range vals {
			v = strings.TrimSpace(v)
			if v == "" || len(v) > 120 {
				return condition{}, fmt.Errorf("invalid %s value", r.Field)
			}
			if field == "country" {
				out = append(out, CountryAliases(v)...)
			} else {
				out = append(out, strings.ToLower(v))
			}
		}
		c.values = out
	case "year", "rating", "votes", "popularity", "runtime":
		if r.Op != "is" && r.Op != "gte" && r.Op != "lte" && r.Op != "between" {
			return condition{}, fmt.Errorf("unsupported %s comparison", r.Field)
		}
		var n float64
		if json.Unmarshal(r.Value, &n) == nil {
			c.nums = []float64{n}
		} else if json.Unmarshal(r.Value, &c.nums) != nil {
			return condition{}, fmt.Errorf("%s needs numbers", r.Field)
		}
		want := 1
		if r.Op == "between" {
			want = 2
		}
		if len(c.nums) != want {
			return condition{}, fmt.Errorf("invalid %s range", r.Field)
		}
		for _, n := range c.nums {
			if n < 0 || n > 1e9 {
				return condition{}, fmt.Errorf("invalid %s value", r.Field)
			}
		}
		if want == 2 && c.nums[0] > c.nums[1] {
			return condition{}, fmt.Errorf("%s range is reversed", r.Field)
		}
	default:
		return condition{}, fmt.Errorf("unsupported catalogue field %q", r.Field)
	}
	return c, nil
}

func textValues(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("needs text")
	}
	var v string
	if json.Unmarshal(raw, &v) == nil {
		return []string{v}, nil
	}
	var ss []string
	if json.Unmarshal(raw, &ss) == nil {
		return ss, nil
	}
	var one struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &one) == nil && one.Name != "" {
		return []string{one.Name}, nil
	}
	var objs []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &objs) == nil {
		var out []string
		for _, o := range objs {
			if o.Name != "" {
				out = append(out, o.Name)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("needs text")
}

// compileRatings reads a content-rating rule. Values are any certification
// label or level key ("TV-PG", "PG", "gb/PG" are one level) or "NR" for
// titles without a rating; a value naming no level matches nothing. Ops are
// set membership (in, notIn, is, isNot) or a bound (gte "at least", lte "at
// most") on one rated level, which never matches unrated titles.
func compileRatings(op string, raw json.RawMessage) ([]contentrating.Level, error) {
	var values []string
	var v string
	if json.Unmarshal(raw, &v) == nil {
		values = []string{v}
	} else if json.Unmarshal(raw, &values) != nil {
		return nil, fmt.Errorf("contentRating needs text values")
	}
	if len(values) == 0 || len(values) > 100 {
		return nil, fmt.Errorf("contentRating needs 1–100 values")
	}
	var out []contentrating.Level
	seen := map[contentrating.Level]bool{}
	for _, v := range values {
		if v = strings.TrimSpace(v); v == "" || len(v) > 120 {
			return nil, fmt.Errorf("invalid contentRating value")
		}
		key, ok := contentrating.Canonical(v)
		if !ok {
			continue
		}
		l := contentrating.LevelOf(key)
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	switch op {
	case "in", "notIn", "is", "isNot":
	case "gte", "lte":
		if len(values) != 1 || len(out) != 1 || out[0] == contentrating.Unrated {
			return nil, fmt.Errorf("contentRating %s needs one rating such as PG or TV-14", op)
		}
	default:
		return nil, fmt.Errorf("unsupported contentRating comparison")
	}
	return out, nil
}

// TMDB movie production countries use names; TV origins use ISO codes.
// Match both forms, never infer a country from the spoken language.
func CountryAliases(v string) []string {
	v = strings.ToLower(strings.TrimSpace(v))
	out := []string{v}
	for _, r := range language.Supported.Regions() {
		code := strings.ToLower(r.String())
		name := strings.ToLower(display.English.Regions().Name(r))
		alias := name
		if code == "us" {
			alias = "united states of america"
		}
		if code == "kr" {
			alias = "south korea"
		}
		if v == code || v == name || v == alias {
			out = append(out, code, name, alias)
			break
		}
	}
	return out
}

// ForSearch returns the filter explicit searches use: the same rules without
// the ones scoped to discovery. Only hide rules carry that scope, so a title
// visible while browsing is never hidden from search. Compile it once per
// config change; it shares the compiled conditions.
func (f *Filter) ForSearch() *Filter {
	if f == nil {
		return nil
	}
	out := &Filter{
		movieFallback: f.movieFallback,
		showFallback:  f.showFallback,
		rules:         make([]rule, 0, len(f.rules)),
	}
	for _, g := range f.rules {
		if !g.discovery {
			out.rules = append(out.rules, g)
		}
	}
	return out
}

// Facts reports the facts outside a title's row that the rules read
// (core.TitleFacts): genres, studios and networks, countries, keywords.
// Every other condition is on the row itself.
func (f *Filter) Facts() core.TitleFacts {
	if f == nil {
		return 0
	}
	var out core.TitleFacts
	var visit func(condition)
	visit = func(c condition) {
		switch c.field {
		case "genre":
			out |= core.FactGenres
		case "studio":
			out |= core.FactStudios
		case "country":
			out |= core.FactCountries
		case "keyword":
			out |= core.FactKeywords
		}
		for _, child := range c.children {
			visit(child)
		}
	}
	for _, g := range f.rules {
		for _, c := range g.rules {
			visit(c)
		}
	}
	return out
}

// Passive reports that the filter hides nothing.
func (f *Filter) Passive() bool {
	return f == nil || (len(f.rules) == 0 && f.movieFallback != "hide" && f.showFallback != "hide")
}

// FallbackForType returns "show" or "hide" for the given media type.
func (f *Filter) FallbackForType(typ core.MediaType) string {
	if f == nil {
		return "show"
	}
	fb := f.movieFallback
	if typ == core.TypeShow || typ == core.TypeSeason || typ == core.TypeEpisode {
		fb = f.showFallback
	}
	if fb == "" {
		return "show"
	}
	return fb
}

// Fallback is "show" or "hide".
func (f *Filter) Fallback() string {
	if f == nil {
		return "show"
	}
	if f.movieFallback == "hide" || f.showFallback == "hide" {
		return "hide"
	}
	return "show"
}

// MatchedHideRule evaluates f's ordered rules against t and reports whether t
// is hidden, along with the matching hide rule's index (index >= 0) or
// byFallback == true when no rule matched and the default visibility is hide.
func (f *Filter) MatchedHideRule(t *core.Title) (hidden bool, index int, byFallback bool) {
	if f == nil || t == nil {
		return false, -1, false
	}
	if t.Type != core.TypeMovie && t.Type != core.TypeShow {
		return false, -1, false
	}
	for i, g := range f.rules {
		if g.typ != "mixed" && g.typ != string(t.Type) {
			continue
		}
		if g.hit(t) {
			if g.action == "hide" {
				return true, i, false
			}
			return false, -1, false
		}
	}
	if f.FallbackForType(t.Type) == "hide" {
		return true, -1, true
	}
	return false, -1, false
}

func (f *Filter) Hidden(t *core.Title) bool {
	hidden, _, _ := f.MatchedHideRule(t)
	return hidden
}

// Decidable reports whether every condition of every hide rule that applies
// to t's type can be judged from t's own fields. A condition on an unknown
// field (no certification, countries, genres...) answers "no match", so a
// hide rule silently lets such a title through. An unknown field in a show
// rule can only make a title hidden, never shown, so show rules don't count.
// Callers that must fail closed (a restricted viewer's rules on a summary the
// catalogue has no details for) drop titles that are not decidable.
// Known/unknown conditions and vote counts (0 is a real value) are always
// decidable.
func (f *Filter) Decidable(t *core.Title) bool {
	if f == nil || t == nil || (t.Type != core.TypeMovie && t.Type != core.TypeShow) {
		return true
	}
	for _, g := range f.rules {
		if g.action != "hide" || (g.typ != "mixed" && g.typ != string(t.Type)) {
			continue
		}
		for _, c := range g.rules {
			if c.op == "unknown" || c.op == "known" || c.field == "votes" {
				continue
			}
			if !c.known(t) {
				return false
			}
		}
	}
	return true
}

// SQLUndecidable is Decidable's complement for catalog_titles AS t: true when
// some condition of an applicable hide rule is on a field the row does not
// know. "0=1" when every row is decidable.
func (f *Filter) SQLUndecidable() string {
	if f == nil {
		return "0=1"
	}
	var parts []string
	for _, g := range f.rules {
		if g.action != "hide" {
			continue
		}
		var unknown []string
		for _, c := range g.rules {
			if c.op == "unknown" || c.op == "known" || c.field == "votes" {
				continue
			}
			if q := c.knownSQL(); q != "" {
				unknown = append(unknown, "NOT ("+q+")")
			}
		}
		if len(unknown) == 0 {
			continue
		}
		q := "(" + strings.Join(unknown, " OR ") + ")"
		if g.typ == "mixed" {
			q = "(t.type IN ('movie','show') AND " + q + ")"
		} else {
			q = "(t.type='" + g.typ + "' AND " + q + ")"
		}
		parts = append(parts, q)
	}
	if len(parts) == 0 {
		return "0=1"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func (g rule) hit(t *core.Title) bool {
	yes := g.match == "all"
	for _, c := range g.rules {
		v := c.matches(t)
		if g.match == "all" {
			yes = yes && v
		} else {
			yes = yes || v
		}
	}
	return yes
}

func (c condition) matches(t *core.Title) bool {
	if c.field == "group" {
		if len(c.children) == 0 {
			return c.op != "isNot"
		}
		yes := c.op != "in"
		for _, child := range c.children {
			hit := child.matches(t)
			if c.op == "in" {
				yes = yes || hit
			} else {
				yes = yes && hit
			}
		}
		if c.op == "isNot" {
			yes = !yes
		}
		return yes
	}
	if c.predicate != nil {
		if c.predicateMatch == nil {
			return false
		}
		yes := c.predicateMatch(t)
		if c.op == "isNot" {
			yes = !yes
		}
		return yes
	}

	if c.op == "unknown" || c.op == "known" {
		return c.known(t) == (c.op == "known")
	}
	if c.field == "contentRating" {
		l := contentrating.LevelOf(t.ContentRating)
		switch c.op {
		case "gte":
			return l != contentrating.Unrated && l >= c.levels[0]
		case "lte":
			return l != contentrating.Unrated && l <= c.levels[0]
		}
		in := false
		for _, w := range c.levels {
			in = in || w == l
		}
		if c.op == "notIn" || c.op == "isNot" {
			return !in
		}
		return in
	}
	if len(c.nums) > 0 {
		var v float64
		switch c.field {
		case "year":
			v = float64(t.Year)
		case "rating":
			v = t.Rating
		case "votes":
			v = float64(t.VoteCount)
		case "popularity":
			v = t.Popularity
		case "runtime":
			v = t.Runtime.Minutes()
		}
		// 0 means unknown for year, rating, popularity and runtime. For votes
		// it is a real value (nobody has voted), so "votes ≤ 99" and
		// "votes is 0" match zero-vote titles (#257).
		if v == 0 && c.field != "votes" {
			return false
		}
		switch c.op {
		case "gte":
			return v >= c.nums[0]
		case "lte":
			return v <= c.nums[0]
		case "is":
			return v == c.nums[0]
		case "between":
			return v >= c.nums[0] && v <= c.nums[1]
		}
	}
	var values []string
	switch c.field {
	case "country":
		values = t.Countries
	case "genre":
		values = t.Genres
	case "studio":
		values = t.Studios
	case "keyword":
		values = t.Keywords
	case "language":
		if t.Language != "" {
			values = []string{t.Language}
		}
	}
	if len(values) == 0 {
		return false
	}
	yes := false
	for _, v := range values {
		matched := false
		for _, w := range c.values {
			if strings.EqualFold(v, w) {
				matched = true
				break
			}
		}
		if c.allCountries && !matched {
			return false
		}
		yes = yes || matched
	}
	if c.op == "notIn" || c.op == "isNot" {
		return !yes
	}
	return yes
}

func (c condition) known(t *core.Title) bool {
	if c.predicate != nil {
		return t != nil && t.Key != 0
	}
	if c.field == "group" {
		for _, child := range c.children {
			if child.op != "known" && child.op != "unknown" && child.field != "votes" && !child.known(t) {
				return false
			}
		}
		return true
	}
	switch c.field {
	case "year":
		return t.Year != 0
	case "rating":
		return t.Rating != 0
	case "votes":
		return t.VoteCount != 0
	case "popularity":
		return t.Popularity != 0
	case "runtime":
		return t.Runtime != 0
	case "language":
		return strings.TrimSpace(t.Language) != ""
	case "contentRating":
		return contentrating.LevelOf(t.ContentRating) != contentrating.Unrated
	case "country":
		return len(t.Countries) > 0
	case "genre":
		return len(t.Genres) > 0
	case "studio":
		return len(t.Studios) > 0
	case "keyword":
		return len(t.Keywords) > 0
	}
	return false
}

// SQLHidden returns a parameterized predicate for catalog_titles AS t.
// Hide-only rules with a show default stay an OR, which is the same predicate
// as first-match. A show rule or a hide default compiles to a CASE so the
// first match wins.
func (f *Filter) SQLHidden() (string, []any) {
	if f.Passive() {
		return "0=1", nil
	}
	if len(f.rules) == 0 {
		switch {
		case f.movieFallback == "hide" && f.showFallback == "hide":
			return "1=1", nil
		case f.movieFallback == "hide":
			return "t.type='movie'", nil
		default:
			return "t.type='show'", nil
		}
	}
	if f.legacyOr() {
		return f.sqlOr()
	}
	var b strings.Builder
	var args []any
	b.WriteString("CASE")
	for _, g := range f.rules {
		q, a := g.sql()
		args = append(args, a...)
		bit := "0"
		if g.action == "hide" {
			bit = "1"
		}
		b.WriteString(" WHEN ")
		b.WriteString(q)
		b.WriteString(" THEN ")
		b.WriteString(bit)
	}
	switch {
	case f.movieFallback == "hide" && f.showFallback == "hide":
		b.WriteString(" ELSE 1 END=1")
	case f.movieFallback == "hide":
		b.WriteString(" WHEN t.type='movie' THEN 1 ELSE 0 END=1")
	case f.showFallback == "hide":
		b.WriteString(" WHEN t.type='show' THEN 1 ELSE 0 END=1")
	default:
		b.WriteString(" ELSE 0 END=1")
	}
	return b.String(), args
}

// SQLHiddenForType specializes the policy for a known media type. This
// retains first-match ordering while dropping rules for other media types.
func (f *Filter) SQLHiddenForType(typ core.MediaType) (string, []any) {
	if f == nil {
		return "0=1", nil
	}
	fb := f.FallbackForType(typ)
	copy := &Filter{movieFallback: fb, showFallback: fb}
	for _, r := range f.rules {
		if r.typ == "mixed" || r.typ == string(typ) {
			r.typ = "mixed"
			copy.rules = append(copy.rules, r)
		}
	}
	return copy.SQLHidden()
}

func (f *Filter) legacyOr() bool {
	if f.movieFallback == "hide" || f.showFallback == "hide" {
		return false
	}
	for _, g := range f.rules {
		if g.action == "show" {
			return false
		}
	}
	return true
}

func (f *Filter) sqlOr() (string, []any) {
	var groups []string
	var args []any
	for _, g := range f.rules {
		q, a := g.sql()
		args = append(args, a...)
		groups = append(groups, "("+q+")")
	}
	return strings.Join(groups, " OR "), args
}

func (g rule) sql() (string, []any) {
	var rules []string
	var args []any
	for _, c := range g.rules {
		q, a := c.sql()
		args = append(args, a...)
		rules = append(rules, "("+q+")")
	}
	join := " AND "
	if g.match == "any" {
		join = " OR "
	}
	q := "1=1"
	if len(rules) > 0 {
		q = "(" + strings.Join(rules, join) + ")"
	}
	if g.typ != "mixed" {
		q = "t.type=? AND " + q
		args = append([]any{g.typ}, args...)
	}
	return q, args
}

func (c condition) sql() (string, []any) {
	if c.field == "group" {
		parts := []string{}
		args := []any{}
		for _, child := range c.children {
			q, a := child.sql()
			parts = append(parts, "("+q+")")
			args = append(args, a...)
		}
		if len(parts) == 0 {
			if c.op == "isNot" {
				return "0=1", nil
			}
			return "1=1", nil
		}
		join := " AND "
		if c.op == "in" {
			join = " OR "
		}
		q := "(" + strings.Join(parts, join) + ")"
		if c.op == "isNot" {
			q = "NOT " + q
		}
		return q, args
	}

	if c.predicate != nil {
		if c.predicateSQL == "" {
			return "0=1", nil
		}
		q := "(" + c.predicateSQL + ")"
		if c.op == "isNot" {
			q = "NOT " + q
		}
		return q, c.predicateArgs
	}

	if c.op == "unknown" || c.op == "known" {
		q := c.knownSQL()
		if c.op == "unknown" {
			q = "NOT (" + q + ")"
		}
		return q, nil
	}
	if c.field == "contentRating" {
		var q string
		switch c.op {
		case "gte":
			q = "t.content_rating_rank>0 AND t.content_rating_rank>=?"
		case "lte":
			q = "t.content_rating_rank>0 AND t.content_rating_rank<=?"
		default:
			q = "0=1"
			if len(c.levels) > 0 {
				q = "t.content_rating_rank IN (" + store.Placeholders(len(c.levels)) + ")"
			}
			if c.op == "notIn" || c.op == "isNot" {
				q = "NOT (" + q + ")"
			}
		}
		args := make([]any, len(c.levels))
		for i, l := range c.levels {
			args[i] = int(l)
		}
		return q, args
	}
	if len(c.nums) > 0 {
		col := map[string]string{"year": "year", "rating": "rating", "votes": "vote_count", "popularity": "popularity", "runtime": "runtime_sec"}[c.field]
		col = "t." + col
		op := map[string]string{"gte": ">=", "lte": "<=", "is": "=", "between": ">="}[c.op]
		q := col + op + "?"
		if c.field != "votes" { // 0 is unknown except for votes (see matches)
			q = col + ">0 AND " + q
		}
		v := c.nums[0]
		if c.field == "runtime" {
			v *= 60
		}
		args := []any{v}
		if c.op == "between" {
			q += " AND " + col + "<=?"
			v = c.nums[1]
			if c.field == "runtime" {
				v *= 60
			}
			args = append(args, v)
		}
		return q, args
	}
	ph := store.Placeholders(len(c.values))
	args := make([]any, len(c.values))
	for i, v := range c.values {
		args[i] = v
	}
	switch c.field {
	case "country", "genre", "studio", "keyword":
		if c.allCountries {
			return "EXISTS (SELECT 1 FROM catalog_countries co WHERE co.key=t.key) AND NOT EXISTS (SELECT 1 FROM catalog_countries co WHERE co.key=t.key AND LOWER(co.country) NOT IN (" + ph + "))", args
		}
		table, col := "catalog_countries", "country"
		switch c.field {
		case "genre":
			table, col = "catalog_genres", "genre"
		case "studio":
			table, col = "catalog_studios", "studio"
		case "keyword":
			table, col = "catalog_keywords", "keyword"
		}
		match := "EXISTS (SELECT 1 FROM " + table + " lf WHERE lf.key=t.key AND LOWER(lf." + col + ") IN (" + ph + "))"
		if c.op == "notIn" || c.op == "isNot" {
			// Known values only: a title with none of this facet is not hidden.
			return "EXISTS (SELECT 1 FROM " + table + " lf WHERE lf.key=t.key) AND NOT (" + match + ")", args
		}
		return match, args
	default:
		q := "LOWER(t.language) IN (" + ph + ")"
		if c.op == "notIn" || c.op == "isNot" {
			q = "NOT (" + q + ")"
		}
		return "t.language<>'' AND " + q, args
	}
}

func (c condition) knownSQL() string {
	if c.predicate != nil {
		return "1=1"
	}
	if c.field == "group" {
		parts := []string{"1=1"}
		for _, child := range c.children {
			if child.op == "known" || child.op == "unknown" || child.field == "votes" {
				continue
			}
			parts = append(parts, "("+child.knownSQL()+")")
		}
		return strings.Join(parts, " AND ")
	}
	switch c.field {
	case "year":
		return "t.year>0"
	case "rating":
		return "t.rating>0"
	case "votes":
		return "t.vote_count>0"
	case "popularity":
		return "t.popularity>0"
	case "runtime":
		return "t.runtime_sec>0"
	case "language":
		return "t.language<>''"
	case "contentRating":
		return "t.content_rating_rank>0"
	case "country":
		return "EXISTS (SELECT 1 FROM catalog_countries lf WHERE lf.key=t.key)"
	case "genre":
		return "EXISTS (SELECT 1 FROM catalog_genres lf WHERE lf.key=t.key)"
	case "studio":
		return "EXISTS (SELECT 1 FROM catalog_studios lf WHERE lf.key=t.key)"
	case "keyword":
		return "EXISTS (SELECT 1 FROM catalog_keywords lf WHERE lf.key=t.key)"
	}
	return "0=1"
}

// CatchCountsSQL returns one movie count and one show count per rule, in
// order. A title is counted on the first rule it matches.
func (f *Filter) CatchCountsSQL() (string, []any) {
	if f == nil || len(f.rules) == 0 {
		return "", nil
	}
	var cols []string
	var args []any
	for i := range f.rules {
		for _, typ := range []string{"movie", "show"} {
			var b strings.Builder
			b.WriteString("(")
			for j := 0; j < i; j++ {
				if j > 0 {
					b.WriteString(" AND ")
				}
				pq, pa := f.rules[j].sql()
				b.WriteString("NOT (")
				b.WriteString(pq)
				b.WriteString(")")
				args = append(args, pa...)
			}
			if i > 0 {
				b.WriteString(" AND ")
			}
			q, a := f.rules[i].sql()
			b.WriteString(q)
			b.WriteString(" AND t.type=?)")
			args = append(args, a...)
			args = append(args, typ)
			cols = append(cols, "COALESCE(SUM(CASE WHEN "+b.String()+" THEN 1 ELSE 0 END),0)")
		}
	}
	return strings.Join(cols, ","), args
}

// BindCatalogue compiles compound conditions using the catalogue's indexed
// browse compiler. It returns a copy; shared policies remain immutable.
func (f *Filter) BindCatalogue(compile func(core.Filter) (string, []any, error), evaluate func(string, []any, *core.Title) bool) (*Filter, error) {
	if f == nil {
		return f, nil
	}
	out := *f
	out.rules = append([]rule(nil), f.rules...)
	var bind func(condition) (condition, error)
	bind = func(c condition) (condition, error) {
		if c.predicate != nil {
			q, a, err := compile(*c.predicate)
			if err != nil {
				return c, err
			}
			c.predicateSQL = q
			c.predicateArgs = a
			c.predicateMatch = func(t *core.Title) bool { return evaluate(q, a, t) }
		}
		c.children = append([]condition(nil), c.children...)
		for i, child := range c.children {
			v, err := bind(child)
			if err != nil {
				return c, err
			}
			c.children[i] = v
		}
		return c, nil
	}
	for i, g := range out.rules {
		g.rules = append([]condition(nil), g.rules...)
		for j, c := range g.rules {
			v, err := bind(c)
			if err != nil {
				return nil, err
			}
			g.rules[j] = v
		}
		out.rules[i] = g
	}
	return &out, nil
}

// SQLShown keeps a single allow rule sargable: its indexed predicates appear
// directly in WHERE rather than inside an ordered CASE. More involved policy
// orderings retain the general first-match compiler.
func (f *Filter) SQLShown() (string, []any) {
	if f != nil && f.movieFallback == "hide" && f.showFallback == "hide" && len(f.rules) == 1 && f.rules[0].action == "show" {
		return f.rules[0].sql()
	}
	q, a := f.SQLHidden()
	return "NOT (" + q + ")", a
}
