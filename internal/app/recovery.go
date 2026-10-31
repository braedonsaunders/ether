package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/startup"
)

// recovery is the API a failed start offers on a standalone server, so the
// owner can restore a backup from the status page when the database will
// not open. Nothing that can check a console session is running, so the
// owner proves who they are by signing in with Plex (the account must be
// the one that owns this server) and gets a short-lived recovery token.
// Before setup completes the local network has access, as everywhere else,
// and a browser on the server itself (loopback, addressed as localhost)
// always has: whoever uses that machine can replace the data directory
// anyway, and it is the way in for an owner who does not sign in with Plex.
// Mounted at startup.RecoveryPrefix (LAN-only, like the console):
//
//	GET  access                 {granted, plex}: whether this browser may
//	                            restore, and whether Plex sign-in can grant it
//	POST pin                    start a Plex sign-in → {handle, code, authUrl}
//	GET  pin/{handle}           → {done, token}
//	GET  backups                bundles kept on this server
//	POST restore/snapshot/{n}   restore one and start again
//	POST restore                restore an uploaded encrypted bundle
//	                            (X-Ether-Backup-Password: base64 password)
type recovery struct {
	dataDir string
	front   *frontDoor
	log     *slog.Logger
	// plex signs the owner in; nil when the settings cannot be read.
	plex recoveryPlex

	mu     sync.Mutex
	pins   map[string]*plextv.PIN
	tokens map[string]time.Time
	busy   bool
}

// recoveryPlex is the plex.tv sign-in recovery uses (a *plextv.Client).
type recoveryPlex interface {
	CreatePIN(ctx context.Context) (*plextv.PIN, error)
	AuthURL(pin *plextv.PIN, forwardURL string) string
	CheckPIN(ctx context.Context, id int64, code string) (*plextv.PIN, error)
	User(ctx context.Context, token string) (*plextv.Account, error)
}

const (
	recoveryTokenHeader = "X-Ether-Recovery-Token"
	recoveryTokenTTL    = 30 * time.Minute
)

func newRecovery(dataDir string, front *frontDoor, log *slog.Logger) *recovery {
	r := &recovery{dataDir: dataDir, front: front, log: log, pins: map[string]*plextv.PIN{}, tokens: map[string]time.Time{}}
	if cfg, err := config.Read(dataDir); err == nil && cfg.Plex.ClientIdentifier != "" {
		if c, err := plextv.New(plextv.Options{Identity: plextv.AppIdentity(cfg, ""), Logger: log}); err == nil {
			r.plex = c
		}
	}
	return r
}

// settings are the server's settings as on disk (defaults when unreadable).
func (rc *recovery) settings() config.Config {
	cfg, err := config.Read(rc.dataDir)
	if err != nil {
		return config.Defaults(rc.dataDir)
	}
	return cfg
}

func (rc *recovery) handler() http.Handler {
	mux := http.NewServeMux()
	prefix := strings.TrimSuffix(startup.RecoveryPrefix, "/")
	mux.HandleFunc("GET "+prefix+"/access", rc.access)
	mux.HandleFunc("POST "+prefix+"/pin", rc.mutating(rc.startPIN))
	mux.HandleFunc("GET "+prefix+"/pin/{handle}", rc.pollPIN)
	mux.HandleFunc("GET "+prefix+"/backups", rc.authed(rc.backups))
	mux.HandleFunc("POST "+prefix+"/restore/snapshot/{name}", rc.mutating(rc.authed(rc.restoreSnapshot)))
	mux.HandleFunc("POST "+prefix+"/restore", rc.authed(rc.restoreUpload))
	return mux
}

func recoveryJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func recoveryError(w http.ResponseWriter, code int, kind, msg string) {
	recoveryJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
}

// mutating requires the console's CSRF header (and, with a body, JSON).
func (rc *recovery) mutating(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
		if r.Header.Get("X-Ether-Request") != "1" || (r.ContentLength > 0 && !strings.EqualFold(ct, "application/json")) {
			recoveryError(w, http.StatusForbidden, "csrf", "mutating requests require the X-Ether-Request: 1 header and a JSON content type")
			return
		}
		next(w, r)
	}
}

