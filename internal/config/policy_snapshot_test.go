package config

import (
	"context"
	"testing"
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
	if s.policyMu.TryLock() {
		s.policyMu.Unlock()
		t.Fatal("snapshot lost policy lease")
	}
	lease.Close()
	lease.Close()
	if !s.policyMu.TryLock() {
		t.Fatal("snapshot close did not release policy lease")
	}
	s.policyMu.Unlock()
}
