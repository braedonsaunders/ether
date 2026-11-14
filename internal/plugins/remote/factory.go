package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// This file is the one place the remote package meets the plugin
// host's contract (internal/plugins/contract.go). It converts
// between the host's Request/Response shapes and the transport
// shapes in remote.go, implements Host over plugins.Host, and
// provides the RuntimeFactory registered for runtime kind "remote".

// Factory builds remote runtimes for the plugin host.
type Factory struct {
	// Tunnel serves tunnel-mode plugins. Required for plugins with
	// no URL endpoint.
	Tunnel *TunnelServer
	// TrustedLAN reports whether the owner confirmed the plugin's
	// URL as a LAN origin at install (Manager grants carry it).
	TrustedLAN func(pluginID string) bool
	// Credential returns the credential Ether presents to the plugin
	// on URL calls, or "" for none (plain Stremio add-ons).
	Credential func(pluginID string) string
	// Guard guards URL fetches; nil uses a zero safefetch.Guard.
	Guard *safefetch.Guard
	// Locale is the fallback X-Ether-Locale for calls that carry none.
	Locale string
	Logger *slog.Logger
}

// RuntimeFactory returns the plugins.RuntimeFactory for kind
// "remote". Endpoint selection, from Installed.SourceURL:
//
//   - "" or a tunnel: URL ("tunnel://", "ether+tunnel://"): the
//     sidecar connects out; calls go over Factory.Tunnel.
//   - an http(s) manifest URL, or any other http(s) URL that is not
//     a github.com repo page: URL mode against that base.
//   - a github.com repo URL: a repo is not an endpoint, so an Ether
//     plugin is served by a sidecar over the tunnel. A plain Stremio
//     add-on published as static files in the repo (manifest.json at
//     its root, e.g. "USA TV Next") needs no sidecar: its routes are
//     read from raw.githubusercontent.com, where the installer found
//     its manifest.
func (f *Factory) RuntimeFactory() plugins.RuntimeFactory {
	return func(inst plugins.Installed) (plugins.Runtime, error) {
		if inst.Manifest.Ether != nil && inst.Manifest.Ether.Runtime.Kind != "" &&
			inst.Manifest.Ether.Runtime.Kind != "remote" {
			return nil, fmt.Errorf("plugin %s: remote factory cannot open runtime kind %q", inst.ID, inst.Manifest.Ether.Runtime.Kind)
		}
		mode, base := endpointOf(inst.SourceURL, inst.Manifest)
		switch mode {
		case endpointTunnel:
			if f.Tunnel == nil {
				return nil, fmt.Errorf("plugin %s: tunnel endpoint is not configured", inst.ID)
			}
			return &contractRuntime{inner: f.Tunnel.RuntimeFor(inst.ID), plugin: inst.ID}, nil
		case endpointURL:
			opts := URLOptions{
				Base:                 base,
				AllowPublicRedirects: inst.Manifest.IsStremioOnly(),
				Plain:                inst.Manifest.IsStremioOnly(),
				Guard:                f.Guard,
				Locale:               f.Locale,
				Logger:               f.Logger,
			}
			if f.Credential != nil && !inst.Manifest.IsStremioOnly() {
				opts.Credential = f.Credential(inst.ID)
			}
			if f.TrustedLAN != nil && f.TrustedLAN(inst.ID) {
				opts.TrustedOrigin = base
			}
			client, err := NewURLClient(opts)
			if err != nil {
				return nil, fmt.Errorf("plugin %s: %w", inst.ID, err)
			}
			return &contractRuntime{inner: client, plugin: inst.ID}, nil
		default:
			return nil, fmt.Errorf("plugin %s: no remote endpoint in source URL", inst.ID)
		}
	}
}

