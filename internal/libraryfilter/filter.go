// Package libraryfilter evaluates owner catalogue exclusions without network
// access. The same compiled conditions drive local previews and Plex policy.
package libraryfilter

import (
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
	"strings"
)

type condition struct {
	field, op string
	values    []string
	nums      []float64
}
type group struct {
	typ, match string
	rules      []condition
}
type Filter struct{ groups []group }

func Compile(specs []core.LibraryExclusion) (*Filter, error) {
	if len(specs) > 32 {
		return nil, fmt.Errorf("at most 32 exclusion groups")
	}
	f := &Filter{}
	for i, s := range specs {
		if len(strings.TrimSpace(s.Name)) == 0 || len(s.Name) > 120 {
			return nil, fmt.Errorf("group %d needs a name of 1–120 characters", i+1)
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
		g := group{typ: s.MediaType, match: s.Match}
		for _, r := range s.Rules {
			c := condition{field: r.Field, op: r.Op}
			switch r.Field {
			case "country", "language", "genre", "contentRating":
				if r.Op != "in" && r.Op != "notIn" && r.Op != "is" && r.Op != "isNot" {
					return nil, fmt.Errorf("unsupported %s comparison", r.Field)
				}
				var v string
				if json.Unmarshal(r.Value, &v) == nil {
					c.values = []string{v}
				} else if json.Unmarshal(r.Value, &c.values) != nil {
					return nil, fmt.Errorf("%s needs text values", r.Field)
				}
				if len(c.values) == 0 || len(c.values) > 100 {
					return nil, fmt.Errorf("%s needs 1–100 values", r.Field)
				}
				var vals []string
				for _, v := range c.values {
					v = strings.TrimSpace(v)
					if v == "" || len(v) > 120 {
						return nil, fmt.Errorf("invalid %s value", r.Field)
					}
					if r.Field == "country" {
						vals = append(vals, CountryAliases(v)...)
					} else {
						vals = append(vals, strings.ToLower(v))
					}
				}
				c.values = vals
			case "year", "rating", "votes", "popularity", "runtime":
				if r.Op != "is" && r.Op != "gte" && r.Op != "lte" && r.Op != "between" {
					return nil, fmt.Errorf("unsupported %s comparison", r.Field)
				}
				var n float64
				if json.Unmarshal(r.Value, &n) == nil {
					c.nums = []float64{n}
				} else if json.Unmarshal(r.Value, &c.nums) != nil {
					return nil, fmt.Errorf("%s needs numbers", r.Field)
				}
				want := 1
				if r.Op == "between" {
					want = 2
				}
				if len(c.nums) != want {
					return nil, fmt.Errorf("invalid %s range", r.Field)
				}
				for _, n := range c.nums {
					if n < 0 || n > 1e9 {
						return nil, fmt.Errorf("invalid %s value", r.Field)
					}
				}
				if want == 2 && c.nums[0] > c.nums[1] {
					return nil, fmt.Errorf("%s range is reversed", r.Field)
				}
			default:
				return nil, fmt.Errorf("unsupported catalogue field %q", r.Field)
			}
			g.rules = append(g.rules, c)
		}
		f.groups = append(f.groups, g)
	}
	return f, nil
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
func (f *Filter) Hidden(t *core.Title) bool {
	if f == nil || t == nil {
		return false
	}
	for _, g := range f.groups {
		if g.typ != "mixed" && g.typ != string(t.Type) {
			continue
		}
		if t.Type != core.TypeMovie && t.Type != core.TypeShow {
			continue
		}
		yes := g.match == "all"
		for _, c := range g.rules {
			v := c.matches(t)
			if g.match == "all" {
				yes = yes && v
			} else {
				yes = yes || v
			}
		}
		if yes {
			return true
		}
	}
	return false
}
func (c condition) matches(t *core.Title) bool {
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
		} // unknown metadata does not match exclusions
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
	case "language":
		if t.Language != "" {
			values = []string{t.Language}
		}
	case "contentRating":
		if t.ContentRating != "" {
			values = []string{t.ContentRating}
		}
	}
	if len(values) == 0 {
		// A blank certification is not one of the listed ratings. Negative
		// content-rating rules therefore match, so "none of" cannot be
		// bypassed by a missing rating. Unknown country, genre, language
		// and numbers still do not match. Known ratings compare as the
		// listed text, case-insensitively; PG-13 and PG13 stay distinct.
		return c.field == "contentRating" && (c.op == "notIn" || c.op == "isNot")
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

// SQLHidden returns a parameterized predicate for catalog_titles AS t.
// Its fixed column whitelist matches Hidden, including blank content ratings
// on negative rating rules and known-value guards for every other field.
func (f *Filter) SQLHidden() (string, []any) {
	var groups []string
	var args []any
	for _, g := range f.groups {
		var rules []string
		for _, c := range g.rules {
			if len(c.nums) > 0 {
				col := map[string]string{"year": "year", "rating": "rating", "votes": "vote_count", "popularity": "popularity", "runtime": "runtime_sec"}[c.field]
				col = "t." + col
				op := map[string]string{"gte": ">=", "lte": "<=", "is": "=", "between": ">="}[c.op]
				q := col + ">0 AND " + col + op + "?"
				v := c.nums[0]
				if c.field == "runtime" {
					v *= 60
				}
				args = append(args, v)
				if c.op == "between" {
					q += " AND " + col + "<=?"
					v = c.nums[1]
					if c.field == "runtime" {
						v *= 60
					}
					args = append(args, v)
				}
				rules = append(rules, "("+q+")")
				continue
			}
			ph := strings.TrimSuffix(strings.Repeat("?,", len(c.values)), ",")
			var q, known string
			switch c.field {
			case "country", "genre":
				table, col := "catalog_countries", "country"
				if c.field == "genre" {
					table, col = "catalog_genres", "genre"
				}
				prefix := "SELECT 1 FROM " + table + " f WHERE f.key=t.key"
				known = "EXISTS (" + prefix + ")"
				q = "EXISTS (" + prefix + " AND LOWER(f." + col + ") IN (" + ph + "))"
			default:
				col := "t.language"
				if c.field == "contentRating" {
					col = "t.content_rating"
				}
				known = col + "<>''"
				q = "LOWER(" + col + ") IN (" + ph + ")"
			}
			for _, v := range c.values {
				args = append(args, v)
			}
			if c.op == "notIn" || c.op == "isNot" {
				q = "NOT (" + q + ")"
			}
			if c.field == "contentRating" && (c.op == "notIn" || c.op == "isNot") {
				rules = append(rules, "("+q+")")
				continue
			}
			rules = append(rules, "("+known+" AND "+q+")")
		}
		join := " AND "
		if g.match == "any" {
			join = " OR "
		}
		q := "(" + strings.Join(rules, join) + ")"
		if g.typ != "mixed" {
			q = "t.type=? AND " + q // type argument precedes this group's rules
			n := 0
			for _, c := range g.rules {
				n += len(c.values) + len(c.nums)
			}
			at := len(args) - n
			args = append(args, nil)
			copy(args[at+1:], args[at:])
			args[at] = g.typ
		}
		groups = append(groups, "("+q+")")
	}
	if len(groups) == 0 {
		return "0=1", nil
	}
	return strings.Join(groups, " OR "), args
}
