package plex

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// /library/sections/{id}/query/parse translates a section browse URL's
// parameters into PMS's query tree. Plex Web's Advanced Filters editor asks
// for it before it renders (#278): it replaces /all with /query/parse on the
// current view and spins until the tree arrives, so it opens pre-filled.
//
// Query rules preserve parameter order, scopes and parentheses. Operators are
// encoded in parameter keys; numeric and date values are not evaluated here.

type queryRule struct {
	Type       string      `json:"type,omitempty" plex:"type,omitempty"`
	Field      string      `json:"field,omitempty" plex:"field,omitempty"`
	Operator   string      `json:"operator" plex:"operator"`
	FieldTitle *string     `json:"fieldTitle,omitempty" plex:"fieldTitle,omitempty"`
	Value      *string     `json:"value,omitempty" plex:"value,omitempty"`
	ValueTitle *string     `json:"valueTitle,omitempty" plex:"valueTitle,omitempty"`
	Rule       []queryRule `json:"Rule,omitempty" plex:"Rule"`
	joins      []string
	next       string
	pending    bool
	invalid    string
}

type queryOrder struct {
	Field     string `json:"field" plex:"field"`
	Ascending bool   `json:"ascending" plex:"ascending"`
}

type queryLimit struct {
	Limit int `plex:"limit"`
}
type queryGroup struct {
	Field string `plex:"field"`
}
type queryError struct {
	Type     string `plex:"type"`
	Value    string `plex:"value"`
	Expected string `plex:"expected,omitempty"`
}
type parsedQuery struct {
	Type   int          `plex:"type"`
	Rule   *queryRule   `plex:"Rule,object"`
	Order  []queryOrder `plex:"Order"`
	Limit  []queryLimit `plex:"Limit"`
	Group  []queryGroup `plex:"Group"`
	Error  []queryError `plex:"Error"`
	status int          `plex:"-"`
}

func (parsedQuery) XMLElementName() string { return "Query" }

// queryFieldTitles names fields PMS reports that the section Meta does not list.
var queryFieldTitles = map[string]string{
	"addedAt":      "Date Added",
	"lastViewedAt": "Last Played",
	"viewCount":    "Plays",
	"label":        "Label",
	"resolution":   "Resolution",
	"producer":     "Producer",
	"rating":       "Critic Rating",
	"userRating":   "Rating",
}

func (s *Server) handleSectionQueryParse(w http.ResponseWriter, r *http.Request, sec Section) {
	typ := typeNumber(sec.Type)
	if values, ok := r.URL.Query()["type"]; ok {
		var err error
		typ, err = strconv.Atoi(values[0])
		if err != nil {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
	}
	titles := map[string]string{}
	for _, f := range s.metaType(sec, sec.Type, "").Field {
		titles[stripFieldTypePrefix(f.Key)] = f.Title
	}
	for k, v := range queryFieldTitles {
		if _, ok := titles[k]; !ok {
			titles[k] = v
		}
	}
	q := parseQueryTree(r.URL.RawQuery, typ, titles)
	if q.status != 0 {
		writeHTTPError(w, q.status)
		return
	}
	tags := map[string]bool{}
	for _, f := range s.metaType(sec, sec.Type, "").Field {
		if f.Type == "tag" {
			switch stripFieldTypePrefix(f.Key) {
			case "genre", "collection", "director", "writer", "producer", "actor", "country", "label", "network":
				tags[stripFieldTypePrefix(f.Key)] = true
			}
		}
	}
	var addTitles func(*queryRule)
	addTitles = func(rule *queryRule) {
		if rule == nil {
			return
		}
		for i := range rule.Rule {
			addTitles(&rule.Rule[i])
		}
		field := stripFieldTypePrefix(rule.Field)
		if tags[field] && rule.Value != nil {
			if _, err := strconv.Atoi(*rule.Value); err != nil {
				return
			}
			v := s.facetTitle(r.Context(), sec.Type, field, *rule.Value)
			if v != *rule.Value {
				rule.ValueTitle = &v
			}
		}
	}
	addTitles(q.Rule)
	s.write(w, r, &q)
}

// parseQueryTree retains the order of parameters, including nested groups.
func parseQueryTree(rawQuery string, typ int, titles map[string]string) parsedQuery {
	q := parsedQuery{Type: typ}
	// Groups are built in parameter order, which url.Values loses.
	stack := []*queryRule{{Type: "composite", Operator: "and"}}
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawVal, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		val, err := url.QueryUnescape(rawVal)
		if err != nil {
			continue
		}
		top := stack[len(stack)-1]
		switch {
		case key == "type":
			if n, err := strconv.Atoi(val); err == nil {
				q.Type = n
			}
		case key == "sort":
			for _, term := range strings.Split(val, ",") {
				parts := strings.Split(strings.TrimSpace(term), ":")
				field := parts[0]
				if field != "" {
					if titles != nil && !querySortKnown(field) {
						q.status = http.StatusInternalServerError
					}
					asc := true
					for _, mod := range parts[1:] {
						if mod == "desc" {
							asc = false
						}
					}
					q.Order = append(q.Order, queryOrder{Field: field, Ascending: asc})
				}
			}
		case key == "limit":
			n, err := strconv.Atoi(val)
			if err != nil {
				q.status = http.StatusInternalServerError
			} else {
				q.Limit = append(q.Limit, queryLimit{n})
			}
		case key == "group":
			q.Group = append(q.Group, queryGroup{val})
		case key == "push":
			stack = append(stack, &queryRule{Type: "composite", Operator: "and"})
		case key == "pop":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
				parent := stack[len(stack)-1]
				if g := finishQueryGroup(top, titles != nil, &q); g != nil {
					appendQueryRule(parent, *g)
				}
			}
		case key == "or" || key == "and":
			if len(top.Rule) == 0 {
				top.invalid = key
			}
			top.next = key
			top.pending = true
		case queryParamIgnored(key) && (titles == nil || (key != "checkFiles" && key != "asyncCheckFiles")):
		default:
			field := strings.TrimRight(key, "!<>=")
			if field == "" {
				continue
			}
			op := key[len(field):] + "="
			name := stripFieldTypePrefix(field)
			if titles != nil && !queryFieldKnown(name, op, titles) {
				q.Error = append(q.Error, queryError{Type: "field", Value: key})
				continue
			}
			title := titles[name]
			scope := ""
			if titles != nil && name != field {
				scope = strings.TrimSuffix(field, "."+name)
				field = name
				if title != "" {
					title = strings.ToUpper(scope[:1]) + scope[1:] + " " + title
				}
			}
			if q.Type == 3 {
				title = ""
			}
			v := val
			appendQueryRule(top, queryRule{Type: scope, Field: field, Operator: op, FieldTitle: &title, Value: &v})
		}
	}
	// Preserve usable rules while reporting the unfinished group.
	if len(stack) > 1 {
		q.Error = append(q.Error, queryError{Type: "mismatch", Value: "eof", Expected: "pop"})
	}
	for len(stack) > 1 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if g := finishQueryGroup(top, titles != nil, &q); g != nil {
			appendQueryRule(stack[len(stack)-1], *g)
		}
	}
	q.Rule = finishQueryGroup(stack[0], titles != nil, &q)
	return q
}

