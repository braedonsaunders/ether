// Package sharefilter parses plex.tv shared-server filter specs.
package sharefilter

import (
	"net/url"
	"strings"
)

// Clause is one contentRating or label predicate from a plex.tv filter.
type Clause struct {
	Field  string
	Negate bool
	LTE    bool
	GTE    bool
	Values []string
}

// Part is one owner-readable clause of a plex.tv share filter.
type Part struct {
	Kind   string   `json:"kind"`
	Values []string `json:"values"`
}

// Parse splits a plex.tv filter spec. Multi-value lists are joined with %2C,
// so values are unescaped before they are split on commas.
func Parse(spec string) []Clause {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	var out []Clause
	for _, raw := range strings.Split(spec, "|") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		c, ok := parseClause(raw)
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// Format turns a plex.tv filter spec into owner-readable parts.
func Format(spec string) []Part {
	var out []Part
	for _, c := range Parse(spec) {
		switch {
		case c.Field == "contentRating" && c.LTE && len(c.Values) > 0:
			out = append(out, Part{Kind: "rating-max", Values: []string{c.Values[0]}})
		case c.Field == "contentRating" && c.GTE && len(c.Values) > 0:
			out = append(out, Part{Kind: "rating-min", Values: []string{c.Values[0]}})
		case c.Field == "contentRating" && c.Negate:
			out = append(out, Part{Kind: "rating-deny", Values: append([]string(nil), c.Values...)})
		case c.Field == "contentRating":
			out = append(out, Part{Kind: "rating-allow", Values: append([]string(nil), c.Values...)})
		case c.Field == "label" && c.Negate:
			out = append(out, Part{Kind: "label-deny", Values: append([]string(nil), c.Values...)})
		case c.Field == "label":
			out = append(out, Part{Kind: "label-allow", Values: append([]string(nil), c.Values...)})
		}
	}
	return out
}

func parseClause(raw string) (Clause, bool) {
	var c Clause
	switch {
	case strings.Contains(raw, "!="):
		field, values, ok := strings.Cut(raw, "!=")
		if !ok {
			return c, false
		}
		c.Field, c.Negate, c.Values = strings.TrimSpace(field), true, splitValues(values)
	case strings.Contains(raw, "<<="):
		field, values, ok := strings.Cut(raw, "<<=")
		if !ok {
			return c, false
		}
		c.Field, c.LTE, c.Values = strings.TrimSpace(field), true, splitValues(values)
	case strings.Contains(raw, ">>="):
		field, values, ok := strings.Cut(raw, ">>=")
		if !ok {
			return c, false
		}
		c.Field, c.GTE, c.Values = strings.TrimSpace(field), true, splitValues(values)
	case strings.Contains(raw, "<="):
		field, values, ok := strings.Cut(raw, "<=")
		if !ok {
			return c, false
		}
		c.Field, c.LTE, c.Values = strings.TrimSpace(field), true, splitValues(values)
	case strings.Contains(raw, ">="):
		field, values, ok := strings.Cut(raw, ">=")
		if !ok {
			return c, false
		}
		c.Field, c.GTE, c.Values = strings.TrimSpace(field), true, splitValues(values)
	default:
		field, values, ok := strings.Cut(raw, "=")
		if !ok {
			return c, false
		}
		c.Field, c.Values = strings.TrimSpace(field), splitValues(values)
	}
	switch c.Field {
	case "contentRating", "label":
		return c, true
	default:
		return Clause{}, false
	}
}

func splitValues(raw string) []string {
	if decoded, err := url.QueryUnescape(raw); err == nil {
		raw = decoded
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
