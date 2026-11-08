package catalog

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// childVisibilityConds matches the policy applied to a hydrated episode.
// An episode inherits its show's rating and keywords only when its own are absent.
func (c *Catalog) childVisibilityConds(p *core.Filter) ([]string, []any) {
	return c.childVisibilityCondsOn(p, episodeVisibilityCols)
}

// childCols names the episode facts childVisibilityConds reads: the stored
// episode (e) or a series' episode rating histogram (h). key is empty where
// per-episode rows are not at hand.
type childCols struct{ rating, rank, collection, key string }

var episodeVisibilityCols = childCols{rating: "e.content_rating", rank: "e.content_rating_rank", collection: "e.collection_name", key: "e.key"}

// histogramVisible reports whether childVisibilityConds needs only the
// facts a rating histogram row keeps.
func (c *Catalog) histogramVisible(p *core.Filter) bool {
	if p == nil {
		return true
	}
	if p.SaferPlexRating && c.saferRating != nil && p.MaxContentRating != "" {
		return false
	}
	return len(p.ExcludeGenres) == 0 && len(p.ExcludeKeywords) == 0
}

func (c *Catalog) childVisibilityCondsOn(p *core.Filter, e childCols) ([]string, []any) {
	if p == nil {
		return nil, nil
	}
	var conds []string
	var args []any
	rank := "(CASE WHEN " + e.rating + "='' THEN t.content_rating_rank ELSE " + e.rank + " END)"
	rating := "LOWER(TRIM(CASE WHEN " + e.rating + "='' THEN t.content_rating ELSE " + e.rating + " END))"
	// Catalogue rules apply to the parent show, not to episodes. Viewer
	// restrictions below still apply to the hydrated episode's own facts.
	if p.MaxContentRating != "" {
		level := contentrating.LevelOf(p.MaxContentRating)
		if level == contentrating.Unrated {
			conds = append(conds, "1=0")
		} else {
			conds = append(conds, rank+">0 AND "+rank+"<=?")
			args = append(args, int(level))
			if p.SaferPlexRating && c.saferRating != nil {
				conds = append(conds, c.saferRating.HideExceeding(e.key))
				args = append(args, int(level))
			}
		}
	}
	if p.MinContentRating != "" {
		level := contentrating.LevelOf(p.MinContentRating)
		if level == contentrating.Unrated {
			conds = append(conds, "1=0")
		} else {
			conds = append(conds, rank+">0 AND "+rank+">=?")
			args = append(args, int(level))
		}
	}
	matchRatings := func(values []string) (string, []any) {
		var parts []string
		var vals []any
		for _, value := range values {
			if strings.EqualFold(value, "NR") || strings.EqualFold(value, "None") {
				parts = append(parts, "("+rating+"='' OR REPLACE(REPLACE(REPLACE(REPLACE("+rating+",' ',''),'-',''),'_',''),'.','') IN ('nr','notrated','unrated'))")
			} else {
				parts = append(parts, rating+"=?")
				vals = append(vals, strings.ToLower(strings.TrimSpace(value)))
			}
		}
		return "(" + strings.Join(parts, " OR ") + ")", vals
	}
	if len(p.ContentRatings) > 0 {
		sql, vals := matchRatings(p.ContentRatings)
		conds = append(conds, sql)
		args = append(args, vals...)
	}
	if len(p.ExcludeContentRatings) > 0 {
		sql, vals := matchRatings(p.ExcludeContentRatings)
		conds = append(conds, "NOT "+sql)
		args = append(args, vals...)
	}
	for _, part := range []struct {
		values []string
		negate bool
	}{{p.IncludeCollectionNames, false}, {p.ExcludeCollectionNames, true}} {
		if len(part.values) == 0 {
			continue
		}
		op := " IN "
		if part.negate {
			op = " NOT IN "
		}
		conds = append(conds, "(TRIM("+e.collection+")='' OR LOWER(TRIM("+e.collection+"))"+op+"("+placeholders(len(part.values))+"))")
		for _, v := range part.values {
			args = append(args, strings.ToLower(strings.TrimSpace(v)))
		}
	}
	for _, part := range []struct {
		table, column string
		values        []string
	}{{"catalog_genres", "genre", p.ExcludeGenres}, {"catalog_keywords", "keyword", p.ExcludeKeywords}} {
		if len(part.values) == 0 {
			continue
		}
		conds = append(conds, "NOT EXISTS (SELECT 1 FROM "+part.table+" cv WHERE cv.key="+e.key+" AND LOWER(TRIM(cv."+part.column+")) IN ("+placeholders(len(part.values))+"))")
		for _, v := range part.values {
			args = append(args, strings.ToLower(strings.TrimSpace(v)))
		}
	}
	return conds, args
}
