// Package vela presents the Vela v0 protocol through neutral host services.
package vela

import (
	"cmp"
	"context"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/webclient"
)

type object = map[string]any

type Frontend struct {
	AssetsDir string
	DB        *sql.DB
	ReplayKey []byte
	Skins     *SkinStore
}

const DefaultPort = 8098

// DedicatedPort extracts a valid optional dedicated listener port (1..65535)
// from Vela settings, or returns 0 when Vela uses only Ether's main port.
func DedicatedPort(s map[string]any) int {
	if s == nil {
		return 0
	}
	switch v := s["port"].(type) {
	case float64:
		if v >= 1 && v <= 65535 && math.Trunc(v) == v {
			return int(v)
		}
	case int:
		if v >= 1 && v <= 65535 {
			return v
		}
	}
	return 0
}

// ServeOnMainPort reports whether Vela should continue serving /vela and
// /sessions/media on Ether's main (Plex) port. Defaults to true when omitted.
func ServeOnMainPort(s map[string]any) bool {
	if s == nil {
		return true
	}
	if v, ok := s["serveOnMainPort"].(bool); ok {
		return v
	}
	return true
}

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{
		ID:          "vela",
		Name:        "Vela",
		Version:     "0",
		DefaultPort: DefaultPort,
		SettingsSchema: frontend.SettingsSchema{
			SignInHelp: true,
			Sections: []frontend.SettingsSection{
				{
					Key: "appearance",
					Fields: []frontend.SettingsField{
						{Key: "defaultSkinId", Kind: "string"},
						{Key: "force", Kind: "bool"},
						{Key: "appName", Kind: "string", Max: frontend.Int(MaxAppNameChars)},
						{Key: "wordmark", Kind: "wordmark"},
						{Key: "appIcon", Kind: "appIcon"},
					},
				},
				{
					Key: "signIn",
					Fields: []frontend.SettingsField{
						{Key: "passwordSignIn", Kind: "bool"},
						{Key: "plexSignIn", Kind: "bool"},
						{Key: "quickConnect", Kind: "bool"},
					},
				},
				{
					Key: "liveTV",
					Fields: []frontend.SettingsField{
						{Key: "livePreview", Kind: "string", Options: []string{LivePreviewAuto, LivePreviewOnClick, LivePreviewOff}},
					},
				},
				{
					Key: "find",
					Fields: []frontend.SettingsField{
						{Key: "port", Kind: "port", Min: frontend.Int(1), Max: frontend.Int(65535)},
						{Key: "serveOnMainPort", Kind: "bool"},
						{Key: "serverName", Kind: "serverName"},
					},
				},
			},
		},
	}
}

const (
	LivePreviewAuto    = "auto"
	LivePreviewOnClick = "on-click"
	LivePreviewOff     = "off"
)

// ValidLivePreviewMode reports whether mode is a supported Vela Live TV
// preview setting value.
func ValidLivePreviewMode(mode string) bool {
	return mode == LivePreviewAuto || mode == LivePreviewOnClick || mode == LivePreviewOff
}

func livePreviewModeFromSettings(settings map[string]any) string {
	if mode, ok := settings["livePreview"].(string); ok && ValidLivePreviewMode(mode) {
		return mode
	}
	return LivePreviewAuto
}

// ValidateSettings keeps the owner setting additive without accepting arbitrary config.
func (f Frontend) ValidateSettings(s frontend.Settings) error {
	for key, value := range s {
		switch key {
		case "defaultSkinId":
			id, ok := value.(string)
			if !ok || !ValidSkinID(id) {
				return errors.New("invalid Vela setting")
			}
			if f.Skins != nil && !IsBuiltinSkinID(id) && !f.Skins.HasInstalled(context.Background(), id) {
				return errors.New("unknown Vela skin")
			}
		case "force", "passwordSignIn", "plexSignIn", "quickConnect", "serveOnMainPort":
			if _, ok := value.(bool); !ok {
				return errors.New("invalid Vela setting")
			}
		case "port":
			n, ok := value.(float64)
			if !ok || n < 1 || n > 65535 || math.Trunc(n) != n {
				return errors.New("invalid port")
			}
		case "appName":
			name, ok := value.(string)
			if !ok {
				return errors.New("invalid Vela setting")
			}
			trimmed := strings.TrimSpace(name)
			if utf8.RuneCountInString(trimmed) > MaxAppNameChars || hasControlRune(trimmed) {
				return errors.New("invalid Vela setting")
			}
		case "wordmark":
			if value == nil {
				continue
			}
			wm, ok := parseWordmarkSetting(value)
			if !ok {
				return errors.New("invalid Vela setting")
			}
			if err := validateWordmarkInDB(f.DB, wm); err != nil {
				return err
			}
		case "livePreview":
			mode, ok := value.(string)
			if !ok || !ValidLivePreviewMode(mode) {
				return errors.New("invalid Vela setting")
			}
		case "appIcon":
			if value == nil {
				continue
			}
			icon, ok := parseAppIconSetting(value)
			if !ok {
				return errors.New("invalid Vela setting")
			}
			if err := validateAppIconInDB(f.DB, icon); err != nil {
				return err
			}
		default:
			return errors.New("invalid Vela setting")
		}
	}
	if serveMain, ok := s["serveOnMainPort"].(bool); ok && !serveMain && DedicatedPort(s) == 0 {
		return errors.New("turning off /vela on the main port requires a dedicated port")
	}
	return nil
}

