package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// Backends are the Ether services host calls end in. Any may be nil; the
// matching host call then returns ErrUnavailable. App wiring fills them.
type Backends struct {
	CatalogSearch func(ctx context.Context, query string, limit, offset int) (json.RawMessage, error)
	CatalogGet    func(ctx context.Context, id string) (json.RawMessage, error)
	ViewerHistory func(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error)
	ViewerNames   func(ctx context.Context, ids []int64) (json.RawMessage, error)
	// SetWatchState applies a watch-state write and returns its inverse
	// (the previous state) for the journal.
	SetWatchState func(ctx context.Context, viewerID int64, titleID string, watched bool) (previous bool, err error)
	// CollectionsUpsert creates or updates a collection and returns the
	// previous document (empty when new) for the journal.
	CollectionsUpsert func(ctx context.Context, doc json.RawMessage) (previous json.RawMessage, err error)
	// CollectionsDelete removes a collection by the id in its document;
	// used by undo for a create.
	CollectionsDelete func(ctx context.Context, doc json.RawMessage) error
	// Notice posts a console notice ("console notice" in SPEC §5.2).
	Notice func(text string)
}

// Deps are the plugin host's explicit dependencies.
type Deps struct {
	ImportStylePack func(context.Context, string, map[string][]byte) error
	OnAirContent    *onair.PluginContentCache
	DB              *sql.DB
	Sealer          config.FieldSealer
	Registry        *extend.Registry
	Tools           *tools.Catalogue
	Notifications   *notify.Service
	Logger          *slog.Logger
	// IsLeader reports cluster leadership; nil means always leader.
	IsLeader func() bool
	// Bus broadcasts invalidate:plugins; nil disables it.
	Bus clusterbus.Bus
	// FetchClient fetches manifests and artifacts; nil builds one from
	// safefetch and egress CapPlugins. Tests inject an httptest client.
	FetchClient *http.Client
	// CheckPackage runs the conformance kit during install previews.
	CheckPackage func(context.Context, []byte, map[string][]byte) error
	Backends     Backends
	Now          func() time.Time
}

// Manager is the plugin host. It is safe for concurrent use.
type Manager struct {
	// relay holds the add-on settings pages open in the console.
	relay       *addonRelay
	relayOnce   sync.Once
	store       *storage
	deps        Deps
	log         *slog.Logger
	now         func() time.Time
	client      *http.Client
	mu          sync.Mutex
	catalogue   Catalogue
	fatories    map[string]RuntimeFactory
	runtimes    map[string]Runtime
	removeHooks []func(context.Context, string)
	syncHooks   []func(ctx context.Context, r Record)
	eventSubs   map[string]map[string]Record
	feedLogs    sync.Map // feed key -> *faillog.Tracker
	notifyLocks sync.Map // plugin id -> *sync.Mutex
	// schedule bookkeeping: "plugin|call" -> next run
	nextRun map[string]time.Time
	// rate-limit bookkeeping for host log and notify
	notifyTimes map[string][]time.Time
	logTimes    map[string][]time.Time
	// healthMu serializes health read-modify-writes so concurrent calls
	// (resolves on the hot path, scheduled jobs) never lose an update.
	healthMu sync.Mutex
	// failing caches which plugins last recorded an error, so a successful
	// call clears it without a health read on every call (nil = not loaded).
	failing map[string]bool
	// rankingSkipping caches whether a plugin's ranking step is currently
	// being left out (Health.RankingSkipped > 0), so a resolve whose
	// plugins all ranked in time writes nothing (nil = not loaded).
	rankingSkipping map[string]bool
	pickMu          sync.Mutex
	picks           map[string]subtitlePick
	// warming holds plugin ids whose runtime is loading in the background
	// (guarded by mu), so the hot path never waits for a load.
	warming map[string]bool
	// jobs holds "plugin|path" for action-started jobs still running.
	jobs sync.Map
	// shadow mirrors each plugin's shadow flag (guarded by mu) so the play
	// path can skip shadow plugins without a database read.
	shadow map[string]bool
}

