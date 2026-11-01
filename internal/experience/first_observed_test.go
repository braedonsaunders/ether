package experience

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFirstObservedLedgerParentAndSourceScope(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	a, _ := NewFirstObservedLedger(db)
	b, _ := NewFirstObservedLedger(db)
	movie, other := core.MovieKey(1), core.MovieKey(2)
	source := "provider:source-with-credential"
	first := time.Date(2026, 1, 1, 12, 0, 0, 123, time.UTC)
	if got, err := a.FirstObservedAt(ctx, movie, source); err != nil || got != nil {
		t.Fatalf("unknown %v %v", got, err)
	}
	if got, err := a.Observe(ctx, movie, source, first); err != nil || !got.Equal(first) {
		t.Fatal(got, err)
	}
	if got, err := b.Observe(ctx, movie, source, first.Add(time.Hour)); err != nil || !got.Equal(first) {
		t.Fatal(got, err)
	}
	earlier := first.Add(-time.Hour)
	if got, err := b.Observe(ctx, movie, source, earlier); err != nil || !got.Equal(first) {
		t.Fatal(got, err)
	}
	if got, err := a.FirstObservedAt(ctx, other, source); err != nil || got != nil {
		t.Fatal("different parent", got, err)
	}
	if got, err := a.FirstObservedAt(ctx, movie, "other-source"); err != nil || got != nil {
		t.Fatal("different source", got, err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT source_hash FROM release_observations`).Scan(&stored); err != nil || strings.Contains(stored, "credential") {
		t.Fatal("source stored directly", stored, err)
	}
	if _, err := a.Observe(ctx, movie, source, time.Time{}); err == nil {
		t.Fatal("zero time accepted")
	}
	if _, err := a.Observe(ctx, core.PlaylistKey(1), source, first); err == nil {
		t.Fatal("non-title parent accepted")
	}
	if _, err := a.FirstObservedAt(ctx, movie, "   "); err == nil {
		t.Fatal("blank source accepted")
	}
	if _, err := a.FirstObservedAt(ctx, movie, ""); err == nil {
		t.Fatal("empty source accepted")
	}
	if _, err := a.Observe(ctx, movie, source, time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestFirstObservedConcurrentNodesKeepFirstCommitted(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	a, _ := NewFirstObservedLedger(db)
	b, _ := NewFirstObservedLedger(db)
	base := time.Unix(1800000000, 123).UTC()
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	values := make(chan time.Time, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ledger := a
			if i%2 != 0 {
				ledger = b
			}
			first, err := ledger.Observe(ctx, core.MovieKey(1), "indexer:stable-release", base.Add(time.Duration(n-i)*time.Second))
			errs <- err
			values <- first
		}(i)
	}
	wg.Wait()
	close(errs)
	close(values)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := a.FirstObservedAt(ctx, core.MovieKey(1), "indexer:stable-release")
	if err != nil || got == nil {
		t.Fatalf("first observation %v %v", got, err)
	}
	for first := range values {
		if !first.Equal(*got) {
			t.Fatalf("observation changed: %v then %v", first, *got)
		}
	}
}

func TestFirstObservedBatchAtomicAndStable(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	ledger, _ := NewFirstObservedLedger(db)
	now := time.Unix(1000, 0)
	parent := core.MovieKey(5)
	first, err := ledger.ObserveBatch(ctx, parent, []string{"candidate:a", "candidate:b", "candidate:a"}, now)
	if err != nil || len(first) != 2 || !first["candidate:a"].Equal(now) {
		t.Fatal(first, err)
	}
	second, err := ledger.ObserveBatch(ctx, parent, []string{"candidate:b", "candidate:a"}, now.Add(time.Hour))
	if err != nil || !second["candidate:a"].Equal(now) || !second["candidate:b"].Equal(now) {
		t.Fatal(second, err)
	}
	if _, err = ledger.ObserveBatch(ctx, parent, []string{"candidate:new", ""}, now); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if got, err := ledger.FirstObservedAt(ctx, parent, "candidate:new"); err != nil || got != nil {
		t.Fatal("partial batch persisted", got, err)
	}
}

func TestFirstObservedBatchRollsBackOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	ledger, _ := NewFirstObservedLedger(db)
	parent := core.MovieKey(7)
	firstSource, rejectedSource := "candidate:a", "candidate:b"
	firstHash, err := observationIdentity(parent, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := observationIdentity(parent, rejectedSource)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash > hash {
		firstSource, rejectedSource = rejectedSource, firstSource
		hash = firstHash
	}
	ddl := `CREATE TRIGGER reject_observation BEFORE INSERT ON release_observations WHEN NEW.source_hash='` + hash + `' BEGIN SELECT RAISE(ABORT,'injected failure'); END`
	if store.DialectOf(db) == store.Postgres {
		ddl = `CREATE FUNCTION reject_observation_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_hash='` + hash + `' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_observation BEFORE INSERT ON release_observations FOR EACH ROW EXECUTE FUNCTION reject_observation_fn()`
	}
	_, err = db.ExecContext(ctx, ddl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.ObserveBatch(ctx, parent, []string{"candidate:a", "candidate:b"}, time.Unix(1000, 0)); err == nil {
		t.Fatal("failed batch accepted")
	}
	got, err := ledger.FirstObservedAt(ctx, parent, firstSource)
	if err != nil || got != nil {
		t.Fatalf("partial write: %v %v", got, err)
	}
}

func TestFirstObservedWarmPathDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	ledger, _ := NewFirstObservedLedger(db)
	parent := core.MovieKey(11)
	now := time.Unix(1000, 0)
	if _, err := ledger.ObserveBatch(ctx, parent, []string{"a", "b"}, now); err != nil {
		t.Fatal(err)
	}
	ddl := `CREATE TRIGGER no_insert_observation BEFORE INSERT ON release_observations BEGIN SELECT RAISE(ABORT,'unexpected insert'); END; CREATE TRIGGER no_update_observation BEFORE UPDATE ON release_observations BEGIN SELECT RAISE(ABORT,'unexpected update'); END`
	if store.DialectOf(db) == store.Postgres {
		ddl = `CREATE FUNCTION no_write_observation_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected write'; END; $$; CREATE TRIGGER no_insert_observation BEFORE INSERT OR UPDATE ON release_observations FOR EACH ROW EXECUTE FUNCTION no_write_observation_fn()`
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Observe(ctx, parent, "a", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	values, err := ledger.ObserveBatch(ctx, parent, []string{"b", "a"}, now.Add(time.Hour))
	if err != nil || !values["a"].Equal(now) || !values["b"].Equal(now) {
		t.Fatal(values, err)
	}
}

func TestFirstObservedBatchConcurrentNodes(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	ledger, _ := NewFirstObservedLedger(db)
	parent := core.MovieKey(12)
	start := make(chan struct{})
	type outcome struct {
		values map[string]time.Time
		err    error
	}
	results := make(chan outcome, 12)
	for i := 0; i < 12; i++ {
		go func(i int) {
			<-start
			sources := []string{"a", "b", "c"}
			if i%2 == 0 {
				sources = []string{"c", "b", "a"}
			}
			values, err := ledger.ObserveBatch(ctx, parent, sources, time.Unix(int64(1000+i), 0))
			results <- outcome{values, err}
		}(i)
	}
	close(start)
	var first map[string]time.Time
	for i := 0; i < 12; i++ {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first == nil {
			first = got.values
			continue
		}
		for _, source := range []string{"a", "b", "c"} {
			if !first[source].Equal(got.values[source]) {
				t.Fatalf("%s changed: %v vs %v", source, first[source], got.values[source])
			}
		}
	}
}
