package app

import (
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

// These versions identify the announced candidate, not the frozen delivered
// fixture. Keep the latter at 9503; every transition is applied by the binary.
var prismCandidateMigrations = map[string]int{
	"sessions":              9,
	"sessions_samples":      1,
	"viewer_policy":         4,
	"availability":          4,
	"capability_tickets":    1,
	"capability_headers":    3,
	"frontend_image_grants": 7,
	"frontend_tickets":      3,
	"experience_receipts":   5,
	"handle_reservations":   2,
	"plexwatch":             4,
	"state_plexwatch":       3,
}

func assertPrismCandidateBoundary(t *testing.T) {
	t.Helper()
	known := store.KnownMigrations()
	for component, version := range prismCandidateMigrations {
		if known[component] != version {
			t.Fatalf("candidate %s/%d missing: got %d", component, version, known[component])
		}
	}
}

func TestPrismUpgradeCandidateMigrationFailures(t *testing.T) {
	assertPrismCandidateBoundary(t)
	components := make([]string, 0, len(prismCandidateMigrations))
	for component := range prismCandidateMigrations {
		components = append(components, component)
	}
	sort.Strings(components)
	for _, dialect := range []store.Dialect{store.SQLite, store.Postgres} {
		for _, component := range components {
			first := prismCandidateMigrations[component]
			if component == "viewer_policy" {
				first = 1
			}
			for version := first; version <= prismCandidateMigrations[component]; version++ {
				t.Run(fmt.Sprintf("%s/%s/%d", dialect, component, version), func(t *testing.T) { verifyPrismMigrationFailure(t, dialect, component, version) })
			}
		}
	}
}

func assertPrismCandidateArtifacts(t *testing.T, db *sql.DB) {
	t.Helper()
	assertUpgradeInventory(t, db)
	var generation, used int64
	if err := db.QueryRow(`SELECT generation FROM viewer_policy_epoch WHERE id=1`).Scan(&generation); err != nil || generation < 1 {
		t.Fatalf("policy singleton generation %d: %v", generation, err)
	}
	if err := db.QueryRow(`SELECT used FROM sessions_lazy_capacity WHERE id=1`).Scan(&used); err != nil || used < 0 {
		t.Fatalf("lazy capacity singleton %d: %v", used, err)
	}
	for _, query := range []string{
		`SELECT generation FROM viewer_policy_epoch WHERE id=1`,
		`SELECT used FROM sessions_lazy_capacity WHERE id=1`,
		`SELECT viewer_id,used FROM sessions_lazy_viewer_capacity`,
		`SELECT session_id,installation_id,sequence,fingerprint FROM sessions_sample_receipts`,
		`SELECT namespace,digest,purpose FROM capability_claims`,
		`SELECT namespace,digest,purpose,source_hash,device FROM capability_tickets`,
		`SELECT policy_digest FROM availability_releases`,
		`SELECT policy_digest FROM availability_feed_items`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("candidate artifact %s: %v", query, err)
		}
		rows.Close()
	}
	for _, table := range []string{"frontend_tickets", "handle_reservations", "experience_receipt_actions"} {
		query := `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`
		if store.DialectOf(db) == store.Postgres {
			query = `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?`
		}
		var count int
		if err := db.QueryRow(query, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("retired table %s remains: %d %v", table, count, err)
		}
	}
}
