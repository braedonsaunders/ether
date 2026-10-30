package plex

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestEncodedCredentialsNeverReachRequestLogs(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPut, "/:/prefs", http.StatusOK},
		{http.MethodGet, "/unknown-audit-endpoint", http.StatusNotFound},
	} {
		t.Run(tc.method, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			var logs bytes.Buffer
			f.srv.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			rec := f.request(t, tc.method, tc.path+"?%58-Plex-Token="+ownerToken+"&X-Ether-Stream-%54oken=audit-stream-secret&keep=visible", map[string]string{"X-Plex-Token": ""})
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if strings.Contains(logs.String(), ownerToken) || strings.Contains(logs.String(), "audit-stream-secret") {
				t.Fatal("credential leaked in request log")
			}
			if !strings.Contains(logs.String(), "keep=visible") {
				t.Fatal("missing non-secret query diagnostic")
			}
		})
	}
}

func TestRedactQueryEncodedNames(t *testing.T) {
	for _, q := range []string{
		"%58-Plex-Token=secret",
		"X%2dPlex%2dToken=secret&X-Plex-Token=secret",
		"x-plex-%74oken=secret&X-Ether-Stream-%54oken=secret",
		"unrelated=%zz&%58-Plex-Token=secret",
	} {
		if got := redactQuery(q); strings.Contains(got, "secret") {
			t.Errorf("redactQuery(%q) = %q", q, got)
		}
	}
}
