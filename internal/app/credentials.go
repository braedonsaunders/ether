package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/store"
)

// Credential upgrade (config.UpgradeCredentials).
//
// Database secrets are sealed and config.json moves to v2 credentials only
// once no older release can read this node's data:
//
//   - the running release is confirmed (a self-update still confirming may
//     roll back to the previous binary, which cannot read v2); and
//   - on a cluster, every live node reports the "credentials" migration,
//     i.e. runs a release that reads sealed columns. During a rolling
//     update older nodes keep reading plaintext rows and v1 settings.
//
// Then config.json and the shared settings are rewritten in v2 and each
// component seals the plaintext rows it finds (MigrateSecrets). The pass
// repeats hourly: a restored pre-upgrade backup or a row written by an
// older node during the update is sealed on the next run. Rows that do not
// open are never rewritten; they are reported.

func init() {
	// Heartbeated by every node (store.KnownMigrations): a release that
	// registers it reads sealed database columns. The table records when
	// the cluster's credentials were upgraded.
	store.Register("credentials", 1, `CREATE TABLE credentials_format (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL,
  upgraded_at INTEGER NOT NULL,
  key_fingerprint TEXT NOT NULL DEFAULT ''
);`)
}

// credentialsMigration is the migration a node must know to read v2.
const credentialsMigration = "credentials"

const (
	credentialCheckEvery = time.Minute
	credentialPassEvery  = time.Hour
)

// sealedTables are the database columns that hold sealed values.
var sealedTables = []struct{ table, column string }{
	{"simkl_devices", "device_code"},
	{"simkl_tokens", "access_token"},
	{"simkl_tokens", "refresh_token"},
	{"trakt_tokens", "access_token"},
	{"trakt_tokens", "refresh_token"},
	{"notify_channels", "config"},
	{"notify_push_keys", "private_key"},
	{"livetv_sources", "data"},
	{"cardigann_sessions", "cookies"},
	{"ratings_settings", "value"},
	{"clusterconfig_secrets", "value"},
}

// credentialKeyRejectedFile records, in the data directory, the
// fingerprint of a credential key that checkCredentialKey refused: Open
// had generated it while the database held values sealed with another.
// The key file it wrote is kept (nothing valid is deleted) and the marker
// keeps refusing it on later starts, until the original key is restored.
const credentialKeyRejectedFile = ".credentials-key-rejected"

var errCredentialKeyLost = errors.New("credentials: the database holds credentials sealed with a key this data directory does not have; restore the original .credentials-key (from the complete backup) before starting")

// checkCredentialKey refuses to start when this node's credential key is
// not the one the database's sealed values were written with:
//
//   - the database records the fingerprint of the key it was upgraded
//     with (credentials_format), and this key differs; or
//   - Open generated a new key (the key file was lost: a partial restore
//     or a copied database) while the database holds sealed values. The
//     refusal is recorded in credentialKeyRejectedFile, because the new
//     key file Open wrote would otherwise pass the next start.
//
// Starting would leave every stored credential unreadable while new ones
// are sealed with the wrong key.
func (a *App) checkCredentialKey(ctx context.Context) error {
	fp := a.cfg.KeyFingerprint()
	var recorded string
	err := a.db.QueryRowContext(ctx, `SELECT key_fingerprint FROM credentials_format WHERE id=1`).Scan(&recorded)
	switch {
	case err == nil && recorded != "" && recorded != fp:
		return errCredentialKeyLost
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		a.log.Warn("credentials: could not read the database's credential key fingerprint", "err", err)
	}
	marker := filepath.Join(filepath.Dir(a.cfg.Path()), credentialKeyRejectedFile)
	if b, err := os.ReadFile(marker); err == nil {
		if strings.TrimSpace(string(b)) == fp {
			return errCredentialKeyLost
		}
		// A different key now: the refused one was replaced. Check it
		// afresh below; the marker no longer applies.
		defer func() { _ = os.Remove(marker) }()
	}
	if !a.cfg.KeyCreated() {
		return nil
	}
	for _, t := range sealedTables {
		var n int
		err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.table+` WHERE `+t.column+` LIKE '%ether-sealed:%'`).Scan(&n)
		if err != nil {
			a.log.Warn("credentials: could not check for values sealed with another key", "table", t.table, "err", err)
			continue
		}
		if n > 0 {
			if err := os.WriteFile(marker, []byte(fp+"\n"), 0o600); err != nil {
				return fmt.Errorf("%w (and recording the refusal failed: %v)", errCredentialKeyLost, err)
			}
			return errCredentialKeyLost
		}
	}
	return nil
}

