package plextv

import (
	"context"
	"encoding/json"
	"errors"
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

func TestShareServerResolvesNameWithSpacesOrRejects(t *testing.T) {
	var postV2Calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/server/access_tokens":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><token id="999" username="ChrisnordDenaud" title="Chrisnord Denaud" token="t"/></MediaContainer>`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
			postV2Calls.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["invitedId"] != float64(999) {
				t.Fatalf("expected invitedId=999, got %v", payload["invitedId"])
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
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

	if err := c.ShareServer(context.Background(), "owner-token", "Chrisnord Denaud", 0, nil); err != nil {
		t.Fatalf("ShareServer with matched spaced name failed: %v", err)
	}
	if err := c.ShareServer(context.Background(), "owner-token", "Unknown Person", 0, nil); err == nil {
		t.Fatal("expected error for unmatched spaced name")
	}
	if postV2Calls.Load() != 1 {
		t.Fatalf("expected 1 POST /api/v2/shared_servers call, got %d", postV2Calls.Load())
	}
}

func TestCommunityClientRetries429AndRecovers(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"allFriendsV2":[{"user":{"id":"1","username":"friend1"}}]}}`))
	}))
	defer srv.Close()

	cc := &CommunityClient{URL: srv.URL, MinInterval: time.Millisecond}
	friends, err := cc.Friends(context.Background(), "tok")
	if err != nil {
		t.Fatalf("expected recovery after 429 retries, got %v", err)
	}
	if len(friends) != 1 || friends[0].Username != "friend1" || calls.Load() != 3 {
		t.Fatalf("unexpected result: friends=%+v calls=%d", friends, calls.Load())
	}
}

func TestShareServerResolvesNonSpacedNameViaAccountUsers(t *testing.T) {
	var postV2Calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/users":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer>
  <User id="703916194" uuid="uuid-sakib" username="sakib99" title="Sakib" email="sakib@example.com" thumb="https://plex.tv/users/sakib/avatar"/>
</MediaContainer>`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/server/users":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer/>`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/server/access_tokens":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer/>`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
			postV2Calls.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["invitedId"] != float64(703916194) {
				t.Fatalf("expected invitedId=703916194, got %v", payload["invitedId"])
			}
			if _, hasEmail := payload["invitedEmail"]; hasEmail {
				t.Fatalf("expected invitedEmail omitted when invitedId is resolved, got %v", payload["invitedEmail"])
			}
			if payload["skipFriendship"] != true {
				t.Fatalf("expected skipFriendship=true, got %v", payload["skipFriendship"])
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
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

	users, err := c.AccountUsers(context.Background(), "owner-token")
	if err != nil {
		t.Fatalf("AccountUsers failed: %v", err)
	}
	if len(users) != 1 || users[0].PlexID != 703916194 || users[0].Email != "sakib@example.com" {
		t.Fatalf("unexpected AccountUsers result: %+v", users)
	}

	if err := c.ShareServer(context.Background(), "owner-token", "Sakib", 0, nil); err != nil {
		t.Fatalf("ShareServer failed for non-spaced title resolved via AccountUsers: %v", err)
	}
	if postV2Calls.Load() != 1 {
		t.Fatalf("expected 1 POST /api/v2/shared_servers call, got %d", postV2Calls.Load())
	}
}

func TestSharedServerUsersAndPendingResolve(t *testing.T) {
	var sharedIDs []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/users",
			r.Method == http.MethodGet && r.URL.Path == "/api/v2/server/users",
			r.Method == http.MethodGet && r.URL.Path == "/api/home/users",
			r.Method == http.MethodGet && r.URL.Path == "/api/v2/server/access_tokens":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer/>`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/owned/accepted":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":1001,"machineIdentifier":"other-pms","invited":{"id":880011,"username":"ppoliard","title":"ppoliard","friendlyName":"Patricson Poliard","email":"pat@example.com"}}
			]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/invites/owned/pending":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"invited":{"id":990022,"username":"dsabelag","title":"dsabelag","email":"dsabelag@example.com"},"sharedServers":[{"id":44310104,"machineIdentifier":"test-mid"}]}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			id, _ := payload["invitedId"].(float64)
			sharedIDs = append(sharedIDs, id)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
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

	users, err := c.SharedServerUsers(context.Background(), "owner-token")
	if err != nil {
		t.Fatalf("SharedServerUsers error: %v", err)
	}
	if len(users) != 2 || users[0].FriendlyName != "Patricson Poliard" || users[1].Username != "dsabelag" {
		t.Fatalf("unexpected SharedServerUsers: %+v", users)
	}
	if !users[0].AcceptedShare || users[0].PendingShare || !slices.Contains(users[0].Servers, "other-pms") {
		t.Fatalf("expected accepted share provenance for Patricson Poliard, got %+v", users[0])
	}
	if !users[1].PendingShare || users[1].AcceptedShare || !slices.Contains(users[1].PendingServers, "test-mid") {
		t.Fatalf("expected pending share provenance for dsabelag, got %+v", users[1])
	}

	if err := c.ShareServer(context.Background(), "owner-token", "Patricson Poliard", 0, nil); err != nil {
		t.Fatalf("ShareServer failed for FriendlyName: %v", err)
	}
	if err := c.ShareServer(context.Background(), "owner-token", "dsabelag", 0, nil); err != nil {
		t.Fatalf("ShareServer failed for pending share user: %v", err)
	}
	if len(sharedIDs) != 2 || sharedIDs[0] != 880011 || sharedIDs[1] != 990022 {
		t.Fatalf("expected invitedIds [880011, 990022], got %v", sharedIDs)
	}
}