func (m *Manager) notificationLock(id string) *sync.Mutex {
	lock, _ := m.notifyLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// New builds the host and re-registers descriptors for enabled plugins.
func New(deps Deps) (*Manager, error) {
	if deps.OnAirContent == nil {
		deps.OnAirContent = onair.NewPluginContentCache()
	}
	if deps.DB == nil {
		return nil, fmt.Errorf("plugins: a database is required")
	}
	now := deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	client := deps.FetchClient
	if client == nil {
		client = (&safefetch.Guard{}).Client(egress.Client(egress.CapPlugins, 30*time.Second))
	}
	m := &Manager{
		store: &storage{db: deps.DB, sealer: deps.Sealer, now: now}, deps: deps,
		log: log, now: now, client: client,
		fatories: map[string]RuntimeFactory{}, runtimes: map[string]Runtime{}, warming: map[string]bool{}, shadow: map[string]bool{},
		eventSubs: map[string]map[string]Record{},
		nextRun:   map[string]time.Time{}, notifyTimes: map[string][]time.Time{}, logTimes: map[string][]time.Time{},
	}
	return m, nil
}

// Start re-registers descriptors and subscribes to cluster invalidations.
// Run starts the health, scheduler and outbox loops.
func (m *Manager) Start(ctx context.Context) error {
	recs, err := m.store.list(ctx)
	if err != nil {
		return err
	}
	for _, r := range recs {
		m.syncDescriptors(ctx, r)
	}
	return nil
}

// OnSync registers a callback notified whenever a plugin's enabled state or
// manifest changes (install, update, rollback, enable, disable, auto-disable,
// or remove). Any already-installed plugins are synced immediately.
func (m *Manager) OnSync(fn func(ctx context.Context, r Record)) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.syncHooks = append(m.syncHooks, fn)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if recs, err := m.store.list(ctx); err == nil {
		for _, r := range recs {
			fn(ctx, r)
		}
	}
}

// RecordFailure reports an adapter-observed failure (such as an HTTP 5xx or
// malformed payload) into the plugin's health record and failure policy.
func (m *Manager) RecordFailure(ctx context.Context, pluginID, reason string) {
	m.recordFailure(ctx, pluginID, reason, false)
}

// RegisterRuntime adds a runtime factory for a runtime kind ("hosted" or
// "remote"). WS8 calls it at wiring time. Safe to call after Start.
func (m *Manager) RegisterRuntime(kind string, f RuntimeFactory) {
	m.mu.Lock()
	m.fatories[kind] = f
	m.mu.Unlock()
}

// SetCatalogue attaches the signed plugin catalogue (WS12).
func (m *Manager) SetCatalogue(c Catalogue) {
	m.mu.Lock()
	m.catalogue = c
	m.mu.Unlock()
}

func (m *Manager) getCatalogue() Catalogue {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.catalogue
}

func (m *Manager) isLeader() bool {
	if m.deps.IsLeader == nil {
		return true
	}
	return m.deps.IsLeader()
}

func (m *Manager) broadcast(ctx context.Context) {
	if m.deps.Bus != nil {
		_ = m.deps.Bus.Publish(ctx, "invalidate:plugins", []byte("plugins"))
	}
}

// SubscribeInvalidations re-syncs descriptors when another node changes a
// plugin. Returns an unsubscribe function (nil bus: no-op).
func (m *Manager) SubscribeInvalidations() func() {
	if m.deps.Bus == nil {
		return func() {}
	}
	return m.deps.Bus.Subscribe("invalidate:plugins", func([]byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		recs, err := m.store.list(ctx)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, r := range recs {
			seen[r.ID] = true
			m.syncDescriptors(ctx, r)
			m.closeRuntime(r.ID)
		}
		m.mu.Lock()
		for _, subscribers := range m.eventSubs {
			for id := range subscribers {
				if !seen[id] {
					delete(subscribers, id)
				}
			}
		}
		m.mu.Unlock()
		if m.deps.Notifications != nil {
			for _, kind := range m.deps.Notifications.Kinds().List() {
				if kind.Owner != "" && kind.Owner != extend.Builtin && !seen[kind.Owner] {
					m.deps.Notifications.Kinds().RemovePlugin(kind.ID, kind.Owner)
				}
			}
		}
	})
}

// List returns every installed plugin.
func (m *Manager) List(ctx context.Context) ([]Record, error) { return m.store.list(ctx) }

