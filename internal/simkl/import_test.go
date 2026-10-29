package simkl

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestParseHistory(t *testing.T) {
	raw := []byte(`{"movies":[{"movie":{"ids":{"tmdb":"12"}},"last_watched_at":"2026-01-01T00:00:00Z"},{"movie":{"ids":{}},"last_watched_at":"2026-01-01T00:00:00Z"}],"shows":[{"show":{"ids":{"tmdb":"33"}},"seasons":[{"number":2,"episodes":[{"number":3,"watched_at":"2026-02-02T01:02:03Z"},{"number":4}]}]}],"anime":[{}]}`)
	records, skipped, err := ParseHistory(7, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || skipped != 3 || records[0].Key != core.MovieKey(12) || records[1].Key != core.EpisodeKey(33, 2, 3) || records[1].UserID != 7 {
		t.Fatalf("records=%+v skipped=%d", records, skipped)
	}
}

func TestPullHistoryDoesNotAdvanceOnPartialFailure(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/sync/activities":
			_, _ = w.Write([]byte(`{"all":"2026-03-01T00:00:00Z"}`))
		case "/sync/all-items/movies":
			_, _ = w.Write([]byte(`{"movies":[{"movie":{"ids":{"tmdb":"12"}},"last_watched_at":"2026-01-01T00:00:00Z"}]}`))
		case "/sync/all-items/shows":
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	records, snapshot, _, err := c.PullHistory(context.Background(), 5, "token", "")
	if err == nil || snapshot != "" || len(records) != 0 {
		t.Fatalf("partial pull leaked: %v %q %+v", err, snapshot, records)
	}
	if len(paths) != 3 || paths[0] != "/sync/activities" || paths[1] != "/sync/all-items/movies" {
		t.Fatalf("paths %v", paths)
	}
}
func TestPullHistoryActivityGate(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"all":"2026-03-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	_, snapshot, _, err := c.PullHistory(context.Background(), 5, "token", "2026-03-01T00:00:00Z")
	if err != nil || calls != 1 || snapshot != "2026-03-01T00:00:00Z" {
		t.Fatalf("gate: %v %d %q", err, calls, snapshot)
	}
}

func TestCommitHistoryAtomicAndViewerScoped(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := TokenStore{DB: db, Sealer: testSealer{}}
	tok := Token{ClientID: "id", AccessToken: "a", RefreshToken: "r", Scope: "media:read media:write", ExpiresIn: 300}
	if err := s.Save(ctx, 1, tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	var sealed string
	if err := db.QueryRow(`SELECT access_token FROM simkl_tokens WHERE user_id=1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	st := state.New(db, state.Options{})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := state.ImportRecord{UserID: 1, Key: core.MovieKey(123), Views: 1, ViewedAt: at}
	lease, err := s.AcquireLease(ctx, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := commitLeasedHistory(ctx, st, 1, sealed, "", "snapshot", lease, []state.ImportRecord{rec}); err != nil {
		t.Fatal(err)
	}
	if err := commitLeasedHistory(ctx, st, 1, sealed, "snapshot", "next", lease, []state.ImportRecord{rec}); err != nil {
		t.Fatal(err)
	}
	h, err := st.History(ctx, 1, time.Time{}, 10)
	if err != nil || len(h) != 1 || !h[0].ViewedAt.Equal(at) {
		t.Fatalf("history %+v %v", h, err)
	}
	if err := commitLeasedHistory(ctx, st, 1, sealed, "snapshot", "stale", lease, []state.ImportRecord{rec}); err == nil {
		t.Fatal("stale snapshot committed")
	}
	rec.UserID = 2
	if err := commitLeasedHistory(ctx, st, 1, sealed, "next", "foreign", lease, []state.ImportRecord{rec}); err == nil {
		t.Fatal("foreign viewer committed")
	}
	h, err = st.History(ctx, 2, time.Time{}, 10)
	if err != nil || len(h) != 0 {
		t.Fatalf("foreign history %+v %v", h, err)
	}
}

func TestPullHistorySplitsOversizeResponse(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/sync/activities" {
			_, _ = w.Write([]byte(`{"all":"2026-03-01T00:00:00Z"}`))
			return
		}
		if r.URL.Path == "/sync/all-items/shows" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"max_items"}`))
			return
		}
		_, _ = w.Write([]byte(`{"movies":[],"shows":[]}`))
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	_, snapshot, _, err := c.PullHistory(context.Background(), 1, "token", "")
	if err != nil || snapshot != "2026-03-01T00:00:00Z" || calls != 8 {
		t.Fatalf("split: snapshot %q calls %d err %v", snapshot, calls, err)
	}
}
func TestPullHistoryCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"all":"2026-03-01T00:00:00Z"}`)) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	if _, _, _, err := c.PullHistory(ctx, 1, "token", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestPullHistoryRejectsNullEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sync/activities" {
			_, _ = w.Write([]byte(`{"all":"2026-03-01T00:00:00Z"}`))
			return
		}
		_, _ = w.Write([]byte(`null`))
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	_, snapshot, _, err := c.PullHistory(context.Background(), 1, "token", "")
	if err == nil || snapshot != "" {
		t.Fatalf("null envelope accepted: %q %v", snapshot, err)
	}
}