// authed admits a valid recovery token, or anyone on the local network
// before setup completes.
func (rc *recovery) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !standaloneDataDir(rc.dataDir) {
			recoveryError(w, http.StatusConflict, "guided_restore", "a cluster node restores through the guided cluster restore")
			return
		}
		if rc.validToken(r.Header.Get(recoveryTokenHeader)) || rc.open(r) {
			next(w, r)
			return
		}
		recoveryError(w, http.StatusUnauthorized, "unauthorized", "sign in with the Plex account that owns this server")
	}
}

// open reports whether r may restore without signing in.
func (rc *recovery) open(r *http.Request) bool {
	return admin.SetupOpen(rc.settings().SetupComplete, r) || onThisMachine(r)
}

// onThisMachine is a browser on the server itself: a loopback peer that
// addressed it as localhost (not a public name re-pointed at 127.0.0.1).
func onThisMachine(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	return h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()
}

func (rc *recovery) access(w http.ResponseWriter, r *http.Request) {
	if !standaloneDataDir(rc.dataDir) {
		recoveryJSON(w, http.StatusOK, map[string]bool{"granted": false, "plex": false, "available": false})
		return
	}
	plex := rc.plex != nil && rc.settings().Plex.OwnerPlexID != 0
	recoveryJSON(w, http.StatusOK, map[string]bool{"granted": rc.open(r), "plex": plex, "available": true})
}

