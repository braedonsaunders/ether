// Package reservations registers retirement migrations for the former store.
// All runtime issuance is owned by capability; no old API remains.
package reservations

import "github.com/braedonsaunders/ether/internal/store"

func init() {
	store.Register("handle_reservations", 1, `CREATE TABLE handle_reservations (namespace TEXT NOT NULL,digest TEXT NOT NULL,shard INTEGER NOT NULL,viewer_id BIGINT NOT NULL,surface TEXT NOT NULL,expires BIGINT NOT NULL,PRIMARY KEY(namespace,digest)); CREATE INDEX handle_reservations_shard_expiry ON handle_reservations(namespace,shard,expires); CREATE INDEX handle_reservations_scope ON handle_reservations(namespace,shard,viewer_id,surface,expires);`)
	store.RegisterAfter("handle_reservations", "capability_headers")
	store.Register("handle_reservations", 2, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) SELECT namespace,digest,(viewer_id % 64),viewer_id,surface,'',expires FROM handle_reservations WHERE true ON CONFLICT(namespace,digest) DO NOTHING; DROP TABLE handle_reservations;`)
}
