package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// ChannelBatch changes only the named fields, preserving stream credentials,
// programming and concurrent edits to unrelated fields. All IDs commit together.
type ChannelBatch struct {
	IDs      []string `json:"ids"`
	Enabled  *bool    `json:"enabled,omitempty"`
	Favorite *bool    `json:"favorite,omitempty"`
	Group    *string  `json:"group,omitempty"`
}

func (s *Service) UpdateChannels(ctx context.Context, change ChannelBatch) (int, error) {
	if len(change.IDs) == 0 || len(change.IDs) > 50000 {
		return 0, fmt.Errorf("select between 1 and 50000 channels")
	}
	patch := map[string]any{}
	if change.Enabled != nil {
		patch["enabled"] = *change.Enabled
	}
	if change.Favorite != nil {
		patch["favorite"] = *change.Favorite
	}
	if change.Group != nil {
		group := strings.TrimSpace(*change.Group)
		if len(group) > 200 {
			return 0, fmt.Errorf("group name is too long")
		}
		patch["group"] = group
		patch["customGroup"] = true
	}
	if len(patch) == 0 {
		return 0, fmt.Errorf("choose a channel change")
	}
	ids := slices.Clone(change.IDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if ids[0] == "" {
		return 0, fmt.Errorf("channel id is required")
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return 0, err
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := s.lockChannelBatchSources(ctx, tx, ids); err != nil {
		return 0, err
	}
	expression := "json_patch(data,?)"
	if store.DialectOf(s.o.DB) == store.Postgres {
		expression = "(data::jsonb || ?::jsonb)::text"
	}
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		args := make([]any, 0, len(batch)+1)
		args = append(args, string(payload))
		for _, id := range batch {
			args = append(args, id)
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		result, e := tx.ExecContext(ctx, "UPDATE livetv_channels SET data="+expression+" WHERE id IN ("+marks+")", args...)
		if e != nil {
			return 0, e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return 0, e
		}
		if n != int64(len(batch)) {
			return 0, core.ErrNotFound
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	// Favourites and grouping do not interrupt playback. Taking channels off air does.
	if change.Enabled != nil && !*change.Enabled {
		for _, id := range ids {
			s.invalidate(id, "")
		}
	}
	if err = s.refreshVirtualGuide(ctx); err != nil {
		return 0, err
	}
	s.bumpLineup()
	return len(ids), nil
}

// Source refresh takes this same lock before reading owner overrides. Acquire
// every source in a stable order before touching channels, including sources
// whose row does not yet exist (hence advisory locks rather than row locks).
func (s *Service) lockSourceEdits(ctx context.Context, tx *sql.Tx, ids []string) error {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if id == "" {
			continue
		}
		if err := store.LockKey(ctx, store.DialectOf(s.o.DB), tx, "livetv-source-edit:"+id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) lockChannelBatchSources(ctx context.Context, tx *sql.Tx, ids []string) error {
	if store.DialectOf(s.o.DB) != store.Postgres {
		return nil
	}
	var sources []string
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		rows, err := tx.QueryContext(ctx, "SELECT DISTINCT COALESCE("+store.JSONText(store.Postgres, "data", "sourceId")+",'') FROM livetv_channels WHERE id IN ("+marks+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			sources = append(sources, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return s.lockSourceEdits(ctx, tx, sources)
}
