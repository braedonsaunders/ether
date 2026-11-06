package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// One loss and one restoration are logged per unavailable interval; changes of
// reason while unavailable do not replace or repeatedly log the first failure.
// These diagnostics describe the first failed decision in an unavailable
// interval. They are never consulted by readiness, admission or authority.
type privateReadinessReason string

const (
	readinessCluster  privateReadinessReason = "cluster_mismatch"
	readinessRead     privateReadinessReason = "membership_read"
	readinessMissing  privateReadinessReason = "self_missing"
	readinessBoot     privateReadinessReason = "boot_mismatch"
	readinessFuture   privateReadinessReason = "age_below_minimum"
	readinessStale    privateReadinessReason = "age_at_or_above_maximum"
	readinessNotLive  privateReadinessReason = "not_live"
	readinessRoles    privateReadinessReason = "invalid_roles"
	readinessPool     privateReadinessReason = "set_pool"
	readinessTarget   privateReadinessReason = "set_target"
	readinessRuntime  privateReadinessReason = "runtime_not_ready"
	readinessPublic   privateReadinessReason = "public_not_ready"
	readinessListener privateReadinessReason = "private_listener_failed"
	readinessIdentity privateReadinessReason = "mounted_identity_changed"
	readinessRotation privateReadinessReason = "credential_rotation_failed"
	readinessClosed   privateReadinessReason = "private_closed"
	readinessQuota    privateReadinessReason = "initial_quota_read"
)

type privateReadinessSnapshot struct {
	reason     privateReadinessReason
	at         time.Time
	lastSeen   time.Time
	memberAt   time.Time
	age        time.Duration
	hasMember  bool
	deadline   time.Time
	errorClass string
}

// No error text, SQL state/code or identity is retained. Driver error interfaces
// classify SQL failures without exposing their potentially sensitive messages.
func privateReadinessError(err error) string {
	var state interface{ SQLState() string }
	var code interface{ Code() int }
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "cancel"
	case errors.As(err, &state), errors.As(err, &code), errors.Is(err, sql.ErrConnDone), errors.Is(err, sql.ErrTxDone), errors.Is(err, sql.ErrNoRows):
		return "sql"
	default:
		return "other"
	}
}

func (a *App) recordPrivateReadiness(s privateReadinessSnapshot) {
	p := a.deployment
	if p == nil || a.log == nil {
		return
	}
	if s.reason == "" {
		if previous := p.readinessLoss.Swap(nil); previous != nil {
			a.log.Info("private readiness restored", "component", "private-runtime", "role", privateReadinessRole(p.settings.Roles), "previous_reason", previous.reason, "evaluated_at", time.Now().UTC())
		}
		return
	}
	stage := "lifecycle"
	switch s.reason {
	case readinessCluster, readinessRead, readinessMissing, readinessBoot, readinessFuture, readinessStale, readinessNotLive, readinessRoles:
		stage = "membership"
	case readinessQuota:
		stage = "quota"
	case readinessPool:
		stage = "placement_pool"
	case readinessTarget:
		stage = "placement_target"
	}
	if s.at.IsZero() {
		s.at = time.Now()
	}
	if s.errorClass == "" {
		s.errorClass = "none"
	}
	if !p.readinessLoss.CompareAndSwap(nil, &s) {
		return
	}
	attrs := []any{"component", "private-runtime", "stage", stage, "reason", s.reason, "role", privateReadinessRole(p.settings.Roles), "evaluated_at", s.at.UTC(), "error_category", s.errorClass}
	if !s.deadline.IsZero() {
		attrs = append(attrs, "context_deadline", s.deadline.UTC())
	}
	if s.hasMember {
		attrs = append(attrs, "membership_evaluated_at", s.memberAt.UTC(), "self_last_seen", s.lastSeen.UTC(), "self_age", s.age, "age_min_inclusive", -time.Second, "age_max_exclusive", clusterLiveAfter)
	}
	a.log.Warn("private readiness unavailable", attrs...)
}

func privateReadinessRole(roles config.NodeRoles) string {
	if roles.FullServer() {
		return "full_server"
	}
	if len(roles) == 1 {
		switch roles[0] {
		case config.RoleServer, config.RoleSource, config.RoleDelivery, config.RoleTranscode, config.RoleBackground:
			return string(roles[0])
		}
	}
	return "mixed_or_unknown"
}