// endpointOf picks a plugin's endpoint: endpointFor, except that a plain
// Stremio add-on from a GitHub repo reads its static routes from
// raw.githubusercontent.com instead of waiting for a sidecar.
func endpointOf(source string, man plugins.Manifest) (endpointMode, string) {
	mode, base := endpointFor(source)
	if mode == endpointTunnel && man.IsStremioOnly() {
		if raw, ok := githubRawBase(source); ok {
			return endpointURL, raw
		}
	}
	return mode, base
}

// SidecarState reports whether a remote plugin is served by a sidecar over
// the tunnel and whether that sidecar is connected now (plugins.SidecarState).
func (f *Factory) SidecarState(r plugins.Record) (needsSidecar, connected bool) {
	if r.Manifest.RuntimeKind() != "remote" {
		return false, false
	}
	if mode, _ := endpointOf(r.SourceURL, r.Manifest); mode != endpointTunnel {
		return false, false
	}
	return true, f.Tunnel != nil && f.Tunnel.IsConnected(r.ID)
}

type endpointMode int

const (
	endpointNone endpointMode = iota
	endpointURL
	endpointTunnel
)

func endpointFor(source string) (endpointMode, string) {
	source = strings.TrimSpace(source)
	if source == "" {
		return endpointTunnel, ""
	}
	u, err := url.Parse(source)
	if err != nil {
		return endpointNone, ""
	}
	switch u.Scheme {
	case "tunnel", "ether-tunnel", "ether+tunnel":
		return endpointTunnel, ""
	case "http", "https":
		if strings.EqualFold(u.Hostname(), "github.com") {
			return endpointTunnel, ""
		}
		return endpointURL, source
	default:
		return endpointNone, ""
	}
}

// githubRawBase maps a github.com repo link to the raw-file base its
// static Stremio routes are served from: the branch the link names
// (/tree/<branch> or /blob/<branch>/...), else HEAD, which GitHub
// resolves to the default branch.
func githubRawBase(source string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(source))
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
		return "", false
	}
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if len(parts) < 2 {
		return "", false
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	ref := "HEAD"
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
		ref = parts[3]
	}
	for _, seg := range []string{owner, repo, ref} {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return "https://raw.githubusercontent.com/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + url.PathEscape(ref), true
}

// contractRuntime adapts a transport Runtime to plugins.Runtime.
type contractRuntime struct {
	inner  Runtime
	plugin string
}

func (r *contractRuntime) Call(ctx context.Context, req plugins.Request) (plugins.Response, error) {
	resp, err := r.inner.Call(ctx, toRequest(req))
	if err != nil {
		return plugins.Response{}, err
	}
	return fromResponse(resp), nil
}

func (r *contractRuntime) Close() error { return r.inner.Close() }

// CallStream streams one call for adapters that relay bodies (file
// ranges for `file` streams). It is the remote counterpart of the
// transport Streamer interface, exposed in contract shapes so WS9
// adapters do not need transport types.
func (r *contractRuntime) CallStream(ctx context.Context, req plugins.Request) (plugins.Response, io.ReadCloser, error) {
	streamer, ok := r.inner.(Streamer)
	if !ok {
		return plugins.Response{}, nil, fmt.Errorf("plugin %s: runtime cannot stream", r.plugin)
	}
	stream, err := streamer.CallStream(ctx, toRequest(req))
	if err != nil {
		return plugins.Response{}, nil, err
	}
	return fromResponse(Response{Status: stream.Status, Headers: stream.Headers}), stream.Body, nil
}

// StreamCall is the optional streaming surface WS9 adapters look for
// on a plugins.Runtime.
type StreamCall interface {
	CallStream(ctx context.Context, req plugins.Request) (plugins.Response, io.ReadCloser, error)
}

var _ StreamCall = (*contractRuntime)(nil)
var _ plugins.Runtime = (*contractRuntime)(nil)

