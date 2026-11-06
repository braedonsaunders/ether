package nodecontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/store"
)

type target struct {
	value   Observed
	calls   int
	failure string
}

func (t *target) Observe() (Observed, []Capability) {
	return t.value, []Capability{{Key: "weight", Scope: "node", Supported: true}}
}
func (t *target) Apply(_ context.Context, p Patch) string {
	t.calls++
	if t.failure != "" {
		return t.failure
	}
	if p.Weight != nil {
		t.value.Weight = *p.Weight
	}
	return ""
}
func TestExactTargetAcknowledgementAndCAS(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	local := &target{value: Observed{Weight: 1}}
	s := &Service{Store: clusterconfig.NewNodeControls(db), NodeID: "n", Boot: "boot-a", Target: local}
	s.reconcile(ctx)
	weight := 3
	r := Request{RequestID: "request", Boot: "boot-a", Patch: Patch{Weight: &weight}}
	got, err := s.Submit(ctx, "n", r)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "pending" || got.AppliedVersion != 0 || local.calls != 0 {
		t.Fatal("enqueue falsely acknowledged")
	}
	other := *s
	other.Boot = "boot-b"
	other.reconcile(ctx)
	if local.calls != 0 {
		t.Fatal("other boot applied request")
	}
	if _, err := s.Submit(ctx, "n", Request{RequestID: "different", Boot: "boot-a", Patch: r.Patch}); !errors.Is(err, clusterconfig.ErrNodeControlConflict) {
		t.Fatal("pending CAS accepted")
	}
	s.reconcile(ctx)
	got, err = s.Get(ctx, "n", "boot-a")
	if err != nil || got.State != "applied" || got.AppliedVersion != 1 || got.Observed.Weight != 3 {
		t.Fatalf("ack=%+v err=%v", got, err)
	}
	if _, err = s.Submit(ctx, "n", r); err != nil {
		t.Fatal(err)
	}
	s.reconcile(ctx)
	if local.calls != 1 {
		t.Fatal("idempotent replay applied twice")
	}
	changed := 4
	r.Patch.Weight = &changed
	if _, err = s.Submit(ctx, "n", r); !errors.Is(err, clusterconfig.ErrNodeControlConflict) {
		t.Fatal("changed payload reused id")
	}
	local.failure = "restart_required"
	r.RequestID = "fail"
	r.ExpectedVersion = 1
	if _, err = s.Submit(ctx, "n", r); err != nil {
		t.Fatal(err)
	}
	s.reconcile(ctx)
	got, _ = s.Get(ctx, "n", "boot-a")
	if got.State != "failed" || got.AppliedVersion != 1 || got.RequestedVersion != 2 || got.ErrorCode != "restart_required" {
		t.Fatalf("failure acknowledged incorrectly %+v", got)
	}
}
