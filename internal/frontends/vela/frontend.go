// Package vela presents the Vela v0 protocol through neutral host services.
package vela

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/webclient"
)

type object = map[string]any

type Frontend struct {
	AssetsDir string
	DB        *sql.DB
	ReplayKey []byte
}

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "vela", Name: "Vela", Version: "0", SettingsSchema: frontend.SettingsSchema{SignInHelp: true, Sections: []frontend.SettingsSection{{Key: "find", Fields: []frontend.SettingsField{{Key: "serverName", Kind: "serverName"}}}}}}
}
func (f Frontend) Mount(ctx context.Context, host frontend.Host) (frontend.Instance, error) {
	if host.Identity == nil || host.Experience == nil || host.Sessions == nil {
		return nil, errors.New("vela requires identity, experience and sessions")
	}
	ctx, cancel := context.WithCancel(ctx)
	i := &instance{host: host, ctx: ctx, cancel: cancel, mux: http.NewServeMux(), sessions: map[string]*playSession{}, replays: map[string]*replay{}, assets: f.AssetsDir}
	seal, err := encryption(f.ReplayKey)
	if err != nil {
		cancel()
		return nil, err
	}
	i.rowMeta = map[string]frontend.RowView{}
	i.db = f.DB
	i.seal = seal
	i.routes()
	return i, nil
}

type replay struct {
	digest  string
	done    chan struct{}
	status  int
	header  http.Header
	body    []byte
	expires time.Time
}
type instance struct {
	db      *sql.DB
	seal    cipher.AEAD
	rowMu   sync.Mutex
	rowMeta map[string]frontend.RowView

	host     frontend.Host
	ctx      context.Context
	cancel   context.CancelFunc
	mux      *http.ServeMux
	assets   string
	mu       sync.Mutex
	sessions map[string]*playSession
	replayMu sync.Mutex
	replays  map[string]*replay
}

func (i *instance) Handler() http.Handler { return http.HandlerFunc(i.serve) }
func (*instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/vela"}}
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (i *instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "0", Conformance: "unverified", WebPlayerReady: webclient.VelaReady(i.assets)}
}
func (i *instance) Close(context.Context) error { i.cancel(); return nil }
func hash(s string) string                      { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, class, code string) {
	send(w, status, object{"error": object{"class": class, "code": code, "message": strings.ReplaceAll(code, "_", " "), "retry": object{"allowed": false}}})
}
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, frontend.ErrInteractionReceipt):
		problem(w, 400, "request", "invalid_receipt")
	case errors.Is(err, frontend.ErrRowViewStale):
		problem(w, 409, "conflict", "cursor_expired")
	case errors.Is(err, frontend.ErrTranscodeCapacity), errors.Is(err, frontend.ErrStreamLimit), errors.Is(err, frontend.ErrSessionBusy):
		var delay time.Duration
		var busy *frontend.SessionBusyError
		var encoder *frontend.TranscodeCapacityError
		if errors.As(err, &busy) {
			delay = busy.RetryAfter
		}
		if errors.As(err, &encoder) {
			delay = encoder.RetryAfter
		}
		if delay > 0 && delay <= 5*time.Minute {
			seconds := min(300, int(delay/time.Second))
			if delay%time.Second != 0 && seconds < 300 {
				seconds++
			}
			seconds = max(seconds, 1)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			send(w, 503, object{"error": object{"class": "capacity", "code": "playback_capacity", "message": "playback capacity", "retry": object{"allowed": true, "afterSeconds": seconds, "maxAttempts": 3}}})
		} else {
			problem(w, 503, "capacity", "playback_capacity")
		}
	case errors.Is(err, frontend.ErrNoSources), errors.Is(err, frontend.ErrSourceUnavailable), errors.Is(err, frontend.ErrSourceGone):
		problem(w, 502, "source", "source_unavailable")
	case errors.Is(err, frontend.ErrNotFound):
		problem(w, 404, "not-found", "not_found")
	case errors.Is(err, frontend.ErrUnauthorized):
		problem(w, 401, "auth", "unauthorized")
	case errors.Is(err, frontend.ErrRateLimited):
		problem(w, 503, "capacity", "rate_limited")
	default:
		problem(w, 503, "transient", "service_unavailable")
	}
}
func cache(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fail(w, err)
		return
	}
	tag := `"` + hash(string(b)+r.Header.Get("Authorization")+r.Header.Get("X-Vela-Device-Id")+r.URL.RequestURI()) + `"`
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", tag)
	w.Header().Set("Vary", "Authorization, X-Vela-Device-Id")
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimSpace(candidate) == tag || strings.TrimSpace(candidate) == "*" {
			w.WriteHeader(304)
			return
		}
	}
	_, _ = w.Write(b)
}
func (i *instance) identityContext(r *http.Request) context.Context {
	ctx := r.Context()
	if i.host.ClientIP != nil {
		if ip, ok := i.host.ClientIP(r); ok {
			ctx = frontend.WithClientIP(ctx, ip)
		}
	}
	return ctx
}

