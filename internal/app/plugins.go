package app

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/extend"
)

// pluginRuntime is the plugin host and its adapters (WS7, WS8, WS9).
type pluginRuntime struct{}

func (a *App) buildPlugins(ctx context.Context, cfg config.Config) (*pluginRuntime, error) {
	return &pluginRuntime{}, nil
}

// pluginHandlers mounts the owner plugin API under /api/plugins (WS7).
func (a *App) pluginHandlers(m map[string]http.Handler) {}

// mountPlugins mounts the plugin tunnel and plugin host API (WS8b).
func (a *App) mountPlugins(mux *http.ServeMux) {}

// pluginTextResolver resolves plugin strings for the registry (WS7).
func (a *App) pluginTextResolver() extend.TextResolver { return nil }
