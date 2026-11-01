package collections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ImportTitlesTx is the collections-owned writer for a reviewed import. It
// takes a time, not a number, so the caller cannot pick the wrong unit. The
// caller commits its migration bookkeeping in the same transaction. Imported
// lists retain stable IDs and can never replace built-ins or another viewer's.
func ImportTitlesTx(ctx context.Context, tx *sql.Tx, id, userID int64, name string, keys []core.RatingKey, at time.Time) (int64, error) {
	if userID <= 0 || name == "" {
		return 0, errors.New("invalid imported collection")
	}
	now := at.Unix() // collections_specs stamps are unix seconds
	if id == 0 {
		var err error
		if id, err = nextSpecID(ctx, tx); err != nil {
			return 0, err
		}
	} else {
		var owner int64
		var builtin bool
		if err := tx.QueryRowContext(ctx, `SELECT owner_user_id,builtin FROM collections_specs WHERE id=?`, id).Scan(&owner, &builtin); err != nil {
			return 0, err
		}
		if builtin || owner != userID {
			return 0, core.ErrUnauthorized
		}
	}
	ks := []string{}
	for _, k := range keys {
		if !k.Valid() {
			return 0, errors.New("invalid collection item")
		}
		ks = append(ks, k.String())
	}
	sp := core.CollectionSpec{Name: name, MediaType: "mixed", Sources: []core.CollectionSource{{Kind: "titles", Keys: ks}}, Match: "all", Sort: "source", Refresh: "live", Enabled: true, OwnerUserID: userID, Targets: core.CollectionTargets{CollectionsTab: true, Users: core.UserTargets{IDs: []int64{userID}}}}
	raw, err := json.Marshal(sp)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO collections_specs(id,owner_user_id,spec_json,enabled,created_at,updated_at) VALUES(?,?,?,1,?,?) ON CONFLICT(id) DO UPDATE SET spec_json=excluded.spec_json,updated_at=excluded.updated_at`, id, userID, string(raw), now, now)
	return id, err
}
