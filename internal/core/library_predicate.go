package core

import (
	"encoding/json"
	"sort"
)

// LibraryPredicateConditions converts a browse predicate to the shared rules
// language. Every persisted field is retained; nested boolean groups become
// nested rule groups. There is no saved library predicate after conversion.
func LibraryPredicateConditions(f Filter) []CollectionRule {
	f.Type = ""
	f.CatalogueSources = nil
	raw, _ := json.Marshal(f)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	out := predicateFields(fields)
	if len(out) == 0 {
		return []CollectionRule{{Field: "group", Op: "is", Value: json.RawMessage(`[]`)}}
	}
	return out
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
			out = append(out, predicateGroup(g))
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
			rules = append(rules, predicateGroup(child))
		}
	}
	op := "is"
	if g.Operator == "or" {
		op = "in"
	}
	if g.Operator == "not" {
		op = "isNot"
	}
	v, _ := json.Marshal(rules)
	return CollectionRule{Field: "group", Op: op, Value: v}
}
