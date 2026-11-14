package remote

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/httpjson"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Host is the set of Ether services a remote plugin may call back
// into (SPEC §5.1.5). It mirrors plugins.Host method for method for
// the services remote plugins may use; secret.get is hosted-only and
// deliberately absent (remote plugins receive secrets in
// /ether/configure). Every method is expected to re-check the
// plugin's live grants and return ErrPermissionDenied when a grant
// is missing or revoked.
type Host interface {
	KVGet(ctx context.Context, pluginID, key string) ([]byte, bool, error)
	KVPut(ctx context.Context, pluginID, key string, value []byte) error
	KVDelete(ctx context.Context, pluginID, key string) error
	KVList(ctx context.Context, pluginID, prefix string) ([]string, error)

	CatalogSearch(ctx context.Context, pluginID, query string, limit, offset int) (json.RawMessage, error)
	CatalogGet(ctx context.Context, pluginID, id string) (json.RawMessage, error)

	ViewerGranted(ctx context.Context, pluginID string) (json.RawMessage, error)
	ViewerHistory(ctx context.Context, pluginID string, viewerID int64, limit, offset int) (json.RawMessage, error)
	SetWatchState(ctx context.Context, pluginID string, viewerID int64, titleID string, watched bool) error

	CollectionsUpsert(ctx context.Context, pluginID string, collection json.RawMessage) error

	KindUpsert(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (int, error)
	KindDelete(ctx context.Context, pluginID, kindKey string, externalIDs []string) error
	KindItems(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error)
	ResolveSources(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error)
	CachePut(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (string, error)
	CacheOpen(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error)

	// StreamURL mints a scoped stream ticket for one of the plugin's own
	// kind items for viewerID and returns the Ether stream URL (spec §6.5).
	StreamURL(ctx context.Context, pluginID string, viewerID int64, itemRef string) (string, error)

	Settings(ctx context.Context, pluginID string) (map[string]any, error)

	NotifyOwner(ctx context.Context, pluginID, text string) error
	Log(ctx context.Context, pluginID, level, msg string, attrs map[string]any) error
}

// Body caps for host calls. KV values get the largest allowance; the
// host's per-plugin quota still applies on top.
const (
	maxKVValueBytes    = 5 << 20
	maxHostBodyBytes   = 1 << 20
	maxCollectionBytes = 2 << 20
	maxKindUpsertBytes = 8 << 20
	maxCacheBlobBytes  = 256 << 20
	defaultPageLimit   = 50
	maxPageLimit       = 200
)

// HostHandler serves /ether/plugin-host/* for remote plugins. Every
// request is authenticated with a plugin credential; the resolved
// plugin id is the only identity a call can act as, so a plugin can
// never name another plugin in a request body and be believed.
type HostHandler struct {
	host Host
	auth CredentialResolver
	log  *slog.Logger
	mux  *http.ServeMux
}

// HostOptions configures a HostHandler.
type HostOptions struct {
	Host   Host
	Auth   CredentialResolver
	Logger *slog.Logger
}

// NewHostHandler builds the plugin-host API. Mount the result at
// HostPath (or use Mount to register it on a mux).
func NewHostHandler(opts HostOptions) *HostHandler {
	h := &HostHandler{host: opts.Host, auth: opts.Auth, log: opts.Logger}
	if h.log == nil {
		h.log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ether/plugin-host/data/{service}", h.dataCall)
	mux.HandleFunc("GET /ether/plugin-host/kv", h.kvList)
	mux.HandleFunc("GET /ether/plugin-host/kv/{key...}", h.kvGet)
	mux.HandleFunc("PUT /ether/plugin-host/kv/{key...}", h.kvPut)
	mux.HandleFunc("DELETE /ether/plugin-host/kv/{key...}", h.kvDelete)
	mux.HandleFunc("GET /ether/plugin-host/catalog/search", h.catalogSearch)
	mux.HandleFunc("GET /ether/plugin-host/catalog/{id...}", h.catalogGet)
	mux.HandleFunc("GET /ether/plugin-host/viewers/{viewerId}/history", h.viewerHistory)
	mux.HandleFunc("GET /ether/plugin-host/viewers/granted", h.viewerGranted)
	mux.HandleFunc("POST /ether/plugin-host/viewers/watch-state", h.setWatchState)
	mux.HandleFunc("POST /ether/plugin-host/collections/upsert", h.collectionsUpsert)
	mux.HandleFunc("GET /ether/plugin-host/kinds/{kind}/items", h.kindItems)
	mux.HandleFunc("POST /ether/plugin-host/kinds/{kind}/upsert", h.kindUpsert)
	mux.HandleFunc("POST /ether/plugin-host/kinds/{kind}/delete", h.kindDelete)
	mux.HandleFunc("POST /ether/plugin-host/sources/resolve", h.resolveSources)
	mux.HandleFunc("PUT /ether/plugin-host/cache/{libraryKey}/{name...}", h.cachePut)
	mux.HandleFunc("POST /ether/plugin-host/cache/{libraryKey}", h.cachePut)
	mux.HandleFunc("GET /ether/plugin-host/cache/{blobRef...}", h.cacheOpen)
	mux.HandleFunc("POST /ether/plugin-host/frontend/stream-url", h.frontendStreamURL)
	mux.HandleFunc("GET /ether/plugin-host/settings", h.settings)
	mux.HandleFunc("POST /ether/plugin-host/notify", h.notify)
	mux.HandleFunc("POST /ether/plugin-host/log", h.logLine)
	h.mux = mux
	return h
}

// Mount registers the handler's routes on mux.
func (h *HostHandler) Mount(mux *http.ServeMux) {
	mux.Handle(HostPath, h)
}

func (h *HostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || h.host == nil {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "Plugin host API is not configured")
		return
	}
	pluginID, err := h.auth.ResolvePluginCredential(r.Context(), credentialFromRequest(r))
	if err != nil || pluginID == "" {
		writeHostError(w, http.StatusUnauthorized, "unauthorized", "Plugin credential rejected")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), pluginIDContextKey{}, pluginID))
	h.mux.ServeHTTP(w, r)
}

