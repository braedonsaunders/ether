package app

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/config"
)

// consoleStreamRuntime serves server-sent events to the owner console (WS2).
type consoleStreamRuntime struct{}

func (a *App) buildConsoleStream(ctx context.Context, cfg config.Config) (*consoleStreamRuntime, error) {
	return &consoleStreamRuntime{}, nil
}

// consoleStreamHandlers mounts GET /api/stream (WS2).
func (a *App) consoleStreamHandlers(m map[string]http.Handler) {}