func (rc *recovery) validToken(token string) bool {
	if token == "" {
		return false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := time.Now()
	ok := false
	for t, exp := range rc.tokens {
		if now.After(exp) {
			delete(rc.tokens, t)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

func (rc *recovery) startPIN(w http.ResponseWriter, r *http.Request) {
	if rc.plex == nil {
		recoveryError(w, http.StatusServiceUnavailable, "signin_unavailable", "Plex sign-in is unavailable because the server's settings cannot be read")
		return
	}
	pin, err := rc.plex.CreatePIN(r.Context())
	if err != nil {
		recoveryError(w, http.StatusBadGateway, "plextv", "Could not reach plex.tv: "+err.Error())
		return
	}
	handle := rand.Text()
	rc.mu.Lock()
	for h, p := range rc.pins {
		if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt) {
			delete(rc.pins, h)
		}
	}
	rc.pins[handle] = pin
	rc.mu.Unlock()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	recoveryJSON(w, http.StatusOK, map[string]string{"handle": handle, "code": pin.Code, "authUrl": rc.plex.AuthURL(pin, scheme+"://"+r.Host+"/ether/")})
}

// pollPIN completes a sign-in: only the account that owns this server
// receives a recovery token.
func (rc *recovery) pollPIN(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	rc.mu.Lock()
	pin := rc.pins[handle]
	rc.mu.Unlock()
	if pin == nil || rc.plex == nil {
		recoveryError(w, http.StatusNotFound, "expired", "This sign-in expired. Start again.")
		return
	}
	got, err := rc.plex.CheckPIN(r.Context(), pin.ID, pin.Code)
	if err != nil {
		recoveryError(w, http.StatusBadGateway, "plextv", "Could not check the sign-in with plex.tv: "+err.Error())
		return
	}
	if got.AuthToken == "" {
		recoveryJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	rc.mu.Lock()
	delete(rc.pins, handle)
	rc.mu.Unlock()
	acct, err := rc.plex.User(r.Context(), got.AuthToken)
	owner := rc.settings().Plex.OwnerPlexID
	if err != nil || owner == 0 || acct.ID != owner {
		recoveryError(w, http.StatusForbidden, "not_owner", "That Plex account doesn't own this server. Sign in as the owner.")
		return
	}
	token := "ether-recovery-" + rand.Text()
	rc.mu.Lock()
	rc.tokens[token] = time.Now().Add(recoveryTokenTTL)
	rc.mu.Unlock()
	rc.log.Info("recovery: owner signed in on the startup page", "account", strconv.FormatInt(acct.ID, 10))
	recoveryJSON(w, http.StatusOK, map[string]any{"done": true, "token": token})
}

func (rc *recovery) backups(w http.ResponseWriter, r *http.Request) {
	snaps, err := backup.LocalSnapshots(rc.dataDir)
	if err != nil {
		recoveryError(w, http.StatusInternalServerError, "backups", err.Error())
		return
	}
	recoveryJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

// begin serialises restores: each can take a few GB of disk.
func (rc *recovery) begin(w http.ResponseWriter) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.busy {
		recoveryError(w, http.StatusConflict, "busy", "another restore is in progress")
		return false
	}
	rc.busy = true
	return true
}

func (rc *recovery) end() {
	rc.mu.Lock()
	rc.busy = false
	rc.mu.Unlock()
}

func (rc *recovery) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	if !rc.begin(w) {
		return
	}
	defer rc.end()
	st, err := backup.StageLocal(r.Context(), rc.dataDir, r.PathValue("name"))
	if errors.Is(err, os.ErrNotExist) {
		recoveryError(w, http.StatusNotFound, "not_found", "no such backup")
		return
	}
	rc.restore(w, st, err)
}

func (rc *recovery) restoreUpload(w http.ResponseWriter, r *http.Request) {
	// Same rules as the console upload: the header and a same-origin
	// octet-stream body (which a cross-site form cannot send).
	ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if r.Header.Get("X-Ether-Request") != "1" || !strings.EqualFold(ct, "application/octet-stream") || !sameOriginRequest(r) {
		recoveryError(w, http.StatusForbidden, "csrf", "upload the backup from the Ether console")
		return
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Header.Get("X-Ether-Backup-Password")))
	if len(raw) == 0 {
		recoveryError(w, http.StatusBadRequest, "password_required", "enter the password the backup was downloaded with")
		return
	}
	if !rc.begin(w) {
		return
	}
	defer rc.end()
	body := http.MaxBytesReader(w, r.Body, 9<<30)
	head := make([]byte, 8)
	n, _ := io.ReadFull(body, head)
	if !backup.IsEncrypted(head[:n]) {
		recoveryError(w, http.StatusBadRequest, "encrypted_only", "restore an encrypted backup downloaded from Ether here; older unencrypted bundles need the running console")
		return
	}
	st, err := backup.StageUpload(r.Context(), rc.dataDir, io.MultiReader(bytes.NewReader(head[:n]), body), string(raw), uploadFileName(r))
	if errors.Is(err, backup.ErrBadPassword) {
		recoveryError(w, http.StatusBadRequest, "bad_password", err.Error())
		return
	}
	rc.restore(w, st, err)
}

// restore requests the staged restore and starts the server again at once.
func (rc *recovery) restore(w http.ResponseWriter, st backup.Staged, err error) {
	if err == nil {
		err = backup.PrepareStaged(rc.dataDir, st.ID, rc.settings().Server)
	}
	if err == nil {
		err = backup.RequestStaged(rc.dataDir, st.ID)
	}
	if err != nil {
		if st.ID != "" {
			_ = backup.DiscardStaged(rc.dataDir, st.ID)
		}
		recoveryError(w, http.StatusBadRequest, "invalid_backup", err.Error())
		return
	}
	rc.log.Info("recovery: restoring a backup from the startup page", "source", st.Source)
	recoveryJSON(w, http.StatusAccepted, st)
	rc.front.retryNow()
}

func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// uploadFileName is the uploaded file's name from X-Ether-Backup-Name.
func uploadFileName(r *http.Request) string {
	name := strings.TrimSpace(r.Header.Get("X-Ether-Backup-Name"))
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if len(name) > 200 || strings.ContainsAny(name, "\x00\r\n") {
		return ""
	}
	return name
}
