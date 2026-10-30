package app

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

// The app imports all production components, so this exercises their entire
// registered migration inventory rather than an isolated component subset.
func TestFreshProductionMigrations(t *testing.T) {
	ctx := context.Background()
	verify := func(t *testing.T, db *sql.DB) {
		t.Helper()
		for component, version := range store.KnownMigrations() {
			var got int
			if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations WHERE component=?`, component).Scan(&got); err != nil || got != version {
				t.Fatalf("component %s version %d: got %d (%v)", component, version, got, err)
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(987,987,0,0)`); err != nil {
			t.Fatal(err)
		}
		insert := `INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires) VALUES('image','source',987,'jellyfin',1,'{}','poster','ref','tag',100)`
		ticket := `INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) VALUES('ticket','jellyfin','test',987,'source','{}',100)`
		if _, err := db.ExecContext(ctx, insert); err == nil {
			t.Fatal("orphaned image grant was accepted")
		}
		if _, err := db.ExecContext(ctx, ticket); err == nil {
			t.Fatal("orphaned frontend ticket was accepted")
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES('source',987,'jellyfin','','','','',0,0,100)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, insert); err == nil {
			t.Fatal("unadmitted image accepted")
		}
		if _, err := db.ExecContext(ctx, ticket); err == nil {
			t.Fatal("retired ticket writer accepted")
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM identity_grants WHERE hash='source'`); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants WHERE hash='image'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("image grant did not cascade: %d %v", count, err)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_tickets WHERE digest='ticket'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("ticket did not cascade: %d %v", count, err)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		db, err := store.Open(ctx, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		verify(t, db)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv(store.TestPostgresEnv)
		if dsn == "" {
			t.Skip("ETHER_TEST_POSTGRES is not configured")
		}
		db, err := store.OpenTestPostgres(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		verify(t, db)
	})
}
