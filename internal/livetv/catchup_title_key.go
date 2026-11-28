package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

// Every guide row carries its catch-up title key (frontend.CatchUpTitleKey),
// so the airings of a page of catch-up shows are one indexed lookup instead
// of a read of the whole archive window. The guide import writes it and
// re-keys a stored row that lacks one; this migration, and a restore of a
// dump from before it, fill it in for rows already stored.
const catchUpTitleKeyDDL = `ALTER TABLE livetv_programmes ADD COLUMN title_key TEXT;
CREATE INDEX livetv_programmes_title ON livetv_programmes(title_key, start_ms);`

const (
	// catchUpTitleKeyPage is how many rows the backfill reads at a time.
	catchUpTitleKeyPage = 1000
	// catchUpTitleKeyBatch is how many rows one key update writes (four
	// parameters each, under SQLite's old default limit of 999).
	catchUpTitleKeyBatch = 200
)

func init() {
	store.Register("livetv", 30, catchUpTitleKeyDDL)
	store.RegisterApply("livetv", 30, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, catchUpTitleKeyDDL); err != nil {
			return err
		}
		return backfillCatchUpTitleKeys(ctx, tx)
	})
	// A dump from before livetv 30 restores its guide rows without keys.
	store.RegisterRestore("livetv_title_keys", func(context.Context, *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		return backfillCatchUpTitleKeys, nil
	})
}

// programmeTitleKey is the title key stored with an encoded guide row.
func programmeTitleKey(title string) string {
	return frontend.CatchUpTitleKey(title)
}

// storedTitleKey is the title key of a stored guide row's data; ok is false
// when the data is not a readable airing.
func storedTitleKey(data []byte) (string, bool) {
	var p struct {
		Item struct {
			Title string `json:"title"`
		} `json:"item"`
	}
	if json.Unmarshal(data, &p) != nil {
		return "", false
	}
	return programmeTitleKey(p.Item.Title), true
}

// titleKeyRow is one guide row's key to store.
type titleKeyRow struct {
	source, channel, key string
	start                int64
}

// catchUpTitleKeyPageSQL reads the next page of unkeyed rows in primary-key
// order. Testing an expression of title_key, not the column, keeps SQLite
// off the title index (all unkeyed rows, re-sorted for every page) and on the
// primary key.
const catchUpTitleKeyPageSQL = `SELECT source_id, channel_id, start_ms, data FROM livetv_programmes
	WHERE title_key||'' IS NULL AND (source_id, channel_id, start_ms) > (?, ?, ?)
	ORDER BY source_id, channel_id, start_ms LIMIT ?`