// Get returns one installed plugin.
func (m *Manager) Get(ctx context.Context, id string) (Record, error) {
	return m.store.record(ctx, id)
}

// HealthOf returns one plugin's health record.
func (m *Manager) HealthOf(ctx context.Context, id string) (Health, error) {
	if _, err := m.store.record(ctx, id); err != nil {
		return Health{}, err
	}
	return m.store.health(ctx, id)
}

// GrantsOf returns one plugin's grants.
func (m *Manager) GrantsOf(ctx context.Context, id string) (Grants, error) {
	if _, err := m.store.record(ctx, id); err != nil {
		return Grants{}, err
	}
	return m.store.grants(ctx, id)
}

// SetGrants replaces one plugin's grants. Host calls re-check grants on
// every call, so a revoke applies mid-call.
func (m *Manager) SetGrants(ctx context.Context, id string, g Grants) error {
	if _, err := m.store.record(ctx, id); err != nil {
		return err
	}
	if err := m.store.putGrants(ctx, id, g); err != nil {
		return err
	}
	m.broadcast(ctx)
	return nil
}

// HostFor returns the host-services handle for one plugin (SPEC §5.1.5).
func (m *Manager) HostFor(pluginID string) Host { return &host{m: m, plugin: pluginID} }

// TextResolver resolves a plugin string for a locale, falling back to en.
func (m *Manager) TextResolver() extend.TextResolver {
	return func(plugin, key, locale string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.ResolveText(ctx, plugin, key, locale)
	}
}

// ResolveText looks up key in the plugin's strings for locale, then en.
func (m *Manager) ResolveText(ctx context.Context, plugin, key, locale string) (string, bool) {
	row, err := m.store.settings(ctx, plugin)
	if err != nil {
		return "", false
	}
	for _, loc := range []string{locale, strings.SplitN(locale, "-", 2)[0], "en"} {
		if loc == "" {
			continue
		}
		if s, ok := row.Strings[loc][key]; ok {
			return s, true
		}
	}
	return "", false
}

// --- runtime plumbing ---

func (m *Manager) runtimeFor(ctx context.Context, r Record) (Runtime, error) {
	m.mu.Lock()
	rt, ok := m.runtimes[r.ID]
	f, hasFactory := m.fatories[r.Manifest.RuntimeKind()]
	m.mu.Unlock()
	if ok {
		return rt, nil
	}
	if !hasFactory {
		return nil, ErrNoRuntime
	}
	inst := Installed{
		ID: r.ID, Manifest: r.Manifest, SourceURL: r.SourceURL, Host: m.HostFor(r.ID),
		Files: func(path string) ([]byte, bool) {
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return m.store.file(fctx, r.ID, r.Version, path)
		},
	}
	rt, err := f(inst)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing, ok := m.runtimes[r.ID]; ok {
		m.mu.Unlock()
		_ = rt.Close()
		return existing, nil
	}
	m.runtimes[r.ID] = rt
	m.mu.Unlock()
	m.deliverSettings(ctx, r.ID, rt)
	return rt, nil
}

// deliverSettings sends a newly opened runtime the plugin's stored
// settings (secrets included; the plugin owns them) through
// /ether/configure, so a plugin that keeps its configuration in memory has
// it after a start, restart or update. Failures are logged, not fatal: the
// plugin can still read settings.get per request.
func (m *Manager) deliverSettings(ctx context.Context, pluginID string, rt Runtime) {
	settings, err := m.mergedSettings(ctx, pluginID)
	if err != nil || len(settings) == 0 {
		return
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DeadlineFor("/ether/configure"))
	defer cancel()
	if _, err := rt.Call(cctx, Request{PluginID: pluginID, Method: "POST", Path: "/ether/configure", Body: body}); err != nil {
		m.log.Warn("plugins: deliver settings to new runtime", "plugin", pluginID, "err", err)
	}
}

// PluginSettings returns a plugin's stored settings with its own secrets,
// for delivery to that plugin only (remote sidecars read it from the
// authenticated plugin-host API after a restart or reconnect).
func (m *Manager) PluginSettings(ctx context.Context, pluginID string) (map[string]any, error) {
	return m.mergedSettings(ctx, pluginID)
}

