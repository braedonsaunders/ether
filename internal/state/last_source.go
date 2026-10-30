package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("state", 4, `
CREATE TABLE state_source_sequence (id INTEGER PRIMARY KEY AUTOINCREMENT);
CREATE TABLE state_last_source (
 user_id INTEGER NOT NULL, rating_key INTEGER NOT NULL, sequence INTEGER NOT NULL,
 revision TEXT NOT NULL, release_id TEXT NOT NULL, video_codec TEXT NOT NULL,
 height INTEGER NOT NULL, hdr INTEGER, PRIMARY KEY(user_id,rating_key)
);`)
}

// RecordLastSource stores only source-bound facts. Its database-generated
// order prevents a delayed earlier transaction from overwriting a later one.
// An unknown revision deliberately clears prior facts.
func (s *State) RecordLastSource(ctx context.Context, userID int64, key core.RatingKey, source core.PlaybackSourceObservation) error {
	if userID <= 0 || key <= 0 {
		return core.ErrUnauthorized
	}
	if source.Revision == "" {
		source = core.PlaybackSourceObservation{}
	}
	if len(source.Revision) > 128 || len(source.ReleaseID) > 512 || len(source.VideoCodec) > 64 || source.Height < 0 || source.Height > 100000 || strings.ContainsRune(source.ReleaseID, '\x00') {
		return core.ErrNotAvailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO state_source_sequence DEFAULT VALUES RETURNING id`).Scan(&seq); err != nil {
		return err
	}
	var hdr any
	if source.HDR != nil {
		if *source.HDR {
			hdr = 1
		} else {
			hdr = 0
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO state_last_source(user_id,rating_key,sequence,revision,release_id,video_codec,height,hdr) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(user_id,rating_key) DO UPDATE SET sequence=excluded.sequence,revision=excluded.revision,release_id=excluded.release_id,video_codec=excluded.video_codec,height=excluded.height,hdr=excluded.hdr WHERE state_last_source.sequence<excluded.sequence`, userID, int64(key), seq, source.Revision, source.ReleaseID, source.VideoCodec, source.Height, hdr)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM state_source_sequence WHERE id=?`, seq); err != nil {
		return err
	}
	return tx.Commit()
}

// LastSource returns nil for unknown source identity or an untouched item.
func (s *State) LastSource(ctx context.Context, userID int64, key core.RatingKey) (*core.PlaybackSourceObservation, error) {
	var source core.PlaybackSourceObservation
	var hdr sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT revision,release_id,video_codec,height,hdr FROM state_last_source WHERE user_id=? AND rating_key=?`, userID, int64(key)).Scan(&source.Revision, &source.ReleaseID, &source.VideoCodec, &source.Height, &hdr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if source.Revision == "" {
		return nil, nil
	}
	if hdr.Valid {
		value := hdr.Int64 != 0
		source.HDR = &value
	}
	return &source, nil
}
