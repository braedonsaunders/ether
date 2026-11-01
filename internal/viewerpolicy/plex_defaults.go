package viewerpolicy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("viewer_policy", 5, "SELECT 1;")
	store.RegisterSQLiteApply("viewer_policy", 5, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='auth_plex_defaults'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "viewer_policy_auth_plex_defaults_" + strings.ToLower(event)
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s AFTER %s ON auth_plex_defaults BEGIN UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1; END;`, name, event)); err != nil {
				return err
			}
		}
		return nil
	})
	store.RegisterDialect("viewer_policy", 5, store.Postgres, `
DO $$ BEGIN
 IF to_regclass('auth_plex_defaults') IS NOT NULL
    AND to_regprocedure('viewer_policy_bump()') IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'viewer_policy_auth_plex_defaults') THEN
  EXECUTE 'CREATE TRIGGER viewer_policy_auth_plex_defaults AFTER INSERT OR UPDATE OR DELETE ON auth_plex_defaults FOR EACH STATEMENT EXECUTE FUNCTION viewer_policy_bump()';
 END IF;
END $$;`)
}