// ErrStreamUnsupported indicates that a runtime cannot relay file bodies.
var ErrStreamUnsupported = errors.New("plugins: runtime does not support streaming")

type streamRuntime interface {
	CallStream(context.Context, Request) (Response, io.ReadCloser, error)
}

type streamBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *streamBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

// CallStream returns a streaming response. The call deadline remains active
// until the body is closed; callers must close it on every path.
func (m *Manager) CallStream(ctx context.Context, pluginID string, req Request) (Response, io.ReadCloser, error) {
	if req.Path == "/ether/subtitles/download" {
		return Response{Status: http.StatusForbidden}, nil, ErrPermissionDenied
	}
	r, err := m.store.record(ctx, pluginID)
	if err != nil {
		return Response{}, nil, err
	}
	if !r.Enabled {
		return Response{}, nil, ErrDisabled
	}
	rt, err := m.runtimeFor(ctx, r)
	if err != nil {
		return Response{}, nil, err
	}
	s, ok := rt.(streamRuntime)
	if !ok {
		return Response{}, nil, ErrStreamUnsupported
	}
	req.PluginID = pluginID
	ctx, cancel := context.WithTimeout(ctx, DeadlineFor(req.Path))
	resp, body, err := s.CallStream(ctx, req)
	if err != nil {
		cancel()
		return Response{}, nil, err
	}
	if body == nil {
		cancel()
		return Response{}, nil, errors.New("plugins: streaming runtime returned no body")
	}
	return resp, &streamBody{ReadCloser: body, cancel: cancel}, nil
}

func (m *Manager) closeRuntime(id string) {
	m.mu.Lock()
	rt := m.runtimes[id]
	addon := m.runtimes[id+addonRuntimeSuffix]
	delete(m.runtimes, id)
	delete(m.runtimes, id+addonRuntimeSuffix)
	m.mu.Unlock()
	for _, r := range []Runtime{rt, addon} {
		if r != nil {
			_ = r.Close()
		}
	}
}

