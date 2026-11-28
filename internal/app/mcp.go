package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/agent"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/mcp"
)

func mcpAllowedHosts() []string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return nil
	}
	hosts := []string{host}
	if !strings.Contains(host, ".") {
		hosts = append(hosts, host+".local")
	}
	return hosts
}

func (a *App) buildMCPPairing() (*mcp.Pairing, error) {
	if a.db == nil || a.users == nil {
		return nil, fmt.Errorf("mcp: database and users required")
	}
	grants, err := identity.NewGrants(a.db, a.users, 30*24*time.Hour, nil)
	if err != nil {
		return nil, err
	}
	return mcp.NewPairing(mcp.PairingOptions{
		DB:           a.db,
		AllowedHosts: mcpAllowedHosts(),
		Grants:       grants,
		ClientIP:     a.frontendClientIP,
		Logger:       a.log.With("component", "mcp-pairing"),
		Authorize: func(ctx context.Context) (int64, error) {
			if acc, ok := admin.RequestAccess(ctx); !ok || !acc.Owner {
				return 0, fmt.Errorf("owner required")
			}
			owner, err := a.users.Owner(ctx)
			if err != nil || owner == nil {
				return 0, fmt.Errorf("owner required")
			}
			return owner.ID, nil
		},
	})
}

// mcpHandlers mounts the console MCP pairing routes under /api/agent/mcp (WS5).
func (a *App) mcpHandlers(m map[string]http.Handler) {
	pairing, err := a.buildMCPPairing()
	if err != nil {
		a.log.Warn("mcp pairing unavailable", "err", err)
		return
	}
	m["agent/mcp"] = pairing.Handler()
}

// mountMCP mounts the LAN-only MCP and OAuth endpoints
// over the tool catalogue (WS5).
func (a *App) mountMCP(mux *http.ServeMux) {
	if a.extendRT == nil || a.extendRT.tools == nil {
		return
	}
	pairing, err := a.buildMCPPairing()
	if err != nil {
		a.log.Warn("mcp unavailable", "err", err)
		return
	}
	store := agent.NewStore(a.db)
	srv, err := mcp.New(mcp.Options{
		Catalogue:    a.extendRT.tools,
		AllowedHosts: mcpAllowedHosts(),
		Approvals:    store,
		Authenticate: pairing.Authenticate,
		Resources:    mcp.DefaultResources(""),
		Version:      a.opts.Version,
		Logger:       a.log.With("component", "mcp"),
		OnApproval: func(ctx context.Context, appr agent.Approval) {
			if a.streamRT != nil {
				_ = a.streamRT.Publish(ctx, admin.StreamAgentApproval, map[string]any{
					"approvalId":  appr.ID,
					"threadId":    appr.ThreadID,
					"tool":        appr.ToolName,
					"category":    appr.Category,
					"description": appr.Description,
					"status":      appr.Status,
				})
			}
		},
		Audit: func(ctx context.Context, e mcp.AuditEvent) {
			_ = store.Audit(ctx, "", e.Tool, fmt.Sprintf("mcp:viewer:%d", e.ViewerID), e.Status, e.Error, e.Duration)
		},
	})
	if err != nil {
		a.log.Warn("mcp server unavailable", "err", err)
		return
	}
	mux.Handle("/ether/mcp", a.perf.wrap("mcp", lanOnly(srv.Handler(), a.cfg.TrustedProxyCIDRs, nil)))
	oauth := lanOnly(pairing.OAuthHandler(), a.cfg.TrustedProxyCIDRs, nil)
	mux.Handle("/.well-known/oauth-protected-resource", oauth)
	mux.Handle("/.well-known/oauth-protected-resource/ether/mcp", oauth)
	mux.Handle("/.well-known/oauth-authorization-server", oauth)
	mux.Handle("/ether/oauth/", oauth)
}
