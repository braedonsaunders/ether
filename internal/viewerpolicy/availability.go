package viewerpolicy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("viewer_policy", "availability", "resolver")
	store.Register("viewer_policy", 2, "SELECT 1;")
	inputs := map[string][]string{
		"availability_releases":   {"rating_key", "source_id", "generation", "payload", "state", "first_seen", "revision"},
		"availability_feed_items": {"source_id", "generation", "match_title", "payload"},
		"resolver_availability":   {"rating_key", "available", "checked_at", "expires_at"},
	}
	store.RegisterSQLiteApply("viewer_policy", 2, func(ctx context.Context, tx *sql.Tx) error {
		for _, table := range []string{"availability_releases", "availability_feed_items", "resolver_availability"} {
			columns := inputs[table]
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
				when := ""
				if event == "UPDATE" {
					var changed []string
					for _, column := range columns {
						changed = append(changed, "OLD."+column+" IS NOT NEW."+column)
					}
					if table == "availability_releases" {
						changed = append(changed, "((OLD.last_seen < unixepoch()-31708800) <> (NEW.last_seen < unixepoch()-31708800))")
					}
					if table == "availability_feed_items" {
						changed = append(changed, "((OLD.seen_at < unixepoch()-31708800) <> (NEW.seen_at < unixepoch()-31708800))")
					}
					when = " WHEN " + strings.Join(changed, " OR ")
					if table == "resolver_availability" {
						when = " WHEN (OLD.available=0 OR NEW.available=0) AND (" + strings.Join(changed, " OR ") + ")"
					}
				} else if table == "resolver_availability" {
					row := "NEW"
					if event == "DELETE" {
						row = "OLD"
					}
					when = " WHEN " + row + ".available=0"
				}
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER viewer_policy_%s_%s AFTER %s ON %s%s BEGIN UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1; END;`, table, strings.ToLower(event), event, table, when)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	pg := ""
	for _, table := range []string{"availability_releases", "availability_feed_items", "resolver_availability"} {
		columns := inputs[table]
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			when := ""
			if event == "UPDATE" {
				var changed []string
				for _, column := range columns {
					changed = append(changed, "OLD."+column+" IS DISTINCT FROM NEW."+column)
				}
				if table == "availability_releases" {
					changed = append(changed, "((OLD.last_seen < EXTRACT(EPOCH FROM CURRENT_TIMESTAMP)-31708800) <> (NEW.last_seen < EXTRACT(EPOCH FROM CURRENT_TIMESTAMP)-31708800))")
				}
				if table == "availability_feed_items" {
					changed = append(changed, "((OLD.seen_at < EXTRACT(EPOCH FROM CURRENT_TIMESTAMP)-31708800) <> (NEW.seen_at < EXTRACT(EPOCH FROM CURRENT_TIMESTAMP)-31708800))")
				}
				when = " WHEN (" + strings.Join(changed, " OR ") + ")"
				if table == "resolver_availability" {
					when = " WHEN ((OLD.available=0 OR NEW.available=0) AND (" + strings.Join(changed, " OR ") + "))"
				}
			} else if table == "resolver_availability" {
				row := "NEW"
				if event == "DELETE" {
					row = "OLD"
				}
				when = " WHEN (" + row + ".available=0)"
			}
			pg += fmt.Sprintf(` DO $$ BEGIN IF to_regclass('%s') IS NOT NULL THEN EXECUTE 'CREATE TRIGGER viewer_policy_%s_%s AFTER %s ON %s FOR EACH ROW%s EXECUTE FUNCTION viewer_policy_bump()'; END IF; END $$;`, table, table, strings.ToLower(event), event, table, when)
		}
	}
	store.RegisterDialect("viewer_policy", 2, store.Postgres, pg)
}