func (f Frontend) Mount(ctx context.Context, host frontend.Host) (frontend.Instance, error) {
	if host.Identity == nil || host.Experience == nil || host.Sessions == nil {
		return nil, errors.New("vela requires identity, experience and sessions")
	}
	ctx, cancel := context.WithCancel(ctx)
	i := &instance{host: host, ctx: ctx, cancel: cancel, mux: http.NewServeMux(), sessions: map[string]*playSession{}, replays: map[string]*replay{}, assets: f.AssetsDir, skins: f.Skins}
	seal, err := encryption(f.ReplayKey)
	if err != nil {
		cancel()
		return nil, err
	}
	i.rowMeta = map[string]frontend.RowView{}
	i.db = f.DB
	i.seal = seal
	if len(f.ReplayKey) > 0 {
		i.nonceKey = append([]byte(nil), f.ReplayKey...)
	}
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
	appearanceMu    sync.Mutex
	appearanceCache *appearanceEntry
	skins           *SkinStore
	brandingMu      sync.Mutex
	brandingBlobs   map[string]brandingBlob
	iconMu          sync.Mutex
	iconBlobs       map[string]webclient.VelaIconAsset
	bootstrapMu     sync.Mutex
	bootstraps      map[string]*bootstrapEntry
	db              *sql.DB
	seal            cipher.AEAD
	nonceMu         sync.Mutex
	nonceKey        []byte
	rowMu           sync.Mutex
	rowMeta         map[string]frontend.RowView

	eventsHeartbeat    time.Duration
	eventsWriteTimeout time.Duration
	disconnectGrace    time.Duration
	liveHeartbeatCh    <-chan time.Time
	liveEventInterval  time.Duration

	host            frontend.Host
	ctx             context.Context
	cancel          context.CancelFunc
	mux             *http.ServeMux
	assets          string
	mu              sync.Mutex
	previewRates    map[int64]previewRate
	previewRequests map[previewRequestKey]previewRequest
	themes          map[string]themeGrant
	sessions        map[string]*playSession
	replayMu        sync.Mutex
	replays         map[string]*replay
	plexSelMu       sync.Mutex
	plexSels        map[string]*plexSelection
	aiMu            sync.Mutex
	aiJobs          map[string]*aiSearchJob
	aiActive        int
	warmMu          sync.Mutex
	warmClients     map[string]negotiatedClient
	warmPending     map[string]bool
	readMu          sync.Mutex
	readTickets     map[string]*readTicket
}

func (i *instance) cursorNonce(domain string, plain []byte) ([]byte, error) {
	i.nonceMu.Lock()
	if len(i.nonceKey) == 0 {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			i.nonceMu.Unlock()
			return nil, err
		}
		i.nonceKey = key
	}
	key := i.nonceKey
	i.nonceMu.Unlock()
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("vela-cursor-nonce\x00"))
	mac.Write([]byte(domain))
	mac.Write([]byte{0})
	mac.Write(plain)
	sum := mac.Sum(nil)
	return sum[:i.seal.NonceSize()], nil
}

// cursorExpiry gives every cursor issued in one two-minute bucket the same
// expiry, eight to ten minutes out. Live ETags carry the bucket, so a page
// answered 304 never holds a cursor that has already expired.
func cursorExpiry(now time.Time) int64 {
	return (now.Unix()/120)*120 + 600
}

func (i *instance) Handler() http.Handler { return http.HandlerFunc(i.serve) }
func (i *instance) Listeners() []frontend.ListenerSpec {
	var settings frontend.Settings
	if i.host.Config != nil {
		settings = i.host.Config()
	}
	port := DedicatedPort(settings)
	var specs []frontend.ListenerSpec
	if port > 0 {
		specs = append(specs, frontend.ListenerSpec{Address: fmt.Sprintf(":%d", port)})
	}
	// Never leave Vela unreachable on both ports: only disable /vela on the
	// main (Plex) port when a dedicated listener port is configured.
	if port == 0 || ServeOnMainPort(settings) {
		specs = append(specs,
			frontend.ListenerSpec{Shared: true, PathPrefix: "/vela"},
			frontend.ListenerSpec{Shared: true, PathPrefix: "/sessions/media"},
		)
	}
	return specs
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (i *instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "0", Conformance: "unverified", WebPlayerReady: webclient.VelaReady(i.assets)}
}
func (i *instance) Close(context.Context) error {
	if i.cancel != nil {
		i.cancel()
	}
	i.mu.Lock()
	for _, s := range i.sessions {
		s.mu.Lock()
		s.clearDisconnectTimerLocked()
		s.mu.Unlock()
	}
	i.mu.Unlock()
	i.closeReadTickets()
	return nil
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, class, code string) {
	problemMessage(w, status, class, code, strings.ReplaceAll(code, "_", " "))
}
func problemMessage(w http.ResponseWriter, status int, class, code, message string) {
	send(w, status, object{"error": object{"class": class, "code": code, "message": message, "retry": object{"allowed": false}}})
}
func fail(w http.ResponseWriter, err error, requests ...*http.Request) {
	failWithPolicy(w, nil, nil, err, requests...)
}

func (i *instance) failFor(w http.ResponseWriter, v *frontend.Viewer, err error, requests ...*http.Request) {
	var policy any
	if i != nil {
		policy = i.host.ViewerPolicy
	}
	failWithPolicy(w, policy, v, err, requests...)
}

