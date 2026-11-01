package catalog

import (
	"context"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A series' date added is its latest aired episode, so sorting series by it
// computes that for every series in the filter. The newest-first page, which
// every Recently Added row asks for, is read from the aired-episode index
// instead: walking episodes newest first meets each series at its date
// added. Only series with no aired episode are dated by their premiere, and
// those that can reach the page premiered no earlier than the page's last
// date, so a short range read finds them.
func init() {
	store.Register("catalog", 21, `CREATE INDEX catalog_titles_episode_recent ON catalog_titles(released_ts DESC, key, show_key) WHERE type='episode' AND release_known=1;`)
}

// recentWalkBatch is the episodes read per step of the walk.
var recentWalkBatch = 256

const (
	// recentWalkMax bounds the walk. A filter that admits few series makes
	// the walk long; past this the sorted query, which filters first, is
	// cheaper.
	recentWalkMax = 50_000
	// recentWalkMinTotal is the filtered total from which the walk is used.
	// Below it the sorted query dates few series.
	recentWalkMinTotal = 1000
)

type addedKey struct {
	key   core.RatingKey
	added int64
}

// recentShowsPage returns the keys of the series page [offset, offset+limit)
// ordered by date added, newest first, then key, as the sorted query orders
// it, and each one's date added. ok is false when the walk does not apply
// and the caller should run the sorted query.
func (c *Catalog) recentShowsPage(ctx context.Context, where string, args []any, asOf time.Time, offset, limit int) ([]addedKey, bool, error) {
	n := offset + limit
	now := asOf.Unix()
	seen := map[core.RatingKey]bool{}
	var found []addedKey
	cursorTS, cursorKey := now+1, int64(0)
	walked := 0
	// cutoff is the page's last date added, once haveCutoff; dates before
	// 1970 are negative.
	var cutoff int64
	haveCutoff := false
	for {
		rows, err := c.db.QueryContext(ctx, `SELECT key, show_key, released_ts FROM catalog_titles WHERE type='episode' AND `+knownReleaseSQL+` AND released_ts<=? AND (released_ts<? OR (released_ts=? AND key>?)) ORDER BY released_ts DESC, key ASC LIMIT ?`, now, cursorTS, cursorTS, cursorKey, recentWalkBatch)
		if err != nil {
			return nil, false, err
		}
		var batch []addedKey
		read := 0
		for rows.Next() {
			var key, show, ts int64
			if err := rows.Scan(&key, &show, &ts); err != nil {
				rows.Close()
				return nil, false, err
			}
			read++
			cursorTS, cursorKey = ts, key
			if haveCutoff && ts < cutoff {
				break
			}
			if k := core.RatingKey(show); show > 0 && !seen[k] {
				seen[k] = true
				batch = append(batch, addedKey{key: k, added: ts})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, false, err
		}
		rows.Close()
		walked += read
		admitted, err := c.admitShows(ctx, where, args, batch)
		if err != nil {
			return nil, false, err
		}
		found = append(found, admitted...)
		if !haveCutoff && len(found) >= n {
			// Keep reading the episodes that tie the page's last date:
			// the sorted query breaks those ties by series key.
			cutoff, haveCutoff = found[n-1].added, true
		}
		if read < recentWalkBatch || (haveCutoff && cursorTS < cutoff) {
			break
		}
		if walked >= recentWalkMax {
			return nil, false, nil
		}
	}
	// Series with no aired episode are dated by their premiere (0 when it is
	// unknown). When every series with an aired episode is on the page, all
	// of them can reach it.
	q := `SELECT t.key, CASE WHEN t.release_known=1 THEN t.released_ts ELSE 0 END FROM catalog_titles t WHERE ` + where + ` AND NOT EXISTS (SELECT 1 FROM catalog_titles e WHERE e.type='episode' AND e.show_key=t.key AND e.release_known=1 AND e.released_ts<=?)`
	qa := append(append([]any{}, args...), now)
	if haveCutoff {
		q += ` AND t.released_ts>=?`
		qa = append(qa, cutoff)
	}
	rows, err := c.db.QueryContext(ctx, q, qa...)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var a addedKey
		var key int64
		if err := rows.Scan(&key, &a.added); err != nil {
			rows.Close()
			return nil, false, err
		}
		a.key = core.RatingKey(key)
		if !seen[a.key] {
			found = append(found, a)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	rows.Close()
	slices.SortFunc(found, func(a, b addedKey) int {
		if a.added != b.added {
			if a.added > b.added {
				return -1
			}
			return 1
		}
		if a.key < b.key {
			return -1
		}
		if a.key > b.key {
			return 1
		}
		return 0
	})
	if offset >= len(found) {
		return []addedKey{}, true, nil
	}
	return found[offset:min(n, len(found))], true, nil
}

// admitShows keeps the series of batch that pass the browse filter, in
// batch order.
func (c *Catalog) admitShows(ctx context.Context, where string, args []any, batch []addedKey) ([]addedKey, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	q := `SELECT t.key FROM catalog_titles t WHERE ` + where + ` AND t.key IN (` + placeholders(len(batch)) + `)`
	qa := append([]any{}, args...)
	for _, a := range batch {
		qa = append(qa, a.key)
	}
	rows, err := c.db.QueryContext(ctx, q, qa...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pass := map[core.RatingKey]bool{}
	for rows.Next() {
		var key int64
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		pass[core.RatingKey(key)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := batch[:0]
	for _, a := range batch {
		if pass[a.key] {
			out = append(out, a)
		}
	}
	return out, nil
}
