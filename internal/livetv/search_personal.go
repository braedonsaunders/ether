package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

func writeSearchProgramme(ctx context.Context, tx *sql.Tx, viewer int64, channel string, p Programme) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO livetv_search_virtual(id,viewer_id,channel_id,start_ms,end_ms,data,body) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id,viewer_id) DO UPDATE SET channel_id=excluded.channel_id,start_ms=excluded.start_ms,end_ms=excluded.end_ms,data=excluded.data,body=excluded.body`, p.ID, viewer, channel, p.Start.UnixMilli(), p.End.UnixMilli(), raw, strings.Join([]string{p.Item.Title, p.Item.EpisodeTitle, p.Item.Summary}, " "))
	return err
}

// Copy only already materialised, viewer-specific rundowns. No picker or
// schedule computation is permitted in search preparation.
func (s *Service) backfillPersonalSearch(ctx context.Context) error {
	now := s.o.Now()
	for offset := 0; ; offset += 500 {
		rows, err := s.o.DB.QueryContext(ctx, `SELECT viewer_id,channel_id,data FROM livetv_personal_programmes WHERE end_ms>? AND start_ms<? ORDER BY viewer_id,channel_id,day,start_ms LIMIT 500 OFFSET ?`, now.UnixMilli(), now.Add(48*time.Hour).UnixMilli(), offset)
		if err != nil {
			return err
		}
		type entry struct {
			viewer  int64
			channel string
			p       Programme
		}
		batch := []entry{}
		for rows.Next() {
			var e entry
			var raw []byte
			if err = rows.Scan(&e.viewer, &e.channel, &raw); err != nil {
				break
			}
			var record personalProgrammeRecord
			if err = json.Unmarshal(raw, &record); err != nil {
				break
			}
			e.p = record.Programme
			batch = append(batch, e)
		}
		re := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if re != nil {
			return re
		}
		if len(batch) == 0 {
			return nil
		}
		tx, err := s.o.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, e := range batch {
			if err = writeSearchProgramme(ctx, tx, e.viewer, e.channel, e.p); err != nil {
				break
			}
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
