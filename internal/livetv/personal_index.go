package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"time"
)

func init() {
	store.Register("livetv", 10, `CREATE TABLE livetv_personal_programmes(viewer_id BIGINT NOT NULL,channel_id TEXT NOT NULL,day TEXT NOT NULL,start_ms BIGINT NOT NULL,end_ms BIGINT NOT NULL,data TEXT NOT NULL,PRIMARY KEY(viewer_id,channel_id,day,start_ms)); CREATE INDEX livetv_personal_latest ON livetv_personal(viewer_id,channel_id,built_at DESC);`)
	store.RegisterViewerTable("livetv", "livetv_personal_programmes", "viewer_id", store.ViewerMergeMove, "channel_id", "day", "start_ms")
}

type personalProgrammeRecord struct {
	Programme Programme
	Title     *core.Title
	Catalog   bool
}

func (s *Service) indexPersonal(ctx context.Context, tx *sql.Tx, viewer int64, channel, day string, records []personalProgrammeRecord) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM livetv_personal_programmes WHERE viewer_id=? AND channel_id=? AND day=?`, viewer, channel, day); err != nil {
		return err
	}
	for _, record := range records {
		p := record.Programme
		b, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_personal_programmes(viewer_id,channel_id,day,start_ms,end_ms,data) VALUES(?,?,?,?,?,?)`, viewer, channel, day, p.Start.UnixMilli(), p.End.UnixMilli(), b); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) personalIndexRecords(ctx context.Context, ps []Programme) []personalProgrammeRecord {
	facts := map[core.RatingKey]*core.Title{}
	records := make([]personalProgrammeRecord, 0, len(ps))
	for _, p := range ps {
		if p.Item.Key != 0 && s.o.Catalog != nil {
			if _, ok := facts[p.Item.Key]; !ok {
				facts[p.Item.Key], _ = s.o.Catalog.Get(ctx, p.Item.Key)
			}
		}

		records = append(records, personalProgrammeRecord{Programme: p, Title: facts[p.Item.Key], Catalog: s.o.Catalog != nil})
	}
	return records
}

// Existing saved rundowns are indexed by the startup/background worker, never
// decoded by a viewer. A whole rebuild commits atomically with its index.
func (s *Service) backfillPersonalIndex(ctx context.Context) error {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT p.viewer_id,p.channel_id,p.day,p.rundown FROM livetv_personal p WHERE NOT EXISTS(SELECT 1 FROM livetv_personal_programmes i WHERE i.viewer_id=p.viewer_id AND i.channel_id=p.channel_id AND i.day=p.day)`)
	if err != nil {
		return err
	}
	type pending struct {
		viewer       int64
		channel, day string
		data         []byte
	}
	var work []pending
	for rows.Next() {
		var p pending
		if err = rows.Scan(&p.viewer, &p.channel, &p.day, &p.data); err != nil {
			rows.Close()
			return err
		}
		work = append(work, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range work {
		var ps []Programme
		if err = json.Unmarshal(p.data, &ps); err != nil {
			return err
		}
		records := s.personalIndexRecords(ctx, ps)
		tx, err := s.o.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}

		// Lock only the version that was decoded. A concurrent node may have
		// published a newer rundown/index while local title facts were loaded.
		result, lockErr := tx.ExecContext(ctx, `UPDATE livetv_personal SET built_at=built_at WHERE viewer_id=? AND channel_id=? AND day=? AND CAST(rundown AS TEXT)=?`, p.viewer, p.channel, p.day, string(p.data))
		if lockErr != nil {
			tx.Rollback()
			return lockErr
		}
		matched, lockErr := result.RowsAffected()
		if lockErr != nil {
			tx.Rollback()
			return lockErr
		}
		if matched == 0 {
			tx.Rollback()
			continue
		}
		err = s.indexPersonal(ctx, tx, p.viewer, p.channel, p.day, records)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) readPersonalClock(ctx context.Context, k personalKey) (personalRow, error) {
	var r personalRow
	var built int64
	err := s.o.DB.QueryRowContext(ctx, `SELECT cursor_ms,built_at FROM livetv_personal WHERE viewer_id=? AND channel_id=? ORDER BY built_at DESC LIMIT 1`, k.viewer, k.channel).Scan(&r.cursor, &built)
	r.built = time.UnixMilli(built)
	return r, err
}
