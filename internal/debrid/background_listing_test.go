package debrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Accepted ETHE-10 D: a cold speculative check must not wait for the
// account listing; A can start foreground work while one list warms alongside.
// These fixtures exercise the real providers' HTTP and Service.Acquire paths.
func TestBackgroundListingDoesNotBlockForeground(t *testing.T) {
	for _, kind := range []string{config.DebridTorBox, config.DebridDebridLink} {
		for _, mode := range []string{"new", "duplicate-inflight", "duplicate-fresh"} {
			duplicate := mode != "new"
			t.Run(fmt.Sprintf("%s/%s", kind, mode), func(t *testing.T) {
				var base http.Handler
				listPath, idParam, addPath, expectedID := "/torrents/mylist", "id", "/torrents/createtorrent", "77"
				if kind == config.DebridTorBox {
					base = (&fakeTorBox{cached: true}).handler(t)
				} else {
					base = (&fakeDL{cached: true, torrents: map[string]bool{"T9": true}}).handler(t)
					listPath, idParam, addPath, expectedID = "/seedbox/list", "ids", "/seedbox/add", "T9"
				}
				started, release, added := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var startOnce, releaseOnce, addOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				var lists, writes atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// No credentials, headers or token-bearing queries enter the transcript.
					t.Logf("HTTP %s %s account-list=%t", r.Method, r.URL.Path, r.URL.Path == listPath && r.URL.Query().Get(idParam) == "")
					if r.URL.Path == listPath && r.URL.Query().Get(idParam) == "" {
						lists.Add(1)
						startOnce.Do(func() { close(started) })
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					if r.Method != http.MethodGet {
						writes.Add(1)
					}
					if r.URL.Path == addPath {
						addOnce.Do(func() { close(added) })
						if duplicate {
							if kind == config.DebridTorBox {
								fmt.Fprint(w, `{"success":false,"error":"DUPLICATE_ITEM","data":null}`)
							} else {
								fmt.Fprint(w, `{"success":false,"error":"alreadyExists","error_description":"torrent already exists"}`)
							}
							return
						}
					}
					base.ServeHTTP(w, r)
				}))
				defer srv.Close()
				defer unblock()
				p, err := NewProvider(config.DebridProvider{Kind: kind, APIKey: testKey}, fastOpts(srv.URL))
				if err != nil {
					t.Fatal(err)
				}
				cfg := config.Debrid{CachedOnly: true, RemoveUncached: true, Providers: []config.DebridProvider{{ID: "provider", Kind: kind, APIKey: testKey, Enabled: true}}}
				svc := NewService(cfg, Options{NewProvider: func(config.DebridProvider, ProviderOptions) (Provider, error) { return p, nil }})
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				bg, cancelBG := context.WithCancel(core.WithBackgroundBudget(ctx, func(context.Context, string, string, int64) error { return nil }))
				defer cancelBG()
				rel := dlRelease()
				// Four concurrent checks must all finish while the server still holds
				// the one account-wide GET. No duration assertion depends on latency.
				results := make(chan error, 4)
				for i := 0; i < 4; i++ {
					go func() { _, err := svc.Acquire(bg, rel); results <- err }()
				}
				for i := 0; i < 4; i++ {
					select {
					case err := <-results:
						if !errors.Is(err, core.ErrForegroundOnly) {
							t.Fatalf("cold check must be undecided: %v", err)
						}
					case <-ctx.Done():
						t.Fatal("background checks waited for the blocked list")
					}
				}
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("cold checks did not start a listing")
				}
				if writes.Load() != 0 || lists.Load() != 1 {
					t.Fatalf("cold checks lists=%d writes=%d", lists.Load(), writes.Load())
				}
				t.Log("four background checks returned undecided with listing blocked; zero account writes")
				cancelBG() // the shared read must survive its speculative caller
				if mode == "duplicate-fresh" {
					unblock()
					if _, err := p.Find(ctx, testHash); err != nil {
						t.Fatal(err)
					}
				}
				// Service.Acquire returns a core payload, not a provider Acquired.
				done := make(chan error, 1)
				go func() {
					payload, err := svc.Acquire(ctx, rel)
					if err == nil {
						var got Payload
						got, err = DecodePayload(payload)
						if err == nil && got.TorrentID != expectedID {
							err = fmt.Errorf("wrong owned torrent: %s", got.TorrentID)
						}
					}
					done <- err
				}()
				select {
				case <-added:
				case <-ctx.Done():
					t.Fatal("foreground did not reach atomic creation while list was blocked")
				}
				if duplicate {
					unblock()
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("foreground did not finish")
				}
				unblock()
				// Join the shared listing before checking counts / closing the server.
				if _, err := p.Find(ctx, testHash); err != nil {
					t.Fatal(err)
				}
				if lists.Load() != 1 || writes.Load() != 1 {
					t.Fatalf("cold play lists=%d writes=%d", lists.Load(), writes.Load())
				}
				t.Logf("foreground played owned torrent %s; account listings=1; foreground adds=1", expectedID)
			})
		}
	}
}

