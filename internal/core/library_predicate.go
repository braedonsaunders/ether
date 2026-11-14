package core

import (
	"encoding/json"
	"sort"
)

// LibraryPredicateConditions converts a browse predicate to the shared rules
// language. Every persisted field is retained; nested boolean groups become
// nested rule groups. An empty result means the source needs no visibility
// rule. There is no saved library predicate after conversion.
func LibraryPredicateConditions(f Filter) []CollectionRule {
	f.Type = ""
	f.CatalogueSources = nil
	raw, _ := json.Marshal(f)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return predicateFields(fields)
}
func predicateFields(fields map[string]json.RawMessage) []CollectionRule {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []CollectionRule
	for _, k := range keys {
		v := fields[k]
		if k == "genres" {
			out = append(out, CollectionRule{Field: "genre", Op: "in", Value: v})
			continue
		}
		if k == "group" {
			var g FilterGroup
			_ = json.Unmarshal(v, &g)
			r := predicateGroup(g)
			if !emptyPredicateGroup(r) {
				out = append(out, r)
			}
			continue
		}
		out = append(out, CollectionRule{Field: "browse:" + k, Op: "is", Value: v})
	}
	return out
}
func predicateGroup(g FilterGroup) CollectionRule {
	var rules []CollectionRule
	state := g
	state.Filter = nil
	state.Children = nil
	state.Operator = ""
	raw, _ := json.Marshal(state)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "operator")
	if g.OnWatchlist != nil || g.StateField != "" || g.Unwatched != nil || g.InProgress != nil || g.LastViewedAfter > 0 || g.LastViewedBefore > 0 {
		return CollectionRule{Field: "viewer", Op: "is", Value: raw}
	}
	if g.Filter != nil {
		g.Operator = "and"
		raw, _ := json.Marshal(g.Filter)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		rules = append(rules, predicateFields(fields)...)
	} else {
		for _, child := range g.Children {
			r := predicateGroup(child)
			if emptyPredicateGroup(r) {
				if g.Operator == "or" {
					return r // true OR anything is true
				}
				if g.Operator != "not" {
					continue // true adds nothing to an intersection
				}
			}
			rules = append(rules, r)
		}
	}
	op := "is"
	if g.Operator == "or" {
		op = "in"
	}
	if g.Operator == "not" {
		op = "isNot"
	}
	if len(rules) == 1 && op != "isNot" {
		return rules[0]
	}
	if len(rules) == 0 {
		rules = []CollectionRule{}
	}
	v, _ := json.Marshal(rules)
	return CollectionRule{Field: "group", Op: op, Value: v}
}

// An empty positive group is the constant true, including an empty OR in
// browse predicates. Keep negated groups intact.
func emptyPredicateGroup(r CollectionRule) bool {
	return r.Field == "group" && r.Op != "isNot" && string(r.Value) == "[]"
}