// toRequest converts a contract request to a transport request. The
// host's per-call context deadline becomes the call timeout when it
// is tighter than the transport default.
func toRequest(req plugins.Request) Request {
	query := make(map[string][]string, len(req.Query))
	for k, v := range req.Query {
		query[k] = []string{v}
	}
	headers := make(map[string][]string, len(req.Headers))
	for k, v := range req.Headers {
		headers[k] = []string{v}
	}
	return Request{
		Method:  req.Method,
		Path:    req.Path,
		Query:   query,
		Headers: headers,
		Body:    []byte(req.Body),
		Locale:  req.Locale,
	}
}

func fromResponse(resp Response) plugins.Response {
	headers := make(map[string]string, len(resp.Headers))
	for k, vs := range resp.Headers {
		headers[k] = strings.Join(vs, ", ")
	}
	return plugins.Response{Status: resp.Status, Headers: headers, Body: json.RawMessage(resp.Body)}
}

// HostAdapter exposes one plugin's plugins.Host through the Host
// interface the plugin-host HTTP API delegates to. HostFor is one
// adapter for every plugin: it binds the plugin id the credential
// resolved to, then calls Manager.HostFor. Errors are translated to
// this package's sentinels so the HTTP layer maps them precisely.
type HostAdapter struct {
	HostFor func(pluginID string) plugins.Host
	// SettingsWithSecrets, when set, returns the plugin's settings with its own
	// secrets (Manager.PluginSettings); a remote sidecar cannot call
	// secret.get, so this is how it recovers configuration after a
	// restart or reconnect. Without it only non-secret settings are served.
	SettingsWithSecrets func(ctx context.Context, pluginID string) (map[string]any, error)
}

func (a HostAdapter) host(pluginID string) (plugins.Host, error) {
	if a.HostFor == nil {
		return nil, ErrUnavailable
	}
	h := a.HostFor(pluginID)
	if h == nil {
		return nil, ErrUnavailable
	}
	return h, nil
}

func mapHostErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, plugins.ErrPermissionDenied), errors.Is(err, plugins.ErrDisabled):
		return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
	case errors.Is(err, plugins.ErrNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, plugins.ErrUnavailable):
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	case errors.Is(err, plugins.ErrQuotaExceeded):
		return fmt.Errorf("%w: %v", ErrTooLarge, err)
	default:
		var ve *plugins.ValidationError
		var collectionInput *plugins.CollectionInputError
		if errors.As(err, &ve) || errors.As(err, &collectionInput) {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		return err
	}
}

func (a HostAdapter) KVGet(ctx context.Context, pluginID, key string) ([]byte, bool, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, false, err
	}
	v, ok, err := h.KVGet(ctx, key)
	return v, ok, mapHostErr(err)
}

func (a HostAdapter) KVPut(ctx context.Context, pluginID, key string, value []byte) error {
	h, err := a.host(pluginID)
	if err != nil {
		return err
	}
	return mapHostErr(h.KVPut(ctx, key, value))
}

func (a HostAdapter) KVDelete(ctx context.Context, pluginID, key string) error {
	h, err := a.host(pluginID)
	if err != nil {
		return err
	}
	return mapHostErr(h.KVDelete(ctx, key))
}

func (a HostAdapter) KVList(ctx context.Context, pluginID, prefix string) ([]string, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	keys, err := h.KVList(ctx, prefix)
	return keys, mapHostErr(err)
}

func (a HostAdapter) CatalogSearch(ctx context.Context, pluginID, query string, limit, offset int) (json.RawMessage, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	out, err := h.CatalogSearch(ctx, query, limit, offset)
	return out, mapHostErr(err)
}

func (a HostAdapter) CatalogGet(ctx context.Context, pluginID, id string) (json.RawMessage, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	out, err := h.CatalogGet(ctx, id)
	return out, mapHostErr(err)
}

func (a HostAdapter) ViewerGranted(ctx context.Context, pluginID string) (json.RawMessage, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	out, err := h.ViewerGranted(ctx)
	return out, mapHostErr(err)
}

