package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/cluster"
)

// This read-only guidance does not create a role operation or authorize restart.
// Membership observations cannot establish ownership retirement or session safety.
func (a *App) nodeRolePreflightHandler() http.Handler {
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
		id, boot := r.PathValue("id"), r.URL.Query().Get("boot")
		if id == "" || boot == "" || len(id) > 128 || len(boot) > 128 {
			fail(400, "target_required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		nodes, err := a.cluster.Nodes(ctx)
		if err != nil {
			fail(503, "membership_unavailable")
			return
		}
		var target *cluster.Node
		full := 0
		for i := range nodes {
			n := &nodes[i]
			if n.Live && n.Caps.Roles.FullServer() {
				full++
			}
			if n.Live && n.ID == id && n.Incarnation == boot {
				target = n
			}
		}
		if target == nil {
			fail(409, "target_not_live")
			return
		}
		reasons := []string{"executor_unavailable", "runtime_guard_not_verified", "ownership_retirement_unknown", "active_sessions_unknown"}
		if target.Caps.Roles.FullServer() && full <= 1 {
			reasons = append(reasons, "last_full_server")
		}
		if target.Leader {
			reasons = append(reasons, "leader_handoff_unverified")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodeId": id, "boot": boot, "observedAt": time.Now().UTC(), "membershipSampledAt": target.LastSeen,
			"eligible": false, "executor": "none", "reasons": reasons,
			"safety": map[string]string{"membership": "observed", "ownershipRetirement": "unknown", "activeSessions": "unknown", "restart": "unsupported"},
			"guidance": map[string]any{
				"scope": "deployment_pool", "deploymentOwnership": "unknown",
				"verifiedAgainst": []string{"packaging/helm/ether/values.yaml", "packaging/helm/ether/templates/statefulsets.yaml", "packaging/helm/ether/templates/validate.yaml"},
				"exampleValues":   "pools:\n  full:\n    replicas: 2\n",
				"impact":          "Example only: sets the desired replica count for the entire full pool. It does not move this node identity, drain sessions, restart pods, or prove role-change safety. Review the installed release values and deployment ownership before using it.",
				"execution":       "No Kubernetes RBAC or Helm executor is available. The chart uses OnDelete updates and disables service-account token mounting. Mixed public-role routing remains guarded; preserve runtime guards.",
			},
		})
	})
}