func TestShareServerV2ZeroLibrariesUpgradeToAll(t *testing.T) {
	var updateCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"errors":[{"code":1123,"message":"You're already sharing this server"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/owned/accepted":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/invites/owned/pending":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"invited":{"id":990022,"username":"dsabelag"},"sharedServers":[{"id":44310104,"machineIdentifier":"test-mid","allLibraries":false,"librarySectionIds":[],"invited":{"id":990022,"username":"dsabelag"}}]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers/44310104":
			updateCalled.Store(true)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if all, ok := payload["allLibraries"].(bool); !ok || !all {
				t.Fatalf("expected allLibraries upgraded to true when existing share had 0 sections, got %v", payload["allLibraries"])
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":44310104,"allLibraries":true}`))
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

	if err := c.ShareServer(context.Background(), "owner-token", "dsabelag", 990022, nil); err != nil {
		t.Fatalf("ShareServer failed: %v", err)
	}
	if !updateCalled.Load() {
		t.Fatal("expected POST /api/v2/shared_servers/44310104 to be called")
	}
}

func TestShareServerV2InvitedIDFallbacks(t *testing.T) {
	t.Run("retries skipFriendship=false on 422", func(t *testing.T) {
		var payloads []map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				payloads = append(payloads, payload)
				if len(payloads) == 1 {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = w.Write([]byte(`{"errors":[{"message":"friendship required"}]}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":9001,"allLibraries":true}`))
			case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/owned/accepted",
				r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/invites/owned/pending":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
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

		if err := c.ShareServer(context.Background(), "owner-token", "Patricson Poliard", 143639010, nil); err != nil {
			t.Fatalf("ShareServer failed: %v", err)
		}
		if len(payloads) != 2 {
			t.Fatalf("expected 2 v2 share POSTs, got %d (%+v)", len(payloads), payloads)
		}
		if payloads[0]["skipFriendship"] != true || payloads[0]["invitedId"] != float64(143639010) {
			t.Fatalf("unexpected first payload: %+v", payloads[0])
		}
		if payloads[1]["skipFriendship"] != false || payloads[1]["invitedId"] != float64(143639010) {
			t.Fatalf("unexpected second payload: %+v", payloads[1])
		}
		if _, hasEmail := payloads[1]["invitedEmail"]; hasEmail {
			t.Fatalf("expected no invitedEmail on invitedId retry, got %+v", payloads[1])
		}
	})

	t.Run("does not fall back to non-email display name when invitedId fails", func(t *testing.T) {
		var sawUsernameInvite bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if _, hasEmail := payload["invitedEmail"]; hasEmail {
					sawUsernameInvite = true
				}
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errors":[{"message":"not found"}]}`))
			case r.Method == http.MethodPost && r.URL.Path == "/api/servers/test-mid/shared_servers":
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if ss, ok := payload["shared_server"].(map[string]any); ok {
					if ss["invited_email"] == "Rose" {
						sawUsernameInvite = true
					}
				}
				w.WriteHeader(http.StatusNotFound)
			case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/owned/accepted",
				r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/invites/owned/pending":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
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

		if err := c.ShareServer(context.Background(), "owner-token", "Rose", 568833244, nil); err == nil {
			t.Fatal("expected error when invitedId 568833244 returns 404")
		}
		if sawUsernameInvite {
			t.Fatal("must not fall back from numeric userID to non-email display name")
		}
	})

	t.Run("retries invitedEmail on 404 when target is an email", func(t *testing.T) {
		var payloads []map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers":
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				payloads = append(payloads, payload)
				if len(payloads) == 1 {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errors":[{"message":"not found"}]}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":9002,"allLibraries":true}`))
			case r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/owned/accepted",
				r.Method == http.MethodGet && r.URL.Path == "/api/v2/shared_servers/invites/owned/pending":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
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

		if err := c.ShareServer(context.Background(), "owner-token", "faysa12@example.com", 33483787, nil); err != nil {
			t.Fatalf("ShareServer failed: %v", err)
		}
		if len(payloads) != 2 {
			t.Fatalf("expected 2 v2 share POSTs, got %d (%+v)", len(payloads), payloads)
		}
		if payloads[0]["invitedId"] != float64(33483787) {
			t.Fatalf("unexpected first payload: %+v", payloads[0])
		}
		if payloads[1]["invitedEmail"] != "faysa12@example.com" {
			t.Fatalf("unexpected second payload: %+v", payloads[1])
		}
		if _, hasID := payloads[1]["invitedId"]; hasID {
			t.Fatalf("expected no invitedId on invitedEmail retry, got %+v", payloads[1])
		}
	})

	t.Run("retries 429 with Retry-After and recovers", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/v2/shared_servers" {
				if attempts.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"errors":[{"code":1003,"message":"API rate limit exceeded"}]}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":9003,"allLibraries":true}`))
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

		if err := c.ShareServer(context.Background(), "owner-token", "friend", 703916194, nil); err != nil {
			t.Fatalf("ShareServer 429 retry failed: %v", err)
		}
		if attempts.Load() != 2 {
			t.Fatalf("expected 2 POST attempts, got %d", attempts.Load())
		}
	})

	t.Run("stops without findV2Share or v1 when 429 persists", func(t *testing.T) {
		var postCalls atomic.Int32
		var fallbackCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v2/shared_servers":
				postCalls.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"errors":[{"code":1003,"message":"API rate limit exceeded"}]}`))
			case "/api/v2/shared_servers/owned/accepted", "/api/v2/shared_servers/invites/owned/pending", "/api/servers/test-mid/shared_servers":
				fallbackCalls.Add(1)
				http.NotFound(w, r)
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

		err = c.ShareServer(context.Background(), "owner-token", "friend", 703916194, nil)
		if err == nil {
			t.Fatal("expected 429 error")
		}
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests {
			t.Fatalf("expected APIError 429, got %v", err)
		}
		if postCalls.Load() != 4 {
			t.Fatalf("expected 4 POST attempts (1 + 3 retries), got %d", postCalls.Load())
		}
		if fallbackCalls.Load() != 0 {
			t.Fatalf("expected no findV2Share or v1 fallback calls after 429, got %d", fallbackCalls.Load())
		}
	})
}
