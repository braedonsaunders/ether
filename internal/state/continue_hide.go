package state

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"strings"
)

func init() {
	store.Register("state_continue_hidden", 1, `CREATE TABLE state_continue_hidden(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,PRIMARY KEY(user_id,item_key));`)
	store.RegisterViewerTable("state", "state_continue_hidden", "user_id", store.ViewerMergeMove, "item_key")
}
func continueIdentity(key core.RatingKey) core.RatingKey {
	if key.Kind() == core.KindEpisode || key.Kind() == core.KindSeason {
		return core.ShowKey(key.TMDB())
	}
	return key
}
func (s *State) HideContinue(ctx context.Context, uid int64, key core.RatingKey) error {
	if err := s.lockWrites(ctx); err != nil {
		return err
	}
	defer s.unlockWrites()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cur, err := loadTx(ctx, tx, uid, key)
	if err != nil {
		return err
	}
	cur.ws.UpdatedAt = s.now()
	if err := saveTx(ctx, tx, uid, cur); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO state_continue_hidden(user_id,item_key) VALUES(?,?)`, uid, int64(continueIdentity(key))); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.gen.Add(1)
	return nil
}
func (s *State) RestoreContinue(ctx context.Context, uid int64, key core.RatingKey) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM state_continue_hidden WHERE user_id=? AND item_key=?`, uid, int64(continueIdentity(key)))
	return err
}
func (s *State) ContinueHidden(ctx context.Context, uid int64, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
	out := map[core.RatingKey]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	args := []any{uid}
	marks := []string{}
	for _, key := range keys {
		args = append(args, int64(continueIdentity(key)))
		marks = append(marks, "?")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT item_key FROM state_continue_hidden WHERE user_id=? AND item_key IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key core.RatingKey
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out[key] = true
	}
	return out, rows.Err()
}
