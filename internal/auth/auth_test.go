package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeValidator struct {
	mu       sync.Mutex
	accounts map[string]plextv.Account
	access   map[string]bool
	down     bool
	delay    time.Duration
	calls    atomic.Int32
	hasCalls atomic.Int32
}

func (v *fakeValidator) Account(ctx context.Context, token string) (*plextv.Account, error) {
	v.calls.Add(1)
	if v.delay > 0 {
		time.Sleep(v.delay)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down {
		return nil, errors.New("plex.tv unreachable")
	}
	a, ok := v.accounts[token]
	if !ok {
		return nil, core.ErrUnauthorized
	}
	return &a, nil
}

func (v *fakeValidator) HasServer(ctx context.Context, token string) (bool, error) {
	v.hasCalls.Add(1)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down {
		return false, errors.New("plex.tv unreachable")
	}
	return v.access[token], nil
}

func (v *fakeValidator) setDown(d bool) { v.mu.Lock(); v.down = d; v.mu.Unlock() }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newClock() *clock               { return &clock{t: time.Unix(1_800_000_000, 0)} }
func openDB(t *testing.T) *sql.DB    { return openDBAt(t, ":memory:") }
func openDBAt(t *testing.T, p string) *sql.DB {
	db, err := store.OpenPath(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func setup(t *testing.T) (*Service, *fakeValidator, *clock) {
	v := &fakeValidator{
		accounts: map[string]plextv.Account{
			"owner-device": {ID: 42, Username: "owner", Title: "The Owner"},
			"alice-device": {ID: 100, Username: "alice", Title: "Alice"},
			"home-user":    {ID: 300, Username: "kid", Title: "Kid", Home: true, Restricted: true},
			"stranger":     {ID: 999, Username: "stranger"},
		},
		access: map[string]bool{"home-user": true},
	}
	c := newClock()
	s, err := New(context.Background(), openDB(t), Options{Validator: v, Now: c.now, CacheTTL: 10 * time.Minute, NegativeTTL: time.Minute, StaleTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.BootstrapOwner(ctx, config.Plex{OwnerPlexID: 42, OwnerUsername: "owner", Token: "server-token"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{
		{Owned: true, Token: "owner-access", PlexID: 42},
		{PlexID: 100, Username: "alice", Title: "Alice", Email: "a@example.com", Token: "alice-access", Sections: []string{"1"}},
		{PlexID: 101, Username: "bob", Token: "bob-access", AllLibs: true},
		{PlexID: 102, Username: "dave", Token: "dave-access", Pending: true},
	}); err != nil {
		t.Fatal(err)
	}
	return s, v, c
}

func TestAuthenticateKnownTokens(t *testing.T) {
	s, v, _ := setup(t)
	ctx := context.Background()
	for tok, want := range map[string]string{"server-token": "owner", "owner-access": "owner", "alice-access": "alice", "bob-access": "bob"} {
		u, err := s.Authenticate(ctx, tok)
		if err != nil || u.Username != want {
			t.Fatalf("%s: %+v %v", tok, u, err)
		}
	}
	if v.calls.Load() != 0 {
		t.Fatal("known tokens hit plex.tv")
	}
	u, _ := s.Authenticate(ctx, "server-token")
	if !u.IsOwner || u.PlexID != 42 {
		t.Fatalf("%+v", u)
	}
	a, _ := s.Authenticate(ctx, "alice-access")
	if a.IsOwner || len(a.AllowedSections) != 1 || a.AllowedSections[0] != 1 || a.Email != "a@example.com" {
		t.Fatalf("alice %+v", a)
	}
	if _, err := s.Authenticate(ctx, "dave-access"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("pending friend: %v", err)
	}
	if _, err := s.Authenticate(ctx, "  "); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestAuthenticateValidationAndCaching(t *testing.T) {
	s, v, c := setup(t)
	ctx := context.Background()

	// Owner on another device: validated via plex.tv, then cached.
	u, err := s.Authenticate(ctx, "owner-device")
	if err != nil || !u.IsOwner {
		t.Fatal(u, err)
	}
	s.Authenticate(ctx, "owner-device")
	if v.calls.Load() != 1 || v.hasCalls.Load() != 0 {
		t.Fatalf("calls=%d has=%d", v.calls.Load(), v.hasCalls.Load())
	}
	// A synced friend using a personal token: no resources check needed.
	if u, err := s.Authenticate(ctx, "alice-device"); err != nil || u.PlexID != 100 {
		t.Fatal(u, err)
	}
	if v.hasCalls.Load() != 0 {
		t.Fatal("unneeded resources check")
	}
	// A home user not in the friend list but with the server in resources.
	u, err = s.Authenticate(ctx, "home-user")
	if err != nil || u.PlexID != 300 || u.Title != "Kid" {
		t.Fatal(u, err)
	}
	// A stranger is rejected and negatively cached.
	if _, err := s.Authenticate(ctx, "stranger"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "garbage"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	n := v.calls.Load()
	s.Authenticate(ctx, "stranger")
	s.Authenticate(ctx, "garbage")
	if v.calls.Load() != n {
		t.Fatal("negative cache missed")
	}
	c.add(2 * time.Minute)
	s.Authenticate(ctx, "garbage")
	if v.calls.Load() != n+1 {
		t.Fatal("negative cache did not expire")
	}

	// Positive cache expiry triggers re-validation; plex.tv down -> stale entry served.
	c.add(11 * time.Minute)
	v.setDown(true)
	if u, err := s.Authenticate(ctx, "owner-device"); err != nil || !u.IsOwner {
		t.Fatalf("stale: %+v %v", u, err)
	}
	// Unknown token while plex.tv is down: an error, but not cached as invalid.
	if _, err := s.Authenticate(ctx, "never-seen"); err == nil || errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	v.setDown(false)
	v.mu.Lock()
	v.accounts["never-seen"] = plextv.Account{ID: 300, Username: "kid"}
	v.access["never-seen"] = true
	v.mu.Unlock()
	if _, err := s.Authenticate(ctx, "never-seen"); err != nil {
		t.Fatal(err)
	}
	// Past the stale window, a down plex.tv fails the request.
	c.add(2 * time.Hour)
	v.setDown(true)
	if _, err := s.Authenticate(ctx, "owner-device"); err == nil {
		t.Fatal("served beyond stale window")
	}
	// Synced tokens keep working regardless.
	if _, err := s.Authenticate(ctx, "alice-access"); err != nil {
		t.Fatal(err)
	}
	// Revoked token: plex.tv says unauthorized.
	v.setDown(false)
	v.mu.Lock()
	delete(v.accounts, "owner-device")
	v.mu.Unlock()
	if _, err := s.Authenticate(ctx, "owner-device"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestAuthenticateSingleflight(t *testing.T) {
	s, v, _ := setup(t)
	v.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Authenticate(context.Background(), "owner-device")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if v.calls.Load() != 1 {
		t.Fatalf("plex.tv called %d times", v.calls.Load())
	}
}

func TestSyncFriendsRemovesAccess(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if _, err := s.Authenticate(ctx, "alice-device"); err != nil {
		t.Fatal(err)
	}
	// Bob is unshared; Alice gets a new token.
	if err := s.SyncFriends(ctx, []plextv.Friend{{PlexID: 100, Username: "alice", Token: "alice-access-2", AllLibs: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "bob-access"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("bob still authorised: %v", err)
	}
	if _, err := s.Authenticate(ctx, "alice-access"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("old alice token kept")
	}
	if u, err := s.Authenticate(ctx, "alice-access-2"); err != nil || len(u.AllowedSections) != 0 {
		t.Fatal(u, err)
	}
	// Profile fields kept when the sync omits them.
	if u, _ := s.Authenticate(ctx, "alice-access-2"); u.Email != "a@example.com" || u.Title != "Alice" {
		t.Fatalf("%+v", u)
	}
	users, _ := s.Users(ctx)
	var names []string
	for _, u := range users {
		names = append(names, u.Username)
	}
	if strings.Join(names, ",") != "owner,alice" {
		t.Fatalf("users %v", names)
	}
	all, _ := s.AllUsers(ctx)
	if len(all) != 4 {
		t.Fatalf("all users %d", len(all))
	}
	// Bob comes back with his settings intact.
	var bobID int64
	for _, u := range all {
		if u.Username == "bob" {
			bobID = u.ID
		}
	}
	if err := s.UpdateUser(ctx, core.User{ID: bobID, StreamLimit: 3}); err != nil {
		t.Fatal(err)
	}
	s.SyncFriends(ctx, []plextv.Friend{{PlexID: 101, Username: "bob", Token: "bob-access"}})
	u, err := s.Authenticate(ctx, "bob-access")
	if err != nil || u.StreamLimit != 3 {
		t.Fatal(u, err)
	}
}

func TestUpdateUserAndPersistence(t *testing.T) {
	path := t.TempDir() + "/ether.db"
	db := openDBAt(t, path)
	ctx := context.Background()
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Owner(ctx); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner", Email: "o@example.com"}, "server-token"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{{PlexID: 100, Username: "alice", Token: "alice-access", Sections: []string{"1", "2"}}}); err != nil {
		t.Fatal(err)
	}
	alice, err := s.Authenticate(ctx, "alice-access")
	if err != nil {
		t.Fatal(err)
	}
	alice.StreamLimit = 1
	alice.MaxContentRating = " PG-13 "
	alice.SubtitleLanguages = []string{"EN", "fr", "en", ""}
	alice.AudioLanguages = []string{"ja"}
	alice.AllowedSections = []int{2}
	alice.Username = "hacked" // identity fields are ignored
	if err := s.UpdateUser(ctx, *alice); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, core.User{ID: 9999}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, core.User{ID: alice.ID, StreamLimit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
	s.FlushSeen(ctx)

	// A fresh service over the same database sees everything.
	db.Close()
	db2 := openDBAt(t, path)
	s2, err := New(ctx, db2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Authenticate(ctx, "alice-access")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if got.StreamLimit != 1 || got.MaxContentRating != "PG-13" || strings.Join(got.SubtitleLanguages, ",") != "en,fr" ||
		got.AudioLanguages[0] != "ja" || len(got.AllowedSections) != 1 || got.AllowedSections[0] != 2 || got.Username != "alice" {
		t.Fatalf("%s", b)
	}
	o, err := s2.Owner(ctx)
	if err != nil || o.PlexID != 42 || o.Email != "o@example.com" {
		t.Fatal(o, err)
	}
	if _, err := s2.Authenticate(ctx, "server-token"); err != nil {
		t.Fatal(err)
	}
	// Tokens are stored hashed.
	var n int
	db2.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE token_hash IN ('alice-access', 'server-token')`).Scan(&n)
	if n != 0 {
		t.Fatal("plain-text tokens stored")
	}
	all, _ := s2.AllUsers(ctx)
	for _, u := range all {
		if u.Username == "alice" && u.LastSeen.IsZero() {
			t.Fatal("last seen not persisted")
		}
	}
}

func TestOwnerChange(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if err := s.SetOwner(ctx, plextv.Account{ID: 100, Username: "alice"}, "new-server-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "server-token"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("old server token still valid")
	}
	o, _ := s.Owner(ctx)
	if o.PlexID != 100 {
		t.Fatalf("%+v", o)
	}
	users, _ := s.Users(ctx)
	for _, u := range users {
		if u.PlexID == 42 {
			t.Fatal("previous owner still active")
		}
	}
}

func TestNoValidator(t *testing.T) {
	s, err := New(context.Background(), openDB(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(context.Background(), "anything"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestCacheBound(t *testing.T) {
	v := &fakeValidator{accounts: map[string]plextv.Account{}}
	s, err := New(context.Background(), openDB(t), Options{Validator: v, MaxCacheEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		s.Authenticate(context.Background(), "bad-"+string(rune('a'+i%26))+time.Duration(i).String())
	}
	s.mu.RLock()
	n := len(s.neg) + len(s.pos)
	s.mu.RUnlock()
	if n > 100 {
		t.Fatalf("cache grew to %d", n)
	}
}

func TestPlexTVValidator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Plex-Token")
		switch r.URL.Path {
		case "/api/v2/user":
			if tok == "bad" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"id":7,"username":"u"}`))
		case "/api/v2/resources":
			if tok == "shared" {
				w.Write([]byte(`[{"clientIdentifier":"mid","provides":"server"}]`))
				return
			}
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	c, err := plextv.New(plextv.Options{BaseURL: srv.URL, Identity: plextv.Identity{Product: "Plex Media Server", ClientIdentifier: "mid"}})
	if err != nil {
		t.Fatal(err)
	}
	v := PlexTVValidator{Client: c, MachineID: "mid"}
	ctx := context.Background()
	if a, err := v.Account(ctx, "good"); err != nil || a.ID != 7 {
		t.Fatal(a, err)
	}
	if _, err := v.Account(ctx, "bad"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if ok, err := v.HasServer(ctx, "shared"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := v.HasServer(ctx, "other"); ok || err != nil {
		t.Fatal(ok, err)
	}
}

func TestValidatedViewerRechecksAccess(t *testing.T) {
	s, v, c := setup(t)
	ctx := context.Background()
	u, err := s.Authenticate(ctx, "home-user")
	if err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.access["home-user"] = false
	v.mu.Unlock()
	c.add(11 * time.Minute)
	if _, err := s.Authenticate(ctx, "home-user"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked validation: %v", err)
	}
	if _, err := s.ActiveUser(ctx, u.ID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("stream access survived revocation: %v", err)
	}
}

func TestSyncRevokesValidatedViewerAndPreservesTokenlessMember(t *testing.T) {
	s, v, c := setup(t)
	ctx := context.Background()
	u, err := s.Authenticate(ctx, "home-user")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{{PlexID: 100, Username: "alice", AllLibs: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveUser(ctx, u.ID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked viewer active: %v", err)
	}
	// Even a stale resources response must not resurrect an explicitly revoked user.
	if _, err := s.Authenticate(ctx, "home-user"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("cached resources resurrected viewer: %v", err)
	}
	if _, err := s.Authenticate(ctx, "alice-device"); err != nil {
		t.Fatalf("tokenless member denied: %v", err)
	}
	c.add(11 * time.Minute)
	v.setDown(true)
	if _, err := s.Authenticate(ctx, "home-user"); err == nil {
		t.Fatalf("outage resurrected viewer: %v", err)
	}
}

func TestUnknownTokenRefreshesFriendsBeforeValidation(t *testing.T) {
	s, v, _ := setup(t)
	ctx := context.Background()
	var refreshes atomic.Int32
	s.SetUnknownTokenRefresher(func(ctx context.Context) error {
		if refreshes.Add(1) == 1 {
			return s.SyncFriends(ctx, []plextv.Friend{
				{Owned: true, Token: "owner-access", PlexID: 42},
				{PlexID: 100, Username: "alice", Title: "Alice", Token: "alice-access", Sections: []string{"1"}},
				{PlexID: 205, Username: "eve", Title: "Eve", Token: "eve-access", AllLibs: true},
			})
		}
		return nil
	})

	u, err := s.Authenticate(ctx, "eve-access")
	if err != nil || u.Username != "eve" || u.PlexID != 205 {
		t.Fatalf("eve after unknown-token friend sync: user=%+v err=%v", u, err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes.Load())
	}
	if v.calls.Load() != 0 {
		t.Fatalf("durable token after refresh should not hit Validator.Account, got %d calls", v.calls.Load())
	}
}
