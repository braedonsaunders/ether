package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/clusterplayback"
	"github.com/braedonsaunders/ether/internal/core"
)

type drainCountRuntime struct {
	privatePlayback
	counts clusterplayback.ResponsibilityCounts
	err    error
}

func (r *drainCountRuntime) PendingDetails(context.Context) (clusterplayback.ResponsibilityCounts, error) {
	return r.counts, r.err
}

type drainSessionList struct {
	core.Playback
	n int
}

func (s *drainSessionList) Sessions() []*core.PlaySession { return make([]*core.PlaySession, s.n) }

func TestDrainStatusSeparatesBarriersAndUnknownAccounting(t *testing.T) {
	dir := t.TempDir()
	rt := &drainCountRuntime{counts: clusterplayback.ResponsibilityCounts{RemoteLive: 1}}
	sessions := &drainSessionList{n: 1}
	a := &App{opts: Options{DataDir: dir}, brokerPlayback: sessions, deployment: &deploymentRuntime{playback: rt}}
	a.cluster = cluster.NewSolo(cluster.Local{Identity: cluster.Identity{NodeID: "test-node"}}, nil)
	defer a.cluster.Close()
	boot := a.cluster.Self().Incarnation
	if err := clusterconfig.WriteDrainControl(dir, clusterconfig.DrainRequestFile, clusterconfig.DrainControl{Boot: boot, Operation: "exact-request", State: "requested"}); err != nil {
		t.Fatal(err)
	}
	a.deployment.ready.Store(true)
	a.pollDrain(context.Background())
	st, err := clusterconfig.ReadDrainControl(dir, clusterconfig.DrainStatusFile)
	if err != nil || st.Pending != 2 || st.PendingRuntime == nil || *st.PendingRuntime != 1 || st.PendingSessions == nil || *st.PendingSessions != 1 || st.PendingCategories == nil || st.PendingCategories.RemoteLive != 1 || st.State != "draining" {
		t.Fatalf("wrong barrier status: %+v %v", st, err)
	}
	if !a.nodeDraining() || !a.deployment.ready.Load() {
		t.Fatal("drain confused admission and existing-work readiness")
	}
	rt.counts.PreparationReceipts = 1
	a.pollDrain(context.Background())
	st, err = clusterconfig.ReadDrainControl(dir, clusterconfig.DrainStatusFile)
	if err != nil || st.Pending != 3 || st.PendingRuntime == nil || *st.PendingRuntime != 2 || st.PendingCategories == nil || st.PendingCategories.PreparationReceipts != 1 || st.PendingCategories.RemoteLive != 1 {
		t.Fatalf("receipt barrier missing from subtotal/category: %+v %v", st, err)
	}
	rt.err = errors.New("private failure https://credential.invalid/secret")
	a.pollDrain(context.Background())
	st, err = clusterconfig.ReadDrainControl(dir, clusterconfig.DrainStatusFile)
	if err != nil || st.Pending != -1 || st.PendingRuntime != nil || st.PendingCategories != nil || st.PendingSessions == nil || *st.PendingSessions != 1 || st.State != "draining" {
		t.Fatalf("read error became zero/known: %+v %v", st, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, clusterconfig.DrainStatusFile))
	if err != nil || strings.Contains(string(raw), "credential.invalid") {
		t.Fatal("raw error leaked into diagnostics")
	}
	rt.err = nil
	rt.counts = clusterplayback.ResponsibilityCounts{}
	sessions.n = 0
	a.pollDrain(context.Background())
	st, err = clusterconfig.ReadDrainControl(dir, clusterconfig.DrainStatusFile)
	if err != nil || st.State != "drained" || st.Pending != 0 || st.PendingCategories == nil {
		t.Fatalf("exact clean barrier snapshot not acknowledged: %+v %v", st, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, clusterconfig.DrainStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	if err = clusterconfig.WriteDrainControl(dir, clusterconfig.DrainRequestFile, clusterconfig.DrainControl{Boot: "stale-boot", Operation: "stale-request", State: "requested"}); err != nil {
		t.Fatal(err)
	}
	a.pollDrain(context.Background())
	after, err := os.ReadFile(filepath.Join(dir, clusterconfig.DrainStatusFile))
	if err != nil || string(before) != string(after) {
		t.Fatal("stale PVC request changed exact status")
	}
}