type pluginIDContextKey struct{}

func pluginIDOf(r *http.Request) string {
	id, _ := r.Context().Value(pluginIDContextKey{}).(string)
	return id
}

// --- KV ---

func (h *HostHandler) kvGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	value, ok, err := h.host.KVGet(r.Context(), pluginIDOf(r), key)
	if !h.finishErr(w, err) {
		return
	}
	if !ok {
		writeHostError(w, http.StatusNotFound, "not_found", "No value for this key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": string(value)})
}

func (h *HostHandler) kvPut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var body struct {
		Value *string `json:"value"`
	}
	// Accept either {"value": "..."} or a raw text body: the SDKs send
	// JSON, hand-rolled sidecars often send the bare value.
	raw, err := readBody(w, r, maxKVValueBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	value := raw
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &body); err != nil || body.Value == nil {
			writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"value": "..."}`)
			return
		}
		value = []byte(*body.Value)
	}
	if !h.finishErr(w, h.host.KVPut(r.Context(), pluginIDOf(r), key, value)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) kvDelete(w http.ResponseWriter, r *http.Request) {
	if !h.finishErr(w, h.host.KVDelete(r.Context(), pluginIDOf(r), r.PathValue("key"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) kvList(w http.ResponseWriter, r *http.Request) {
	keys, err := h.host.KVList(r.Context(), pluginIDOf(r), r.URL.Query().Get("prefix"))
	if !h.finishErr(w, err) {
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// --- Catalog ---

func (h *HostHandler) catalogSearch(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	out, err := h.host.CatalogSearch(r.Context(), pluginIDOf(r), r.URL.Query().Get("q"), limit, offset)
	h.writeRaw(w, out, err)
}

func (h *HostHandler) catalogGet(w http.ResponseWriter, r *http.Request) {
	out, err := h.host.CatalogGet(r.Context(), pluginIDOf(r), r.PathValue("id"))
	h.writeRaw(w, out, err)
}

// --- Viewers ---

func (h *HostHandler) viewerGranted(w http.ResponseWriter, r *http.Request) {
	out, err := h.host.ViewerGranted(r.Context(), pluginIDOf(r))
	h.writeRaw(w, out, err)
}

func (h *HostHandler) viewerHistory(w http.ResponseWriter, r *http.Request) {
	viewerID, err := strconv.ParseInt(r.PathValue("viewerId"), 10, 64)
	if err != nil || viewerID <= 0 {
		writeHostError(w, http.StatusBadRequest, "invalid", "viewerId must be a positive number")
		return
	}
	limit, offset := pageParams(r)
	out, err := h.host.ViewerHistory(r.Context(), pluginIDOf(r), viewerID, limit, offset)
	h.writeRaw(w, out, err)
}

func (h *HostHandler) setWatchState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ViewerID int64  `json:"viewerId"`
		TitleID  string `json:"titleId"`
		Watched  bool   `json:"watched"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if body.ViewerID <= 0 || body.TitleID == "" {
		writeHostError(w, http.StatusBadRequest, "invalid", "viewerId and titleId are required")
		return
	}
	if !h.finishErr(w, h.host.SetWatchState(r.Context(), pluginIDOf(r), body.ViewerID, body.TitleID, body.Watched)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Collections, kinds, sources, cache, notify, log ---

func (h *HostHandler) collectionsUpsert(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxCollectionBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	if !json.Valid(raw) {
		writeHostError(w, http.StatusBadRequest, "invalid", "Body must be a collection document as JSON")
		return
	}
	if !h.finishErr(w, h.host.CollectionsUpsert(r.Context(), pluginIDOf(r), raw)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) kindItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := defaultPageLimit
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	page, err := h.host.KindItems(r.Context(), pluginIDOf(r), r.PathValue("kind"), q.Get("cursor"), limit)
	if !h.finishErr(w, err) {
		return
	}
	if page.Items == nil {
		page.Items = []core.KindItem{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *HostHandler) kindUpsert(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxKindUpsertBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	var items []core.KindItem
	var envelope struct {
		Items []core.KindItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Items != nil {
		items = envelope.Items
	} else if err := json.Unmarshal(raw, &items); err != nil {
		writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"items": [...]} or a JSON array of KindItem`)
		return
	}
	accepted, err := h.host.KindUpsert(r.Context(), pluginIDOf(r), r.PathValue("kind"), items)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": accepted})
}

func (h *HostHandler) kindDelete(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(w, r, maxHostBodyBytes)
	if err != nil {
		h.finishErr(w, err)
		return
	}
	var ids []string
	var envelope struct {
		IDs         []string `json:"ids"`
		ExternalIDs []string `json:"externalIds"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && (envelope.IDs != nil || envelope.ExternalIDs != nil) {
		ids = envelope.IDs
		if len(ids) == 0 {
			ids = envelope.ExternalIDs
		}
	} else if err := json.Unmarshal(raw, &ids); err != nil {
		writeHostError(w, http.StatusBadRequest, "invalid", `Body must be {"ids": [...]} or a JSON array of item ids`)
		return
	}
	if !h.finishErr(w, h.host.KindDelete(r.Context(), pluginIDOf(r), r.PathValue("kind"), ids)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) resolveSources(w http.ResponseWriter, r *http.Request) {
	var q core.SourceQuery
	if !h.decode(w, r, maxHostBodyBytes, &q) {
		return
	}
	cands, err := h.host.ResolveSources(r.Context(), pluginIDOf(r), q)
	if !h.finishErr(w, err) {
		return
	}
	if cands == nil {
		cands = []core.SourceCandidate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": cands})
}

func (h *HostHandler) cachePut(w http.ResponseWriter, r *http.Request) {
	libraryKey := r.PathValue("libraryKey")
	name := r.PathValue("name")
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	body := io.LimitReader(r.Body, maxCacheBlobBytes+1)
	ref, err := h.host.CachePut(r.Context(), pluginIDOf(r), libraryKey, name, body)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "blob": ref})
}

func (h *HostHandler) cacheOpen(w http.ResponseWriter, r *http.Request) {
	rc, err := h.host.CacheOpen(r.Context(), pluginIDOf(r), r.PathValue("blobRef"))
	if !h.finishErr(w, err) {
		return
	}
	defer rc.Close()
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, rc)
}

// frontendStreamURL mints a scoped stream ticket for one of the plugin's
// own kind items and returns the Ether stream URL (spec §6.5). The viewer
// id is the one the adapter proxied the request for.
func (h *HostHandler) frontendStreamURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ViewerID int64  `json:"viewerId"`
		ItemRef  string `json:"itemRef"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	u, err := h.host.StreamURL(r.Context(), pluginIDOf(r), body.ViewerID, body.ItemRef)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": u})
}

func (h *HostHandler) settings(w http.ResponseWriter, r *http.Request) {
	v, err := h.host.Settings(r.Context(), pluginIDOf(r))
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": v})
}

func (h *HostHandler) notify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeHostError(w, http.StatusBadRequest, "invalid", "text is required")
		return
	}
	if !h.finishErr(w, h.host.NotifyOwner(r.Context(), pluginIDOf(r), body.Text)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *HostHandler) logLine(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Attrs   map[string]any `json:"attrs"`
	}
	if !h.decode(w, r, maxHostBodyBytes, &body) {
		return
	}
	if body.Level == "" {
		body.Level = "info"
	}
	if !h.finishErr(w, h.host.Log(r.Context(), pluginIDOf(r), body.Level, body.Message, body.Attrs)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- helpers ---

func (h *HostHandler) writeRaw(w http.ResponseWriter, out json.RawMessage, err error) {
	if !h.finishErr(w, err) {
		return
	}
	if len(out) == 0 {
		out = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (h *HostHandler) decode(w http.ResponseWriter, r *http.Request, max int64, into any) bool {
	raw, err := readBody(w, r, max)
	if err != nil {
		h.finishErr(w, err)
		return false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		writeHostError(w, http.StatusBadRequest, "invalid", "Body is not valid JSON")
		return false
	}
	return true
}

// finishErr maps a host error onto the EPP error shape and reports
// whether the handler may continue. Host errors are mapped, never
// echoed raw: a storage error must not leak SQL or paths to a
// plugin.
func (h *HostHandler) finishErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, ErrPermissionDenied):
		writeHostError(w, http.StatusForbidden, "permission_denied", "This plugin does not have the grant this call needs")
	case errors.Is(err, ErrNotFound):
		writeHostError(w, http.StatusNotFound, "not_found", "Not found")
	case errors.Is(err, ErrTooLarge):
		writeHostError(w, http.StatusRequestEntityTooLarge, "too_large", "Request body is too large")
	case errors.Is(err, ErrInvalid):
		writeHostError(w, http.StatusBadRequest, "invalid", cleanMessage(err))
	case errors.Is(err, ErrUnavailable):
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "This host service is not available")
	case errors.Is(err, ErrUnauthorized):
		writeHostError(w, http.StatusUnauthorized, "unauthorized", "Plugin credential rejected")
	case errors.Is(err, context.DeadlineExceeded):
		writeHostError(w, http.StatusGatewayTimeout, "deadline", "The call ran past its deadline")
	default:
		h.log.Warn("plugin host call failed", "err", err)
		writeHostError(w, http.StatusInternalServerError, "error", "The host call failed")
	}
	return false
}

// cleanMessage strips the sentinel prefix from an ErrInvalid so the
// plugin sees the specific reason ("path escapes...") only.
func cleanMessage(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "remote: invalid request: ")
	msg = strings.TrimPrefix(msg, "remote: invalid request")
	if msg == "" {
		return "The request is not valid"
	}
	return msg
}

// pageParams reads limit/offset, or page/pageSize, clamped.
func pageParams(r *http.Request) (limit, offset int) {
	q := r.URL.Query()
	limit = defaultPageLimit
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if v, err := paging.ParseOffset(q.Get("offset"), limit); err == nil && v >= 0 {
		offset = v
	}
	if q.Get("page") != "" {
		pageSize := limit
		if v, err := strconv.Atoi(q.Get("pageSize")); err == nil && v > 0 {
			pageSize = v
		}
		if v, err := strconv.Atoi(q.Get("page")); err == nil && v >= 1 {
			limit = pageSize
			offset = paging.PageOffset(v, pageSize)
		}
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	return limit, offset
}

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, ErrTooLarge
	}
	return raw, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpjson.Write(w, status, v)
}

// writeHostError emits the EPP error shape (SPEC §5.1.2).
func writeHostError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// dataCall exposes only bounded blob/archive services, authenticated like all
// other plugin-host calls. HostAdapter performs the live grant checks.
func (h *HostHandler) dataCall(w http.ResponseWriter, r *http.Request) {
	var args json.RawMessage
	if !h.decode(w, r, maxHostBodyBytes, &args) {
		return
	}
	service, ok := h.host.(interface {
		CallDataHost(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	})
	if !ok {
		writeHostError(w, http.StatusServiceUnavailable, "unavailable", "Data services unavailable")
		return
	}
	out, err := service.CallDataHost(r.Context(), pluginIDOf(r), r.PathValue("service"), args)
	if !h.finishErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