// credentialUpgradeReady reports whether v2 may be written, with the
// reason when not.
func (a *App) credentialUpgradeReady(ctx context.Context) (bool, string) {
	if a.updates != nil && a.updates.mgr != nil && a.updates.mgr.Status().Confirming {
		return false, "the running update is not confirmed yet"
	}
	if !a.clustered() {
		return true, ""
	}
	nodes, err := a.cluster.AllNodes(ctx)
	if err != nil {
		return false, "cluster membership unavailable"
	}
	if older := nodesWithoutMigration(nodes, credentialsMigration, 1); len(older) > 0 {
		return false, "live node(s) " + strings.Join(older, ", ") + " run a release without sealed credentials"
	}
	return true, ""
}

// nodesWithoutMigration names the live nodes whose binary does not know
// component's migration version (nodes that predate the report count).
func nodesWithoutMigration(nodes []cluster.Node, component string, version int) []string {
	var out []string
	for _, n := range nodes {
		if n.Live && n.Migrations[component] < version {
			name := n.Name
			if name == "" {
				name = n.ID
			}
			out = append(out, fmt.Sprintf("%s (%s)", name, n.Version))
		}
	}
	return out
}

// runCredentialUpgrade waits until the upgrade is safe, performs it, then
// repeats the database pass hourly.
func (a *App) runCredentialUpgrade(ctx context.Context) {
	var waitingFor string
	// The first pass already ran before the listener opened. Repeating it
	// here would hold the database across the first play.
	t := time.NewTimer(credentialCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(a.credentialPass(ctx, &waitingFor))
	}
}

// credentialPass is one run of runCredentialUpgrade; it returns the delay
// until the next. waitingFor holds the last logged deferral reason.
func (a *App) credentialPass(ctx context.Context, waitingFor *string) time.Duration {
	log := a.log.With("component", "credentials")
	upgraded := a.cfg.CredentialsUpgraded()
	if !upgraded {
		if ok, why := a.credentialUpgradeReady(ctx); !ok {
			if why != *waitingFor {
				log.Info("credentials: v2 upgrade deferred", "reason", why)
				*waitingFor = why
			}
			return credentialCheckEvery
		}
	}
	// Record which key the database is sealed with before anything is
	// sealed, so a later start with another key is refused.
	if err := a.recordCredentialFormat(ctx); err != nil {
		log.Warn("credentials: record the credential key fingerprint", "err", err)
		return credentialCheckEvery
	}
	// Also when already upgraded: completes a shared-settings rewrite an
	// earlier run did not finish (a no-op otherwise).
	if err := a.cfg.UpgradeCredentials(ctx); err != nil {
		log.Warn("credentials: upgrade", "err", err)
		return credentialCheckEvery
	}
	if !upgraded {
		log.Info("credentials: configuration upgraded to path- and destination-bound credentials")
	}
	if n, err := a.migrateStoredSecrets(ctx); err != nil {
		log.Warn("credentials: some stored secrets were not sealed", "sealed", n, "err", err)
	} else if n > 0 {
		log.Info("credentials: sealed stored secrets", "rows", n)
	}
	return credentialPassEvery
}

// recordCredentialFormat records the v2 upgrade and the fingerprint of the
// key the database is sealed with (the first node to upgrade sets it; all
// nodes share the key).
func (a *App) recordCredentialFormat(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, `INSERT INTO credentials_format(id, version, upgraded_at, key_fingerprint) VALUES(1, 2, ?, ?)
ON CONFLICT(id) DO UPDATE SET key_fingerprint=excluded.key_fingerprint WHERE credentials_format.key_fingerprint=''`,
		time.Now().Unix(), a.cfg.KeyFingerprint())
	return err
}

// migrateStoredSecrets runs every component's MigrateSecrets and purges
// search-cache rows that still hold indexer API keys. It returns the
// number of rows changed and the errors joined.
func (a *App) migrateStoredSecrets(ctx context.Context) (int, error) {
	total := 0
	var errs []error
	run := func(name string, fn func(context.Context) (int, error)) {
		n, err := fn(ctx)
		total += n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if a.trakt != nil {
		run("trakt", a.trakt.MigrateSecrets)
	}
	if a.notifications != nil {
		run("notifications", a.notifications.MigrateSecrets)
	}
	if a.ratings != nil {
		run("ratings", a.ratings.MigrateSecrets)
	}
	if a.livetv != nil {
		run("livetv", a.livetv.MigrateSecrets)
	}
	if a.sites != nil {
		indexers := a.cfg.Get().Indexers
		run("indexer sites", func(ctx context.Context) (int, error) { return a.sites.MigrateSecrets(ctx, indexers) })
	}
	if a.indexers != nil {
		run("search cache", func(ctx context.Context) (int, error) {
			n, err := a.indexers.PurgeLegacyCache(ctx)
			return int(n), err
		})
	}
	return total, errors.Join(errs...)
}