func (a HostAdapter) ViewerHistory(ctx context.Context, pluginID string, viewerID int64, limit, offset int) (json.RawMessage, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	out, err := h.ViewerHistory(ctx, viewerID, limit, offset)
	return out, mapHostErr(err)
}

func (a HostAdapter) SetWatchState(ctx context.Context, pluginID string, viewerID int64, titleID string, watched bool) error {
	h, err := a.host(pluginID)
	if err != nil {
		return err
	}
	return mapHostErr(h.SetWatchState(ctx, viewerID, titleID, watched))
}

func (a HostAdapter) CollectionsUpsert(ctx context.Context, pluginID string, collection json.RawMessage) error {
	h, err := a.host(pluginID)
	if err != nil {
		return err
	}
	return mapHostErr(h.CollectionsUpsert(ctx, collection))
}

func (a HostAdapter) KindUpsert(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return 0, err
	}
	n, err := h.KindUpsert(ctx, kindKey, items)
	return n, mapHostErr(err)
}

func (a HostAdapter) KindDelete(ctx context.Context, pluginID, kindKey string, externalIDs []string) error {
	h, err := a.host(pluginID)
	if err != nil {
		return err
	}
	return mapHostErr(h.KindDelete(ctx, kindKey, externalIDs))
}

func (a HostAdapter) KindItems(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return core.KindPage{}, err
	}
	page, err := h.KindItems(ctx, kindKey, cursor, limit)
	return page, mapHostErr(err)
}

func (a HostAdapter) ResolveSources(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	cands, err := h.ResolveSources(ctx, q)
	return cands, mapHostErr(err)
}

func (a HostAdapter) CachePut(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (string, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return "", err
	}
	ref, err := h.CachePut(ctx, libraryKey, name, r)
	return ref, mapHostErr(err)
}

func (a HostAdapter) CacheOpen(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	rc, err := h.CacheOpen(ctx, blobRef)
	return rc, mapHostErr(err)
}

func (a HostAdapter) StreamURL(ctx context.Context, pluginID string, viewerID int64, itemRef string) (string, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return "", err
	}
	u, err := h.StreamURL(plugins.WithViewer(ctx, viewerID), itemRef)
	return u, mapHostErr(err)
}

func (a HostAdapter) Settings(ctx context.Context, pluginID string) (map[string]any, error) {
	if a.SettingsWithSecrets != nil {
		if _, err := a.host(pluginID); err != nil {
			return nil, err
		}
		v, err := a.SettingsWithSecrets(ctx, pluginID)
		return v, mapHostErr(err)
	}
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	v, err := h.Settings(ctx)
	return v, mapHostErr(err)
}

func (a HostAdapter) NotifyOwner(ctx context.Context, pluginID, text string) error {
	h, err := a.host(pluginID)
	if err != nil {
		return err
	}
	return mapHostErr(h.NotifyOwner(ctx, text))
}

func (a HostAdapter) Log(ctx context.Context, pluginID, level, msg string, attrs map[string]any) error {
	h, err := a.host(pluginID)
	if err != nil {
		return err
	}
	return mapHostErr(h.Log(ctx, level, msg, attrs))
}

var _ Host = HostAdapter{}

// GrantsLAN adapts a Manager-like grants lookup to Factory.TrustedLAN,
// bounding the lookup so opening a runtime never hangs on storage.
func GrantsLAN(grantsOf func(ctx context.Context, pluginID string) (plugins.Grants, error)) func(string) bool {
	if grantsOf == nil {
		return nil
	}
	return func(pluginID string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g, err := grantsOf(ctx, pluginID)
		return err == nil && g.LAN
	}
}

func (a HostAdapter) CallDataHost(ctx context.Context, pluginID, name string, args json.RawMessage) (json.RawMessage, error) {
	h, err := a.host(pluginID)
	if err != nil {
		return nil, err
	}
	out, err := plugins.CallDataHost(ctx, h, name, args)
	return out, mapHostErr(err)
}
