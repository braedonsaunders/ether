package viewerpolicy

import (
	"context"
	"database/sql"
	"errors"
)

// CheckSharedConfigVersion rechecks the exact version captured by a held
// config policy lease using the same transaction that already fenced the
// durable viewer-policy epoch. A clustered caller must not substitute a
// separate pooled connection or silently ignore missing schema/errors. The
// epoch trigger on clusterconfig_shared prevents a writer from committing a
// new shared version while that fence is held. This is not authorization.
func CheckSharedConfigVersion(ctx context.Context, tx *sql.Tx, expected int64) error {
	if tx == nil || expected < 0 {
		return ErrChanged
	}
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM clusterconfig_shared WHERE id=1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		version, err = 0, nil
	}
	if err != nil {
		return err
	}
	if version != expected {
		return ErrChanged
	}
	return nil
}
