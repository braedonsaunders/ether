package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRulesRedactionRateLimitAndDigest(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var message string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		var e Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Error(err)
		}
		message = e.Message
		w.WriteHeader(204)
	}))
	defer up.Close()
	s := New(db, Options{Now: func() time.Time { return now }})
	c := Channel{ID: "test", Kind: "webhook", Enabled: true, Endpoint: up.URL + "/secret-hook", Token: "secret", Events: []string{"playback.error"}, PerMinute: 1, DigestMinutes: 5}
	if err := s.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Channels(ctx)
	if err != nil || cs[0].Endpoint != Redacted || cs[0].Token != Redacted {
		t.Fatalf("unredacted channels %+v %v", cs, err)
	}
	if err := s.Save(ctx, cs[0]); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{{Kind: "playback.start"}, {Kind: "playback.error", Message: "one"}, {Kind: "playback.error", Message: "two"}} {
		if err := s.Emit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("digest sent early")
	}
	now = now.Add(5 * time.Minute)
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(message, "one") || !strings.Contains(message, "two") {
		t.Fatalf("digest got %d %q", calls.Load(), message)
	}
	if err := s.Emit(ctx, Event{Kind: "playback.error"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("second digest missing")
	}
}
func TestFailedDeliveryDoesNotLeakSecrets(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret in response", 500) }))
	defer up.Close()
	s := New(db, Options{})
	if err := s.Save(ctx, Channel{ID: "one", Kind: "discord", Endpoint: up.URL + "/secret", Enabled: true, Events: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, Event{Kind: "playback.error"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "/secret") || strings.Contains(w.Body.String(), "secret in response") || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatalf("log: %s", w.Body.String())
	}
}
func TestTransportFormatsAndNoRedirect(t *testing.T) {
	for _, kind := range []string{"discord", "slack", "ntfy", "gotify", "webhook"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "ntfy" {
					if r.Header.Get("Title") != "Test" {
						t.Error("ntfy title missing")
					}
				} else {
					var v map[string]any
					if json.NewDecoder(r.Body).Decode(&v) != nil {
						t.Error("invalid payload")
					}
					key := "message"
					if kind == "discord" {
						key = "content"
					}
					if kind == "slack" {
						key = "text"
					}
					if v[key] == nil {
						t.Errorf("missing %s", key)
					}
				}
				http.Redirect(w, r, "/redirected", 302)
			}))
			defer up.Close()
			s := New(db, Options{})
			if err := s.deliver(ctx, Channel{Kind: kind, Endpoint: up.URL}, Event{Title: "Test", Message: "hello"}); err == nil {
				t.Fatal("followed redirect or accepted 302")
			}
		})
	}
}
func TestValidateSMTPAndTelegram(t *testing.T) {
	if validateChannel(Channel{Kind: "smtp", Endpoint: "smtp.example:587", From: "a@example\r\nBcc: x", To: "b@example"}) == nil {
		t.Fatal("header injection accepted")
	}
	if validateChannel(Channel{Kind: "telegram", Token: "abc/other", Target: "123"}) == nil {
		t.Fatal("invalid token accepted")
	}
}

func TestNewsletterRequiresSelectedSMTPChannel(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	e := Event{Kind: "newsletter.weekly", Title: "Weekly", HTML: "<h1>Report</h1>"}
	if err := s.Emit(ctx, e); err == nil {
		t.Fatal("reported queue success with no SMTP channel")
	}
	if err := s.Save(ctx, Channel{ID: "mail", Kind: "smtp", Endpoint: "smtp.example:587", From: "owner@example", To: "viewer@example", Enabled: true, Events: []string{"newsletter.weekly"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, e); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT event FROM notify_queue`).Scan(&raw); err != nil || !strings.Contains(raw, "Report") {
		t.Fatalf("HTML not queued %q %v", raw, err)
	}
}
func TestImmediateRateLimit(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer up.Close()
	s := New(db, Options{Now: func() time.Time { return now }})
	if err := s.Save(ctx, Channel{ID: "one", Kind: "webhook", Endpoint: up.URL, Enabled: true, Events: []string{"*"}, PerMinute: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Emit(ctx, Event{Kind: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("rate limit sent %d", calls)
	}
	now = now.Add(61 * time.Second)
	if err := s.Drain(ctx); err != nil || calls != 2 {
		t.Fatalf("did not release queued item %d %v", calls, err)
	}
}

// Events arriving at different times within one digest window go out as a
// single digest once the window's first event is due.
func TestDigestBatchesEventsAcrossTheWindow(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var message string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var e Event
		_ = json.NewDecoder(r.Body).Decode(&e)
		message = e.Message
		w.WriteHeader(204)
	}))
	defer up.Close()
	s := New(db, Options{Now: func() time.Time { return now }})
	if err := s.Save(ctx, Channel{ID: "d", Kind: "webhook", Enabled: true, Endpoint: up.URL, Events: []string{"*"}, PerMinute: 5, DigestMinutes: 5}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"one", "two", "three"} {
		if err := s.Emit(ctx, Event{Kind: "playback.error", Message: m}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	now = now.Add(2 * time.Minute) // five minutes after "one"
	for i := 0; i < 3; i++ {
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if calls.Load() != 1 || !strings.Contains(message, "one") || !strings.Contains(message, "two") || !strings.Contains(message, "three") {
		t.Fatalf("digest sent %d messages, last %q", calls.Load(), message)
	}
}

// An indexer outage alerts once, not once per failed request; a success
// re-arms the alert.
func TestIndexerOutageAlertsOncePerOutage(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	if err := s.Save(ctx, Channel{ID: "x", Kind: "webhook", Enabled: true, Endpoint: "https://hooks.example/x", Events: []string{"indexer.outage", "indexer.budget"}}); err != nil {
		t.Fatal(err)
	}
	queued := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM notify_queue`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	fail := newznab.Event{Indexer: "ix", Kind: "search"}
	for i := 0; i < 5; i++ {
		s.ObserveIndexer(fail)
	}
	if n := queued(); n != 1 {
		t.Fatalf("outage queued %d notifications", n)
	}
	s.ObserveIndexer(newznab.Event{Indexer: "ix", Kind: "search", Success: true})
	s.ObserveIndexer(fail)
	if n := queued(); n != 2 {
		t.Fatalf("a new outage after recovery should alert again: %d", n)
	}
	s.ObserveIndexer(newznab.Event{Indexer: "ix", Kind: "search", BudgetExhausted: true})
	if n := queued(); n != 3 {
		t.Fatalf("budget exhaustion should alert: %d", n)
	}
}

func TestDrainSkipsWritesWhenQueueIdle(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	statsCtx, qs := store.WithQueryStats(ctx)
	if err := s.Drain(statsCtx); err != nil {
		t.Fatal(err)
	}
	if qs.Writes() != 0 {
		t.Fatalf("idle Drain issued %d writes, want 0", qs.Writes())
	}
}
