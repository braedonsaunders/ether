package notify

import (
	_ "github.com/braedonsaunders/ether/internal/auth"     // Registers membership transition events.
	_ "github.com/braedonsaunders/ether/internal/identity" // Registers grant transition events.
	"github.com/braedonsaunders/ether/internal/store"
)

// The subscribers run in the transition transaction, including changes made
// by another cluster node. A later reactivation cannot restore old endpoints.
func init() {
	store.Register("viewer_push", 2, `
CREATE TRIGGER viewer_push_on_deactivation AFTER INSERT ON identity_viewer_deactivations BEGIN
 DELETE FROM viewer_push_subscriptions WHERE viewer_id=NEW.viewer_id;
END;
CREATE TRIGGER viewer_push_on_grant_revocation AFTER INSERT ON identity_grant_revocations BEGIN
 DELETE FROM viewer_push_subscriptions WHERE viewer_id=NEW.viewer_id AND grant_hash=NEW.grant_hash;
END;`)
	store.RegisterDialect("viewer_push", 2, store.Postgres, `
CREATE FUNCTION viewer_push_purge_deactivated() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN DELETE FROM viewer_push_subscriptions WHERE viewer_id=NEW.viewer_id; RETURN NEW; END;
$$;
CREATE TRIGGER viewer_push_on_deactivation AFTER INSERT ON identity_viewer_deactivations FOR EACH ROW EXECUTE FUNCTION viewer_push_purge_deactivated();
CREATE FUNCTION viewer_push_purge_revoked_grant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN DELETE FROM viewer_push_subscriptions WHERE viewer_id=NEW.viewer_id AND grant_hash=NEW.grant_hash; RETURN NEW; END;
$$;
CREATE TRIGGER viewer_push_on_grant_revocation AFTER INSERT ON identity_grant_revocations FOR EACH ROW EXECUTE FUNCTION viewer_push_purge_revoked_grant();`)
}
