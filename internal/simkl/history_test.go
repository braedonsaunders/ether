package simkl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestHistoryPayload(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		key  core.RatingKey
		want []string
	}{
		{core.MovieKey(123), []string{`"movies"`, `"tmdb":"123"`, `"watched_at":"2026-01-02T03:04:05Z"`}},
		{core.EpisodeKey(456, 2, 3), []string{`"shows"`, `"tmdb":"456"`, `"number":2`, `"number":3`, `"watched_at":"2026-01-02T03:04:05Z"`}},
	} {
		payload, err := HistoryPayload(tc.key, at)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(payload)
		for _, want := range tc.want {
			if !strings.Contains(string(b), want) {
				t.Errorf("missing %s in %s", want, b)
			}
		}
	}
	if _, err := HistoryPayload(core.ShowKey(456), at); err == nil {
		t.Fatal("accepted show")
	}
	if _, err := HistoryPayload(core.MovieKey(123), time.Time{}); err == nil {
		t.Fatal("accepted empty time")
	}
}

func TestPushHistoryRejectsUnverifiedEpisodeAndNotFound(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"not_found":{"movies":[{}],"shows":[],"episodes":[]}}`))
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	at := time.Now()
	if err := c.PushHistory(context.Background(), "token", core.EpisodeKey(12, 1, 2), at, false); err == nil || calls != 0 {
		t.Fatal("unverified episode sent")
	}
	if err := c.PushHistory(context.Background(), "token", core.MovieKey(12), at, true); err == nil || calls != 1 {
		t.Fatal("not_found accepted")
	}
}

func TestRedirectClassificationNeverFollows(t *testing.T) {
	kind := "tv"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tmdb") != "12" || r.URL.Query().Get("type") != "tv" {
			t.Error("bad lookup")
		}
		w.Header().Set("Location", "https://simkl.com/"+kind+"/42/name")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	ok, err := c.IsOrdinaryTV(context.Background(), 12)
	if err != nil || !ok {
		t.Fatalf("tv: %v %v", ok, err)
	}
	kind = "anime"
	ok, err = c.IsOrdinaryTV(context.Background(), 12)
	if err != nil || ok {
		t.Fatalf("anime: %v %v", ok, err)
	}
}
