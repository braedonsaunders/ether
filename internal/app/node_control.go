package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/nodecontrol"
)

func (a *App) nodeControlService() *nodecontrol.Service {
	self := a.cluster.Self()
	return &nodecontrol.Service{Store: clusterconfig.NewNodeControls(a.db), Bus: a.clusterNode(), NodeID: self.ID, Boot: self.Incarnation, Target: nodeControlTarget{a}}
}
func (a *App) runNodeControl(ctx context.Context) {
	if a.clustered() {
		a.nodeControlService().Run(ctx)
	}
}

type nodeControlTarget struct{ a *App }

func (t nodeControlTarget) Observe() (nodecontrol.Observed, []nodecontrol.Capability) {
	a := t.a
	caps := []nodecontrol.Capability{
		{Key: "weight", Scope: "node", Supported: true, Reason: "live_budget_weight"},
		{Key: "draining", Scope: "node", Reason: "ownership_interlock_required"},
		{Key: "maxStreams", Scope: "fleet", Reason: "fleet_setting"},
		{Key: "defaultStreamLimit", Scope: "fleet", Reason: "fleet_setting"},
		{Key: "maxTranscodes", Scope: "node", Reason: "not_allowlisted"},
		{Key: "hwAccel", Scope: "node", Reason: "restart_required"},
		{Key: "roles", Scope: "node", Reason: "role_lifecycle_unavailable"},
		{Key: "pause", Scope: "node", Reason: "no_safe_setter"},
	}
	if a.deployment != nil {
		caps[4].Reason = "restart_required"
	}
	return nodecontrol.Observed{Weight: a.cfg.Get().Server.ClusterWeight, Draining: a.nodeDraining()}, caps
}
func (t nodeControlTarget) Apply(ctx context.Context, p nodecontrol.Patch) string {
	a := t.a
	if ctx.Err() != nil {
		return "cancelled"
	}
	// A status snapshot cannot interlock with updater/CLI/shutdown drain owners.
	// Reject even previously queued or mixed requests before any local mutation.
	if p.Draining != nil {
		return "ownership_interlock_required"
	}
	if p.Weight != nil {
		if *p.Weight < 1 || *p.Weight > 100 {
			return "invalid_weight"
		}
		if err := a.cfg.Update(func(c *config.Config) error { c.Server.ClusterWeight = *p.Weight; return nil }); err != nil {
			return "save_failed"
		}
	}
	if pg, ok := a.cluster.(*cluster.Postgres); ok {
		pg.Beat()
	}
	obs, _ := t.Observe()
	if p.Weight != nil && obs.Weight != *p.Weight {
		return "observation_mismatch"
	}
	return ""
}

// Mounted under the authenticated owner cluster extension, never a peer RPC.
func (a *App) nodeControlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
		}
		if access, ok := admin.RequestAccess(r.Context()); !ok || !access.Owner {
			fail(401, "owner_required")
			return
		}
		if !a.clustered() || !a.cfg.Get().SetupComplete {
			fail(409, "not_clustered")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		node := r.PathValue("id")
		boot := r.URL.Query().Get("boot")
		var req nodecontrol.Request
		if r.Method == http.MethodPost {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			dec.DisallowUnknownFields()
			if dec.Decode(&req) != nil {
				fail(400, "invalid_request")
				return
			}
			var extra any
			if dec.Decode(&extra) != io.EOF {
				fail(400, "invalid_request")
				return
			}
			boot = req.Boot
		}
		if node == "" || boot == "" {
			fail(400, "target_required")
			return
		}
		nodes, err := a.cluster.Nodes(ctx)
		if err != nil {
			fail(503, "membership_unavailable")
			return
		}
		live := false
		for _, n := range nodes {
			if n.ID == node && n.Incarnation == boot && n.Live {
				live = true
			}
		}
		if !live {
			fail(409, "target_not_live")
			return
		}
		svc := a.nodeControlService()
		var out nodecontrol.Control
		if r.Method == http.MethodPost {
			out, err = svc.Submit(ctx, node, req)
		} else {
			out, err = svc.Get(ctx, node, boot)
		}
		if err != nil {
			switch {
			case errors.Is(err, sql.ErrNoRows):
				fail(404, "control_unavailable")
			case errors.Is(err, nodecontrol.ErrInvalid):
				fail(400, "invalid_request")
			case errors.Is(err, nodecontrol.ErrUnsupported):
				fail(422, "unsupported")
			case errors.Is(err, clusterconfig.ErrNodeControlConflict):
				fail(409, "control_conflict")
			default:
				fail(503, "control_unavailable")
			}
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}