// Accepted D retains PR #44's read-only warm-account discovery on both
// providers. An expired snapshot must not be used as current availability.
func TestBackgroundListingWarmDiscoveryAndExpiry(t *testing.T) {
	for _, kind := range []string{config.DebridTorBox, config.DebridDebridLink} {
		for _, expired := range []bool{false, true} {
			// TorBox's warm-ready/not-ready discovery belongs to the PR #44
			// test in torbox_reuse_test.go; do not duplicate that fact here.
			if kind == config.DebridTorBox && !expired {
				continue
			}
			t.Run(fmt.Sprintf("%s/expired=%t", kind, expired), func(t *testing.T) {
				var base http.Handler
				path, param := "/torrents/mylist", "id"
				if kind == config.DebridTorBox {
					base = (&fakeTorBox{cached: true}).handler(t)
				} else {
					base = (&fakeDL{cached: true, torrents: map[string]bool{"T9": true}}).handler(t)
					path, param = "/seedbox/list", "ids"
				}
				var lists, writes atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == path && r.URL.Query().Get(param) == "" {
						lists.Add(1)
					}
					if r.Method != http.MethodGet {
						writes.Add(1)
					}
					base.ServeHTTP(w, r)
				}))
				defer srv.Close()
				var clock atomic.Int64
				clock.Store(time.Now().UnixNano())
				opts := fastOpts(srv.URL)
				opts.Now = func() time.Time { return time.Unix(0, clock.Load()) }
				p, err := NewProvider(config.DebridProvider{Kind: kind, APIKey: testKey}, opts)
				if err != nil {
					t.Fatal(err)
				}
				if id, err := p.Find(t.Context(), testHash); err != nil || id == "" {
					t.Fatalf("prime list: %q %v", id, err)
				}
				if expired {
					clock.Add(int64(listTTL + time.Second))
				}
				cfg := config.Debrid{CachedOnly: true, Providers: []config.DebridProvider{{ID: "p", Kind: kind, Enabled: true}}}
				svc := NewService(cfg, Options{NewProvider: func(config.DebridProvider, ProviderOptions) (Provider, error) { return p, nil }})
				bg := core.WithBackgroundBudget(t.Context(), func(context.Context, string, string, int64) error { return nil })
				payload, err := svc.Acquire(bg, dlRelease())
				wantLists := int64(1)
				if expired {
					if !errors.Is(err, core.ErrForegroundOnly) {
						t.Fatalf("expired positive snapshot must be undecided: %v", err)
					}
					if _, err := p.Find(t.Context(), testHash); err != nil {
						t.Fatal(err)
					}
					wantLists = 2
					t.Log("expired positive snapshot returned undecided and refreshed read-only")
				} else if err != nil || !IsPayload(payload) {
					t.Fatalf("warm discovery: %v", err)
				} else {
					t.Log("fresh account snapshot discovered and played existing episode without an account write")
				}
				if lists.Load() != wantLists || writes.Load() != 0 {
					t.Fatalf("discovery lists=%d want=%d writes=%d", lists.Load(), wantLists, writes.Load())
				}
			})
		}
	}
}
