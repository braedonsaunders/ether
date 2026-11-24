package plextv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInviteLink(t *testing.T) {
	for body, want := range map[string]string{
		`{"id":1,"invite":{"url":"https://app.plex.tv/invite/abc"}}`: "https://app.plex.tv/invite/abc",
		`{"inviteUrl":"https://plex.tv/i/xyz"}`:                      "https://plex.tv/i/xyz",
		`{"id":1,"thumb":"https://plex.tv/users/a/avatar"}`:          "",
		`{"inviteUrl":"https://evil.example/invite"}`:                "",
		`not json`: "",
	} {
		if got := inviteLink([]byte(body)); got != want {
			t.Errorf("inviteLink(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestUnshareServerDeletesTheMatchingShare(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/shared_servers"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><SharedServer id="11" userID="100" username="alice" email="a@example.com"/><SharedServer id="12" userID="101" username="bob"/></MediaContainer>`))
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, Identity: ServerIdentity(testConfig(), ""), RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.UnshareServer(ctx, "owner", 101, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.UnshareServer(ctx, "owner", 0, "A@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.UnshareServer(ctx, "owner", 999, ""); !errors.Is(err, ErrShareNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	if len(deleted) != 2 || !strings.HasSuffix(deleted[0], "/shared_servers/12") || !strings.HasSuffix(deleted[1], "/shared_servers/11") {
		t.Fatalf("deleted = %v", deleted)
	}
}
