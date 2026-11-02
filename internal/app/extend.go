package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/extend"
)

// extendRuntime owns the descriptor registry and the tool catalogue that the
// console, Ask Ether, MCP and the plugin host share (docs/extend/SPEC.md §5.9,
// §5.10). Owned by WS0; workstreams register through their own files.
type extendRuntime struct {
	registry *extend.Registry
	tools    *tools.Catalogue
}

// buildExtensibility constructs the registry, registers every built-in
// descriptor and tool, then builds the console stream, the plugin host and
// the agent, in that order: each later part may use the earlier ones.
func (a *App) buildExtensibility(ctx context.Context, cfg config.Config) error {
	a.extendRT = &extendRuntime{registry: extend.New(), tools: tools.NewCatalogue()}
	if err := a.registerBuiltinDescriptors(a.extendRT.registry); err != nil {
		return fmt.Errorf("extension registry: %w", err)
	}
	if err := a.registerBuiltinTools(a.extendRT.tools); err != nil {
		return fmt.Errorf("agent tools: %w", err)
	}
	var err error
	if a.streamRT, err = a.buildConsoleStream(ctx, cfg); err != nil {
		return fmt.Errorf("console stream: %w", err)
	}
	if a.pluginsRT, err = a.buildPlugins(ctx, cfg); err != nil {
		return fmt.Errorf("plugins: %w", err)
	}
	if a.agentRT, err = a.buildAgent(ctx, cfg); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// extensibilityHandlers mounts owner API subtrees (under /ether/api/).
func (a *App) extensibilityHandlers(m map[string]http.Handler) {
	if a.extendRT == nil {
		return
	}
	m["extend/registry"] = extend.Handler(a.extendRT.registry, a.pluginTextResolver())
	a.consoleStreamHandlers(m)
	a.pluginHandlers(m)
	a.agentHandlers(m)
}

// mountExtensibility mounts routes that do not use owner-token auth: the MCP
// endpoint (MCP grants) and the plugin tunnel and host API (plugin
// credentials). Each is LAN-only like the console.
func (a *App) mountExtensibility(mux *http.ServeMux) {
	a.mountMCP(mux)
	a.mountPlugins(mux)
}