// stremioDeadline is the per-route deadline for a third-party Stremio
// add-on. Their stream searches routinely take seconds (they search
// trackers and debrid caches live); the caller's own deadline (the
// resolver's search budget) still bounds every call.
func stremioDeadline(path string) time.Duration {
	switch {
	case strings.HasPrefix(path, "/stream/"), strings.HasPrefix(path, "/catalog/"), strings.HasPrefix(path, "/manifest.json"):
		return 20 * time.Second
	case strings.HasPrefix(path, "/meta/"), strings.HasPrefix(path, "/subtitles/"):
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

// DeadlineFor returns the per-route deadline from SPEC §5.1.2.
func DeadlineFor(path string) time.Duration {
	switch {
	case strings.HasPrefix(path, "/ether/configure"), strings.HasPrefix(path, "/ether/health"):
		return 5 * time.Second
	case strings.HasPrefix(path, "/ether/test"):
		return 15 * time.Second
	case strings.HasPrefix(path, "/ether/options"), strings.HasPrefix(path, "/ether/view"):
		return 2 * time.Second
	case strings.HasPrefix(path, "/ether/open"), strings.HasPrefix(path, "/stream/"):
		return 3 * time.Second
	case strings.HasPrefix(path, "/ether/tool"):
		return 15 * time.Second
	case strings.HasPrefix(path, "/ether/job"):
		return 5 * time.Minute
	case strings.HasPrefix(path, "/meta/"), strings.HasPrefix(path, "/ether/metadata/"), strings.HasPrefix(path, "/catalog/"), strings.HasPrefix(path, "/ether/event"), strings.HasPrefix(path, "/ether/action"), strings.HasPrefix(path, "/ether/markers"):
		return 10 * time.Second
	case strings.HasPrefix(path, "/subtitles/"), strings.HasPrefix(path, "/ether/subtitles/"):
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}

// Call makes one call to a plugin through its runtime, records it in the
// inspector ring (secrets redacted) and feeds failures into health.
// Adapters (WS9) call plugins only through here.
func (m *Manager) Call(ctx context.Context, pluginID string, req Request) (Response, error) {
	if req.Path == "/ether/subtitles/download" {
		var body struct {
			ID string `json:"id"`
		}
		if req.Method != http.MethodPost || json.Unmarshal(req.Body, &body) != nil || !m.consumeSubtitlePick(req.Headers["X-Ether-Pick"], pluginID, req.Headers["X-Ether-Title"], body.ID) {
			return Response{Status: http.StatusForbidden}, ErrPermissionDenied
		}
	}
	r, err := m.store.record(ctx, pluginID)
	if err != nil {
		return Response{}, err
	}
	if !r.Enabled {
		return Response{}, ErrDisabled
	}
	req.PluginID = pluginID
	start := m.now()
	deadline := DeadlineFor(req.Path)
	if r.Manifest.SpeaksStremio() {
		deadline = stremioDeadline(req.Path)
	}
	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	req.Headers["X-Ether-Deadline"] = start.Add(deadline).UTC().Format(time.RFC3339)
	if req.Locale != "" {
		req.Headers["X-Ether-Locale"] = req.Locale
	}
	rt, err := m.runtimeFor(ctx, r)
	if err != nil {
		message := m.store.redactKnown(ctx, pluginID, err.Error())
		m.recordFailure(ctx, pluginID, "runtime unavailable: "+message, false)
		if message != err.Error() {
			err = errors.New(message)
		}
		return Response{}, err
	}
	resp, err := rt.Call(cctx, req)
	ms := m.now().Sub(start).Milliseconds()
	rec := CallRecord{Plugin: pluginID, Route: req.Path, Method: req.Method, Status: resp.Status, MS: ms,
		Request: m.store.redactKnown(ctx, pluginID, redactJSON(req.Body)), Response: m.store.redactKnown(ctx, pluginID, redactJSON(resp.Body))}
	if req.Path == "/ether/notify" || req.Path == "/ether/configure" || strings.HasPrefix(req.Path, "/subtitles/") || req.Path == "/ether/subtitles/download" {
		rec.Request = ""
		rec.Response = ""
	}
	if err != nil && ctx.Err() != nil {
		// The caller gave up (a search that already had its answer, a
		// viewer who left): not the plugin's failure.
		rec.Error = "cancelled by Ether"
		m.store.addCall(ctx, rec)
		return Response{}, ctx.Err()
	}
	if err != nil {
		if req.Path == "/ether/notify" || req.Path == "/ether/configure" {
			err = errors.New("notification delivery failed")
		}
		rec.Error = m.store.redactKnown(ctx, pluginID, err.Error())
		m.store.addCall(ctx, rec)
		// Crash policy: drop the instance so the next call restarts it.
		m.closeRuntime(pluginID)
		m.recordFailure(ctx, pluginID, rec.Error, true)
		if rec.Error != err.Error() {
			err = errors.New(rec.Error)
		}
		return Response{}, err
	}
	if len(rec.Request) > 64*1024 {
		rec.Request = ""
	}
	if len(rec.Response) > 64*1024 {
		rec.Response = ""
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
		rec.Status = resp.Status
	}
	m.store.addCall(ctx, rec)
	if ms > deadline.Milliseconds()/2 && ms > 1000 {
		m.RecordSlow(ctx, pluginID, 1)
	}
	if resp.Status < 400 {
		m.recoverHealth(ctx, pluginID)
	}
	return resp, nil
}

// recoverHealth clears a recorded error once the plugin answers again.
// Without it an add-on with no runtime health check (Stremio add-ons) kept
// showing a problem forever after one bad response.
func (m *Manager) recoverHealth(ctx context.Context, pluginID string) {
	m.healthMu.Lock()
	if m.failing == nil {
		m.failing = map[string]bool{}
	}
	failing, known := m.failing[pluginID]
	m.healthMu.Unlock()
	if known && !failing {
		return
	}
	if !known {
		h, err := m.store.health(ctx, pluginID)
		if err != nil {
			return
		}
		m.healthMu.Lock()
		m.failing[pluginID] = h.Status == "error"
		m.healthMu.Unlock()
		if h.Status != "error" {
			return
		}
	}
	m.recordOK(ctx, pluginID)
}

// redactJSON masks values under secret-looking keys before anything is
// stored in the inspector ring.
func redactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	v = redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "secret") || strings.Contains(lk, "token") || strings.Contains(lk, "password") || strings.Contains(lk, "api_key") || strings.Contains(lk, "apikey") || lk == "key" || logredact.Sensitive(k) {
				t[k] = "Saved"
				continue
			}
			t[k] = redactValue(val)
		}
	case []any:
		for i, item := range t {
			t[i] = redactValue(item)
		}
	case string:
		// Signed URLs and free text can carry credentials in query
		// parameters or as bare JWTs.
		return logredact.String(t)
	}
	return v
}

