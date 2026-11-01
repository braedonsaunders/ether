package perfharness

import (
	"context"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestPlaceholders(t *testing.T) {
	for q, want := range map[string]int{
		"SELECT 1": 0,
		"SELECT a FROM t WHERE a=? AND b IN (?,?)": 3,
		"SELECT '?' FROM t WHERE a=?":              1,
		"SELECT a -- ?\nFROM t WHERE b=?":          1,
		"SELECT ?2, ?1":                            2,
	} {
		if got := placeholders(q); got != want {
			t.Errorf("placeholders(%q) = %d, want %d", q, got, want)
		}
	}
}

// The audit flags a full scan and a temporary sort on SQLite, and plans
// generic statements on PostgreSQL when the suite runs against it.
func TestExplainFlagsScans(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE audit_t (id INTEGER PRIMARY KEY, a INTEGER, b TEXT)`); err != nil {
		t.Fatal(err)
	}
	q := `SELECT id FROM audit_t WHERE b=? ORDER BY a`
	if store.DialectOf(db) == store.Postgres {
		var version int
		if err := db.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil || version < 160000 {
			t.Skip("EXPLAIN (GENERIC_PLAN) needs PostgreSQL 16")
		}
		plan, issues := explainPostgres(ctx, db, q, map[string]int64{"audit_t": 10000})
		if len(plan) == 0 || strings.HasPrefix(plan[0], "explain failed") || !strings.Contains(strings.Join(issues, ";"), "sequential scan of audit_t") {
			t.Fatalf("plan %v issues %v", plan, issues)
		}
		return
	}
	plan, issues := explain(ctx, db, q, map[string]int64{"audit_t": 10000}, nil)
	joined := strings.Join(issues, ";")
	if !strings.Contains(joined, "full table scan of audit_t") || !strings.Contains(joined, "temp b-tree for order by") {
		t.Fatalf("plan %v issues %v", plan, issues)
	}
}
