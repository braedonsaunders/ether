package identity

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

// PostgreSQL old writers renew or revoke frontend_image_grants without taking
// capability:image:<shard>. These tests hold that row lock, then require
// PruneImageGrantShard to show up in pg_blocking_pids before the writer
// commits, rolls back, or the prune context is cancelled.

func TestImageCleanupOldWriterInterleavings(t *testing.T) {
	db := openImageCleanupPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cutoff := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0),(2,124,0,0)`); err != nil {
		t.Fatal(err)
	}
	service, credential := imageAdapterService(t, db, "jellyfin", func() time.Time { return cutoff })
	source, ok := grantHash(credential)
	if !ok {
		t.Fatal("fixture credential was not a grant")
	}
	secret, target := imageCleanupSecret(t, 9)
	fx := imageCleanupFixture{db: db, source: source, secret: secret, target: target, cutoff: cutoff}

	t.Run("RenewalRecheck", func(t *testing.T) {
		fx.reset(t, ctx)
		fx.seed(t, ctx)
		fx.interleave(t, ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `UPDATE frontend_image_grants SET expires=? WHERE hash=? AND expires<=?`, fx.renewed(), target, fx.ms())
			return imageCleanupWroteOne(res, err)
		}, func(outcome pruneOutcome) {
			if outcome.err != nil || outcome.n != 3 {
				t.Fatalf("prune removed=%d err=%v", outcome.n, outcome.err)
			}
		})
		fx.assertRow(t, ctx, target, 1, fx.renewed(), 0)
		fx.assertRow(t, ctx, fx.sacrificial, 0, 0, 0)
		fx.assertRow(t, ctx, fx.live, 1, fx.renewed(), 0)
		fx.assertRow(t, ctx, fx.other, 1, fx.ms()-3000, 0)
		if n := fx.headers(t, ctx, imageCapabilityNamespace, target); n != 0 {
			t.Fatalf("unrenewed header for the preserved legacy row = %d", n)
		}
		if n := fx.headers(t, ctx, imageCapabilityNamespace, fx.other); n != 1 {
			t.Fatalf("other-shard header = %d", n)
		}
		if n := fx.headers(t, ctx, "ticket", fx.ticket); n != 1 {
			t.Fatalf("ticket header = %d", n)
		}
		if code := fx.serve(service); code != 200 {
			t.Fatalf("renewed grant serve = %d", code)
		}
	})

	t.Run("RevokeCannotReauthorize", func(t *testing.T) {
		fx.reset(t, ctx)
		fx.seed(t, ctx)
		fx.interleave(t, ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `UPDATE frontend_image_grants SET revoked=1, expires=? WHERE hash=? AND revoked=0 AND expires<=?`, fx.renewed(), target, fx.ms())
			return imageCleanupWroteOne(res, err)
		}, func(outcome pruneOutcome) {
			if outcome.err != nil || outcome.n != 3 {
				t.Fatalf("prune removed=%d err=%v", outcome.n, outcome.err)
			}
		})
		fx.assertRow(t, ctx, target, 1, fx.renewed(), 1)
		fx.assertRow(t, ctx, fx.sacrificial, 0, 0, 0)
		fx.assertRow(t, ctx, fx.other, 1, fx.ms()-3000, 0)
		if n := fx.headers(t, ctx, "ticket", fx.ticket); n != 1 {
			t.Fatalf("ticket header = %d", n)
		}
		if code := fx.serve(service); code != 404 {
			t.Fatalf("revoked grant serve = %d", code)
		}
	})

	t.Run("RenewalRollback", func(t *testing.T) {
		fx.reset(t, ctx)
		fx.seed(t, ctx)
		fx.interleaveFinish(t, ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `UPDATE frontend_image_grants SET expires=? WHERE hash=? AND expires<=?`, fx.renewed(), target, fx.ms())
			return imageCleanupWroteOne(res, err)
		}, func(tx *sql.Tx) error {
			return tx.Rollback()
		}, func(outcome pruneOutcome) {
			if outcome.err != nil || outcome.n != 4 {
				t.Fatalf("prune removed=%d err=%v", outcome.n, outcome.err)
			}
		})
		fx.assertRow(t, ctx, target, 0, 0, 0)
		fx.assertRow(t, ctx, fx.live, 1, fx.renewed(), 0)
		fx.assertRow(t, ctx, fx.other, 1, fx.ms()-3000, 0)
		if n := fx.headers(t, ctx, "ticket", fx.ticket); n != 1 {
			t.Fatalf("ticket header = %d", n)
		}
		if code := fx.serve(service); code != 404 {
			t.Fatalf("rolled-back grant serve = %d", code)
		}
	})

	t.Run("PruneCancelRollback", func(t *testing.T) {
		fx.reset(t, ctx)
		fx.seed(t, ctx)
		fx.interleaveCancel(t, ctx)
		fx.assertRow(t, ctx, target, 1, fx.renewed(), 0)
		fx.assertRow(t, ctx, fx.sacrificial, 1, fx.ms()-2000, 0)
		fx.assertRow(t, ctx, fx.other, 1, fx.ms()-3000, 0)
		if n := fx.headers(t, ctx, imageCapabilityNamespace, fx.sacrificial); n != 1 {
			t.Fatalf("sacrificial header = %d", n)
		}
		if n := fx.headers(t, ctx, "ticket", fx.ticket); n != 1 {
			t.Fatalf("ticket header = %d", n)
		}
	})
}

// TestImageCleanupSkewedShardScanPlan records whether LIMIT 40 bounds the
// legacy expiry scan when earlier-expiring rows belong to other shards.
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
	if _, err := db.ExecContext(ctx, `INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,channel,image_subject)
		SELECT lpad(to_hex(g), 64, '0'), ?, CASE WHEN g > ? THEN 1 ELSE 2 END, 'jellyfin', 1, '{"ID":"tv"}', 'poster', 'opaque-poster', 'v1', CASE WHEN g > ? THEN ? ELSE ? - g END, '', 'tmdb:1'
		FROM generate_series(1, ?) AS g`, source, otherRows, otherRows, cutoff-1, cutoff-100_000, total); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires)
		SELECT ?, lpad(to_hex(g), 64, '0'), CASE WHEN g > ? THEN 1 ELSE 2 END, CASE WHEN g > ? THEN 1 ELSE 2 END, 'jellyfin', 'tv', CASE WHEN g > ? THEN ? ELSE ? - g END
		FROM generate_series(1, ?) AS g`, imageCapabilityNamespace, otherRows, otherRows, otherRows, cutoff-1, cutoff-100_000, total); err != nil {
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

	legacySelect := `SELECT hash FROM frontend_image_grants WHERE expires<=? AND (viewer_id % ?)=? ORDER BY expires,hash LIMIT ?`
	legacyArgs := []any{cutoff, capability.HeaderShardCount, shard, capability.HeaderBatchMax}
	legacyDelete := `DELETE FROM frontend_image_grants WHERE expires<=? AND (viewer_id % ?)=? AND hash IN (SELECT hash FROM frontend_image_grants WHERE expires<=? AND (viewer_id % ?)=? ORDER BY expires,hash LIMIT ?)`
	legacyDeleteArgs := []any{cutoff, capability.HeaderShardCount, shard, cutoff, capability.HeaderShardCount, shard, capability.HeaderBatchMax}
	headerSelect := `SELECT digest FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? ORDER BY expires,digest LIMIT ?`
	headerArgs := []any{imageCapabilityNamespace, shard, cutoff, capability.HeaderBatchMax}

	defaultLegacy := explainPlan(t, ctx, db, true, legacySelect, legacyArgs...)
	forcedLegacy := explainPlanTx(t, ctx, db, legacySelect, legacyArgs...)
	t.Logf("legacy select default removed=%.0f scan_actual=%.0f indexes=%v", defaultLegacy.stats.removed, defaultLegacy.stats.scanActual, defaultLegacy.stats.indexes)
	t.Logf("legacy select default plan:\n%s", defaultLegacy.text)
	t.Logf("legacy select index-forced removed=%.0f scan_actual=%.0f indexes=%v", forcedLegacy.stats.removed, forcedLegacy.stats.scanActual, forcedLegacy.stats.indexes)
	t.Logf("legacy select index-forced plan (enable_seqscan=off, enable_bitmapscan=off):\n%s", forcedLegacy.text)
	t.Logf("legacy delete plan (not executed):\n%s", readExplain(t, ctx, db, "EXPLAIN", legacyDelete, legacyDeleteArgs...))
	if !planExceedsLimit(forcedLegacy.stats, float64(capability.HeaderBatchMax)) {
		t.Fatalf("index-forced legacy scan did not examine more than LIMIT %d: removed=%.0f scan_actual=%.0f", capability.HeaderBatchMax, forcedLegacy.stats.removed, forcedLegacy.stats.scanActual)
	}
	if !planExceedsLimit(defaultLegacy.stats, float64(capability.HeaderBatchMax)) {
		t.Fatalf("default legacy scan did not examine more than LIMIT %d: removed=%.0f scan_actual=%.0f", capability.HeaderBatchMax, defaultLegacy.stats.removed, defaultLegacy.stats.scanActual)
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

type imageCleanupFixture struct {
	db                               *sql.DB
	source, secret, target           string
	sacrificial, live, other, ticket string
	cutoff                           time.Time
}

func (f *imageCleanupFixture) ms() int64 { return f.cutoff.UnixMilli() }

func (f *imageCleanupFixture) renewed() int64 { return f.cutoff.Add(time.Hour).UnixMilli() }

func (f *imageCleanupFixture) reset(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.db.ExecContext(ctx, `DELETE FROM capability_headers`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM frontend_image_grants`); err != nil {
		t.Fatal(err)
	}
}

func (f *imageCleanupFixture) seed(t *testing.T, ctx context.Context) {
	t.Helper()
	f.sacrificial = fmt.Sprintf("%064x", 11)
	f.live = fmt.Sprintf("%064x", 12)
	f.other = fmt.Sprintf("%064x", 13)
	f.ticket = fmt.Sprintf("%064x", 14)
	for _, hash := range []string{f.sacrificial, f.live, f.other, f.ticket} {
		if hash == f.target {
			t.Fatal("synthetic hash collided with the served grant")
		}
	}
	for _, row := range []struct {
		hash    string
		viewer  int64
		expires int64
	}{
		{f.target, 1, f.ms() - 1000},
		{f.sacrificial, 1, f.ms() - 2000},
		{f.live, 1, f.renewed()},
		{f.other, 2, f.ms() - 3000},
	} {
		if _, err := f.db.ExecContext(ctx, `INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,channel,image_subject) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, row.hash, f.source, row.viewer, "jellyfin", int64(core.MovieKey(7)), `{"ID":"tv"}`, frontend.ImagePoster, "opaque-poster", "v1", row.expires, "", "tmdb:1"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES(?,?,?,?,?,?,?)`, imageCapabilityNamespace, row.hash, row.viewer%capability.HeaderShardCount, row.viewer, "jellyfin", "tv", row.expires); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES('ticket',?,1,1,'jellyfin','tv',1)`, f.ticket); err != nil {
		t.Fatal(err)
	}
}

func (f *imageCleanupFixture) interleave(t *testing.T, ctx context.Context, write func(*sql.Tx) error, check func(pruneOutcome)) {
	t.Helper()
	f.interleaveFinish(t, ctx, write, func(tx *sql.Tx) error { return tx.Commit() }, check)
}

func (f *imageCleanupFixture) interleaveFinish(t *testing.T, ctx context.Context, write func(*sql.Tx) error, finish func(*sql.Tx) error, check func(pruneOutcome)) {
	t.Helper()
	f.withHeldRow(t, ctx, write, func(hold *sql.Tx, out <-chan pruneOutcome, _ func()) {
		if err := finish(hold); err != nil {
			t.Fatal(err)
		}
		check(waitPrune(t, ctx, out))
	})
}

func (f *imageCleanupFixture) interleaveCancel(t *testing.T, ctx context.Context) {
	t.Helper()
	f.withHeldRow(t, ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE frontend_image_grants SET expires=? WHERE hash=? AND expires<=?`, f.renewed(), f.target, f.ms())
		return imageCleanupWroteOne(res, err)
	}, func(hold *sql.Tx, out <-chan pruneOutcome, cancelPrune func()) {
		cancelPrune()
		outcome := waitPrune(t, ctx, out)
		if outcome.err == nil {
			t.Fatal("prune committed after its context was cancelled")
		}
		if err := hold.Commit(); err != nil {
			t.Fatal(err)
		}
	})
}

func (f *imageCleanupFixture) withHeldRow(t *testing.T, ctx context.Context, write func(*sql.Tx) error, after func(*sql.Tx, <-chan pruneOutcome, func())) {
	t.Helper()
	hold, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	pruneCtx, pruneCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		pruneCancel()
		_ = hold.Rollback()
		waitImageCleanupWorkers(t, &wg)
	}()
	if err = write(hold); err != nil {
		t.Fatal(err)
	}
	var holderPID int
	if err = hold.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	out := make(chan pruneOutcome, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, err := PruneImageGrantShard(pruneCtx, f.db, f.cutoff, 1)
		out <- pruneOutcome{n: n, err: err}
	}()
	pid, event := waitForImageCleanupBlock(t, ctx, f.db, holderPID, out)
	t.Logf("observed pg_blocking_pids holder=%d blocked=%d wait=%s", holderPID, pid, event)
	after(hold, out, pruneCancel)
}

func (f *imageCleanupFixture) assertRow(t *testing.T, ctx context.Context, hash string, wantN int, wantExpires int64, wantRevoked int) {
	t.Helper()
	var n, revoked int
	var expires sql.NullInt64
	err := f.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(expires),0), COALESCE(MAX(revoked),0) FROM frontend_image_grants WHERE hash=?`, hash).Scan(&n, &expires, &revoked)
	if err != nil {
		t.Fatal(err)
	}
	if n != wantN || (wantN == 1 && (expires.Int64 != wantExpires || revoked != wantRevoked)) {
		t.Fatalf("hash count=%d expires=%d revoked=%d want count=%d expires=%d revoked=%d", n, expires.Int64, revoked, wantN, wantExpires, wantRevoked)
	}
}

func (f *imageCleanupFixture) headers(t *testing.T, ctx context.Context, namespace, digest string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND digest=?`, namespace, digest).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *imageCleanupFixture) serve(service *ImageGrantService) int {
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), f.secret)
	return w.Code
}

type pruneOutcome struct {
	n   int64
	err error
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

func imageCleanupSecret(t *testing.T, b byte) (secret, hash string) {
	t.Helper()
	secret = base64.RawURLEncoding.EncodeToString(bytesRepeat(b))
	hash, ok := validImageSecret(secret)
	if !ok {
		t.Fatal("fixture secret rejected")
	}
	return secret, hash
}

func bytesRepeat(b byte) []byte {
	buf := make([]byte, 32)
	for i := range buf {
		buf[i] = b
	}
	return buf
}

func imageCleanupWroteOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("writer updated %d rows", n)
	}
	return nil
}

func waitForImageCleanupBlock(t *testing.T, ctx context.Context, db *sql.DB, holderPID int, out <-chan pruneOutcome) (int, string) {
	t.Helper()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var last string
	for {
		select {
		case outcome := <-out:
			t.Fatalf("prune returned before a lock wait was observed: removed=%d err=%v", outcome.n, outcome.err)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for pg_blocking_pids; last=%s", last)
		case <-tick.C:
			var pid int
			var kind, event, query string
			err := db.QueryRowContext(ctx, `SELECT pid, COALESCE(wait_event_type,''), COALESCE(wait_event,''), query FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND ? = ANY(pg_blocking_pids(pid)) LIMIT 1`, holderPID).Scan(&pid, &kind, &event, &query)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			shown := query
			if len(shown) > 180 {
				shown = shown[:180]
			}
			last = fmt.Sprintf("pid=%d %s/%s %s", pid, kind, event, shown)
			if kind != "Lock" || event == "advisory" || !strings.Contains(query, "DELETE") || !strings.Contains(query, "frontend_image_grants") {
				continue
			}
			return pid, kind + "/" + event
		}
	}
}

func waitPrune(t *testing.T, ctx context.Context, out <-chan pruneOutcome) pruneOutcome {
	t.Helper()
	select {
	case outcome := <-out:
		return outcome
	case <-ctx.Done():
		t.Fatal("prune did not finish")
		return pruneOutcome{}
	}
}

func waitImageCleanupWorkers(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("prune goroutine did not exit")
	}
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
