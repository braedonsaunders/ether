package config

import (
	"context"
	"testing"
	"time"
)

func TestSpikeVisibilityChangesFencePolicyEpoch(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bumps := 0
	s.SetPolicyEpoch(func() error { bumps++; return nil })
	before := s.Get().Spike.Enabled
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := s.AcquirePolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.policyMu.TryLock() {
		s.policyMu.Unlock()
		release()
		t.Fatal("policy lease did not hold its read lock")
	}
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		done <- s.Update(func(c *Config) error { c.Spike.Enabled = !before; return nil })
	}()
	<-started
	select {
	case err := <-done:
		release()
		t.Fatalf("update passed policy lease: %v", err)
	default:
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("update did not drain")
	}
	if s.Get().Spike.Enabled == before || bumps != 1 {
		t.Fatalf("after transition: spike=%v bumps=%d", s.Get().Spike.Enabled, bumps)
	}
	if err := s.Update(func(c *Config) error { c.Spike.LocalFiles = append(c.Spike.LocalFiles, "fixture"); return nil }); err != nil {
		t.Fatal(err)
	}
	if bumps != 1 {
		t.Fatalf("non-visibility field bumped epoch: %d", bumps)
	}
	if err := s.Update(func(c *Config) error { c.Spike.Enabled = before; return nil }); err != nil {
		t.Fatal(err)
	}
	if bumps != 2 {
		t.Fatalf("reverse transition bumps=%d", bumps)
	}
}