// recordFailure applies the SPEC §5.2 failure policy: exponential backoff
// from 1s to 5 minutes, and five failures in ten minutes disables the
// plugin with a console notice.
func (m *Manager) recordFailure(ctx context.Context, pluginID, reason string, restart bool) {
	// A timed-out call still needs its health failure persisted.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	now := m.now()
	cutoff := now.Add(-10 * time.Minute).Unix()
	kept := h.Failures[:0:0]
	for _, t := range h.Failures {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now.Unix())
	h.Failures = kept
	if restart {
		h.Restarts++
	}
	backoff := time.Second << min(len(kept)-1, 8)
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	h.NextRetry = now.Add(backoff)
	h.LastError = reason
	h.Status = "error"
	h.Reason = reason
	if m.failing == nil {
		m.failing = map[string]bool{}
	}
	m.failing[pluginID] = true
	_ = m.store.putHealth(ctx, pluginID, h)
	if len(kept) >= 5 {
		// A Stremio add-on is one of the owner's sources, edited in Studio
		// or not: an outage backs it off and shows on its health, but never
		// switches it off.
		if r, err := m.store.record(ctx, pluginID); err == nil && r.Enabled && !r.Manifest.SpeaksStremio() {
			r.Enabled = false
			_ = m.store.upsert(ctx, r)
			h.Status = "disabled"
			h.Reason = "Disabled after repeated failures. Last error: " + reason
			_ = m.store.putHealth(ctx, pluginID, h)
			m.closeRuntime(pluginID)
			m.syncDescriptors(ctx, r)
			m.broadcast(ctx)
			msg := "Plugin " + r.Name + " was disabled after repeated failures."
			m.log.Warn(msg, "plugin", pluginID, "reason", reason)
			if m.deps.Backends.Notice != nil {
				m.deps.Backends.Notice(msg)
			}
		}
	}
}

// RecordSlow adds n slow calls or slow hot-path candidates to a plugin's
// health and flags it for the owner's attention. It is safe for concurrent
// use; the ranking adapters call it after a resolve.
func (m *Manager) RecordSlow(ctx context.Context, pluginID string, n int) {
	if n <= 0 {
		return
	}
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	h.Slow += int64(n)
	if h.Status == "ok" {
		h.Status = "attention"
		h.Reason = "Recent calls were slow."
	}
	_ = m.store.putHealth(ctx, pluginID, h)
}

// RankingReport is what one resolve saw of a plugin on the hot path.
type RankingReport struct {
	// Slow counts candidates and conditions that went neutral on budget.
	Slow int
	// Skipped counts ranking steps left out because the plugin ran past
	// its time budget; Ranked counts ranking steps that finished in time.
	Skipped, Ranked int
}

// rankingSkippedReason is the health reason while a plugin's ranking step
// is being left out. It says "slow" so the console's headline reads Slow;
// the drawer explains it from Health.RankingSkipped.
const rankingSkippedReason = "Its ranking step was too slow, so releases were ranked without it."

