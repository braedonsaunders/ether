package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollisionProbeRequiresViewerIsolation(t *testing.T) {
	for _, leak := range []bool{false, true} {
		t.Run(fmt.Sprintf("leak-%v", leak), func(t *testing.T) {
			active := map[string]string{}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Plex-Client-Identifier") != "parity-colliding-client" {
					t.Error("client identifiers did not collide")
				}
				viewer := r.Header.Get("X-Plex-Token")
				id := r.Header.Get("X-Plex-Session-Identifier")
				switch {
				case strings.HasPrefix(r.URL.Path, "/library/parts/"):
					w.WriteHeader(http.StatusPartialContent)
					fmt.Fprint(w, strings.Repeat("x", 4096))
				case r.URL.Path == "/:/timeline":
					if r.URL.Query().Get("state") == "playing" {
						active[id] = viewer
					} else {
						delete(active, id)
					}
					fmt.Fprint(w, `{}`)
				case r.URL.Path == "/status/sessions":
					var items []map[string]any
					for key, owner := range active {
						if viewer == "parity-owner" || owner == viewer || leak {
							items = append(items, map[string]any{"Session": map[string]any{"id": key}})
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": items}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer s.Close()
			h := http.Header{"X-Plex-Token": {"{{token}}"}}
			vars := map[string]map[string]string{"owner": {"movieKey": "123"}, "adult": {"movieKey": "123"}}
			captures, err := collidingClientSessions(context.Background(), s.Client(), s.URL, h, vars)
			if leak {
				if err == nil {
					t.Fatal("cross-viewer session leak was accepted")
				}
				return
			}
			if err != nil || len(captures) != 10 {
				t.Fatalf("captures=%d err=%v", len(captures), err)
			}
		})
	}
}
