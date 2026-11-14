package catalog

import (
	"cmp"
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"slices"
	"strings"
)

// consoleMergedKeys takes only the first offset+limit keys from each type's
// ordered index, then merges those bounded lists using every requested sort.
// This keeps mixed movie/series queries from sorting the whole catalogue.
func (c *Catalog) consoleMergedKeys(ctx context.Context, where string, args []any, q core.BrowseQuery, offset, limit int) ([]core.RatingKey, error) {
	if keys, ok, err := c.rankMergedKeys(ctx, where, args, q, offset, limit); ok || err != nil {
		return keys, err
	}
	specs := append([]core.SortSpec{{Sort: q.Sort, Desc: q.Desc}}, q.SecondarySorts...)
	columns := []string{"t.key"}
	for _, s := range specs {
		columns = append(columns, sortColumn("t", s.Sort))
	}
	type row struct {
		key  core.RatingKey
		vals []any
	}
	var all []row
	hint := c.topicIndexHint(q.Filter)
	if store.DialectOf(c.db) != store.Postgres && !consolePositiveEvidence(q.Filter) && !consoleHasSparseCandidates(q.Filter) {
		if q.Sort == core.SortPopularity && len(q.SecondarySorts) == 0 {
			hint = c.indexHint("catalog_titles_console_pop_page")
		}
		if q.Sort == core.SortYear && q.Desc && len(q.SecondarySorts) == 1 && q.SecondarySorts[0].Sort == core.SortTitle && !q.SecondarySorts[0].Desc {
			hint = c.indexHint("catalog_titles_console_year_title_page")
		}
	}
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		typed := q.Filter
		typed.Type = typ
		typedWhere, typedArgs, err := c.browseWhere(typed)
		if err != nil {
			return nil, err
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+strings.Join(columns, ",")+` FROM catalog_titles t`+hint+` WHERE `+typedWhere+` AND t.type=? ORDER BY `+browseOrder(q)+`,t.key ASC LIMIT ?`, append(append([]any{}, typedArgs...), string(typ), offset+limit)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key int64
			vals := make([]any, len(specs))
			dest := []any{&key}
			for i := range vals {
				dest = append(dest, &vals[i])
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, err
			}
			all = append(all, row{core.RatingKey(key), vals})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(all, func(a, b row) int {
		for i, s := range specs {
			if v := compareSQL(a.vals[i], b.vals[i]); v != 0 {
				if s.Desc {
					return -v
				}
				return v
			}
		}
		return cmp.Compare(a.key, b.key)
	})
	if offset >= len(all) {
		return nil, nil
	}
	out := make([]core.RatingKey, 0, limit)
	for _, r := range all[offset:min(len(all), offset+limit)] {
		out = append(out, r.key)
	}
	return out, nil
}
