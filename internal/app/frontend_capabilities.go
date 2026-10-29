package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func init() {
	store.Register("frontend_client_families", 1, `CREATE TABLE frontend_client_families (surface TEXT NOT NULL, family TEXT NOT NULL, policy TEXT NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY(surface,family));`)
}

type surfaceCapabilities struct {
	app     *App
	surface frontend.Protocol
}

func (s surfaceCapabilities) Build(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
	// Protocol is assigned by the host; a client cannot opt into another rule set.
	req.Device.Protocol = s.surface
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return frontend.CapabilityResult{}, core.ErrUnauthorized
	}
	req.Device.Remote = !identity.LAN(ip)
	declared, err := (caps.Builder{}).Build(ctx, req.Device, req.Profile, req.ProtocolInfo, req.UserAgent)
	if err != nil {
		return frontend.CapabilityResult{}, err
	}
	policy := transcode.ClientPolicyFor(declared)
	if declared.SubtitleCodecs == nil {
		declared.SubtitleCodecs = append([]string(nil), policy.SubtitleCodecs...)
	}
	if len(declared.DVProfiles) == 0 {
		declared.DVProfiles = append([]int(nil), policy.DolbyVisionProfiles...)
	}
	result := frontend.CapabilityResult{Client: declared, Quality: resolver.SelectProfile(s.app.cfg.Get().Quality, declared, req.Viewer)}
	if req.Viewer != nil && s.surface != "plex" {
		s.app.recordFrontendFamily(ctx, s.surface, declared)
	}
	return result, nil
}

type protocolClientPolicy struct {
	transcode.ClientPolicy
	Protocol frontend.Protocol `json:"protocol"`
}

func (a *App) recordFrontendFamily(ctx context.Context, surface frontend.Protocol, caps core.ClientCaps) {
	// Store only the static family policy, not per-device declarations or viewer IDs.
	product := strings.TrimSpace(caps.Product)
	if product == "" || len(product) > 128 || len(caps.Platform) > 128 || len(caps.Device) > 128 {
		return
	}
	policy := transcode.ClientPolicyFor(core.ClientCaps{Protocol: surface, Product: product, Platform: caps.Platform, Device: caps.Device})
	if policy.Family == "generic" || (policy.Family == "Plex Web" && surface != "plex") {
		policy.Family = product
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return
	}
	// Bound the number of client-provided family names stored per surface.
	var count int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_client_families WHERE surface=?`, string(surface)).Scan(&count); err != nil {
		return
	}
	if count >= 256 {
		_, _ = a.db.ExecContext(ctx, `UPDATE frontend_client_families SET policy=?,last_seen=? WHERE surface=? AND family=?`, string(data), time.Now().Unix(), string(surface), policy.Family)
		return
	}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO frontend_client_families(surface,family,policy,last_seen) VALUES(?,?,?,?) ON CONFLICT(surface,family) DO UPDATE SET policy=excluded.policy,last_seen=excluded.last_seen`, string(surface), policy.Family, string(data), time.Now().Unix())
}
func (a *App) clientPolicies(ctx context.Context) ([]protocolClientPolicy, error) {
	defaults := transcode.ClientPolicies()
	rows := make([]protocolClientPolicy, 0, len(defaults))
	for _, p := range defaults {
		rows = append(rows, protocolClientPolicy{ClientPolicy: p, Protocol: "plex"})
	}
	result, err := a.db.QueryContext(ctx, `SELECT surface,policy FROM frontend_client_families WHERE surface<>? ORDER BY surface,family`, "plex")
	if err != nil {
		return nil, err
	}
	defer result.Close()
	for result.Next() {
		var surface, data string
		if err := result.Scan(&surface, &data); err != nil {
			return nil, err
		}
		var policy transcode.ClientPolicy
		if err := json.Unmarshal([]byte(data), &policy); err != nil {
			return nil, err
		}
		rows = append(rows, protocolClientPolicy{ClientPolicy: policy, Protocol: frontend.Protocol(surface)})
	}
	return rows, result.Err()
}

var _ frontend.Capabilities = surfaceCapabilities{}
