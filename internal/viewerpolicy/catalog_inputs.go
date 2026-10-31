package viewerpolicy

import (
	"context"
	"database/sql"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

// catalogPolicyColumns are the catalog_titles columns that decide whether a
// viewer may see a title or where it belongs: rating gates, adult, type,
// browse membership, release (availability), episode placement, collection
// membership, and the stable facts library rules read. Everything else a
// metadata refresh rewrites (popularity, votes, artwork, summaries, fetch
// stamps) changes nothing a viewer is authorized to see, and must not
// advance the global epoch: background refreshes rewrite titles constantly,
// and every advance fails the fence of a home page being built.
var catalogPolicyColumns = []string{"content_rating", "content_rating_rank", "adult", "type", "browsable", "released_ts", "show_key", "season", "collection_id", "language", "year", "runtime_sec"}

func init() {
	var sqliteChanged, pgChanged []string
	for _, c := range catalogPolicyColumns {
		sqliteChanged = append(sqliteChanged, "old."+c+" IS NOT new."+c)
		pgChanged = append(pgChanged, "OLD."+c+" IS DISTINCT FROM NEW."+c)
	}
	// catalog_home_release is re-stamped (attempted_at) on every check; only
	// its outcome decides visibility.
	sqlite := `DROP TRIGGER IF EXISTS viewer_policy_catalog_titles_update;
CREATE TRIGGER viewer_policy_catalog_titles_update AFTER UPDATE ON catalog_titles
WHEN ` + strings.Join(sqliteChanged, " OR ") + ` BEGIN UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1; END;
DROP TRIGGER IF EXISTS viewer_policy_catalog_home_release_update;
CREATE TRIGGER viewer_policy_catalog_home_release_update AFTER UPDATE ON catalog_home_release
WHEN old.checked IS NOT new.checked OR old.earliest_ts IS NOT new.earliest_ts OR old.fallback_admitted IS NOT new.fallback_admitted
BEGIN UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1; END;`
	store.Register("viewer_policy", 3, "SELECT 1;")
	store.RegisterSQLiteApply("viewer_policy", 3, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='catalog_titles'`).Scan(&n); err != nil || n == 0 {
			return err // partial binaries without the catalogue
		}
		_, err := tx.ExecContext(ctx, sqlite)
		return err
	})
	store.RegisterDialect("viewer_policy", 3, store.Postgres, `DO $$ BEGIN IF to_regclass('catalog_titles') IS NOT NULL THEN
 EXECUTE 'DROP TRIGGER IF EXISTS viewer_policy_catalog_titles ON catalog_titles';
 EXECUTE 'CREATE TRIGGER viewer_policy_catalog_titles AFTER INSERT OR DELETE ON catalog_titles FOR EACH STATEMENT EXECUTE FUNCTION viewer_policy_bump()';
 EXECUTE 'CREATE TRIGGER viewer_policy_catalog_titles_update AFTER UPDATE ON catalog_titles FOR EACH ROW WHEN (`+strings.Join(pgChanged, " OR ")+`) EXECUTE FUNCTION viewer_policy_bump()';
END IF;
IF to_regclass('catalog_home_release') IS NOT NULL THEN
 EXECUTE 'DROP TRIGGER IF EXISTS viewer_policy_catalog_home_release ON catalog_home_release';
 EXECUTE 'CREATE TRIGGER viewer_policy_catalog_home_release AFTER INSERT OR DELETE ON catalog_home_release FOR EACH STATEMENT EXECUTE FUNCTION viewer_policy_bump()';
 EXECUTE 'CREATE TRIGGER viewer_policy_catalog_home_release_update AFTER UPDATE ON catalog_home_release FOR EACH ROW WHEN (OLD.checked IS DISTINCT FROM NEW.checked OR OLD.earliest_ts IS DISTINCT FROM NEW.earliest_ts OR OLD.fallback_admitted IS DISTINCT FROM NEW.fallback_admitted) EXECUTE FUNCTION viewer_policy_bump()';
END IF; END $$;`)
}
