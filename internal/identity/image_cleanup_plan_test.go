package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
	"os"
	"strings"
	"testing"
	"time"
)

// EXPLAIN ANALYZE runs only for the SELECT shape. The production DELETE is
// planned with EXPLAIN and is not executed.
func TestImageCleanupSkewedShardScanPlan(t *testing.T) {
	db := openImageCleanupPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const (
		otherRows  int64 = 2000
		targetRows int64 = 40
		shard      int64 = 1
	)
	total := otherRows + targetRows
	cutoff := time.Now().UTC().Truncate(time.Millisecond).UnixMilli()
	source := strings.Repeat("a", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0),(2,124,0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'jellyfin','tv','','','',0,0,?)`, source, cutoff+3_600_000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,2,'jellyfin','tv','','','',0,0,?)`, strings.Repeat("b", 64), cutoff+3_600_000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires)
		SELECT ?, lpad(to_hex(g), 64, '0'), CASE WHEN g > ? THEN 1 ELSE 2 END, CASE WHEN g > ? THEN 1 ELSE 2 END, 'jellyfin', 'tv', CASE WHEN g > CAST(? AS BIGINT) THEN CAST(? AS BIGINT) ELSE CAST(? AS BIGINT) - g END
		FROM generate_series(1, ?) AS g`, imageCapabilityNamespace, otherRows, otherRows, otherRows, cutoff-1, cutoff-100_000, total); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,channel,image_subject)
		SELECT lpad(to_hex(g), 64, '0'), CASE WHEN g > CAST(? AS BIGINT) THEN CAST(? AS TEXT) ELSE CAST(? AS TEXT) END, CASE WHEN g > ? THEN 1 ELSE 2 END, 'jellyfin', 1, '{"ID":"tv"}', 'poster', 'opaque-poster', 'v1', CASE WHEN g > CAST(? AS BIGINT) THEN CAST(? AS BIGINT) ELSE CAST(? AS BIGINT) - g END, '', 'tmdb:1'
		FROM generate_series(1, ?) AS g`, otherRows, source, strings.Repeat("b", 64), otherRows, otherRows, cutoff-1, cutoff-100_000, total); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE frontend_image_grants`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE capability_headers`); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := db.QueryRowContext(ctx, `SHOW server_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("server_version=%s other_rows=%d target_rows=%d modulus=%d limit=%d cutoff_ms=%d shard=%d", version, otherRows, targetRows, capability.HeaderShardCount, capability.HeaderBatchMax, cutoff, shard)

	legacySelect := `SELECT hash FROM frontend_image_grants WHERE expires<=? AND (viewer_id % 64)=? ORDER BY expires,hash LIMIT ?`
	legacyArgs := []any{cutoff, shard, capability.HeaderBatchMax}
	legacyDelete := `DELETE FROM frontend_image_grants WHERE expires<=? AND (viewer_id % 64)=? AND hash IN (SELECT hash FROM frontend_image_grants WHERE expires<=? AND (viewer_id % 64)=? ORDER BY expires,hash LIMIT ?)`
	legacyDeleteArgs := []any{cutoff, shard, cutoff, shard, capability.HeaderBatchMax}
	headerSelect := `SELECT digest FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? ORDER BY expires,digest LIMIT ?`
	headerArgs := []any{imageCapabilityNamespace, shard, cutoff, capability.HeaderBatchMax}

	defaultLegacy := explainPlan(t, ctx, db, true, legacySelect, legacyArgs...)
	forcedLegacy := explainPlanTx(t, ctx, db, legacySelect, legacyArgs...)
	t.Logf("legacy select default removed=%.0f scan_actual=%.0f indexes=%v", defaultLegacy.stats.removed, defaultLegacy.stats.scanActual, defaultLegacy.stats.indexes)
	t.Logf("legacy select default plan:\n%s", defaultLegacy.text)
	t.Logf("legacy select index-forced removed=%.0f scan_actual=%.0f indexes=%v", forcedLegacy.stats.removed, forcedLegacy.stats.scanActual, forcedLegacy.stats.indexes)
	t.Logf("legacy select index-forced plan (enable_seqscan=off, enable_bitmapscan=off):\n%s", forcedLegacy.text)
	t.Logf("legacy delete plan (not executed):\n%s", readExplain(t, ctx, db, "EXPLAIN", legacyDelete, legacyDeleteArgs...))
	for label, plan := range map[string]explained{"default": defaultLegacy, "forced": forcedLegacy} {
		if !strings.Contains(strings.Join(plan.stats.indexes, ","), "frontend_image_grants_shard_expiry") || planExceedsLimit(plan.stats, float64(capability.HeaderBatchMax)) {
			t.Fatalf("%s legacy plan missed shard index or exceeded limit: removed=%.0f scan_actual=%.0f indexes=%v", label, plan.stats.removed, plan.stats.scanActual, plan.stats.indexes)
		}
	}

	defaultHeader := explainPlan(t, ctx, db, true, headerSelect, headerArgs...)
	forcedHeader := explainPlanTx(t, ctx, db, headerSelect, headerArgs...)
	t.Logf("header select default removed=%.0f scan_actual=%.0f indexes=%v", defaultHeader.stats.removed, defaultHeader.stats.scanActual, defaultHeader.stats.indexes)
	t.Logf("header select default plan:\n%s", defaultHeader.text)
	t.Logf("header select index-forced removed=%.0f scan_actual=%.0f indexes=%v cond=%v", forcedHeader.stats.removed, forcedHeader.stats.scanActual, forcedHeader.stats.indexes, forcedHeader.stats.indexCond)
	t.Logf("header select index-forced plan:\n%s", forcedHeader.text)
	if !headerPlanUsesShard(forcedHeader) {
		t.Fatalf("header plan did not use the shard expiry index or scanned other shards: removed=%.0f scan_actual=%.0f indexes=%v", forcedHeader.stats.removed, forcedHeader.stats.scanActual, forcedHeader.stats.indexes)
	}

	var left int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_image_grants`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != int(total) {
		t.Fatalf("explain changed legacy rows: %d", left)
	}
}

