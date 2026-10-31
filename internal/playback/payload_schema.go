package playback

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/braedonsaunders/ether/internal/store"
)

const payloadSchema = `
CREATE TABLE playback_payload_incarnations (
 domain TEXT NOT NULL, incarnation TEXT NOT NULL, witness_slot INTEGER NOT NULL,
 PRIMARY KEY(domain,incarnation)
);
CREATE UNIQUE INDEX playback_payload_incarnation_slot ON playback_payload_incarnations(domain,witness_slot);
CREATE TABLE playback_payload_owners (
 namespace TEXT NOT NULL DEFAULT 'playback-source', digest TEXT NOT NULL,
 viewer_id BIGINT NOT NULL, surface TEXT NOT NULL, device_id TEXT NOT NULL,
 domain TEXT NOT NULL, incarnation TEXT NOT NULL, source_lifetime TEXT NOT NULL,
 payload_hash TEXT NOT NULL, purpose TEXT NOT NULL,
 PRIMARY KEY(namespace,digest),
 FOREIGN KEY(namespace,digest) REFERENCES capability_headers(namespace,digest) ON DELETE RESTRICT,
 FOREIGN KEY(domain,incarnation) REFERENCES playback_payload_incarnations(domain,incarnation) ON DELETE RESTRICT
);
CREATE INDEX playback_payload_owner_hash ON playback_payload_owners(domain,payload_hash);
CREATE INDEX playback_payload_owner_lifetime ON playback_payload_owners(incarnation,source_lifetime);
CREATE TABLE playback_payload_deleting (
 domain TEXT NOT NULL, payload_hash TEXT NOT NULL, digest TEXT NOT NULL,
 incarnation TEXT NOT NULL, PRIMARY KEY(domain,payload_hash),
 FOREIGN KEY(domain,incarnation) REFERENCES playback_payload_incarnations(domain,incarnation) ON DELETE RESTRICT
);`

// A failed/crashed migration cannot leave these objects without its version
// row: SQLite commits the whole version; PostgreSQL commits the whole pending
// set. Refuse unversioned objects instead of adopting a possibly wrong schema.
func init() {
	store.RegisterAfter("playback", "capability_headers")
	store.Register("playback", 4, payloadSchema)
	store.RegisterSQLiteApply("playback", 4, func(ctx context.Context, tx *sql.Tx) error {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('playback_payload_incarnations','playback_payload_owners','playback_payload_deleting','playback_payload_incarnation_slot','playback_payload_owner_hash','playback_payload_owner_lifetime')`).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return fmt.Errorf("playback/4: refusing %d unversioned payload schema objects", existing)
		}
		_, err := tx.ExecContext(ctx, payloadSchema)
		return err
	})
	store.RegisterDialect("playback", 4, store.Postgres, `DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('playback_payload_incarnations','playback_payload_owners','playback_payload_deleting','playback_payload_incarnation_slot','playback_payload_owner_hash','playback_payload_owner_lifetime')) THEN
 RAISE EXCEPTION 'playback/4: refusing unversioned payload schema objects';
 END IF; END $$;`+store.TranslateDDL(payloadSchema))
}
