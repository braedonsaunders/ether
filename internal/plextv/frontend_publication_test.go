package plextv

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestPlexAppsTogglePreservesMembershipAndResumesPublication(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessXML, f.usersXML = accessTokensXML, usersXML
	cfg := testConfig()
	cfg.Plex.Claimed, cfg.Plex.Token = true, "server-token-42"
	cfg.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
	store := &memConfig{cfg: cfg}
	users := &fakeUsers{}
	r, err := NewRegistrar(RegistrarOptions{Config: store, Client: newServerClient(t, f), Users: users, WAN: &fakeWAN{}, Mapper: &fakeMapper{}, LAN: func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r.runDue(ctx)
	if err := r.PublishNow(ctx); err == nil {
		t.Fatal("direct publication succeeded while disabled")
	}
	if f.count("PUT /devices/") != 0 || f.count("DELETE /devices/") != 0 {
		t.Fatal("disabled apps changed server device")
	}
	users.mu.Lock()
	n := len(users.friends)
	users.mu.Unlock()
	if n == 0 {
		t.Fatal("membership did not sync while apps disabled")
	}
	if !store.Get().Plex.Claimed || store.Get().Plex.Token == "" {
		t.Fatal("account was unclaimed")
	}
	if err := store.Update(func(c *config.Config) error {
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.onConfig(store.Get())
	r.runDue(ctx)
	if f.count("PUT /devices/") == 0 {
		t.Fatal("publication did not resume")
	}
	if err := store.Update(func(c *config.Config) error {
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.onConfig(store.Get())
	before := f.count("PUT /devices/")
	r.schedule(taskFriends, time.Time{})
	r.runDue(ctx)
	if f.count("PUT /devices/") != before || f.count("DELETE /devices/") != 0 {
		t.Fatal("disabled apps changed server device")
	}
}

func TestPlexAppsToggleStopsAndRestartsPresence(t *testing.T) {
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": ready\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-req.Context().Done()
		stopped <- struct{}{}
	}))
	defer sse.Close()
	host := strings.TrimPrefix(sse.URL, "http://")
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, `<Servers size="1"><Server host=%q region="yyz"/></Servers>`, host)
	}))
	defer tv.Close()
	client, err := New(Options{BaseURL: tv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Plex.Token, cfg.Plex.OwnerPlexID = "token", 42
	store := &memConfig{cfg: cfg}
	r, err := NewRegistrar(RegistrarOptions{Config: store, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	r.presence = NewPresence(PresenceOptions{Client: client, Credentials: func() (int64, string, string, bool) { return 42, testMachineID, "token", true }, StreamClient: sse.Client(), StreamURL: func(h string, owner int64, machine, token string) string { return sse.URL }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.runPresence(ctx) }()
	defer func() { cancel(); <-done }()
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	wait(started, "initial presence")
	store.Update(func(c *config.Config) error {
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		return nil
	})
	r.onConfig(store.Get())
	wait(stopped, "presence shutdown")
	store.Update(func(c *config.Config) error {
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: true}}
		return nil
	})
	r.onConfig(store.Get())
	wait(started, "presence restart")
}