// backfillCatchUpTitleKeys stores the key of every guide row that has none,
// a page of rows at a time in primary-key order. A row whose data is not a
// readable airing keeps no key.
func backfillCatchUpTitleKeys(ctx context.Context, tx *sql.Tx) error {
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM livetv_programmes WHERE title_key IS NULL`).Scan(&total); err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	done := 0
	store.WorkProgress(ctx, "catchup_title_keys", done, total)
	var last *titleKeyRow
	for {
		args := []any{"", "", int64(math.MinInt64), catchUpTitleKeyPage}
		if last != nil {
			args = []any{last.source, last.channel, last.start, catchUpTitleKeyPage}
		}
		rows, err := tx.QueryContext(ctx, catchUpTitleKeyPageSQL, args...)
		if err != nil {
			return err
		}
		var page []titleKeyRow
		read := 0
		for rows.Next() {
			var r titleKeyRow
			var data []byte
			if err = rows.Scan(&r.source, &r.channel, &r.start, &data); err != nil {
				break
			}
			read++
			last = &titleKeyRow{source: r.source, channel: r.channel, start: r.start}
			var ok bool
			if r.key, ok = storedTitleKey(data); ok {
				page = append(page, r)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if read == 0 {
			return nil
		}
		if err := updateTitleKeys(ctx, tx, page); err != nil {
			return err
		}
		done += read
		store.WorkProgress(ctx, "catchup_title_keys", done, total)
	}
}

// updateTitleKeys stores rows' keys in batches.
func updateTitleKeys(ctx context.Context, tx *sql.Tx, rows []titleKeyRow) error {
	for batch := range slices.Chunk(rows, catchUpTitleKeyBatch) {
		values := make([]string, 0, len(batch))
		args := make([]any, 0, 4*len(batch))
		for _, r := range batch {
			values = append(values, "(CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT),CAST(? AS TEXT))")
			args = append(args, r.source, r.channel, r.start, r.key)
		}
		if _, err := tx.ExecContext(ctx, `WITH v(s, c, st, k) AS (VALUES `+strings.Join(values, ",")+`)
			UPDATE livetv_programmes SET title_key=(SELECT v.k FROM v WHERE v.s=livetv_programmes.source_id AND v.c=livetv_programmes.channel_id AND v.st=livetv_programmes.start_ms)
			WHERE (source_id, channel_id, start_ms) IN (SELECT s, c, st FROM v)`, args...); err != nil {
			return err
		}
	}
	return nil
}

const (
	// catchUpKeyBatch is how many title keys one airing lookup names.
	catchUpKeyBatch = 200
	// catchUpGuideFilter is how many archive guide channels the lookup names
	// in SQL (two parameters each, with up to catchUpKeyBatch keys well under
	// SQLite's old default limit of 999).
	catchUpGuideFilter = 350
)

// catchUpGroupAiringsSQL is the airing lookup for one title filter (with its
// start bounds) and the optional guide channel filter.
func catchUpGroupAiringsSQL(filter, guideFilter string) string {
	return `SELECT source_id, channel_id, data FROM livetv_programmes
			WHERE ` + filter + guideFilter + ` AND end_ms<=? AND end_ms>start_ms`
}

// catchUpGroupAirings returns, for each show or season id in groups, the
// airings in it this viewer may play, newest first. It reads only the guide
// rows of those titles inside the archive window (the title_key index), never
// the whole window, so its cost follows the groups asked for. view is the
// viewer's catch-up view when the caller already read it, or nil.
func (h liveTVHost) catchUpGroupAirings(ctx context.Context, u *core.User, view *catchUpView, groups []string) (map[string][]Programme, error) {
	whole := make(map[string][]string)  // title key -> group ids
	prefix := make(map[string][]string) // GUID key prefix -> group ids
	seen := make(map[string]bool, len(groups))
	for _, id := range groups {
		if seen[id] {
			continue
		}
		seen[id] = true
		if key, ok := frontend.CatchUpGroupKey(id); ok {
			whole[key] = append(whole[key], id)
		} else if key != "" {
			prefix[key] = append(prefix[key], id)
		}
	}
	if len(whole) == 0 && len(prefix) == 0 {
		return nil, nil
	}
	if view == nil {
		var err error
		if ctx, view, err = h.catchUpViewOf(ctx, u); err != nil || view == nil {
			return nil, err
		}
	}
	channels, policy, ratings, maxDays := view.channels, view.policy, view.ratings, view.maxDays
	if maxDays == 0 {
		return nil, nil
	}
	byGuide := make(map[[2]string][]Channel)
	byID := make(map[string]Channel)
	for _, ch := range channels {
		if ch.GuideID != "" && ch.ArchiveDays() > 0 {
			k := [2]string{guideSource(ch), ch.GuideID}
			byGuide[k] = append(byGuide[k], ch)
			byID[ch.ID] = ch
		}
	}
	now := h.s.clockNow()
	earliest := now.Add(-time.Duration(maxDays) * 24 * time.Hour).UnixMilli()
	// Only archive channels' rows are wanted. Filtered in SQL, other
	// channels' airings of a common title are not read out; the expressions
	// keep both engines on the title index rather than a per-channel window
	// scan. Past catchUpGuideFilter pairs, the check below does it alone.
	guideFilter, guideArgs := "", []any{}
	if len(byGuide) <= catchUpGuideFilter {
		pairs := make([]string, 0, len(byGuide))
		for _, k := range slices.SortedFunc(maps.Keys(byGuide), func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) }) {
			pairs = append(pairs, "(?,?)")
			guideArgs = append(guideArgs, k[0], k[1])
		}
		guideFilter = ` AND (source_id||'', channel_id||'') IN (` + strings.Join(pairs, ",") + `)`
	}
	// Each filter carries its own start bound. A GUID names only a key
	// prefix, a range SQLite would otherwise serve from the start-time index
	// (the whole window); start_ms+0 keeps it on the title index.
	var filters []string
	var filterArgs [][]any
	keys := slices.Sorted(maps.Keys(whole))
	for batch := range slices.Chunk(keys, catchUpKeyBatch) {
		args := make([]any, 0, len(batch))
		for _, k := range batch {
			args = append(args, k)
		}
		filters = append(filters, `title_key IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`) AND start_ms>=? AND start_ms<?`)
		filterArgs = append(filterArgs, args)
	}
	for _, p := range slices.Sorted(maps.Keys(prefix)) {
		filters = append(filters, `title_key>=? AND title_key<=? AND start_ms+0>=? AND start_ms+0<?`)
		filterArgs = append(filterArgs, []any{p + "0000", p + "ffff"})
	}
	var found []Programme
	for i, filter := range filters {
		args := append(filterArgs[i], earliest, now.Add(time.Millisecond).UnixMilli())
		args = append(append(args, guideArgs...), now.UnixMilli())
		rows, err := h.s.o.DB.QueryContext(ctx, catchUpGroupAiringsSQL(filter, guideFilter), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source, guide string
			var raw []byte
			if err = rows.Scan(&source, &guide, &raw); err != nil {
				break
			}
			for _, ch := range byGuide[[2]string{source, guide}] {
				var p Programme
				var ok bool
				if p, ok, err = catchUpRow(ch, raw, now); ok {
					found = append(found, p)
				}
				if err != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	found, err := h.s.catchUpAllowed(ctx, u, policy, ratings, byID, found)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]Programme)
	for _, p := range found {
		key := frontend.CatchUpTitleKey(p.Item.Title)
		for _, id := range whole[key] {
			out[id] = append(out[id], p)
		}
		for _, id := range prefix[key[:28]] {
			out[id] = append(out[id], p)
		}
	}
	return out, nil
}

// catchUpMembers is catchUpGroupAirings as programme ids, for the roll-up.
func (h liveTVHost) catchUpMembers(ctx context.Context, u *core.User, view *catchUpView, groups []string) (map[string][]string, error) {
	airings, err := h.catchUpGroupAirings(ctx, u, view, groups)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(groups))
	for _, id := range groups {
		ids := make([]string, 0, len(airings[id]))
		for _, p := range airings[id] {
			ids = append(ids, p.ID)
		}
		out[id] = ids
	}
	return out, nil
}
