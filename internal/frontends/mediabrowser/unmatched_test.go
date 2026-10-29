package mediabrowser

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestUnmatchedLogRedactsCredentialsAndRateLimits(t *testing.T) {
	var out bytes.Buffer
	i := &instance{host: frontend.Host{Log: slog.New(slog.NewTextHandler(&out, nil))}}
	logger := &unmatchedLogger{}
	for n := 0; n < 3; n++ {
		r := httptest.NewRequest("GET", "/unmatched/account-secret?api_key=query-secret", nil)
		r.Header.Set("X-Emby-Token", "header-secret")
		logger.log(i, r, 404)
	}
	s := out.String()
	for _, secret := range []string{"account-secret", "query-secret", "header-secret"} {
		if strings.Contains(s, secret) {
			t.Fatalf("credential leaked: %q", secret)
		}
	}
	if strings.Count(s, "unmatched MediaBrowser route") != 1 {
		t.Fatal(s)
	}
	logger.last = time.Now().Add(-time.Minute)
	req := httptest.NewRequest("SECRET_METHOD", "/other", nil)
	logger.log(i, req, 405)
	if text := out.String(); !strings.Contains(text, "suppressed=2") || strings.Contains(text, "SECRET_METHOD") || !strings.Contains(text, "method=OTHER") {
		t.Fatal(text)
	}
}