// RecordRanking records a resolve's hot-path outcome for one plugin. A
// ranking step left out for running past its budget flags the plugin for
// the owner until one of its rankings finishes in time again; the
// periodic health check does not clear it, since the plugin's own health
// answer says nothing about its ranking. A resolve where the plugin kept
// to time and was not being skipped writes nothing. Safe for concurrent
// use; the ranking adapters call it when a resolve ends.
func (m *Manager) RecordRanking(ctx context.Context, pluginID string, r RankingReport) {
	if r.Slow <= 0 && r.Skipped <= 0 && r.Ranked <= 0 {
		return
	}
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	if m.rankingSkipping == nil {
		m.rankingSkipping = map[string]bool{}
	}
	if skipping, known := m.rankingSkipping[pluginID]; known && !skipping && r.Slow <= 0 && r.Skipped <= 0 {
		return
	}
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	before := h
	h.Slow += int64(r.Slow)
	switch {
	case r.Skipped > 0:
		h.RankingSkipped += int64(r.Skipped)
		h.RankingSkippedAt = m.now()
		if h.Status == "ok" || h.Status == "attention" {
			h.Status, h.Reason = "attention", rankingSkippedReason
		}
	case r.Ranked > 0 && h.RankingSkipped > 0:
		// It kept to time again: its ranking counts and the flag clears.
		h.RankingSkipped = 0
		if h.Status == "attention" && h.Reason == rankingSkippedReason {
			h.Status, h.Reason = "ok", ""
		}
	case r.Slow > 0 && h.Status == "ok":
		h.Status, h.Reason = "attention", "Recent calls were slow."
	}
	m.rankingSkipping[pluginID] = h.RankingSkipped > 0
	if h.Slow == before.Slow && h.RankingSkipped == before.RankingSkipped && h.Status == before.Status {
		return
	}
	_ = m.store.putHealth(ctx, pluginID, h)
}

// okUnlessSkipping marks a plugin healthy, unless its ranking step is
// being left out: only a ranking that finishes in time clears that.
func okUnlessSkipping(h *Health) {
	h.Status, h.Reason = "ok", ""
	if h.RankingSkipped > 0 {
		h.Status, h.Reason = "attention", rankingSkippedReason
	}
}

func (m *Manager) recordOK(ctx context.Context, pluginID string) {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, pluginID)
	if err != nil {
		return
	}
	okUnlessSkipping(&h)
	h.LastOK = m.now()
	_ = m.store.putHealth(ctx, pluginID, h)
	if m.failing == nil {
		m.failing = map[string]bool{}
	}
	m.failing[pluginID] = false
}

// Revoke disables an installed plugin that was marked revoked in the
// signed catalogue, records the reason in its health, and posts a console
// notice (SPEC WS12, §10). It returns true when an installed plugin was
// newly disabled or updated with the revocation reason.
func (m *Manager) Revoke(ctx context.Context, id, reason string) (bool, error) {
	r, err := m.store.record(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "This plugin was revoked by the catalogue."
	}
	healthReason := "Revoked: " + reason
	h, _ := m.store.health(ctx, id)
	if !r.Enabled && h.Status == "disabled" && h.Reason == healthReason {
		return false, nil
	}
	r.Enabled = false
	if err := m.store.upsert(ctx, r); err != nil {
		return false, err
	}
	h.Status = "disabled"
	h.Reason = healthReason
	h.LastError = reason
	_ = m.store.putHealth(ctx, id, h)
	m.closeRuntime(id)
	m.syncDescriptors(ctx, r)
	m.broadcast(ctx)
	msg := "Plugin " + r.Name + " was disabled because it was revoked: " + reason
	m.log.Warn(msg, "plugin", id, "reason", reason)
	if m.deps.Backends.Notice != nil {
		m.deps.Backends.Notice(msg)
	}
	return true, nil
}

// SetEnabled enables or disables a plugin and refreshes its descriptors.
func (m *Manager) SetEnabled(ctx context.Context, id string, enabled bool) error {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return err
	}
	if enabled {
		if cat := m.getCatalogue(); cat != nil {
			if _, cerr := cat.Resolve(ctx, id, ""); errors.Is(cerr, ErrRevoked) {
				return cerr
			}
		}
	}
	r.Enabled = enabled
	if err := m.store.upsert(ctx, r); err != nil {
		return err
	}
	if enabled {
		h, _ := m.store.health(ctx, id)
		h.Failures = nil
		if h.Status == "disabled" {
			h.Status, h.Reason = "ok", ""
		}
		_ = m.store.putHealth(ctx, id, h)
	} else {
		m.closeRuntime(id)
	}
	m.syncDescriptors(ctx, r)
	m.broadcast(ctx)
	return nil
}