func openImageCleanupPostgres(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("requires PostgreSQL transaction interleaving")
	}
	db := store.OpenTest(t)
	if store.DialectOf(db) != store.Postgres {
		t.Fatal("ETHER_TEST_POSTGRES did not open postgres")
	}
	return db
}

type explained struct {
	text  string
	stats planStats
}

type planStats struct {
	removed, scanActual float64
	indexes             []string
	indexCond           []string
}

type explainDocument struct {
	Plan explainNode `json:"Plan"`
}

type explainNode struct {
	NodeType   string        `json:"Node Type"`
	ActualRows float64       `json:"Actual Rows"`
	Removed    float64       `json:"Rows Removed by Filter"`
	IndexName  string        `json:"Index Name"`
	IndexCond  string        `json:"Index Cond"`
	Plans      []explainNode `json:"Plans"`
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func explainPlan(t *testing.T, ctx context.Context, db queryer, analyze bool, query string, args ...any) explained {
	t.Helper()
	prefix := "EXPLAIN"
	if analyze {
		prefix = "EXPLAIN (ANALYZE, BUFFERS)"
	}
	text := readExplain(t, ctx, db, prefix, query, args...)
	if !analyze {
		return explained{text: text}
	}
	raw := readExplain(t, ctx, db, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)", query, args...)
	return explained{text: text, stats: parsePlanStats(t, raw)}
}

func explainPlanTx(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) explained {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL enable_bitmapscan = off`); err != nil {
		t.Fatal(err)
	}
	return explainPlan(t, ctx, tx, true, query, args...)
}

func readExplain(t *testing.T, ctx context.Context, db queryer, prefix, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, prefix+" "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func parsePlanStats(t *testing.T, raw string) planStats {
	t.Helper()
	var docs []explainDocument
	if err := json.Unmarshal([]byte(raw), &docs); err != nil {
		t.Fatalf("plan json: %v\n%s", err, raw)
	}
	if len(docs) != 1 {
		t.Fatalf("plan documents = %d", len(docs))
	}
	var stats planStats
	walkPlan(&docs[0].Plan, &stats)
	return stats
}

func walkPlan(n *explainNode, stats *planStats) {
	stats.removed += n.Removed
	if strings.Contains(n.NodeType, "Scan") && n.ActualRows > stats.scanActual {
		stats.scanActual = n.ActualRows
	}
	if n.IndexName != "" {
		stats.indexes = append(stats.indexes, n.IndexName)
	}
	if n.IndexCond != "" {
		stats.indexCond = append(stats.indexCond, n.IndexCond)
	}
	for i := range n.Plans {
		walkPlan(&n.Plans[i], stats)
	}
}

func planExceedsLimit(stats planStats, limit float64) bool {
	return stats.removed > limit || stats.scanActual > limit
}

func headerPlanUsesShard(got explained) bool {
	if got.stats.removed >= 2000 || got.stats.scanActual > float64(capability.HeaderBatchMax) {
		return false
	}
	for _, name := range got.stats.indexes {
		if name == "capability_headers_expiry" {
			return true
		}
	}
	for _, cond := range got.stats.indexCond {
		if strings.Contains(cond, "shard") {
			return true
		}
	}
	return false
}
