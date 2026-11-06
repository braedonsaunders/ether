package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	ddl := `
ALTER TABLE state_history ADD COLUMN view_type INTEGER NOT NULL DEFAULT 1;
ALTER TABLE state_history ADD COLUMN device_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE state_history ADD COLUMN snapshot TEXT NOT NULL DEFAULT '';
CREATE INDEX state_history_seconds ON state_history(user_id,view_type,(viewed_at/1000000000),id);
CREATE INDEX state_history_global_seconds ON state_history(view_type,(viewed_at/1000000000),id);
CREATE INDEX state_history_kind ON state_history(user_id, view_type, viewed_at, id);
CREATE INDEX state_history_item ON state_history(rating_key, user_id, view_type, viewed_at, id);
CREATE INDEX state_history_section ON state_history((rating_key/1000000000000000),user_id,view_type,(viewed_at/1000000000),id);
CREATE INDEX state_history_device ON state_history(device_id, user_id, view_type, viewed_at, id);
`
	store.Register("state", 8, ddl)
	// Some prerelease databases recorded these columns in a separate component.
	// Inspect the table shape so moving to the ordered state migration is safe.
	store.RegisterApply("state", 8, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT * FROM state_history WHERE 1=0")
		if err != nil {
			return err
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, c := range columns {
			have[c] = true
		}
		for _, column := range []struct{ name, definition string }{{"view_type", "INTEGER NOT NULL DEFAULT 1"}, {"device_id", "INTEGER NOT NULL DEFAULT 0"}, {"snapshot", "TEXT NOT NULL DEFAULT ''"}} {
			if !have[column.name] {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE state_history ADD COLUMN "+column.name+" "+column.definition); err != nil {
					return err
				}
			}
		}
		indexes := ddl[strings.Index(ddl, "CREATE INDEX"):]
		_, err = tx.ExecContext(ctx, strings.ReplaceAll(indexes, "CREATE INDEX ", "CREATE INDEX IF NOT EXISTS "))
		return err
	})
}

type historyScrobbleKey struct{}

func historyViewType(ctx context.Context) int {
	if yes, _ := ctx.Value(historyScrobbleKey{}).(bool); yes {
		return 1
	}
	return 0
}

func scanHistory(row interface{ Scan(...any) error }) (core.HistoryEntry, error) {
	var h core.HistoryEntry
	var at int64
	var snapshot string
	err := row.Scan(&h.ID, &h.UserID, &h.Key, &at, &h.Completed, &h.ViewType, &h.DeviceID, &snapshot)
	h.ViewedAt = time.Unix(0, at).UTC()
	if snapshot != "" {
		var title core.Title
		if json.Unmarshal([]byte(snapshot), &title) == nil {
			h.Snapshot = &title
		}
	}
	return h, err
}

const historyColumns = `id,user_id,rating_key,viewed_at,completed,view_type,device_id,snapshot`

func (s *State) QueryHistory(ctx context.Context, q core.HistoryQuery) ([]core.HistoryEntry, error) {
	where := []string{"1=1"}
	args := []any{}
	add := func(clause string, value any) { where = append(where, clause); args = append(args, value) }
	if q.UserID != 0 {
		add("user_id=?", q.UserID)
	}
	if q.Kind != 0 {
		add("rating_key/1000000000000000=?", int(q.Kind))
	}
	if q.Key != 0 {
		add("rating_key=?", int64(q.Key))
	}
	if q.ViewType >= 0 {
		add("view_type=?", q.ViewType)
	}
	if q.DeviceID != nil {
		add("device_id=?", *q.DeviceID)
	}
	// Integer division compares seconds without overflowing a nanosecond conversion.
	if q.Since != nil {
		add("viewed_at/1000000000>=?", *q.Since)
	}
	if q.Until != nil {
		add("viewed_at/1000000000<=?", *q.Until)
	}
	if q.At != nil {
		add("viewed_at/1000000000=?", *q.At)
	}
	order := "DESC"
	if q.Asc {
		order = "ASC"
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	q.Limit = min(q.Limit, 500)
	args = append(args, q.Limit, max(q.Offset, 0))
	rows, err := s.db.QueryContext(ctx, "SELECT "+historyColumns+" FROM state_history WHERE "+strings.Join(where, " AND ")+" ORDER BY viewed_at/1000000000 "+order+", id ASC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.HistoryEntry
	for rows.Next() {
		h, err := scanHistory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s *State) HistoryByID(ctx context.Context, userID, id int64) (core.HistoryEntry, error) {
	return scanHistory(s.db.QueryRowContext(ctx, "SELECT "+historyColumns+" FROM state_history WHERE id=? AND (user_id=? OR ?=0)", id, userID, userID))
}
func (s *State) DeleteHistory(ctx context.Context, userID, id int64) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM state_history WHERE id=? AND (user_id=? OR ?=0)", id, userID, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

// WithHistoryTitle retains the display identity if the catalog item is later removed.
func WithHistoryTitle(ctx context.Context, t *core.Title) context.Context {
	if t == nil {
		return ctx
	}
	snapshot := *t
	origin := originFrom(ctx)
	origin.HistoryTitle = &snapshot
	return WithOrigin(ctx, origin)
}
func historySnapshot(ctx context.Context) string {
	title := originFrom(ctx).HistoryTitle
	if title == nil {
		return ""
	}
	raw, _ := json.Marshal(title)
	return string(raw)
}