func failWithPolicy(w http.ResponseWriter, policy any, viewer *frontend.Viewer, err error, requests ...*http.Request) {
	var capErr *frontend.ProviderConnectionCapError
	if errors.As(err, &capErr) {
		sendProviderConnectionCap(w, capErr)
		return
	}
	var bedtime *frontend.ErrBedtime
	if errors.As(err, &bedtime) {
		send(w, http.StatusForbidden, object{"error": object{"class": "policy", "code": "bedtime", "message": bedtime.Error(), "nextAllowedAt": bedtime.NextAllowedAt.UTC().Format(time.RFC3339), "retry": object{"allowed": false}}})
		return
	}

	var incompatible *frontend.NoCompatiblePathError
	var reqLang *frontend.RequiredAudioLanguageError
	var changed *bootstrapChangedError
	switch {
	case errors.As(err, &incompatible):
		message := "no compatible playback path"
		if errors.Is(err, frontend.ErrVideoTranscodingDisabled) {
			message = frontend.VideoTranscodingOffMessage(policy, viewer)
		} else if errors.As(err, &reqLang) {
			message = reqLang.Error()
		}
		send(w, 422, object{"error": object{"class": "playback", "code": "no_compatible_path", "message": message, "rejectedPaths": incompatible.RejectedPaths, "retry": object{"allowed": false}}})
	case errors.Is(err, frontend.ErrVideoTranscodingDisabled):
		send(w, 422, object{"error": object{"class": "playback", "code": "no_compatible_path", "message": frontend.VideoTranscodingOffMessage(policy, viewer), "rejectedPaths": []frontend.RejectedPlaybackPath{{Path: "video", Reasons: []string{frontend.ErrVideoTranscodingDisabled.Error()}}}, "retry": object{"allowed": false}}})
	case errors.As(err, &reqLang):
		send(w, http.StatusUnprocessableEntity, object{"error": object{"class": "source", "code": "required_audio_language", "message": reqLang.Error(), "retry": object{"allowed": false}}})
	case errors.As(err, &changed):
		w.Header().Set("Retry-After", "1")
		send(w, 409, object{"error": object{"class": "conflict", "code": "bootstrap_changed", "message": "home changed while loading", "retry": object{"allowed": true, "afterSeconds": 1, "maxAttempts": 3}}})
	case errors.Is(err, frontend.ErrPolicyUnconfirmed):
		problem(w, 403, "policy", "policy_unconfirmed")
	case errors.Is(err, frontend.ErrUnreleased):
		problem(w, 409, "source", "title_unreleased")
	case errors.Is(err, frontend.ErrTrackNotFound):
		problem(w, 422, "playback", "track_not_found")
	case errors.Is(err, frontend.ErrDownloadNeedsTranscode), errors.Is(err, frontend.ErrSubtitleAuthoringUnavailable), errors.Is(err, frontend.ErrSubtitleFontsUnavailable):
		problem(w, 422, "playback", "unsupported_conversion")
	case errors.Is(err, frontend.ErrArchiveUnstreamable):
		logFailure(err, requests...)
		send(w, http.StatusUnprocessableEntity, object{"error": object{"class": "source", "code": "archive_unstreamable", "message": frontend.ErrArchiveUnstreamable.Error(), "retry": object{"allowed": false}}})
	case errors.Is(err, frontend.ErrInvalidMultiview):
		problem(w, 400, "request", "invalid_multiview")
	case errors.Is(err, frontend.ErrInvalidPreferences), errors.Is(err, frontend.ErrBadPlaylist), errors.Is(err, frontend.ErrPeopleQuery), errors.Is(err, frontend.ErrImageBatchLimit), errors.Is(err, frontend.ErrInvalidScreenDate), errors.Is(err, frontend.ErrInvalidEntityKey):
		problem(w, 400, "request", "invalid_request")
	case errors.Is(err, frontend.ErrPreferencesLimit), errors.Is(err, frontend.ErrDownloadBudget):
		problem(w, 429, "capacity", "resource_capacity")
	case errors.Is(err, frontend.ErrQuickConnectPending):
		w.Header().Set("Retry-After", "2")
		send(w, 409, object{"error": object{"class": "conflict", "code": "authorization_pending", "message": "authorization pending", "retry": object{"allowed": true, "afterSeconds": 2, "maxAttempts": 3}}})
	case errors.Is(err, frontend.ErrIdentityUnavailable):
		problem(w, 403, "policy", "identity_unavailable")
	case errors.Is(err, context.Canceled):
		problem(w, 408, "request", "request_canceled")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, frontend.ErrPrepareSlow):
		w.Header().Set("Retry-After", "2")
		send(w, 503, object{"error": object{"class": "capacity", "code": "preparation_pending", "message": "preparation pending", "retry": object{"allowed": true, "afterSeconds": 2, "maxAttempts": 3}}})
	case errors.Is(err, frontend.ErrPreparing):
		seconds := frontend.PreparingRetrySeconds(err, 5)
		msg := "item is still being prepared"
		if detail := frontend.DownloadQueuedDetail(err); detail != "" {
			msg = detail
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		send(w, http.StatusServiceUnavailable, object{"error": object{"class": "source", "code": "preparing", "message": msg, "retry": object{"allowed": true, "afterSeconds": seconds, "maxAttempts": 3}}})
	case errors.Is(err, frontend.ErrJobsBusy):
		seconds := frontend.JobsBusyRetrySeconds(err, 5)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		w.Header().Set("X-Ether-Media-Error", "jobs_busy")
		send(w, http.StatusServiceUnavailable, object{"error": object{"class": "capacity", "code": "jobs_busy", "message": frontend.JobsBusyMessage(err), "retry": object{"allowed": true, "afterSeconds": seconds, "maxAttempts": 3}}})
	case errors.Is(err, frontend.ErrBudgetExhausted):
		problem(w, 429, "capacity", "indexer_budget_exhausted")
	case errors.Is(err, frontend.ErrDownloadLimit), errors.Is(err, frontend.ErrDownloadPaused):
		problem(w, 503, "capacity", "source_capacity")
	case errors.Is(err, frontend.ErrDebridAuth):
		problem(w, 502, "source", "source_authentication_failed")
	case errors.Is(err, frontend.ErrDownloadQueued), errors.Is(err, frontend.ErrDownloadRefused), errors.Is(err, frontend.ErrNotCached), errors.Is(err, frontend.ErrReleaseRejected), errors.Is(err, frontend.ErrArticleCorrupt), errors.Is(err, frontend.ErrArticleMissing):
		problem(w, 502, "source", "source_unavailable")
	case errors.Is(err, frontend.ErrReceiptCapacity):
		var capacity *frontend.ReceiptCapacityError
		seconds := 1
		if errors.As(err, &capacity) {
			seconds = max(1, int((capacity.RetryAfter+time.Second-1)/time.Second))
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		send(w, 429, object{"error": object{"class": "capacity", "code": "receipt_capacity", "message": "receipt capacity", "retry": object{"allowed": true, "afterSeconds": seconds, "maxAttempts": 3}}})
	case errors.Is(err, frontend.ErrTrackSourceUnavailable):
		w.Header().Set("Retry-After", "2")
		send(w, 502, object{"error": object{"class": "source", "code": "track_source_unavailable", "message": "track source unavailable", "retry": object{"allowed": true, "afterSeconds": 2, "maxAttempts": 3}}})
	case errors.Is(err, frontend.ErrInteractionReceipt):
		problem(w, 400, "request", "invalid_receipt")
	case errors.Is(err, frontend.ErrSourceChanged):
		problem(w, 409, "conflict", "source_changed")
	case errors.Is(err, frontend.ErrSeekUnavailable):
		problem(w, 409, "playback", "seek_unavailable")
	case errors.Is(err, frontend.ErrRowViewStale), errors.Is(err, frontend.ErrLiveGuideStale):
		problem(w, 409, "conflict", "cursor_expired")
	case errors.Is(err, frontend.ErrTooManyTranscodes), errors.Is(err, frontend.ErrTranscodeCapacity), errors.Is(err, frontend.ErrStreamLimit), errors.Is(err, frontend.ErrSessionBusy):
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
	case errors.Is(err, frontend.ErrBadPlaylist):
		problem(w, 400, "request", "invalid_request")
	case errors.Is(err, frontend.ErrPlaylistReadOnly):
		problem(w, 409, "conflict", "playlist_read_only")
	case errors.Is(err, frontend.ErrPlaylistTooLarge):
		problem(w, 422, "request", "playlist_too_large")
	case errors.Is(err, frontend.ErrPlaylistUnsupportedKind):
		problem(w, 422, "request", "unsupported_kind")
	case errors.Is(err, frontend.ErrVersionUnavailable):
		problem(w, 409, "source", "version_unavailable")
	case errors.Is(err, frontend.ErrNotAvailable):
		problem(w, 409, "source", "not_available")
	case errors.Is(err, frontend.ErrNotFound):
		problem(w, 404, "not-found", "not_found")
	case errors.Is(err, frontend.ErrForbidden):
		problem(w, 403, "policy", "forbidden")
	case errors.Is(err, frontend.ErrUnauthorized):
		problem(w, 401, "auth", "unauthorized")
	case errors.Is(err, frontend.ErrRateLimited):
		var limited *frontend.IdentityRateLimitError
		if errors.As(err, &limited) && limited.RetryAfter > 0 && limited.RetryAfter <= 15*time.Minute {
			seconds := int((limited.RetryAfter + time.Second - 1) / time.Second)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			send(w, 503, object{"error": object{"class": "capacity", "code": "rate_limited", "message": "rate limited", "retry": object{"allowed": true, "afterSeconds": seconds, "maxAttempts": 2}}})
		} else {
			problem(w, 503, "capacity", "rate_limited")
		}
	default:
		logFailure(err, requests...)
		problem(w, 503, "transient", "service_unavailable")
	}
}

// Partial detail snapshots always return a body, even when a client presents
// their weak ETag again: it must keep polling until the missing fields arrive.
func cacheDetail(w http.ResponseWriter, r *http.Request, out object, pending *frontend.DetailPending) {
	fields := pending.Fields()
	if len(fields) == 0 {
		cache(w, r, out)
		return
	}
	out["pending"] = fields
	out["refreshAfterMs"] = 2000
	b, err := json.Marshal(out)
	if err != nil {
		fail(w, err, r)
		return
	}
	tag := `W/"` + hash(string(b)+r.Header.Get("Authorization")+r.Header.Get("X-Vela-Device-Id")+r.URL.RequestURI()) + `"`
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", tag)
	if w.Header().Get("Vary") == "" {
		w.Header().Set("Vary", "Authorization, X-Vela-Device-Id")
	}
	_, _ = w.Write(b)
}

func cache(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fail(w, err, r)
		return
	}
	tag := `"` + hash(string(b)+r.Header.Get("Authorization")+r.Header.Get("X-Vela-Device-Id")+r.URL.RequestURI()) + `"`
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", tag)
	if w.Header().Get("Vary") == "" {
		w.Header().Set("Vary", "Authorization, X-Vela-Device-Id")
	}
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
	d := device{ID: r.Header.Get("X-Vela-Device-Id"), Name: r.Header.Get("X-Vela-Device-Name"), Platform: r.Header.Get("X-Vela-Platform"), AppVersion: r.Header.Get("X-Vela-Version")}.neutral()
	d.Language = cmp.Or(r.Header.Get("X-Vela-Language"), r.Header.Get("Accept-Language"))
	return d
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
func (i *instance) resolvedRedirectPrefix(r *http.Request) string {
	if i.host.SurfaceURL != nil {
		if raw, err := i.host.SurfaceURL(r.Context(), r); err == nil {
			if u, err := url.Parse(raw); err == nil {
				p := strings.TrimRight(u.Path, "/")
				if p == "/vela" || strings.HasSuffix(p, "/vela") {
					p = strings.TrimSuffix(p, "/vela")
				}
				if p != "" && p != "/" {
					return p
				}
			}
		}
	}
	if origin, ok := i.host.Origin(r); ok {
		return origin.Prefix
	}
	return ""
}
func (i *instance) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/sessions/media/") {
		w.Header().Set("Cache-Control", "no-store")
		// Media elements cannot attach the API bearer/device headers. The
		// surface-bound broker authenticates the URL grant, viewer and current
		// policy on every read, including playlists, segments and subtitles.
		i.serveMedia(w, r.WithContext(i.identityContext(r)))
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/vela/v0/") {
		brand := i.velaBrand()
		brand.RedirectPrefix = i.resolvedRedirectPrefix(r)
		webclient.ServeVelaWithBrand(w, r, i.assets, brand)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx := i.identityContext(r)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if !reqmemo.Active(ctx) {
			ctx = reqmemo.With(ctx)
		}
		if frontend.RequestMemoFrom(ctx) == nil {
			ctx = frontend.WithRequestMemo(ctx)
		}
	}
	r = r.WithContext(ctx)
	// Authentication/idempotency can fail before ServeMux dispatches. Record
	// only its registered route template for those warnings too. Unknown routes
	// and unsupported methods dispatch straight to ServeMux so they answer 404
	// or 405 instead of 400/401.
	_, r.Pattern = i.mux.Handler(r)
	isLivePreferencesWrite := (r.Method == http.MethodPut || r.Method == http.MethodPatch) && r.URL.Path == "/vela/v0/live/preferences"
	if r.Pattern == "" || r.Pattern == "/vela/v0/" || (!isLivePreferencesWrite && r.Method != "POST" && r.Method != "PATCH" && r.Method != "DELETE") || (r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/vela/v0/favorites/entities/")) {
		i.mux.ServeHTTP(w, r)
		return
	}
	// A bounded read, followed by per-key serialization, makes refresh rotation
	// and playback admission safe to retry. No worker or allocation exists at idle.
	optionalMultiviewIdem := (r.Method == http.MethodPost && r.URL.Path == "/vela/v0/multiview") ||
		(r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/vela/v0/multiview/"))
	var raw json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&raw); err != nil {
		// POST /vela/v0/live/events/{id}/reveal and DELETE /vela/v0/multiview/{groupId}
		// are naturally idempotent and may be called with an empty body.
		if errors.Is(err, io.EOF) && ((r.Method == "POST" && r.Pattern == "POST /vela/v0/live/events/{id}/reveal") || (r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/vela/v0/multiview/"))) {
			r.Body = http.NoBody
			i.mux.ServeHTTP(w, r)
			return
		}
		problem(w, 400, "request", "invalid_json")
		return
	}
	if r.Method == "POST" && r.URL.Path == "/vela/v0/live/guide/window" {
		// Bulk guide window is a pure read with optional idempotencyKey and
		// ETag revalidation rather than a persisted mutation.
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			problem(w, 400, "request", "invalid_json")
			return
		}
		r.Body = http.NoBody
		i.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, raw)))
		return
	}
	var envelope struct {
		IdempotencyKey *string         `json:"idempotencyKey"`
		Kind           string          `json:"kind"`
		Device         json.RawMessage `json:"device"`
		Client         struct {
			Device device `json:"device"`
		} `json:"client"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		if isLivePreferencesWrite {
			problem(w, 400, "request", "invalid_request")
			return
		}
		problem(w, 400, "request", "idempotency_key_required")
		return
	}
	if (envelope.IdempotencyKey == nil || *envelope.IdempotencyKey == "") && r.Method == "POST" && r.Pattern == "POST /vela/v0/live/events/{id}/reveal" {
		r.Body = http.NoBody
		i.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, raw)))
		return
	}
	optionalReplanIdem := r.Method == http.MethodPost && r.Pattern == "POST /vela/v0/sessions/{id}/commands" && envelope.Kind == "replan"
	if ((optionalMultiviewIdem || optionalReplanIdem) && envelope.IdempotencyKey == nil) || (isLivePreferencesWrite && (envelope.IdempotencyKey == nil || *envelope.IdempotencyKey == "")) {
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			problem(w, 400, "request", "invalid_json")
			return
		}
		r.Body = http.NoBody
		i.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, raw)))
		return
	}
	if envelope.IdempotencyKey == nil || *envelope.IdempotencyKey == "" || len(*envelope.IdempotencyKey) > 256 {
		problem(w, 400, "request", "idempotency_key_required")
		return
	}
	idemKey := *envelope.IdempotencyKey
	public := strings.HasPrefix(r.URL.Path, "/vela/v0/auth/") && !strings.HasSuffix(r.URL.Path, "/approve") && !strings.HasSuffix(r.URL.Path, "/revoke") && r.URL.Path != "/vela/v0/auth/web-code" && r.URL.Path != "/vela/v0/auth/plex/profiles/challenge"
	public = public || r.URL.Path == "/vela/v0/negotiate"
	scope := ""
	if !public {
		_, token, err := i.viewer(r)
		if err == nil {
			scope, err = i.grantScope(r.Context(), token, requestDevice(r))
		}
		if err != nil {
			fail(w, err, r)
			return
		}
	} else {
		var d device
		if len(envelope.Device) > 0 && json.Unmarshal(envelope.Device, &d) != nil {
			problem(w, 400, "request", "idempotency_key_required")
			return
		}
		if d.ID == "" {
			d = envelope.Client.Device
		}
		if d.ID == "" {
			problem(w, 400, "request", "device_required")
			return
		}
		scope = d.ID
	}
	key := hash(scope + "\x00" + r.Method + "\x00" + r.URL.Path + "\x00" + idemKey)
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
		if len(old.body) > 0 {
			_, _ = w.Write(old.body)
		}
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
			fail(out, claimErr, r)
		}
	} else if conflict {
		problem(out, 409, "conflict", "idempotency_conflict")
	} else if persisted != nil {
		for k, v := range persisted.Header {
			out.Header()[k] = v
		}
		out.WriteHeader(persisted.Status)
		if len(persisted.Body) > 0 {
			_, _ = out.Write(persisted.Body)
		}
	} else {
		i.mux.ServeHTTP(out, r)
		var saveErr error
		if (out.Code == http.StatusAccepted && r.URL.Path == "/vela/v0/playback") ||
			((out.Code == http.StatusServiceUnavailable || out.Code == http.StatusBadGateway) && (r.URL.Path == "/vela/v0/playback" || r.Pattern == "POST /vela/v0/libraries/{id}/live/play") && out.Header().Get("Retry-After") != "") ||
			(out.Code == http.StatusServiceUnavailable && strings.HasSuffix(r.URL.Path, "/read") && out.Header().Get("Retry-After") != "") ||
			(out.Code == http.StatusServiceUnavailable && strings.HasSuffix(r.URL.Path, "/commands") && out.Header().Get("Retry-After") != "") ||
			(out.Code == http.StatusTooManyRequests && strings.HasSuffix(r.URL.Path, "/approve") && out.Header().Get("Retry-After") != "") {
			_, saveErr = i.db.ExecContext(r.Context(), `DELETE FROM vela_replays WHERE key=? AND response IS NULL`, key)
		} else {
			saveErr = i.save(r.Context(), key, savedResponse{Status: out.Code, Header: out.Header(), Body: out.Body.Bytes()})
		}
		if saveErr != nil {
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
	if len(entry.body) > 0 {
		_, _ = w.Write(entry.body)
	}
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
	i.route("GET", "/appearance", i.appearance)
	i.route("HEAD", "/appearance", i.appearance)
	i.route("GET", "/appearance/packs/{id}/{version}/{file}", i.servePackAsset)
	i.route("HEAD", "/appearance/packs/{id}/{version}/{file}", i.servePackAsset)
	i.route("GET", "/appearance/skins/{id}/{versionJSON}", i.serveSkinDocumentJSON)
	i.route("HEAD", "/appearance/skins/{id}/{versionJSON}", i.serveSkinDocumentJSON)
	i.route("GET", "/appearance/skins/{id}/{version}/{file}", i.serveSkinDocumentAsset)
	i.route("HEAD", "/appearance/skins/{id}/{version}/{file}", i.serveSkinDocumentAsset)
	i.route("GET", "/branding/{asset}", i.brandingAsset)
	i.route("HEAD", "/branding/{asset}", i.brandingAsset)
	i.route("GET", "/events", i.appearanceEvents)
	i.route("GET", "/libraries", i.libraries)
	i.route("GET", "/libraries/{id}/items", i.libraryItems)
	i.route("GET", "/libraries/{id}/collections", i.libraryCollections)
	i.route("GET", "/libraries/{id}/facets/{field}", i.libraryFacets)
	i.route("GET", "/libraries/{id}/rows", i.libraryRows)
	i.route("GET", "/libraries/{id}/live", i.libraryLive)
	i.route("POST", "/libraries/{id}/live/play", i.libraryLivePlay)
	i.route("GET", "/people/{id}", i.person)
	i.route("GET", "/people/{id}/titles", i.personTitles)
	i.route("GET", "/capabilities", func(w http.ResponseWriter, r *http.Request) { cache(w, r, i.requestCapabilities(r)) })
	i.route("POST", "/negotiate", i.negotiate)
	for _, p := range []string{"sign-in", "plex", "plex/profile", "plex/profiles/challenge", "refresh", "revoke", "web-code", "web-code/redeem", "device", "device/poll", "device/approve", "quick-connect", "quick-connect/poll", "quick-connect/approve"} {
		i.route("POST", "/auth/"+p, i.auth)
	}
	i.route("GET", "/bootstrap", i.bootstrap)
	i.route("GET", "/rows", i.rows)
	i.route("GET", "/rows/{id}", i.rows)
	i.route("GET", "/themes/{secret}/theme.m4a", i.theme)
	i.route("HEAD", "/themes/{secret}/theme.m4a", i.theme)
	i.route("GET", "/titles/{id}", i.detail)
	i.route("GET", "/titles/{id}/children", i.children)
	i.route("GET", "/titles/{id}/extras", i.detail)
	i.route("GET", "/titles/{id}/versions", i.versions)
	i.route("DELETE", "/titles/{id}/versions/choice", i.clearVersionChoice)
	i.route("POST", "/search", i.search)
	i.route("POST", "/search/ai", i.startAISearch)
	i.route("GET", "/search/ai/{id}", i.pollAISearch)
	i.route("POST", "/search/ai/{id}/cancel", i.cancelAISearch)
	i.route("GET", "/images/{secret}", i.image)
	i.route("POST", "/interactions", i.interactions)
	i.route("POST", "/multiview", i.createMultiviewGroup)
	i.route("DELETE", "/multiview/{groupId}", i.deleteMultiviewGroup)
	i.route("POST", "/playback", i.playback)
	i.route("POST", "/titles/{id}/trailer-preview", i.trailerPreview)
	i.route("GET", "/titles/{id}/trailer-preview", i.trailerPreviewStatus)
	i.route("POST", "/sessions/{id}/audio-fallback", i.audioFallback)
	i.route("DELETE", "/sessions/{id}/audio-fallback/{audioId}", i.audioFallback)
	i.route("GET", "/sessions/{id}", i.snapshot)
	i.route("GET", "/sessions/{id}/events", i.events)
	i.route("POST", "/sessions/{id}/commands", i.command)
	i.route("POST", "/sessions/{id}/observations", i.observe)
	i.route("GET", "/live/channels", i.channels)
	i.route("GET", "/live/guide", i.guide)
	i.route("POST", "/live/guide/window", i.guideWindow)
	i.route("GET", "/live/events", i.liveEventsSnapshot)
	i.route("GET", "/live/events/{id}", i.liveEventDetail)
	i.route("POST", "/live/events/{id}/reveal", i.liveEventReveal)
	i.route("GET", "/live/stream", i.liveEventsStream)
	i.route("GET", "/live/preferences", i.liveEventsPreferencesGet)
	i.route("PUT", "/live/preferences", i.liveEventsPreferencesSet)
	i.route("PATCH", "/live/preferences", i.liveEventsPreferencesSet)
	i.route("GET", "/live/entities", i.liveEntities)
	i.route("GET", "/live/ticker", i.liveTicker)
	i.route("GET", "/screens/{id}", i.screen)
	i.route("PUT", "/favorites/entities/{key}", i.favoriteEntity)
	i.route("DELETE", "/favorites/entities/{key}", i.favoriteEntity)
	i.route("POST", "/titles/{id}/watch", i.watch)
	i.route("POST", "/titles/{id}/read", i.read)
	i.route("GET", "/read/{ticket}", i.readFile)
	i.route("HEAD", "/read/{ticket}", i.readFile)
	i.route("POST", "/titles/{id}/reading-progress", i.readingProgress)
	i.route("GET", "/titles/{id}/playlists", i.titlePlaylists)
	i.route("GET", "/playlists", i.listPlaylists)
	i.route("POST", "/playlists", i.createPlaylist)
	i.route("GET", "/playlists/{id}", i.playlistDetail)
	i.route("PATCH", "/playlists/{id}", i.renamePlaylist)
	i.route("DELETE", "/playlists/{id}", i.deletePlaylist)
	i.route("POST", "/playlists/{id}/entries", i.addPlaylistEntries)
	i.route("DELETE", "/playlists/{id}/entries/{entryId}", i.removePlaylistEntry)
	i.route("POST", "/playlists/{id}/entries/{entryId}/move", i.movePlaylistEntry)
	i.mux.HandleFunc("/vela/v0/", func(w http.ResponseWriter, r *http.Request) { problem(w, 404, "not-found", "not_found") })
}
func (*instance) unavailable(w http.ResponseWriter, r *http.Request) {
	problem(w, 403, "policy", "feature_unavailable")
}
func (i *instance) velaSettings() (map[string]any, uint64) {
	if i.host.HostConfig != nil {
		cfg := i.host.HostConfig().Frontends["vela"]
		return cfg.Settings, cfg.CapabilitiesRevision
	}
	if i.host.Config != nil {
		return i.host.Config(), 0
	}
	return nil, 0
}

func (i *instance) livePreviewMode() string {
	settings, _ := i.velaSettings()
	return livePreviewModeFromSettings(settings)
}

func (i *instance) signInMethods() SignInMethods {
	settings, _ := i.velaSettings()
	return SignInMethodsFromSettings(settings)
}

func (i *instance) capabilitiesRevision() uint64 {
	_, rev := i.velaSettings()
	return rev
}

func (i *instance) quickConnectApprovalAvailable(v *frontend.Viewer) bool {
	if v == nil || v.ID <= 0 || v.Delegated {
		return false
	}
	auth := frontend.AuthenticationFeatures{}
	if source, ok := i.host.Identity.(frontend.AuthenticationFeatureSource); ok {
		auth = source.AuthenticationFeatures()
	}
	return auth.QuickConnect && i.signInMethods().QuickConnect
}

func (i *instance) capabilities() object {
	f := func(v bool) object { return object{"available": v} }
	s := i.host.ServerIdentity
	descriptive := false
	if source, ok := i.host.Experience.(frontend.DescriptiveSearcher); ok {
		descriptive = source.DescriptiveSearchAvailable() && i.aiSearchAvailable()
	}
	_, live := i.host.LiveTV.(frontend.LiveGuidePager)
	_, trackChanges := i.host.Sessions.(frontend.StreamDecisionReader)
	auth := frontend.AuthenticationFeatures{}
	if source, ok := i.host.Identity.(frontend.AuthenticationFeatureSource); ok {
		auth = source.AuthenticationFeatures()
	}
	methods := i.signInMethods()
	passwordAvail := auth.Password && methods.Password
	plexAvail := auth.PlexAccountSignIn && methods.Plex
	quickAvail := auth.QuickConnect && methods.QuickConnect
	caps := object{"protocolVersion": 0, "revision": i.capabilitiesRevision(), "compatibleClientBuilds": object{"minInclusive": "0.1.0", "maxExclusive": "0.3.0"}, "server": object{"id": s.MachineID, "name": s.FriendlyName, "version": s.Version}, "features": object{"versions": f(i.versionsAvailable()), "passwordSignIn": f(passwordAvail), "deviceGrant": f(quickAvail && i.host.ViewerPortalURL != nil), "quickConnect": f(quickAvail), "descriptiveSearch": f(descriptive), "aiSearch": f(i.aiSearchAvailable()), "proactiveFallback": f(i.proactiveFallbackAvailable()), "liveTV": f(live), "liveEvents": f(i.liveEventsAvailable()), "multiview": object{"available": i.multiviewAvailable(), "maxTiles": frontend.MaxMultiviewTiles}, "livePreview": object{"available": true, "mode": i.livePreviewMode()}, "libraryLive": f(i.host.LibraryLive != nil && i.host.Sessions != nil), "extras": f(i.extrasAvailable()), "initialTrackSelection": f(true), "audioFallback": f(i.audioFallbackAvailable()), "sessionTrackChanges": f(trackChanges), "extraPlayback": f(i.extraPlaybackAvailable()), "trailerPreview": f(i.previewAvailable()), "themeMusic": f(i.themeAvailable()), "playlists": object{"available": i.playlistsAvailable(), "maxEntries": frontend.MaxPlaylistEntries}, "removeFromContinue": f(true), "mediaClasses": object{"available": true, "classes": []string{string(frontend.MediaClassVideo), string(frontend.MediaClassMusic), string(frontend.MediaClassAudiobook), string(frontend.MediaClassBook), string(frontend.MediaClassPodcast), string(frontend.MediaClassPhoto)}}, "audioPlayback": f(true), "reading": object{"available": i.readingAvailable(), "formats": readingFormats}, "pluginLevels": object{"available": true, "tabRows": true, "childRoles": true, "expandOnOpen": true}}, "searchSorts": []object{{"id": "relevance", "directions": []string{"asc"}}, {"id": "title", "directions": []string{"asc", "desc"}}, {"id": "released", "directions": []string{"asc", "desc"}}, {"id": "added", "directions": []string{"asc", "desc"}}, {"id": "rating", "directions": []string{"asc", "desc"}}, {"id": "popularity", "directions": []string{"asc", "desc"}}}, "searchKinds": []object{{"id": "movie", "label": "Movies"}, {"id": "show", "label": "Shows"}}, "searchLibraries": true, "skins": []object{{"id": "cinematic", "name": "Cinematic"}}, "defaultSkinId": i.appearanceDocument().skinID}
	if plexAvail {
		caps["plexAccountSignIn"] = true
	}
	if plexAvail && auth.HouseholdProfiles {
		if _, ok := i.host.Identity.(frontend.HouseholdProfileAuth); ok {
			caps["plexHomeProfiles"] = true
		}
	}
	if _, ok := i.liveGuideWindowReader(); ok {
		caps["liveGuideWindow"] = object{
			"maxChannels":      frontend.MaxLiveGuideWindowChannels,
			"maxWindowMinutes": frontend.MaxLiveGuideWindowMinutes,
		}
	}
	return caps
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
			i.rememberNegotiatedClient(r, b.Client)
			send(w, 200, object{"capabilities": i.requestCapabilities(r), "client": b.Client})
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

func (i *instance) requestCapabilities(r *http.Request) object {
	v, _, err := i.viewer(r)
	if err != nil {
		v = nil
	}
	return i.requestCapabilitiesFor(r, v)
}

// requestCapabilitiesFor builds capabilities for an already resolved viewer
// (nil for none): a signed-in viewer also sees the plugin kinds they can
// open, and a request without a viewer never learns plugin library names.
func (i *instance) requestCapabilitiesFor(r *http.Request, v *frontend.Viewer) object {
	c := i.capabilities()
	if v != nil {
		c["searchKinds"], _ = i.searchKinds(r, v)
		if features, ok := c["features"].(object); ok {
			features["quickConnectApproval"] = object{"available": i.quickConnectApprovalAvailable(v)}
			features["proactiveFallback"] = object{"available": i.proactiveFallbackAvailableFor(v)}
		}
	}
	if i.host.SecureOrigins != nil {
		if origins := i.host.SecureOrigins(r.Context(), r); len(origins) > 0 {
			c["secureOrigins"] = origins
		}
	}
	return c
}

// Check the refresh family on every media request, rather than waiting for the
// event subscription's expiry ticker. The broker still validates the URL grant
// and current title/source policy before accessing the shared byte engine.
func (i *instance) serveMedia(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/media/")
	id, _, ok := strings.Cut(rest, "/")
	i.mu.Lock()
	s := i.sessions[id]
	i.mu.Unlock()
	scopes, has := i.host.Identity.(frontend.GrantScopes)
	if !ok || s == nil || !has {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	ended := !s.ended.IsZero() || (!s.previewExpires.IsZero() && !s.previewExpires.After(time.Now()))
	scope, deviceID := s.scope, s.device
	s.mu.Unlock()
	if ended {
		http.NotFound(w, r)
		return
	}
	if _, err := scopes.GrantScopeCurrent(r.Context(), scope, frontend.Device{ID: deviceID}); err != nil {
		http.NotFound(w, r)
		return
	}
	i.serveObservedMedia(w, r)
}
