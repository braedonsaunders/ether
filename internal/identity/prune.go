package identity

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// Indexes for Prune's inner SELECTs, plus the child-side indexes the
// ON DELETE CASCADE from a pruned family or grant probes.
func init() {
	// The MCP client eviction checks, per candidate client, that no grant
	// names its device. The store runs migrations in a transaction, so
	// PostgreSQL cannot build this CONCURRENTLY; the table is small.
	store.Register("identity_grants", 5, `CREATE INDEX identity_grants_device_surface ON identity_grants(device_id,surface);`)
	store.Register("identity_grants", 4, `CREATE INDEX identity_grants_revoked_expires ON identity_grants(revoked,expires); CREATE INDEX identity_grants_revoked_last_used ON identity_grants(revoked,last_used);`)
	store.Register("identity_refresh", 2, `CREATE INDEX identity_refresh_families_absolute_expiry ON identity_refresh_families(absolute_expiry); CREATE INDEX identity_refresh_families_revoked_idle_expiry ON identity_refresh_families(revoked,idle_expiry); CREATE INDEX identity_refresh_families_access_hash ON identity_refresh_families(access_hash); CREATE INDEX identity_refresh_tokens_family_id ON identity_refresh_tokens(family_id);`)
	// Its own component: capability_tickets' versions are pinned to the
	// delivered schema boundary.
	store.Register("capability_tickets_retention", 1, `CREATE INDEX capability_tickets_source_hash ON capability_tickets(source_hash);`)
	store.RegisterAfter("capability_tickets_retention", "capability_tickets")
	store.RegisterAfter("identity_invites_retention", "identity_invites")
	store.Register("identity_invites_retention", 1, `CREATE INDEX identity_invites_expiry ON identity_invites(expires);`)
}

// pruneGrace is how long a dead credential is kept after it died.
const pruneGrace = 7 * 24 * time.Hour

// Prune deletes credentials that died more than pruneGrace ago, and reports
// the families and grants removed. Every refresh rotation revokes one grant
// and inserts another, so without this both tables grow with every session.
//
// A family is dead once revoked or past its idle or absolute expiry; neither
// expiry is ever extended past death, and nothing un-revokes. Deleting it
// cascades its consumed tokens. Replaying one of them then fails at the
// token lookup with ErrUnauthorized, the same error the dead family gave,
// and there is no live family left for replay detection to revoke. Consumed
// tokens of live families are kept: any of them may be the one a thief
// replays, and the family's 90-day absolute expiry bounds them.
//
// A grant is dead once revoked or expired; Resolve, touch and sign-in all
// require a live grant, so neither state is reversible. A grant some family
// still names as its current access grant is kept whatever its state:
// Refresh copies the new grant's metadata from that row. Deleting a grant
// cascades its capability tickets and image grants, which are only
// honoured while the grant is live. Grant references held elsewhere
// (sessions, push subscriptions) are rechecked against a live grant.
func (g *Grants) Prune(ctx context.Context) (int64, error) {
	now := g.now().UnixMilli()
	cutoff := now - pruneGrace.Milliseconds()
	// A revoked family carries no revocation time; idle_expiry is its last
	// rotation plus refreshIdle, so this bound means "not rotated within
	// the grace period". A revoked grant's last_used plays the same role.
	revokedFamilyCutoff := now + (refreshIdle - pruneGrace).Milliseconds()
	steps := []struct {
		del  string
		args []any
	}{
		{`DELETE FROM identity_refresh_families WHERE id IN (SELECT id FROM identity_refresh_families WHERE absolute_expiry<? ORDER BY absolute_expiry LIMIT ?)`, []any{cutoff}},
		{`DELETE FROM identity_refresh_families WHERE id IN (SELECT id FROM identity_refresh_families WHERE revoked=0 AND idle_expiry<? ORDER BY idle_expiry LIMIT ?)`, []any{cutoff}},
		{`DELETE FROM identity_refresh_families WHERE id IN (SELECT id FROM identity_refresh_families WHERE revoked=1 AND idle_expiry<? ORDER BY idle_expiry LIMIT ?)`, []any{revokedFamilyCutoff}},
		{`DELETE FROM identity_grants WHERE hash IN (SELECT hash FROM identity_grants d WHERE d.revoked=0 AND d.expires<? AND NOT EXISTS (SELECT 1 FROM identity_refresh_families f WHERE f.access_hash=d.hash) ORDER BY d.expires LIMIT ?)`, []any{cutoff}},
		{`DELETE FROM identity_grants WHERE hash IN (SELECT hash FROM identity_grants d WHERE d.revoked=1 AND d.last_used<? AND NOT EXISTS (SELECT 1 FROM identity_refresh_families f WHERE f.access_hash=d.hash) ORDER BY d.last_used LIMIT ?)`, []any{cutoff}},
		// A quick-connect row keeps the issued token only for its short
		// replay window; past that it must not sit in the database.
		{`UPDATE identity_quickconnect SET replay_token='' WHERE code IN (SELECT code FROM identity_quickconnect WHERE replay_token<>'' AND replay_until<? ORDER BY replay_until LIMIT ?)`, []any{now}},
		{`DELETE FROM identity_quickconnect WHERE code IN (SELECT code FROM identity_quickconnect WHERE expires<? AND replay_until<? ORDER BY expires LIMIT ?)`, []any{cutoff, cutoff}},
		{`DELETE FROM identity_invites WHERE hash IN (SELECT hash FROM identity_invites WHERE expires<? ORDER BY expires LIMIT ?)`, []any{cutoff}},
	}
	var total int64
	for _, s := range steps {
		n, err := store.PruneBatched(ctx, g.db, s.del, s.args...)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
