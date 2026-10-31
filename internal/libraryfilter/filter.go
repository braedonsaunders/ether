// Package libraryfilter evaluates owner catalogue visibility rules without
// network access. The same compiled conditions drive local previews and every
// client. Rules run top to bottom and the first match wins. An empty action
// is hide, and an empty default is show, so a saved exclusion list hides the
// same titles as before.
package libraryfilter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

type condition struct {
	field, op string
	values    []string
	nums      []float64
	// levels are a content-rating rule's age levels. Unrated is the explicit
	// "Not rated" choice. gte and lte hold exactly one rated level.
	levels []contentrating.Level
}
type rule struct {
	action, typ, match string
	rules              []condition
}
type Filter struct {
	rules    []rule
	fallback string // show or hide
}

// Compile treats every rule as hide and leaves unmatched titles visible.
func Compile(specs []core.LibraryExclusion) (*Filter, error) {
	return CompilePolicy(specs, "show")
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
	if len(specs) > 32 {
		return nil, fmt.Errorf("at most 32 exclusion groups")
	}
	f := &Filter{fallback: fallback}
	for i, s := range specs {
		if len(strings.TrimSpace(s.Name)) == 0 || len(s.Name) > 120 {
			return nil, fmt.Errorf("group %d needs a name of 1–120 characters", i+1)
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
		if len(s.Rules) == 0 || len(s.Rules) > 20 {
			return nil, fmt.Errorf("group %d needs 1–20 conditions", i+1)
		}
		g := rule{action: action, typ: s.MediaType, match: s.Match}
		for _, r := range s.Rules {
			c, err := compileCondition(r)
			if err != nil {
				return nil, err
			}
			g.rules = append(g.rules, c)
		}
		f.rules = append(f.rules, g)
	}
	return f, nil
}

func compileCondition(r core.CollectionRule) (condition, error) {
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

// Passive reports that the filter hides nothing.
func (f *Filter) Passive() bool {
	return f == nil || (len(f.rules) == 0 && f.fallback != "hide")
}

// Fallback is "show" or "hide".
func (f *Filter) Fallback() string {
	if f == nil || f.fallback == "" {
		return "show"
	}
	return f.fallback
}

func (f *Filter) Hidden(t *core.Title) bool {
	if f == nil || t == nil {
		return false
	}
	if t.Type != core.TypeMovie && t.Type != core.TypeShow {
		return false
	}
	for _, g := range f.rules {
		if g.typ != "mixed" && g.typ != string(t.Type) {
			continue
		}
		if g.hit(t) {
			return g.action == "hide"
		}
	}
	return f.fallback == "hide"
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
		if v == 0 {
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
		for _, w := range c.values {
			if strings.EqualFold(v, w) {
				yes = true
			}
		}
	}
	if c.op == "notIn" || c.op == "isNot" {
		return !yes
	}
	return yes
}

func (c condition) known(t *core.Title) bool {
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
	if f == nil || (len(f.rules) == 0 && f.fallback != "hide") {
		return "0=1", nil
	}
	if len(f.rules) == 0 {
		return "1=1", nil
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
	elseBit := "0"
	if f.fallback == "hide" {
		elseBit = "1"
	}
	b.WriteString(" ELSE ")
	b.WriteString(elseBit)
	b.WriteString(" END=1")
	return b.String(), args
}

func (f *Filter) legacyOr() bool {
	if f.fallback == "hide" {
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
	q := "(" + strings.Join(rules, join) + ")"
	if g.typ != "mixed" {
		q = "t.type=? AND " + q
		n := 0
		for _, c := range g.rules {
			n += len(c.values) + len(c.nums) + len(c.levels)
		}
		at := len(args) - n
		args = append(args, nil)
		copy(args[at+1:], args[at:])
		args[at] = g.typ
	}
	return q, args
}

func (c condition) sql() (string, []any) {
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
				q = "t.content_rating_rank IN (" + strings.TrimSuffix(strings.Repeat("?,", len(c.levels)), ",") + ")"
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
		q := col + ">0 AND " + col + op + "?"
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
	ph := strings.TrimSuffix(strings.Repeat("?,", len(c.values)), ",")
	args := make([]any, len(c.values))
	for i, v := range c.values {
		args[i] = v
	}
	switch c.field {
	case "country", "genre", "studio", "keyword":
		// Key sets, not a correlated EXISTS per title. The database builds
		// each set once and probes it by rowid.
		table, col := "catalog_countries", "country"
		switch c.field {
		case "genre":
			table, col = "catalog_genres", "genre"
		case "studio":
			table, col = "catalog_studios", "studio"
		case "keyword":
			table, col = "catalog_keywords", "keyword"
		}
		match := "t.key IN (SELECT key FROM " + table + " WHERE LOWER(" + col + ") IN (" + ph + "))"
		if c.op == "notIn" || c.op == "isNot" {
			// Known values only: a title with none of this facet is not hidden.
			return "t.key IN (SELECT key FROM " + table + ") AND NOT (" + match + ")", args
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
		return "t.key IN (SELECT key FROM catalog_countries)"
	case "genre":
		return "t.key IN (SELECT key FROM catalog_genres)"
	case "studio":
		return "t.key IN (SELECT key FROM catalog_studios)"
	case "keyword":
		return "t.key IN (SELECT key FROM catalog_keywords)"
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