// appendQueryRule retains each separator so AND binds more tightly than OR.
func appendQueryRule(g *queryRule, r queryRule) {
	if len(g.Rule) > 0 {
		op := g.next
		if op == "" {
			op = "and"
		}
		g.joins = append(g.joins, op)
	}
	g.Rule = append(g.Rule, r)
	g.next, g.pending = "", false
}
func finishQueryGroup(g *queryRule, strict bool, q *parsedQuery) *queryRule {
	if strict && g.invalid != "" {
		q.Error = append(q.Error, queryError{Type: "field", Value: g.invalid})
		return nil
	}
	if strict && g.pending && len(g.Rule) > 0 {
		last := g.Rule[len(g.Rule)-1]
		appendQueryRule(g, last)
		q.Error = append(q.Error, queryError{Type: "field", Value: "eof"})
	}
	if len(g.Rule) == 0 {
		return nil
	}
	var alternatives []queryRule
	start := 0
	for i := 0; i < len(g.Rule); i++ {
		if i == len(g.Rule)-1 || g.joins[i] == "or" {
			part := queryRule{Type: "composite", Operator: "and", Rule: g.Rule[start : i+1]}
			alternatives = append(alternatives, *collapseQueryGroup(part))
			start = i + 1
		}
	}
	return collapseQueryGroup(queryRule{Type: "composite", Operator: "or", Rule: alternatives})
}

// collapseQueryGroup drops an empty group and unwraps a group of one rule.
func collapseQueryGroup(g queryRule) *queryRule {
	switch len(g.Rule) {
	case 0:
		return nil
	case 1:
		return &g.Rule[0]
	}
	return &g
}

// queryParamIgnored reports parameters that shape a response rather than
// filter it, which PMS leaves out of the query tree.
func queryParamIgnored(key string) bool {
	if strings.HasPrefix(key, "X-Plex-") || strings.HasPrefix(key, "include") || strings.HasPrefix(key, "exclude") {
		return true
	}
	switch key {
	case "limit", "group", "checkFiles", "asyncCheckFiles", "skipRefresh", "asyncRefreshLocalMediaAgent", "asyncRefreshAnalysis", "X-Plex-Container-Start", "X-Plex-Container-Size":
		return true
	}
	return false
}

// These additional columns are queryable even when absent from the section Meta.
func queryFieldKnown(field, op string, titles map[string]string) bool {
	switch field {
	case "genre", "collection", "director", "writer", "producer", "actor", "country", "label", "network":
		return op == "=" || op == "!="
	}
	switch field {
	case "title", "studio", "titleSort", "summary", "tagline", "editionTitle":
		return op == "=" || op == "!=" || op == "==" || op == "!==" || op == "<=" || op == ">="
	}
	if _, ok := titles[field]; ok {
		return true
	}
	switch field {
	case "id", "index", "parentIndex", "titleSort", "summary", "tagline", "editionTitle", "guid", "videoCodec", "audioCodec", "subtitleCodec", "audioLayout", "audioLanguage", "subtitleLanguage", "location", "hdr", "dovi", "hdr10plus", "atmos", "unmatched", "unwatchedLeaves", "mediaSize", "mediaBitrate", "mediaHeight", "viewOffset", "skipCount", "lastRatedAt", "lastSkippedAt", "viewedLeafCount", "viewedOrInProgressLeafCount", "unviewedLeafCount", "contentRatingAge":
		return true
	}
	return false
}
func querySortKnown(field string) bool {
	field = stripFieldTypePrefix(field)
	if _, _, _, ok := sortSpecFor(field, ""); ok {
		return true
	}
	switch field {
	case "id", "index", "contentRating", "mediaBitrate", "mediaHeight", "viewOffset":
		return true
	}
	return false
}
