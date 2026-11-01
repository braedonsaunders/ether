package stats

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/usenet"
)

func TestHistoryTimelineAggregation(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	svc := New(db, Options{Now: func() time.Time { return now }})
	p := &core.PlaySession{ID: "one", UserID: 7, Key: 99, Username: "Sam", TitleName: "<Movie>", StartedAt: now, BytesServed: 100, Client: core.ClientCaps{Platform: "iOS", DeviceName: "Phone"}}
	if err := svc.Record(ctx, p, false, ""); err != nil {
		t.Fatal(err)
	}
	svc.ObserveState(state.Event{Kind: state.EventProgress, UserID: 7, Key: 99, At: now, State: "playing", Duration: time.Hour})
	now = now.Add(30 * time.Second)
	svc.ObserveState(state.Event{Kind: state.EventProgress, UserID: 7, Key: 99, At: now, State: "playing"})
	svc.ObserveState(state.Event{Kind: state.EventCompleted, UserID: 7, Key: 99, At: now, Completed: true})
	p.BytesServed = 200
	now = now.Add(time.Second)
	if err := svc.Record(ctx, p, true, ""); err != nil {
		t.Fatal(err)
	}
	sum, err := svc.Summary(ctx, now.Add(-time.Hour), now.Add(time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Plays != 1 || sum.Bytes != 200 || sum.WatchedMS != 30000 || sum.Completed != 1 || len(sum.Platforms) != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	h, err := svc.History(ctx, now.Add(-time.Hour), now.Add(time.Hour), 7, 10)
	if err != nil || len(h) != 1 || h[0].Ended == 0 || h[0].Device != "Phone" {
		t.Fatalf("history: %+v, %v", h, err)
	}
	r := httptest.NewRequest("GET", "/newsletter", nil)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "&lt;Movie&gt;") {
		t.Fatalf("newsletter escaping: %d %s", w.Code, w.Body.String())
	}
}
func TestAmbiguousTimelineNotAttributed(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	s := New(db, Options{Now: func() time.Time { return now }})
	for _, id := range []string{"one", "two"} {
		if err := s.Record(ctx, &core.PlaySession{ID: id, UserID: 1, Key: 1, StartedAt: now}, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	s.ObserveState(state.Event{Kind: state.EventCompleted, UserID: 1, Key: 1, Completed: true, At: now})
	v, err := s.Summary(ctx, now.Add(-time.Minute), now.Add(time.Minute), 1)
	if err != nil || v.Completed != 0 {
		t.Fatalf("ambiguous completion attributed: %+v %v", v, err)
	}
}
func TestProviderCounterDeltasAndIndexerOperations(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	p := usenet.ProviderStats{ID: "primary", Bytes: 1000, Articles: 5, Misses: 2}
	for _, bytes := range []int64{1000, 1500, 500} {
		p.Bytes = bytes
		if err := s.SnapshotProviders(ctx, []usenet.ProviderStats{p}); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	if err := db.QueryRow(`SELECT SUM(bytes) FROM stats_provider_days`).Scan(&n); err != nil || n != 2000 {
		t.Fatalf("reset delta got %d %v", n, err)
	}
	for _, spills := range []int64{3, 7} {
		p.Spillovers = spills
		if err := s.SnapshotProviders(ctx, []usenet.ProviderStats{p}); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/infrastructure", nil))
	if !strings.Contains(w.Body.String(), `"spillovers":7`) {
		t.Fatalf("spillovers not in provider usage: %s", w.Body.String())
	}
	if err := s.RecordIndexer(ctx, newznab.Event{Indexer: "ix", Kind: "search", Success: true, Duration: 100 * time.Millisecond, AverageAge: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	usage, err := s.IndexerMonth(ctx)
	if err != nil || len(usage) != 1 || usage[0].Successes != 1 || usage[0].AgeSeconds != 86400 {
		t.Fatalf("indexer usage: %+v %v", usage, err)
	}
}
func TestInfrastructureIncludesDailySeriesAndPlayFailures(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := New(db, Options{Now: func() time.Time { return now }})
	if _, err := db.Exec(`INSERT INTO stats_provider_days(provider,day,bytes,articles,misses,errors) VALUES('primary','2026-10-18',100,4,1,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stats_indexer_days(indexer,day,searches,successes,grabs,latency_ms) VALUES('ix','2026-10-18',3,2,1,90)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, &core.PlaySession{ID: "play", UserID: 1, StartedAt: now, Failovers: 2}, true, "source failed"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/infrastructure", nil))
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, needle := range []string{`"providerDays"`, `"2026-10-18"`, `"indexerDays"`, `"failovers":2`, `"failedPlays":1`, `"monthPlays":1`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("missing %s in %s", needle, body)
		}
	}
}

func TestEmptyStatsAndCostValidation(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/summary", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"daily":[]`) {
		t.Fatalf("empty response: %s", w.Body.String())
	}
	if err := s.SetCost(ctx, Cost{Kind: "provider", ID: "p", Monthly: -1, Currency: "USD"}); err == nil {
		t.Fatal("accepted negative price")
	}
}

func TestWeeklyNewsletterOptInAndDurableLedger(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	calls := 0
	s := New(db, Options{Now: func() time.Time { return now }, Newsletter: func(ctx context.Context, html string) error {
		calls++
		if !strings.Contains(html, "Your Ether week") {
			t.Error("not HTML report")
		}
		return nil
	}})
	if err := s.MaybeNewsletter(ctx); err != nil || calls != 0 {
		t.Fatal("newsletter not opt-in", err)
	}
	if err := s.SaveNewsletter(ctx, NewsletterSettings{Enabled: true, Weekday: 1, Hour: 9}); err != nil {
		t.Fatal(err)
	}
	settings, err := s.NewsletterSettings(ctx)
	if err != nil || settings.NextDue != now.Add(time.Hour).Unix() {
		t.Fatalf("schedule: %+v %v", settings, err)
	}
	now = now.Add(2 * time.Hour)
	if err := s.MaybeNewsletter(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.MaybeNewsletter(ctx); err != nil {
		t.Fatal(err)
	}
	settings, err = s.NewsletterSettings(ctx)
	if calls != 1 || err != nil || settings.LastStatus != "queued" || settings.NextDue <= now.Unix() {
		t.Fatalf("calls %d settings %+v err %v", calls, settings, err)
	}
	s2 := New(db, Options{Now: func() time.Time { return now }, Newsletter: func(context.Context, string) error { calls++; return nil }})
	if err := s2.MaybeNewsletter(ctx); err != nil || calls != 1 {
		t.Fatalf("repeated after restart: %d %v", calls, err)
	}
}

func TestHistoricalImportDoesNotChangeActivePlaybackStats(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	svc := New(db, Options{Now: func() time.Time { return now }})
	key := core.MovieKey(99)
	if err := svc.Record(ctx, &core.PlaySession{ID: "active", UserID: 7, Key: key, StartedAt: now}, false, ""); err != nil {
		t.Fatal(err)
	}
	svc.ObserveState(state.Event{Kind: state.EventProgress, UserID: 7, Key: key, At: now, State: "playing", Duration: time.Hour})
	st := state.New(db, state.Options{Now: func() time.Time { return now }})
	st.Subscribe(svc.ObserveState)
	now = now.Add(15 * time.Second)
	if err := st.ImportTransaction(ctx, func(*sql.Tx) (state.ImportBatch, error) {
		return state.ImportBatch{Records: []state.ImportRecord{{UserID: 7, Key: key, Views: 1, ViewedAt: now.Add(-24 * time.Hour), Duration: 2 * time.Hour}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	svc.ObserveState(state.Event{Kind: state.EventProgress, UserID: 7, Key: key, At: now, State: "playing", Duration: time.Hour})
	plays, err := svc.History(ctx, now.Add(-time.Hour), now.Add(time.Hour), 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 || plays[0].Completed || plays[0].WatchedMS != 30000 || plays[0].DurationMS != 3600000 {
		t.Fatalf("historical import changed active play: %+v", plays)
	}
}
