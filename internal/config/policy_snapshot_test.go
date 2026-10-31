package config

import (
	"context"
	"testing"
	"time"
)

func TestPolicySnapshotHoldsDetachedConfigAndSharedVersion(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lease, err := s.AcquirePolicySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	before := lease.Config()
	if _, clustered := lease.SharedVersion(); clustered {
		t.Fatal("standalone snapshot claimed a shared version")
	}
	copy := lease.Config()
	copy.Catalog.IncludeAdult = !copy.Catalog.IncludeAdult
	if lease.Config().Catalog.IncludeAdult != before.Catalog.IncludeAdult {
		t.Fatal("returned config aliases snapshot")
	}
	done := make(chan error, 1)
	go func() {
		done <- s.Update(func(cfg *Config) error {
			cfg.Catalog.IncludeAdult = !before.Catalog.IncludeAdult
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("policy snapshot blocked a writer")
	}
	if lease.Config().Catalog.IncludeAdult != before.Catalog.IncludeAdult {
		t.Fatal("snapshot followed a later write")
	}
	lease.Close()
	lease.Close()
	if !s.policyMu.TryLock() {
		t.Fatal("snapshot close did not release policy lease")
	}
	s.policyMu.Unlock()
}

func TestTrustedProxyCIDRsIsACopy(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(cfg *Config) error {
		cfg.Server.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := s.TrustedProxyCIDRs()
	got[0] = "192.168.0.0/16"
	if live := s.TrustedProxyCIDRs(); len(live) != 1 || live[0] != "10.0.0.0/8" {
		t.Fatalf("stored proxies %v", live)
	}
}
