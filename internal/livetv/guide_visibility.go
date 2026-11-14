package livetv

import (
	"context"
	"database/sql"
	"github.com/braedonsaunders/ether/internal/store"
	"strings"
)

const guideVisibilitySchema = `CREATE TABLE livetv_guide_visibility(kind TEXT NOT NULL,a TEXT NOT NULL,b TEXT NOT NULL,PRIMARY KEY(kind,a,b));`

func init() {
	store.Register("livetv", 23, guideVisibilitySchema+`
 INSERT INTO livetv_guide_visibility SELECT 'configured','','' FROM livetv_owner_guides WHERE id=1;
 INSERT OR IGNORE INTO livetv_guide_visibility SELECT 'all','','' FROM livetv_owner_guides o,json_each(o.data) g WHERE coalesce(json_extract(g.value,'$.hidden'),0)=0 AND json_extract(g.value,'$.all')=1;
 INSERT OR IGNORE INTO livetv_guide_visibility SELECT 'group',json_extract(m.value,'$.sourceId'),json_extract(m.value,'$.name') FROM livetv_owner_guides o,json_each(o.data) g,json_each(g.value,'$.groups') m WHERE coalesce(json_extract(g.value,'$.hidden'),0)=0;
 INSERT OR IGNORE INTO livetv_guide_visibility SELECT 'channel',m.value,'' FROM livetv_owner_guides o,json_each(o.data) g,json_each(g.value,'$.channels') m WHERE coalesce(json_extract(g.value,'$.hidden'),0)=0;
 `)
	store.RegisterDialect("livetv", 23, store.Postgres, guideVisibilitySchema+`
 INSERT INTO livetv_guide_visibility SELECT 'configured','','' FROM livetv_owner_guides WHERE id=1;
 INSERT INTO livetv_guide_visibility SELECT 'all','','' FROM livetv_owner_guides o CROSS JOIN LATERAL jsonb_array_elements(o.data::jsonb) g WHERE coalesce((g->>'hidden')::boolean,false)=false AND (g->>'all')::boolean=true ON CONFLICT DO NOTHING;
 INSERT INTO livetv_guide_visibility SELECT 'group',m->>'sourceId',m->>'name' FROM livetv_owner_guides o CROSS JOIN LATERAL jsonb_array_elements(o.data::jsonb) g CROSS JOIN LATERAL jsonb_array_elements(g->'groups') m WHERE coalesce((g->>'hidden')::boolean,false)=false ON CONFLICT DO NOTHING;
 INSERT INTO livetv_guide_visibility SELECT 'channel',m #>> '{}','' FROM livetv_owner_guides o CROSS JOIN LATERAL jsonb_array_elements(o.data::jsonb) g CROSS JOIN LATERAL jsonb_array_elements(g->'channels') m WHERE coalesce((g->>'hidden')::boolean,false)=false ON CONFLICT DO NOTHING;
 `)
}
func saveGuideVisibility(ctx context.Context, tx *sql.Tx, gs []OwnerGuide) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM livetv_guide_visibility`); err != nil {
		return err
	}
	insert := func(kind, a, b string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO livetv_guide_visibility(kind,a,b) VALUES(?,?,?) ON CONFLICT DO NOTHING`, kind, a, b)
		return err
	}
	if err := insert("configured", "", ""); err != nil {
		return err
	}
	for _, g := range gs {
		if g.Hidden {
			continue
		}
		if g.All {
			if err := insert("all", "", ""); err != nil {
				return err
			}
		}
		for _, group := range g.Groups {
			if err := insert("group", group.SourceID, group.Name); err != nil {
				return err
			}
		}
		for _, id := range g.Channels {
			if err := insert("channel", id, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// Indexed membership checks touch only search candidates, even if a saved guide
// explicitly selects all 10,000 channels. Overlaps are a union, never duplicates.
func (s *Service) searchVisibleGuideIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	visible := map[string]bool{}
	d := store.DialectOf(s.o.DB)
	for start := 0; start < len(ids); start += 400 {
		chunk := ids[start:min(start+400, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		query := `SELECT c.id FROM livetv_channels c WHERE c.id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `) AND (NOT EXISTS(SELECT 1 FROM livetv_guide_visibility WHERE kind='configured') OR EXISTS(SELECT 1 FROM livetv_guide_visibility WHERE kind='all') OR EXISTS(SELECT 1 FROM livetv_guide_visibility WHERE kind='channel' AND a=c.id AND b='') OR EXISTS(SELECT 1 FROM livetv_guide_visibility WHERE kind='group' AND a=coalesce(` + store.JSONText(d, "c.data", "sourceId") + `,'') AND b=coalesce(` + store.JSONText(d, "c.data", "group") + `,'')))`
		rows, err := s.o.DB.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			visible[id] = true
		}
		re := rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if re != nil {
			return nil, re
		}
	}
	return visible, nil
}
