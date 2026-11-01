package newznab

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type recentFeedMode struct {
	identity   string
	unfiltered bool
	checked    time.Time
}

func recentCategories(cfg config.Indexer) []int {
	movies, tv := cfg.MovieCategories, cfg.TVCategories
	if len(movies) == 0 {
		movies = []int{2000}
	}
	if len(tv) == 0 {
		tv = []int{5000}
	}
	return append(append([]int{}, movies...), tv...)
}

// Some Newznab implementations return an empty success for comma-separated
// categories even though their single-category and unrestricted feeds work.
// On an empty first page, probe once without cat (one ordinary budgeted hit).
// Remember a working unrestricted mode per media side and identity, and
// filter categories at ingestion; independent movie/TV offsets must never be
// merged into a fabricated cursor.
func (c *Client) recent(ctx context.Context, media identityMedia, offset, limit int) ([]core.Release, error) {
	c.feedMu.Lock()
	defer c.feedMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := c.Config()
	verb := "search"
	cats := recentCategories(cfg)
	switch media {
	case mediaTV:
		verb = "tvsearch"
		cats = cfg.TVCategories
		if len(cats) == 0 {
			cats = []int{5000}
		}
	case mediaMovie:
		verb = "movie"
		cats = cfg.MovieCategories
		if len(cats) == 0 {
			cats = []int{2000}
		}
	}
	ident, _ := c.identityFor(purposeSearch, media)
	// The row is per media side: a split indexer keeps one learned mode for
	// TV and one for movies. The identity string binds it to the account
	// (URL, key, categories) and its resolved client identity.
	rowID := c.indexerID() + "/" + strconv.Itoa(int(media))
	key := searchIdentity(cfg) + "|" + ident.name
	memKey := rowID + "/" + key
	if c.feedModes == nil {
		c.feedModes = map[string]recentFeedMode{}
	}
	mode, ok := c.feedModes[memKey]
	if !ok {
		mode = recentFeedMode{identity: key}
		if c.db != nil {
			var unfiltered int
			var checked int64
			err := c.db.QueryRowContext(ctx, `SELECT unfiltered,checked_at FROM newznab_feed_modes WHERE indexer_id=? AND identity=?`, rowID, key).Scan(&unfiltered, &checked)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				mode.unfiltered = unfiltered != 0
				mode.checked = time.Unix(checked, 0)
			}
		}
		c.feedModes[memKey] = mode
	}
	q := url.Values{"t": {verb}, "extended": {"1"}, "offset": {strconv.Itoa(max(0, offset))}, "limit": {strconv.Itoa(min(100, max(1, limit)))}}
	if !mode.unfiltered {
		q.Set("cat", joinCats(cats))
	}
	rs, err := c.doSearch(ctx, q, mediaType(media))
	if err != nil || len(rs) > 0 || offset != 0 || mode.unfiltered || c.now().Sub(mode.checked) < 6*time.Hour {
		return rs, err
	}
	q.Del("cat")
	rs, err = c.doSearch(ctx, q, mediaType(media))
	if err != nil {
		return nil, err
	} // don't call a denied/failed probe an empty feed
	mode = recentFeedMode{identity: key, unfiltered: len(rs) > 0, checked: c.now()}
	if c.db != nil {
		flag := 0
		if mode.unfiltered {
			flag = 1
		}
		_, err = c.db.ExecContext(ctx, `INSERT INTO newznab_feed_modes(indexer_id,identity,unfiltered,checked_at) VALUES(?,?,?,?) ON CONFLICT(indexer_id) DO UPDATE SET identity=excluded.identity,unfiltered=excluded.unfiltered,checked_at=excluded.checked_at`, rowID, key, flag, mode.checked.Unix())
		if err != nil {
			return nil, err
		}
	}
	c.feedModes[memKey] = mode
	return rs, nil
}
