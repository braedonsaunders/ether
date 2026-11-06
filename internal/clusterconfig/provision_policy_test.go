package clusterconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestProvisionDatabaseSelectionPersistsLocalMigrationPolicy(t *testing.T) {
	for _, deferMigrations := range []bool{false, true} {
		name := "default_additive"
		if deferMigrations {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, err := config.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer cfg.Close()
			if err = cfg.Update(func(c *config.Config) error {
				c.Database = config.Database{Driver: config.DatabasePostgres, URL: "postgres://old.invalid/fixture", DeferMigrations: deferMigrations}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			supplied := config.Database{Driver: config.DatabasePostgres, URL: "postgres://new.invalid/fixture"}
			if err = cfg.Update(func(c *config.Config) error {
				c.Database = provisionDatabaseSelection(c.Database, supplied)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			cfg.Close()
			reopened, err := config.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got := reopened.Get().Database
			if got.Driver != supplied.Driver || got.URL != supplied.URL || got.DeferMigrations != deferMigrations {
				t.Fatal("database endpoint selection changed local migration policy")
			}
			// Startup reads this unsealed node-local flag before PostgreSQL opens.
			raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var local struct {
				Database struct {
					DeferMigrations bool `json:"deferMigrations"`
				} `json:"database"`
			}
			if err = json.Unmarshal(raw, &local); err != nil || local.Database.DeferMigrations != deferMigrations {
				t.Fatal("startup policy document changed during provisioning")
			}
		})
	}
}
