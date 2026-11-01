package newznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestSearchItemArrivesBeforeFeedFinishes(t *testing.T) {
	seen := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<rss><channel><item><title>Example.2025.1080p.WEB-DL</title><guid>one</guid></item>`)
		w.(http.Flusher).Flush()
		select {
		case <-seen:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `</channel></rss>`)
	}))
	defer srv.Close()
	c := New(config.Indexer{Name: "test", Enabled: true}, nil, Options{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r, err := c.getSearchStream(ctx, resolvedIdentity{name: IdentityEther}, srv.URL, func(batch []core.Release) {
		if len(batch) != 1 || batch[0].Name != "Example.2025.1080p.WEB-DL" {
			t.Errorf("arrival: %+v", batch)
		}
		close(seen)
	})
	if err != nil || len(r) != 1 {
		t.Fatalf("waited for the complete feed: %v, %d", err, len(r))
	}
}

func TestJSONSearchItemArrivesBeforeFeedFinishes(t *testing.T) {
	seen := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rss":{"channel":{"item":[{"title":"Example.2025.1080p.WEB-DL","guid":"one"},`)
		w.(http.Flusher).Flush()
		select {
		case <-seen:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"title":"Example.2025.720p.WEB-DL","guid":"two"}]}}}`)
	}))
	defer srv.Close()
	c := New(config.Indexer{Name: "test", Enabled: true}, nil, Options{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	count := 0
	rs, err := c.getSearchStream(ctx, resolvedIdentity{name: IdentityEther}, srv.URL, func(batch []core.Release) {
		count += len(batch)
		if count == 1 {
			close(seen)
		}
	})
	if err != nil || len(rs) != 2 || count != 2 {
		t.Fatalf("streamed JSON: %v, results %d, arrivals %d", err, len(rs), count)
	}
}
