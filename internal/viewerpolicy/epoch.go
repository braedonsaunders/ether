// Package viewerpolicy fences authorization snapshots against durable policy writes.
package viewerpolicy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

var ErrChanged = errors.New("viewer policy changed")

// Tables includes every durable input to row visibility and membership. A global
// epoch deliberately invalidates other viewers too; issuance retries once.
var tables = []string{
	"auth_users", "auth_membership_epoch", "identity_links",
	"personalize_home_layout", "personalize_overrides", "personalize_discovery_hidden",
	"favorites_items", "favorites_mirror", "notinterested_titles", "notinterested_revisions",
	"collections_specs", "collections_members", "collections_names", "collections_external", "collections_defaults",
	"state_playlists", "state_playlist_items", "state_playlist_markers",
	"catalog_titles", "catalog_genres", "catalog_studios", "catalog_countries", "catalog_keywords", "catalog_credits", "catalog_collections", "catalog_home_release", "catalog_home_release_dates",
	"livetv_policies", "livetv_channels", "livetv_sources", "livetv_programmes", "livetv_guide_channels", "safety_policies", "episodeorder_preferences", "episodeorder_groups",
}

const authPolicyColumns = "active, owner_enabled, plex_shared, is_owner, origin, plex_id, restricted, plex_sections, allowed_sections, max_content_rating"

const schema = `CREATE TABLE viewer_policy_epoch (id INTEGER PRIMARY KEY CHECK(id=1), generation BIGINT NOT NULL CHECK(generation>=0)); INSERT INTO viewer_policy_epoch(id,generation) VALUES(1,1);`

func init() {
	store.RegisterAfter("viewer_policy", "auth", "identity_links", "personalize", "personalize_discovery", "favorites", "notinterested", "collections", "state", "catalog", "livetv", "safety", "episodeorder")
	store.Register("viewer_policy", 1, schema)
	store.RegisterSQLiteApply("viewer_policy", 1, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
		for _, table := range tables {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				continue
			} // Partial binaries have only their registered components.
			for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
				operation := event
				if event == "UPDATE" && table == "auth_users" {
					operation = "UPDATE OF " + authPolicyColumns
				}
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER viewer_policy_%s_%s AFTER %s ON %s BEGIN UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1; END;`, table, strings.ToLower(event), operation, table)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	pg := schema + ` CREATE FUNCTION viewer_policy_bump() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1; RETURN NULL; END $$;`
	for _, table := range tables {
		operation := "INSERT OR UPDATE OR DELETE"
		if table == "auth_users" {
			operation = "INSERT OR UPDATE OF " + authPolicyColumns + " OR DELETE"
		}
		pg += fmt.Sprintf(` DO $$ BEGIN IF to_regclass('%s') IS NOT NULL THEN EXECUTE 'CREATE TRIGGER viewer_policy_%s AFTER %s ON %s FOR EACH STATEMENT EXECUTE FUNCTION viewer_policy_bump()'; END IF; END $$;`, table, table, operation, table)
	}
	store.RegisterDialect("viewer_policy", 1, store.Postgres, pg)
}

func Read(ctx context.Context, db *sql.DB) (int64, error) {
	var generation int64
	err := db.QueryRowContext(ctx, `SELECT generation FROM viewer_policy_epoch WHERE id=1`).Scan(&generation)
	return generation, err
}

// Fence locks the epoch until commit. PostgreSQL policy triggers update this
// same row; SQLite transactions already hold the database write lock.
func Fence(ctx context.Context, db *sql.DB, tx *sql.Tx, expected int64) error {
	query := `SELECT generation FROM viewer_policy_epoch WHERE id=1`
	if store.DialectOf(db) == store.Postgres {
		query += ` FOR SHARE`
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, query).Scan(&generation); err != nil {
		return err
	}
	if generation != expected {
		return ErrChanged
	}
	return nil
}
