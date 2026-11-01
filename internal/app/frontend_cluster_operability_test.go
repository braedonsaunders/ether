package app

import (
	"context"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Exercise the same shared-store refresh path used after a cluster notification
// or polling tick, with independently mounted listeners on both nodes.
func TestFrontendClusterToggleReconcilesBothNodes(t *testing.T) {
	ctx := context.Background()
	dirA, dirB := t.TempDir(), t.TempDir()
	a, err := config.Open(dirA)
	if err != nil {
		t.Fatal(err)
	}
	key, err := config.ReadCredentialKey(dirA)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.InstallCredentialKey(dirB, key); err != nil {
		t.Fatal(err)
	}
	b, err := config.Open(dirB)
	if err != nil {
		t.Fatal(err)
	}
	backend := &frontendSharedConfig{}
	controls := make([]*frontendControl, 0, 2)
	for _, cfg := range []*config.Store{a, b} {
		if err := cfg.AttachShared(ctx, backend, config.SharedOptions{PollInterval: -1}); err != nil {
			t.Fatal(err)
		}
		reg := frontend.NewRegistry(func(frontend.Protocol) frontend.Host { return frontend.Host{} })
		if err := reg.Register(sharedFrontend{id: "files", prefix: "/files"}); err != nil {
			t.Fatal(err)
		}
		control := newFrontendControl(reg, cfg)
		cfg.Subscribe(func(c config.Config) { control.Reconcile(ctx, c) })
		controls = append(controls, control)
		defer control.listeners.close(ctx)
	}
	for _, enabled := range []bool{true, false, true} {
		if err := controls[0].Toggle(ctx, "files", enabled); err != nil {
			t.Fatal(err)
		}
		if err := b.RefreshShared(ctx); err != nil {
			t.Fatal(err)
		}
		for node, control := range controls {
			if got := control.listeners.status(ctx, "files").Mounted; got != enabled {
				t.Fatalf("node %d mounted=%v want %v", node, got, enabled)
			}
		}
	}
}

type frontendSharedConfig struct {
	mu  sync.Mutex
	doc config.SharedDoc
}

func (b *frontendSharedConfig) LoadShared(context.Context) (config.SharedDoc, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.doc, b.doc.Version > 0, nil
}
func (b *frontendSharedConfig) SaveShared(_ context.Context, doc config.SharedDoc, version int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.doc.Version != version {
		return 0, config.ErrSharedConflict
	}
	doc.Version = version + 1
	b.doc = doc
	return doc.Version, nil
}
func (b *frontendSharedConfig) SharedVersion(context.Context) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.doc.Version, nil
}
