package plex

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// childQuery applies the ordinary browse predicates to a known subtree before
// its page window. IncludeKeys prevents a children read from querying peers.
func (s *Server) childQuery(ctx context.Context, pr *Request, items []frontend.Title, q url.Values) ([]frontend.Title, error) {
	if len(items) == 0 {
		return items, nil
	}
	// Clients send metadata/refresh options alongside browse predicates. They
	// are not filters (and must not cause a catalogue browse or refresh).
	filters := make(url.Values, len(q))
	active := false
	for k, values := range q {
		if childQueryParamIgnored(k) {
			continue
		}
		filters[k] = values
		if k != "type" {
			active = true
		}
	}
	if !active {
		return items, nil
	}
	q = filters

	typ := items[0].Type
	sec, _ := s.sectionFor(typ)
	titles := map[string]string{}
	for k, v := range queryFieldTitles {
		titles[k] = v
	}
	for _, f := range s.metaType(sec, typ, "").Field {
		titles[stripFieldTypePrefix(f.Key)] = f.Title
	}
	tree := parseQueryTree(q.Encode(), typeNumber(typ), titles)
	if tree.status != 0 || len(tree.Error) > 0 {
		// Never log the raw query or error values: requests can carry tokens.
		s.log.WarnContext(ctx, "invalid child query", "parameters", rejectedChildQueryParams(q, tree))
		return nil, fmt.Errorf("plex: invalid child query")
	}
	// Index sorting is meaningful within this subtree, not across all shows.
	browseQ := q.Clone()
	browseQ.Del("sort")
	br := s.parseBrowse(ctx, sec, typ, pr.User, browseQ, browseQ.Encode())
	br.applySortParam(q.Get("sort"), typ, true)
	if br.invalidSort {
		// Browse also validates predicate values and supported sort columns.
		// Inspect each parameter only on failure to identify names safely.
		var names []string
		for key, values := range q {
			one := s.parseBrowse(ctx, sec, typ, pr.User, url.Values{key: values})
			if key == "sort" {
				one = browseRequest{}
				one.applySortParam(q.Get("sort"), typ, true)
			}
			if one.invalidSort {
				names = append(names, key)
			}
		}
		slices.Sort(names)
		s.log.WarnContext(ctx, "invalid child query", "parameters", names)
		return nil, fmt.Errorf("plex: invalid child query")
	}
	for _, it := range items {
		br.q.Filter.IncludeKeys = append(br.q.Filter.IncludeKeys, it.Key)
	}
	bounded := *pr
	bounded.HasPage = true
	bounded.Start = 0
	bounded.Size = len(items)
	found, _, _, err := s.browse(ctx, &bounded, br)
	return found, err
}

// These are request options, not catalogue fields. Keep this children-specific:
// the section query editor has its own PMS-compatible validation semantics.
func childQueryParamIgnored(key string) bool {
	// skipCount is a watch predicate, despite sharing the skip option prefix.
	if key == "limit" || key == "group" || strings.TrimRight(key, "!<>=") == "skipCount" {
		return false
	}
	return queryParamIgnored(key) || strings.HasPrefix(key, "async") ||
		strings.HasPrefix(key, "check") || strings.HasPrefix(key, "skip")
}

func rejectedChildQueryParams(q url.Values, tree parsedQuery) []string {
	names := map[string]bool{}
	for _, e := range tree.Error {
		if e.Type == "field" && q.Has(e.Value) {
			names[e.Value] = true
		} else {
			// An unfinished group or trailing conjunction has no field name.
			for _, key := range []string{"push", "pop", "and", "or"} {
				if q.Has(key) {
					names[key] = true
				}
			}
		}
	}
	for _, value := range q["sort"] {
		for _, term := range strings.Split(value, ",") {
			field, _, _ := strings.Cut(strings.TrimSpace(term), ":")
			if field != "" && !querySortKnown(field) {
				names["sort"] = true
			}
		}
	}
	for _, value := range q["limit"] {
		if _, err := strconv.Atoi(value); err != nil {
			names["limit"] = true
		}
	}
	out := make([]string, 0, len(names))
	for key := range names {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