type device struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	AppVersion string `json:"appVersion"`
}

func (d device) neutral() frontend.Device {
	return frontend.Device{ID: d.ID, DeviceName: d.Name, Platform: d.Platform, Version: d.AppVersion, Client: "Vela", Product: "Vela"}
}
func requestDevice(r *http.Request) frontend.Device {
	return device{ID: r.Header.Get("X-Vela-Device-Id"), Name: r.Header.Get("X-Vela-Device-Name"), Platform: r.Header.Get("X-Vela-Platform"), AppVersion: r.Header.Get("X-Vela-Version")}.neutral()
}
func (i *instance) viewer(r *http.Request) (*frontend.Viewer, string, error) {
	token, ok := authhdr.Bearer(r.Header.Get("Authorization"))
	if !ok {
		return nil, "", frontend.ErrUnauthorized
	}
	resolver, ok := i.host.Identity.(frontend.DeviceIdentity)
	if !ok || requestDevice(r).ID == "" {
		return nil, "", frontend.ErrUnauthorized
	}
	v, _, err := resolver.ResolveDevice(r.Context(), token, requestDevice(r))
	if err != nil || v == nil {
		return nil, "", frontend.ErrUnauthorized
	}
	return v, token, nil
}
func (i *instance) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !strings.HasPrefix(r.URL.Path, "/vela/v0/") {
		webclient.ServeVela(w, r, i.assets)
		return
	}
	r = r.WithContext(i.identityContext(r))
	if r.Method != "POST" {
		i.mux.ServeHTTP(w, r)
		return
	}
	// A bounded read, followed by per-key serialization, makes refresh rotation
	// and playback admission safe to retry. No worker or allocation exists at idle.
	var raw json.RawMessage
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw) != nil {
		problem(w, 400, "request", "invalid_json")
		return
	}
	var envelope struct {
		IdempotencyKey string `json:"idempotencyKey"`
		Device         device `json:"device"`
		Client         struct {
			Device device `json:"device"`
		} `json:"client"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.IdempotencyKey == "" || len(envelope.IdempotencyKey) > 256 {
		problem(w, 400, "request", "idempotency_key_required")
		return
	}
	public := strings.HasPrefix(r.URL.Path, "/vela/v0/auth/") && !strings.HasSuffix(r.URL.Path, "/approve") && !strings.HasSuffix(r.URL.Path, "/revoke")
	public = public || r.URL.Path == "/vela/v0/negotiate"
	scope := ""
	if !public {
		_, token, err := i.viewer(r)
		if err == nil {
			scope, err = i.grantScope(r.Context(), token, requestDevice(r))
		}
		if err != nil {
			fail(w, err)
			return
		}
	} else {
		d := envelope.Device
		if d.ID == "" {
			d = envelope.Client.Device
		}
		if d.ID == "" {
			problem(w, 400, "request", "device_required")
			return
		}
		scope = d.ID
	}
	key := hash(scope + "\x00" + r.Method + "\x00" + r.URL.Path + "\x00" + envelope.IdempotencyKey)
	digest := hash(string(raw))
	i.replayMu.Lock()
	if old := i.replays[key]; old != nil {
		i.replayMu.Unlock()
		if old.digest != digest {
			problem(w, 409, "conflict", "idempotency_conflict")
			return
		}
		select {
		case <-old.done:
		case <-r.Context().Done():
			return
		case <-i.ctx.Done():
			return
		}
		for k, v := range old.header {
			w.Header()[k] = v
		}
		w.WriteHeader(old.status)
		_, _ = w.Write(old.body)
		return
	}
	if len(i.replays) >= 10000 {
		i.replayMu.Unlock()
		problem(w, 503, "capacity", "idempotency_capacity")
		return
	}
	entry := &replay{digest: digest, done: make(chan struct{}), expires: time.Now().Add(24 * time.Hour)}
	i.replays[key] = entry
	i.replayMu.Unlock()
	r.Body = http.NoBody
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, raw))
	persisted, conflict, claimErr := i.claim(r.Context(), key, digest)
	out := httptest.NewRecorder()
	if claimErr != nil {
		if errors.Is(claimErr, errPending) {
			problem(out, 409, "conflict", "mutation_outcome_unknown")
		} else {
			fail(out, claimErr)
		}
	} else if conflict {
		problem(out, 409, "conflict", "idempotency_conflict")
	} else if persisted != nil {
		for k, v := range persisted.Header {
			out.Header()[k] = v
		}
		out.WriteHeader(persisted.Status)
		_, _ = out.Write(persisted.Body)
	} else {
		i.mux.ServeHTTP(out, r)
		if err := i.save(r.Context(), key, savedResponse{Status: out.Code, Header: out.Header(), Body: out.Body.Bytes()}); err != nil {
			out = httptest.NewRecorder()
			problem(out, 503, "transient", "mutation_outcome_unknown")
		}
	}

	entry.status = out.Code
	entry.header = out.Header().Clone()
	entry.body = append([]byte(nil), out.Body.Bytes()...)
	close(entry.done)
	i.replayMu.Lock()
	delete(i.replays, key)
	i.replayMu.Unlock()
	for k, v := range entry.header {
		w.Header()[k] = v
	}
	w.WriteHeader(entry.status)
	_, _ = w.Write(entry.body)
}

type bodyKey struct{}

func body(r *http.Request, v any) error {
	b, _ := r.Context().Value(bodyKey{}).(json.RawMessage)
	return json.Unmarshal(b, v)
}
func (i *instance) route(method, path string, h http.HandlerFunc) {
	i.mux.HandleFunc(method+" /vela/v0"+path, h)
}
func (i *instance) routes() {
	i.route("GET", "/capabilities", func(w http.ResponseWriter, r *http.Request) { cache(w, r, i.capabilities()) })
	i.route("POST", "/negotiate", i.negotiate)
	for _, p := range []string{"sign-in", "refresh", "revoke", "web-code/redeem", "device", "device/poll", "device/approve", "quick-connect", "quick-connect/poll", "quick-connect/approve"} {
		i.route("POST", "/auth/"+p, i.auth)
	}
	i.route("GET", "/bootstrap", i.bootstrap)
	i.route("GET", "/rows", i.rows)
	i.route("GET", "/rows/{id}", i.rows)
	i.route("GET", "/titles/{id}", i.detail)
	i.route("GET", "/titles/{id}/children", i.children)
	i.route("GET", "/titles/{id}/extras", i.detail)
	i.route("POST", "/search", i.search)
	i.route("POST", "/search/ai", i.unavailable)
	i.route("GET", "/search/ai/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := i.viewer(r); err != nil {
			fail(w, err)
			return
		}
		i.unavailable(w, r)
	})
	i.route("POST", "/search/ai/{id}/cancel", i.unavailable)
	i.route("GET", "/images/{secret}", func(w http.ResponseWriter, r *http.Request) {
		if i.host.ImageGrants == nil {
			i.unavailable(w, r)
			return
		}
		i.host.ImageGrants.Serve(w, r, r.PathValue("secret"))
	})
	i.route("POST", "/interactions", i.interactions)
	i.route("POST", "/playback", i.playback)
	i.route("GET", "/sessions/{id}", i.snapshot)
	i.route("GET", "/sessions/{id}/events", i.events)
	i.route("POST", "/sessions/{id}/commands", i.command)
	i.route("POST", "/sessions/{id}/observations", i.observe)
	i.route("GET", "/live/channels", i.channels)
	i.route("GET", "/live/guide", i.guide)
	i.route("POST", "/titles/{id}/watch", i.watch)
	i.mux.HandleFunc("/vela/v0/", func(w http.ResponseWriter, r *http.Request) { problem(w, 404, "not-found", "not_found") })
}
func (*instance) unavailable(w http.ResponseWriter, r *http.Request) {
	problem(w, 403, "policy", "feature_unavailable")
}
func (i *instance) capabilities() object {
	f := func(v bool) object { return object{"available": v} }
	s := i.host.ServerIdentity
	auth := frontend.AuthenticationFeatures{}
	if source, ok := i.host.Identity.(frontend.AuthenticationFeatureSource); ok {
		auth = source.AuthenticationFeatures()
	}
	return object{"protocolVersion": 0, "compatibleClientBuilds": object{"minInclusive": "0.1.0", "maxExclusive": "0.2.0"}, "server": object{"id": s.MachineID, "name": s.FriendlyName, "version": s.Version}, "features": object{"passwordSignIn": f(auth.Password), "deviceGrant": f(auth.QuickConnect && i.host.ViewerPortalURL != nil), "quickConnect": f(auth.QuickConnect), "descriptiveSearch": f(true), "aiSearch": f(false), "proactiveFallback": f(false), "liveTV": f(i.host.LiveTV != nil), "extras": f(false), "initialTrackSelection": f(true), "sessionTrackChanges": f(false), "extraPlayback": f(false)}, "skins": []object{{"id": "default", "name": "Vela"}}, "defaultSkinId": "default"}
}
func (i *instance) negotiate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Client client `json:"client"`
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	for _, v := range b.Client.ProtocolVersions {
		if v == 0 {
			if !compatibleBuild(b.Client.Device.AppVersion) {
				problem(w, 400, "request", "unsupported_client_build")
				return
			}
			send(w, 200, object{"capabilities": i.capabilities(), "client": b.Client})
			return
		}
	}
	problem(w, 400, "request", "unsupported_version")
}

func (i *instance) grantScope(ctx context.Context, token string, device frontend.Device) (string, error) {
	service, ok := i.host.Identity.(frontend.GrantScopes)
	if !ok {
		return "", frontend.ErrIdentityUnavailable
	}
	return service.GrantScope(ctx, token, device)
}
