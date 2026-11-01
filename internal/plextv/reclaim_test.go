package plextv

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// subConfig is memConfig with config.Store's Subscribe.
type subConfig struct {
	*memConfig
	mu   sync.Mutex
	subs []func(config.Config)
}

func (s *subConfig) Subscribe(fn func(config.Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func (s *subConfig) Update(fn func(*config.Config) error) error {
	if err := s.memConfig.Update(fn); err != nil {
		return err
	}
	s.mu.Lock()
	subs := append([]func(config.Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(s.Get())
	}
	return nil
}

func newReclaimRegistrar(t *testing.T, f *fakePlexTV, store ConfigStore) *Registrar {
	t.Helper()
	r, err := NewRegistrar(RegistrarOptions{
		Config: store, Client: newServerClient(t, f), Users: &fakeUsers{},
		WAN: &fakeWAN{ip: "198.51.100.1"}, Mapper: &fakeMapper{},
		LAN: func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func accountB() Account {
	return Account{ID: 43, UUID: "uuid-43", Username: "other", Title: "Other Owner", Email: "b@example.com"}
}

func TestReclaimByAnotherAccountReleasesTheDevice(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-token", owner())
	f.addUser("user-b", accountB())
	store := &subConfig{memConfig: &memConfig{cfg: testConfig()}}
	r := newReclaimRegistrar(t, f, store)
	ctx := context.Background()

	if _, err := r.Claim(ctx, "user-token"); err != nil {
		t.Fatal(err)
	}
	r.runDue(ctx)
	if f.deviceOwner != 42 {
		t.Fatalf("device owner = %d after publishing, want 42", f.deviceOwner)
	}

	// The owner switches Claimed off in Settings.
	if err := store.Update(func(c *config.Config) error { c.Plex.Claimed = false; return nil }); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for store.Get().Plex.Token != "" {
		if time.Now().After(deadline) {
			t.Fatal("old server token was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.deviceOwner != 0 || f.deviceDeletes != 1 {
		t.Fatalf("device owner %d, deletes %d: the old account kept the device", f.deviceOwner, f.deviceDeletes)
	}

	// Account B signs in and claims: publishing works under B.
	if _, err := r.Claim(ctx, "user-b"); err != nil {
		t.Fatal(err)
	}
	r.runDue(ctx)
	st := r.Status()
	if st.LastPublishError != "" || st.TokenRejected || f.deviceOwner != 43 {
		t.Fatalf("publish under the new account: err=%q rejected=%v owner=%d", st.LastPublishError, st.TokenRejected, f.deviceOwner)
	}
}

func TestReclaimReportsATokenPlexRejects(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-b", accountB())
	f.deviceOwner = 42 // still on the previous account (e.g. Ether 0.2.18 never released it)
	f.accessXML = accessTokensXML
	f.usersXML = usersXML
	store := &subConfig{memConfig: &memConfig{cfg: testConfig()}}
	r := newReclaimRegistrar(t, f, store)
	ctx := context.Background()

	if _, err := r.Claim(ctx, "user-b"); err != nil {
		t.Fatal(err)
	}
	r.runDue(ctx)
	st := r.Status()
	if !st.TokenRejected || st.LastPublishError == "" {
		t.Fatalf("a 401 from plex.tv must be reported: %+v", st)
	}

	// Unclaim from Ether cannot remove another account's device; the
	// rejection status clears, and the token is forgotten.
	if err := r.Unclaim(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.TokenRejected || st.LastPublishError != "" || store.Get().Plex.Token != "" {
		t.Fatalf("after unclaim: %+v token=%q", st, store.Get().Plex.Token)
	}
}
