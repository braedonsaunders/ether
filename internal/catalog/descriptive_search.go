package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// DescribeCandidates uses the same catalogue rule compiler as collection and
// library rules. There is no COUNT, external discovery, or library snapshot.
// The two indexed popularity walks are one statement and return at most limit
// candidates for viewer filtering and taste ranking in the shared service.
func (c *Catalog) DescribeCandidates(ctx context.Context, sp core.CollectionSpec, limit int) ([]core.Title, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if len(sp.Sources) == 0 {
		return nil, fmt.Errorf("descriptive search has no source")
	}
	// A source union is expressible through the existing browse predicates.
	// Unsupported remote lists fail honestly instead of fetching on search.
	var sources []core.CollectionRule
	wholeCatalog := false
	for _, src := range sp.Sources {
		source := core.Filter{}
		switch src.Kind {
		case "catalog":
			wholeCatalog = true
		case "tmdbKeyword":
			if src.Label == "" {
				return nil, fmt.Errorf("keyword source needs its resolved name")
			}
			source.Keywords = []string{src.Label}
		case "tmdbCompany", "tmdbNetwork":
			if src.Label == "" {
				return nil, fmt.Errorf("studio source needs its resolved name")
			}
			source.Studio = src.Label
		case "tmdbCollection":
			if src.ID <= 0 {
				return nil, fmt.Errorf("collection source needs its resolved id")
			}
			source.Collection = src.ID
		case "tmdbPerson":
			if src.ID <= 0 {
				return nil, fmt.Errorf("person source needs its resolved id")
			}
			source.Person = src.ID
		default:
			return nil, fmt.Errorf("descriptive search source %q is not a local catalogue source", src.Kind)
		}
		raw, _ := json.Marshal(source)
		sources = append(sources, core.CollectionRule{Field: "viewer", Op: "is", Value: raw})
	}
	f := core.Filter{ExtraFallback: "hide"}
	if sp.MediaType == "movie" || sp.MediaType == "show" {
		f.Type = core.MediaType(sp.MediaType)
	}
	// Exclusions precede the allow group: ordered first-match rule semantics.
	if sp.Sort == "rating" {
		f.ExtraRules = append(f.ExtraRules, core.LibraryExclusion{Action: "hide", MediaType: sp.MediaType, Match: "all", Rules: []core.CollectionRule{{Field: "votes", Op: "lte", Value: json.RawMessage("19")}}})
	}
	if len(sp.Exclude) > 0 {
		f.ExtraRules = append(f.ExtraRules, core.LibraryExclusion{Action: "hide", MediaType: sp.MediaType, Match: "any", Rules: sp.Exclude})
	}
	if len(sp.Rules) == 0 {
		sp.Rules = []core.CollectionRule{{Field: "votes", Op: "gte", Value: json.RawMessage("0")}}
	}
	rules := sp.Rules
	match := sp.Match
	if !wholeCatalog {
		op := "is"
		if match == "any" {
			op = "in"
		}
		raw, _ := json.Marshal(rules)
		sourceRaw, _ := json.Marshal(sources)
		rules = []core.CollectionRule{{Field: "group", Op: op, Value: raw}, {Field: "group", Op: "in", Value: sourceRaw}}
		match = "all"
	}
	f.ExtraRules = append(f.ExtraRules, core.LibraryExclusion{Action: "show", MediaType: sp.MediaType, Match: match, Rules: rules})
	f = c.viewerPaging(ctx, f)
	where, args, err := c.browseWhere(f)
	if err != nil {
		return nil, err
	}
	q := core.BrowseQuery{Filter: f, Sort: core.SortPopularity, Desc: true, NoTotal: true, Limit: limit}
	switch core.SortOrder(sp.Sort) {
	case core.SortRating, core.SortReleased, core.SortTitle:
		q.Sort = core.SortOrder(sp.Sort)
		q.Desc = sp.SortDesc
	}
	orderDirection := "ASC"
	if q.Sort != core.SortPopularity && q.Desc {
		orderDirection = "DESC"
	}
	// Reuse the rank planner when ready. Both media types remain in one SQL
	// statement, with each subquery bounded before merging the candidates.
	var parts []string
	var params []any
	types := []core.MediaType{core.TypeMovie, core.TypeShow}
	if f.Type != "" {
		types = []core.MediaType{f.Type}
	}
	for _, typ := range types {
		keys, _, ok := c.rankKeysQuery(ctx, q, where, typ)
		if !ok {
			column := sortColumn("t", q.Sort)
			if q.Sort == core.SortPopularity {
				column = "-t.popularity"
			}
			keys = `SELECT t.key AS page_key,` + column + ` AS page_rk FROM catalog_titles t WHERE ` + where + ` AND t.type='` + string(typ) + `' ORDER BY page_rk ` + orderDirection + `,t.key ASC LIMIT ? OFFSET ?`
		}
		parts = append(parts, `SELECT * FROM (`+keys+`) dp`)
		params = append(params, args...)
		params = append(params, limit, 0)
	}
	sql := `SELECT ` + prefixedCols("t") + ` FROM (` + strings.Join(parts, " UNION ALL ") + `) p JOIN catalog_titles t ON t.key=p.page_key ORDER BY p.page_rk ` + orderDirection + `,p.page_key ASC LIMIT ?`
	params = append(params, limit)
	return c.queryPagedTitles(ctx, sql, params...)
}

var _ core.DescriptiveCatalog = (*Catalog)(nil)
