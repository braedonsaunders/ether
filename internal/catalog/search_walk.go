package catalog

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A search's local candidates are the n most popular matching titles. The
// sorted query reads every match to rank them, so a common term (or a one-
// or two-letter query, which has no trigram index) ranks a large part of
// the catalogue on every keystroke. When a term matches many titles, the
// popularity index finds the same n titles by walking from the most popular
// down and testing each one, which stops after about n / (match density)
// titles.
const (
	// searchDenseMatches is the match count from which the walk is used.
	searchDenseMatches = 2000
	// searchWalkBatch is the matches read per step of one stream.
	searchWalkBatch = 64
)

// searchStream is one (type, browsable) slice of the popularity index.
type searchStream struct {
	typ       string
	browsable int
	buf       []searchHit
	lastPop   float64
	lastKey   int64
	started   bool
	done      bool
	maxPop    float64
}

type searchHit struct {
	key int64
	pop float64
}

// searchByPopularity returns the keys of the n most popular titles of the
// four searchable types matching query, most popular first, exactly as the
// sorted query would choose and order them. ok is false when the walk does
// not apply or cannot prove that: a sparse term (the sorted query is cheap),
// or popularity ties among the chosen titles or at the cut, where the sorted
// query's order between equals is its own.
func (c *Catalog) searchByPopularity(ctx context.Context, query string, n int, unfiltered bool) ([]core.RatingKey, bool, error) {
	if n <= 0 {
		return nil, false, nil
	}
	like := "%" + escapeLike(query) + "%"
	op := store.Like(store.DialectOf(c.db))
	match := `(title ` + op + ` ? ESCAPE '\' OR original_title ` + op + ` ? ESCAPE '\' OR EXISTS (SELECT 1 FROM catalog_alt_titles a WHERE a.key=catalog_titles.key AND a.title ` + op + ` ? ESCAPE '\'))`
	adult := ""
	if unfiltered {
		adult = ` AND adult=0`
	}
	if utf8.RuneCountInString(query) >= 3 && !strings.ContainsAny(query, `%_\`) {
		// Count matches through the trigram index, up to the threshold.
		fts, args := c.titleMatch("", query)
		var count int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM catalog_titles WHERE type IN ('movie','show','season','episode')`+adult+` AND `+fts+` LIMIT ?)`, append(args, searchDenseMatches)...).Scan(&count); err != nil {
			return nil, false, err
		}
		if count < searchDenseMatches {
			return nil, false, nil
		}
	}
	var streams []*searchStream
	for _, typ := range []string{"movie", "show", "season", "episode"} {
		for _, b := range []int{0, 1} {
			s := &searchStream{typ: typ, browsable: b}
			var max *float64
			if err := c.db.QueryRowContext(ctx, `SELECT MAX(popularity) FROM catalog_titles WHERE type=? AND browsable=?`, typ, b).Scan(&max); err != nil {
				return nil, false, err
			}
			if max == nil {
				continue
			}
			s.maxPop = *max
			streams = append(streams, s)
		}
	}
	bound := func(s *searchStream) float64 {
		if len(s.buf) > 0 {
			return s.buf[0].pop
		}
		if !s.started {
			return s.maxPop
		}
		return s.lastPop
	}
	fetch := func(s *searchStream) error {
		q := `SELECT key, popularity FROM catalog_titles WHERE type=? AND browsable=?` + adult + ` AND ` + match
		args := []any{s.typ, s.browsable, like, like, like}
		if s.started {
			q += ` AND popularity<=? AND (popularity<? OR key>?)`
			args = append(args, s.lastPop, s.lastPop, s.lastKey)
		}
		q += ` ORDER BY popularity DESC, key ASC LIMIT ?`
		rows, err := c.db.QueryContext(ctx, q, append(args, searchWalkBatch)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		read := 0
		for rows.Next() {
			var h searchHit
			if err := rows.Scan(&h.key, &h.pop); err != nil {
				return err
			}
			s.buf = append(s.buf, h)
			s.lastPop, s.lastKey = h.pop, h.key
			read++
		}
		s.started = true
		if read < searchWalkBatch {
			s.done = true
		}
		return rows.Err()
	}
	// Merge the streams most popular first, reading a stream only when its
	// next match could be the next pick, until n+1 picks are made.
	var picked []searchHit
	for len(picked) <= n {
		var best *searchStream
		for _, s := range streams {
			if len(s.buf) == 0 && s.done {
				continue
			}
			if best == nil || bound(s) > bound(best) {
				best = s
			}
		}
		if best == nil {
			break
		}
		if len(best.buf) == 0 {
			if err := fetch(best); err != nil {
				return nil, false, err
			}
			continue
		}
		// best's head is the next pick only if no stream with an unread
		// match could beat or tie it.
		head := best.buf[0]
		ready := true
		for _, s := range streams {
			if s != best && len(s.buf) == 0 && !s.done && bound(s) >= head.pop {
				if err := fetch(s); err != nil {
					return nil, false, err
				}
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		best.buf = best.buf[1:]
		picked = append(picked, head)
	}
	// Equal popularities leave the order to the sorted query.
	for i := 1; i < len(picked); i++ {
		if picked[i].pop == picked[i-1].pop {
			return nil, false, nil
		}
	}
	if len(picked) <= n {
		// Fewer matches than asked for: every match is a pick, so a tie with
		// an unpicked title cannot exist; but a sparse term is the sorted
		// query's case.
		return nil, false, nil
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].pop > picked[j].pop })
	out := make([]core.RatingKey, n)
	for i := range n {
		out[i] = core.RatingKey(picked[i].key)
	}
	return out, true, nil
}