// SetRoles switches an installed add-on's roles on or off: off lists the
// capabilities it should stop playing (its catalogs, streams, subtitles,
// Live TV channels…). Only roles its manifest offers can be named.
func (m *Manager) SetRoles(ctx context.Context, id string, off []Capability) (Record, error) {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return Record{}, err
	}
	offered := map[Capability]bool{}
	for _, c := range r.Manifest.AddonRoles() {
		offered[c] = true
	}
	seen := map[Capability]bool{}
	var clean []Capability
	// Roles the add-on does not offer yet but was set to keep off stay off.
	for _, c := range r.RolesOff {
		if !offered[c] && !seen[c] {
			seen[c] = true
			clean = append(clean, c)
		}
	}
	for _, c := range off {
		if !offered[c] {
			return Record{}, &ValidationError{[]FieldError{{"roles", fmt.Sprintf("%s is not a role this add-on offers", c)}}}
		}
		if !seen[c] {
			seen[c] = true
			clean = append(clean, c)
		}
	}
	if err := m.store.setRolesOff(ctx, id, clean); err != nil {
		return Record{}, err
	}
	saved, err := m.store.record(ctx, id)
	if err != nil {
		return Record{}, err
	}
	m.closeRuntime(id)
	m.syncDescriptors(ctx, saved)
	m.broadcast(ctx)
	return saved, nil
}

// SetShadow turns shadow mode on or off.
func (m *Manager) SetShadow(ctx context.Context, id string, shadow bool) error {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return err
	}
	r.Shadow = shadow
	if err := m.store.upsert(ctx, r); err != nil {
		return err
	}
	m.mu.Lock()
	m.shadow[id] = shadow
	m.mu.Unlock()
	m.broadcast(ctx)
	return nil
}

// Remove uninstalls a plugin: it offers undo first (when undoFirst, every
// live journal inverse is replayed), then purges KV, secrets, grants,
// artifacts, logs and its registry entries.
func (m *Manager) Remove(ctx context.Context, id string, undoFirst bool) error {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return err
	}
	if undoFirst {
		if _, err := m.UndoAll(ctx, id); err != nil && !errors.Is(err, ErrUnavailable) {
			m.log.Warn("plugin remove: undo had problems", "plugin", id, "err", err)
		}
	}
	m.closeRuntime(id)
	r.Enabled = false
	m.syncDescriptors(ctx, r)
	m.indexEventSubscriptions(Record{ID: id})
	if m.deps.Registry != nil {
		m.deps.Registry.RemoveOwner(id)
	}
	if m.deps.Tools != nil {
		_ = m.deps.Tools.ReplaceOwner(id, nil)
	}
	if m.deps.Notifications != nil {
		m.deps.Notifications.Kinds().RemovePlugin(extend.PluginID(id, "notify"), id)
	}
	if err := m.store.purge(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	hooks := append([]func(context.Context, string){}, m.removeHooks...)
	m.mu.Unlock()
	for _, fn := range hooks {
		fn(ctx, id)
	}
	m.broadcast(ctx)
	return nil
}

// OnRemove registers a callback run after a plugin is uninstalled.
func (m *Manager) OnRemove(fn func(ctx context.Context, id string)) {
	m.mu.Lock()
	m.removeHooks = append(m.removeHooks, fn)
	m.mu.Unlock()
}

// startActionJob lets a page action start long work: when the action's
// result names a job ({"job": "/ether/job/<name>"}), the host runs that job
// in the background with the job deadline (5 minutes), once at a time per
// plugin and job, so a scan of a large library is not cut off by the
// 10-second action deadline.
func (m *Manager) startActionJob(pluginID string, body json.RawMessage) {
	var out struct {
		Job string `json:"job"`
	}
	if json.Unmarshal(body, &out) != nil || out.Job == "" {
		return
	}
	path := out.Job
	if !strings.HasPrefix(path, "/ether/job/") {
		path = "/ether/job/" + strings.TrimPrefix(path, "/")
	}
	if strings.Contains(path, "..") || strings.ContainsAny(path, "?#") {
		return
	}
	key := pluginID + "|" + path
	if _, running := m.jobs.LoadOrStore(key, true); running {
		return
	}
	go func() {
		defer m.jobs.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), DeadlineFor(path))
		defer cancel()
		if _, err := m.Call(ctx, pluginID, Request{Method: "POST", Path: path}); err != nil {
			m.log.Warn("plugins: action job", "plugin", pluginID, "job", path, "err", err)
		}
	}()
}
