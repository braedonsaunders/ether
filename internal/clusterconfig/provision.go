package clusterconfig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/dburl"
	"github.com/braedonsaunders/ether/internal/store"
)

// ProvisionOptions describes an explicitly trusted deployment. Key is the raw
// 32-byte cluster credential key supplied through a mounted Secret, not a token.
// The caller must exclusively lock DataDir for the complete operation.
type ProvisionOptions struct {
	MigrationGate   store.MigrationGate
	TempDir         string
	CacheDir        string
	DataDir         string
	DatabaseURL     string
	Key             []byte
	Bootstrap       bool
	Roles           config.NodeRoles
	InternalAddress string
}

// Provision is restartable after every local write and after DB initialization.
// Possession of the database credential AND credential key is the enrollment
// authority. It never consumes join tokens or replaces an existing identity/key.
func Provision(ctx context.Context, o ProvisionOptions) error {
	if len(o.Key) != 32 {
		return errors.New("provision: expected a raw 32-byte cluster key")
	}
	database := config.Database{Driver: config.DatabasePostgres, URL: o.DatabaseURL}
	if err := (config.Server{NodeRoles: o.Roles, InternalAddress: o.InternalAddress}).ValidateNodeSettings(database); err != nil {
		return err
	}
	// Validate DB trust before writing local identity material. Fresh initialization
	// uses SaveShared's compare-and-swap, so simultaneous first pods adopt one doc.
	if !dburl.IsPostgres(o.DatabaseURL) {
		return errors.New("provision: PostgreSQL URL required")
	}
	db, _, err := store.OpenWithRetry(ctx, store.Options{Driver: config.DatabasePostgres, URL: o.DatabaseURL, MigrationGate: o.MigrationGate}, store.RetryOptions{})
	if err != nil {
		return errors.New("provision: cannot open PostgreSQL")
	}
	defer db.Close()
	unlock, err := store.SessionLock(ctx, db, "clusterconfig:provision")
	if err != nil {
		return errors.New("provision: cannot lock cluster initialization")
	}
	if unlock != nil {
		defer unlock()
	}
	doc, found, err := NewShared(db).LoadShared(ctx)
	if err != nil {
		return errors.New("provision: cannot read cluster settings")
	}
	if found && doc.KeyFingerprint != config.KeyFingerprint(o.Key) {
		return config.ErrKeyMismatch
	}
	if !found && !o.Bootstrap {
		return config.ErrSharedMissing
	}
	old, err := config.ReadCredentialKey(o.DataDir)
	switch {
	case err == nil:
		if !bytes.Equal(old, o.Key) {
			return config.ErrKeyMismatch
		}
	case os.IsNotExist(err):
		// A config without its original key needs deliberate offline recovery, never
		// silent adoption (even if it looks like an incomplete setup).
		if _, e := os.Lstat(filepath.Join(o.DataDir, "config.json")); !os.IsNotExist(e) {
			return errors.New("provision: existing settings have no credential key; restore the original key")
		}
		if err := config.InstallCredentialKey(o.DataDir, o.Key); err != nil {
			return err
		}
	default:
		return errors.New("provision: cannot read local credential key")
	}
	cfg, err := config.Open(o.DataDir)
	if err != nil {
		return errors.New("provision: cannot open local settings")
	}
	defer cfg.Close()
	if cur := cfg.Get(); !cur.Database.IsPostgres() && (cur.SetupComplete || cur.Plex.Token != "") {
		return errors.New("provision: existing standalone server requires explicit cluster init migration")
	}
	if err := cfg.Update(func(c *config.Config) error {
		if o.TempDir != "" {
			c.Transcode.TempDir = o.TempDir
		}
		if o.CacheDir != "" {
			c.Cache.Dir = o.CacheDir
		}
		c.Database = provisionDatabaseSelection(c.Database, database)
		c.Server.NodeRoles = append(config.NodeRoles(nil), o.Roles...)
		c.Server.InternalAddress = o.InternalAddress
		return nil
	}); err != nil {
		return errors.New("provision: cannot persist local deployment settings")
	}
	if err := Attach(ctx, cfg, db, config.SharedOptions{NoSeed: !o.Bootstrap, PollInterval: -1}); err != nil {
		return err
	}
	return nil
}

// Deployment chooses the database endpoint, not the node's local migration
// deferral policy. Retain that policy across successful restart/provisioning.
func provisionDatabaseSelection(current, supplied config.Database) config.Database {
	supplied.DeferMigrations = current.DeferMigrations
	return supplied
}
