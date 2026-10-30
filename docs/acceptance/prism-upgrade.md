# Prism database upgrade acceptance

Delivered v0.2.48 schema: `9503ece9d544e5f111f2afa531e87a2c3ad0e6c1`.
The fixture contains the released registrations, not a downgraded current
schema. `extract-postgres9503.sh` freezes PostgreSQL DDL using that commit's
translator and lexer; there were no production dialect overrides at 9503.
Both fixture hashes are checked before use.

SQLite commits each migration step. A failed step rolls back its DDL and rows;
completed steps remain. Startup preserves the original pre-migrate snapshot
and does not restore it automatically. Fix the cause and restart to complete
the remaining steps. Tests compare all original columns and rows in every
delivered table, including default seeds and empty tables. PostgreSQL instead
rolls back the entire pending set on failure, including migration bookkeeping.
Restoring a snapshot is an operator rollback, not an automatic startup action.

## PostgreSQL HA transition: all nodes upgraded before serving

The first Prism transition requires a maintenance outage. Current production
is a single SQLite node; this section applies to PostgreSQL HA deployments.

1. Stop every old node and disable its automatic restart/update jobs. Verify
   that no old process retains a database connection. Back up the database and
   each node's configuration and credentials before changing anything.
2. Revoke old database writer access and terminate its existing connections,
   or rotate credentials and ensure all old connections have ended. Merely
   stopping heartbeats, setting draining, waiting for liveness expiry, or
   rotating a password while old connections remain is insufficient.
3. Migrate using the new deployment's writer identity. Prism always defers its
   first identity transition (`auth/5`) while a live node reports an older
   identity inventory, regardless of `database.deferMigrations`.
4. Upgrade every node and configure the new writer credentials. Verify each
   node's migration inventory and that the old writer cannot connect or write.
   Start serving only when every serving node runs Prism.
5. Rollback requires stopping all new nodes, revoking their writer access,
   restoring the pre-upgrade database/configuration backup and old binaries,
   then restoring access for the intended deployment. Do not run an old binary
   against the migrated database as a rollback strategy.

The startup gate is defense in depth, not a complete credential fence. It
runs only while migrations are pending, cannot detect a stopped old node that
later restarts, and cannot change already released binaries. Existing old SQL
can write `auth_users` without incrementing `auth_membership_epoch`, and the
cluster/2 heartbeat schema remains writable by old nodes. Tests characterize
both old-before-new and old-after-new orders explicitly. Do not describe this
transition as a safe mixed-version rolling upgrade.

`state_plexwatch/1-3` and `plexwatch/1-4` remain reserved and unregistered.
The inventory assertions automatically include components on the tested base;
new component state requires additional component-specific seeded upgrade tests.
