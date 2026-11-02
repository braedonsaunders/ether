package app

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/config"
)

// agentRuntime is Ask Ether: the agent loop, transcripts and approvals (WS4).
type agentRuntime struct{}

func (a *App) buildAgent(ctx context.Context, cfg config.Config) (*agentRuntime, error) {
	return &agentRuntime{}, nil
}

// agentHandlers mounts /api/agent (WS4).
func (a *App) agentHandlers(m map[string]http.Handler) {}
