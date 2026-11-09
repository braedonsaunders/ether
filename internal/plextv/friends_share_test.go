package plextv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerSectionIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/servers/test-machine-id" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Plex-Token") != "owner-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer>
  <Server machineIdentifier="test-machine-id">
    <Section id="101" key="1" type="movie" title="Movies"/>
    <Section id="102" key="2" type="show" title="TV Shows"/>
  </Server>
</MediaContainer>`))
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:      srv.URL,
		Identity:     Identity{ClientIdentifier: "test-machine-id"},
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ids, err := c.ServerSectionIDs(context.Background(), "owner-token")
	if err != nil {
		t.Fatalf("ServerSectionIDs error: %v", err)
	}
	want := []int64{101, 102}
	if !slices.Equal(ids, want) {
		t.Fatalf("ServerSectionIDs = %v, want %v", ids, want)
	}
}

func TestShareServerNewUser(t *testing.T) {
	var posted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/servers/test-mid":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Server><Section id="101"/></Server></MediaContainer>`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/servers/test-mid/shared_servers":
			posted.Store(true)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><SharedServer id="999"/></MediaContainer>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:      srv.URL,
		Identity:     Identity{ClientIdentifier: "test-mid"},
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = c.ShareServer(context.Background(), "owner-token", "newviewer", 0, nil)
	if err != nil {
		t.Fatalf("ShareServer failed: %v", err)
	}
	if !posted.Load() {
		t.Fatal("expected POST /api/servers/test-mid/shared_servers to be called")
	}
}

func TestShareServerExistingUserFallbackToPut(t *testing.T) {
	var putCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/servers/test-mid/shared_servers":
			// Simulate Plex returning 400 when already sharing
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<Response code="400" status="You're already sharing this server with olduser"/>`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/servers/test-mid/shared_servers":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer>
  <SharedServer id="777" username="olduser" email="old@example.com" userID="42"/>
</MediaContainer>`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/servers/test-mid/shared_servers/777":
			putCalled.Store(true)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><SharedServer id="777"/></MediaContainer>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:      srv.URL,
		Identity:     Identity{ClientIdentifier: "test-mid"},
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = c.ShareServer(context.Background(), "owner-token", "olduser", 0, []int64{101, 102})
	if err != nil {
		t.Fatalf("ShareServer fallback failed: %v", err)
	}
	if !putCalled.Load() {
		t.Fatal("expected PUT /api/servers/test-mid/shared_servers/777 to be called")
	}
}

func TestShareServerV2AllLibraries(t *testing.T) {
	var v2Called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers" {
			v2Called.Store(true)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if _, hasEmail := body["invitedEmail"]; hasEmail {
				if _, hasID := body["invitedId"]; hasID {
					t.Fatalf("invitedEmail and invitedId are mutually exclusive in v2 sharing")
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":44222332,"allLibraries":true,"accepted":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:      srv.URL,
		Identity:     Identity{ClientIdentifier: "test-mid"},
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = c.ShareServer(context.Background(), "owner-token", "alessiodepoli", 436647798, nil)
	if err != nil {
		t.Fatalf("ShareServer V2 failed: %v", err)
	}
	if !v2Called.Load() {
		t.Fatal("expected POST /api/v2/shared_servers to be called")
	}
}

func TestShareServerV2ExistingUserUpdate(t *testing.T) {
	var updateCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"errors":[{"code":1123,"message":"You're already sharing this server"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/owned/accepted":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":55555,"machineIdentifier":"test-mid","allLibraries":false,"librarySectionIds":[101],"settings":{"filterMovies":"contentRating=PG"},"invited":{"id":436647798,"username":"alessiodepoli"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers/55555":
			updateCalled.Store(true)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode update payload: %v", err)
			}
			// Verify that existing grants and content restrictions are preserved
			if all, ok := payload["allLibraries"].(bool); !ok || all {
				t.Fatalf("expected allLibraries to remain false, got %v", payload["allLibraries"])
			}
			settings, ok := payload["settings"].(map[string]any)
			if !ok || settings["filterMovies"] != "contentRating=PG" {
				t.Fatalf("expected filterMovies to remain contentRating=PG, got %v", settings["filterMovies"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":55555,"allLibraries":false,"accepted":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:      srv.URL,
		Identity:     Identity{ClientIdentifier: "test-mid"},
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = c.ShareServer(context.Background(), "owner-token", "alessiodepoli", 436647798, nil)
	if err != nil {
		t.Fatalf("ShareServer V2 update failed: %v", err)
	}
	if !updateCalled.Load() {
		t.Fatal("expected POST /api/v2/shared_servers/55555 to be called")
	}
}
