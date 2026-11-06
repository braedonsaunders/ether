package plex

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"net/url"
	"strings"
)

// childQuery applies the ordinary browse predicates to a known subtree before
// its page window. IncludeKeys prevents a children read from querying peers.
func (s *Server) childQuery(ctx context.Context, pr *Request, items []core.Title, q url.Values) ([]core.Title, error) {
	if len(items) == 0 {
		return items, nil
	}
	active := false
	for k := range q {
		name := strings.TrimRight(k, "!<>=")
		if !queryParamIgnored(k) && name != "type" {
			active = true
			break
		}
	}
	if !active {
		return items, nil
	}
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
		return nil, fmt.Errorf("plex: invalid child query")
	}
	br := s.parseBrowse(ctx, sec, typ, pr.User, q, q.Encode())
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
