package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("livetv", 11, "SELECT 1;")
	store.RegisterApply("livetv", 11, migrateProgramming)
}

// migrateProgramming freezes existing saved programming as selected titles
// and assigns stable block identities. It retains materialized items, seed, epoch
// and timing/rule fields, preserving the exact programmes already airing.
func migrateProgramming(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,data FROM livetv_channels ORDER BY id`)
	if err != nil {
		return err
	}
	type saved struct {
		id   string
		data map[string]json.RawMessage
	}
	var updates []saved
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var obj map[string]json.RawMessage
		if err = json.Unmarshal(raw, &obj); err != nil {
			break
		}
		var kind string
		json.Unmarshal(obj["kind"], &kind)
		if kind != "virtual" {
			continue
		}
		freezeProgrammingJSON(obj)
		var blocks []map[string]json.RawMessage
		if len(obj["blocks"]) > 0 {
			if err = json.Unmarshal(obj["blocks"], &blocks); err != nil {
				break
			}
		}
		for i, b := range blocks {
			if len(b["id"]) == 0 {
				b["id"], _ = json.Marshal(fmt.Sprintf("%s-block-%d", id, i))
			}
			if len(b["preset"]) == 0 {
				b["preset"] = json.RawMessage(`"custom"`)
			}
			freezeProgrammingJSON(b)
		}
		if blocks != nil {
			obj["blocks"], _ = json.Marshal(blocks)
		}
		updates = append(updates, saved{id, obj})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, u := range updates {
		raw, e := json.Marshal(u.data)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE livetv_channels SET data=? WHERE id=?`, raw, u.id); e != nil {
			return e
		}
	}
	return nil
}

// Existing source queries were materialized when saved. Keep that exact
// snapshot and its playout rules; newly authored sources carry their own kind.
func freezeProgrammingJSON(obj map[string]json.RawMessage) {
	if len(obj["items"]) == 0 || string(obj["items"]) == "null" {
		return
	}
	for _, key := range []string{"sources", "collection", "filter", "watched"} {
		delete(obj, key)
	}
}
