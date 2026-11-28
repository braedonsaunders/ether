package debrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestRealDebridDeadRememberedTorrentClearsPendingAnswer(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/torrents/info/dead-id" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "dead-id",
				"hash":   hash,
				"status": "dead",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	opts := ProviderOptions{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Now:        func() time.Time { return now },
		paces:      &accountPaces{},
	}
	rd := newRealDebrid("rd-key", opts)
	key := answerKey{hash: hash}
	rd.pace.writeAnswer(context.Background(), key, learnedAnswer{verdict: answerPending, torrentID: "dead-id"}, now)

	_, err := rd.Acquire(context.Background(), AcquireRequest{
		InfoHash:   hash,
		Magnet:     "magnet:?xt=urn:btih:" + hash,
		CachedOnly: true,
		Choose:     func(files []File) (File, error) { return files[0], nil },
	})
	if !errors.Is(err, ErrGone) || !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("dead remembered torrent err = %v, want ErrGone + ErrNotCached", err)
	}
	if ans := rd.pace.answer(context.Background(), key, now); ans.torrentID != "" || ans.verdict != "" {
		t.Fatalf("stale pending answer was not cleared: %+v", ans)
	}
}

func TestKnownMapSweepsExpiredEntries(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	svc := NewService(config.Debrid{}, Options{Now: func() time.Time { return now }})
	svc.knownMu.Lock()
	for i := 0; i < knownSweepThreshold; i++ {
		svc.known[fmt.Sprintf("p|%04d", i)] = knownState{cached: true, until: now.Add(-time.Minute)}
	}
	svc.known["p|perm"] = knownState{refused: true} // zero until: permanent
	svc.knownMu.Unlock()

	svc.recordKnown(context.Background(), "p", "freshhash", "", true)
	svc.knownMu.Lock()
	gotLen := len(svc.known)
	_, hasPerm := svc.known["p|perm"]
	svc.knownMu.Unlock()
	if gotLen != 2 || !hasPerm {
		t.Fatalf("after sweep: len=%d hasPerm=%v, want len=2 hasPerm=true", gotLen, hasPerm)
	}
}
