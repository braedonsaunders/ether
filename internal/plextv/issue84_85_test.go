package plextv

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"
)

// TestRequiredPublishesPlainAddressesAndCustom covers issues #84 and #192.
// Required mode must publish something (#84), and like PMS it publishes the
// plain LAN IP rather than its plex.direct host: plex.tv matches the IP
// against localAddresses, so clients list the server as Nearby (#192), and
// httpsRequired makes plex.tv offer only the https plex.direct variants.
func TestRequiredPublishesPlainAddressesAndCustom(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	cfg := testConfig()
	cfg.Plex.Token, cfg.Plex.Claimed = "server-token-42", true
	cfg.Plex.SecureConnections, cfg.Plex.CertHash = "required", fakeHash
	cfg.Plex.RemoteAccess = false
	cfg.Plex.CustomURLs = []string{"https://plex.example.com"}
	r, err := NewRegistrar(RegistrarOptions{Config: &memConfig{cfg: cfg}, Client: newServerClient(t, f), Mapper: &fakeMapper{}, WAN: &fakeWAN{}, LAN: func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.PublishNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	published := r.Status().PublishedURIs
	if !slices.Contains(published, "http://192.168.1.5:32400") || slices.Contains(published, "https://192-168-1-5."+fakeHash+".plex.direct:32400") {
		t.Fatalf("published %v", published)
	}
	if !slices.Contains(r.Status().PublishedURIs, "https://plex.example.com") {
		t.Fatal("custom URL missing")
	}
	f.mu.Lock()
	required := f.httpsRequired
	f.mu.Unlock()
	if required != "1" {
		t.Fatalf("httpsRequired = %q", required)
	}
	resources, err := r.client.Resources(context.Background(), "server-token-42")
	if err != nil {
		t.Fatal(err)
	}
	got := false
	for _, c := range resources[0].Connections {
		if c.Address == "192.168.1.5" {
			got = true
		}
	}
	if !got {
		t.Fatal("fake resources did not expose the published URI")
	}
}

func TestCertificateSubjectMismatchReissuesAfterRestart(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	opts := CertManagerOptions{Dir: t.TempDir(), Client: newServerClient(t, f), Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}}
	m, err := NewCertManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Ensure(context.Background(), "server-token-42", nil, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.certHash = "11111111111111111111111111111111"
	f.mu.Unlock()
	m, err = NewCertManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.Ensure(context.Background(), "server-token-42", nil, false)
	if err != nil || info.Hash != "11111111111111111111111111111111" {
		t.Fatalf("hash=%s err=%v", info.Hash, err)
	}
}

func TestReclaimReissuesValidCertificateAndRetriesAfterRestart(t *testing.T) {
	ctx := context.Background()
	f := newFakePlexTV(t, testMachineID)
	f.addUser("user-token", owner())
	f.addUser("server-token-42", owner())
	f.mu.Lock()
	f.modelAssociation = true
	f.mu.Unlock()
	client := newServerClient(t, f)
	now := time.Now()
	opts := CertManagerOptions{Dir: t.TempDir(), Client: client, Now: func() time.Time { return now }, RetryAfter: time.Hour, Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}}
	m, err := NewCertManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.Claimed = true
	cfg.Plex.RemoteAccess = false
	store := &memConfig{cfg: cfg}
	r, err := NewRegistrar(RegistrarOptions{Config: store, Client: client, Certs: m, Mapper: &fakeMapper{}, WAN: &fakeWAN{}, LAN: func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.5")}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RenewCertificate(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err = r.PublishNow(ctx); err != nil {
		t.Fatal(err)
	}
	old := m.Info().Fingerprint
	if err = r.Unclaim(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Claim(ctx, "user-token"); err != nil {
		t.Fatal(err)
	}
	if err = r.PublishNow(ctx); err != nil {
		t.Fatal(err)
	}
	resources, err := client.Resources(ctx, "server-token-42")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resources[0].Connections {
		if c.Protocol == "https" {
			t.Fatal("new device unexpectedly associated")
		}
	}
	// The subject remains equal, yet this new device has no association.
	_, hash, err := client.CertificateSubject(ctx, "server-token-42")
	if err != nil || hash != m.Info().Hash {
		t.Fatalf("subject %s: %v", hash, err)
	}
	f.mu.Lock()
	f.csrStatus = 429
	f.mu.Unlock()
	if err = r.RenewCertificate(ctx, false); err == nil {
		t.Fatal("expected failure")
	}
	if m.Info().Fingerprint != old || !m.Info().ReissueRequired {
		t.Fatal("old certificate or reissue marker lost")
	}
	m, err = NewCertManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Ensure(ctx, "server-token-42", nil, false); err == nil {
		t.Fatal("restart lost retry backoff")
	}
	f.mu.Lock()
	f.csrStatus = 0
	f.mu.Unlock()
	now = now.Add(2 * time.Hour)
	info, err := m.Ensure(ctx, "server-token-42", nil, false)
	if err != nil || info.ReissueRequired || info.Fingerprint == old {
		t.Fatalf("retry failed: %+v %v", info, err)
	}
	resources, err = client.Resources(ctx, "server-token-42")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range resources[0].Connections {
		if c.Protocol == "https" {
			found = true
		}
	}
	if !found {
		t.Fatal("reissue did not associate fake device")
	}
}

func TestReissueDoesNotTouchCustomCertificate(t *testing.T) {
	ctx := context.Background()
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	managed, err := NewCertManager(CertManagerOptions{Dir: t.TempDir(), Client: newServerClient(t, f), Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := managed.Ensure(ctx, "server-token-42", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := NewCertManager(CertManagerOptions{Dir: t.TempDir(), CustomCertFile: info.CertFile, CustomKeyFile: info.KeyFile})
	if err != nil {
		t.Fatal(err)
	}
	before := custom.Info().Fingerprint
	if err = custom.RequireReissue(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := custom.Ensure(ctx, "server-token-42", nil, false)
	if err != nil || after.Fingerprint != before || after.ReissueRequired {
		t.Fatal("custom certificate changed")
	}
}

func TestReissueMarkerSharedAcrossNodes(t *testing.T) {
	ctx := context.Background()
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	shared := &memBlobs{}
	opts := CertManagerOptions{Dir: t.TempDir(), Client: newServerClient(t, f), Shared: shared, Request: CertRequest{PollInterval: time.Millisecond, Timeout: time.Second}}
	leader, err := NewCertManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = leader.Ensure(ctx, "server-token-42", nil, false); err != nil {
		t.Fatal(err)
	}
	followerOpts := opts
	followerOpts.Dir = t.TempDir()
	follower, err := NewCertManager(followerOpts)
	if err != nil {
		t.Fatal(err)
	}
	if err = leader.RequireReissue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = follower.SyncShared(ctx); err != nil || !follower.Info().ReissueRequired {
		t.Fatalf("shared marker: %v", err)
	}
	if _, err = leader.Ensure(ctx, "server-token-42", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err = follower.SyncShared(ctx); err != nil || follower.Info().ReissueRequired {
		t.Fatalf("shared completion: %v", err)
	}
}

func TestPublishRejectsEmptyAddresses(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	cfg := testConfig()
	cfg.Plex.Token = "server-token-42"
	cfg.Plex.Claimed = true
	cfg.Plex.RemoteAccess = false
	cfg.Plex.CustomURLs = nil
	r, err := NewRegistrar(RegistrarOptions{Config: &memConfig{cfg: cfg}, Client: newServerClient(t, f), Mapper: &fakeMapper{}, WAN: &fakeWAN{}, LAN: func() ([]net.IP, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.PublishNow(context.Background()); err == nil {
		t.Fatal("empty publish succeeded")
	}
	f.mu.Lock()
	count := f.putCount
	f.mu.Unlock()
	if count != 0 || r.Status().LastPublishError == "" {
		t.Fatal("empty publish sent or error not reported")
	}
}
