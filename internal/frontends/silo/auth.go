package silo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}
func (i *instance) identityContext(r *http.Request) context.Context {
	if i.host.ClientIP != nil {
		if ip, ok := i.host.ClientIP(r); ok {
			return frontend.WithClientIP(r.Context(), ip)
		}
	}
	return r.Context()
}
func device(r *http.Request) frontend.Device {
	client := r.Header.Get("X-Silo-Client")
	return frontend.Device{ID: r.Header.Get("X-Silo-Device-Id"), Client: client, Product: client, Version: r.Header.Get("X-Silo-Client-Version"), DeviceName: r.Header.Get("X-Silo-Device-Name"), Platform: r.Header.Get("X-Silo-Device-Platform")}
}
func (i *instance) viewer(r *http.Request) (*frontend.Viewer, string, error) {
	a := strings.Fields(r.Header.Get("Authorization"))
	if len(a) != 2 || !strings.EqualFold(a[0], "Bearer") || a[1] == "" {
		return nil, "", errors.New("missing credential")
	}
	v, err := i.host.Identity.Resolve(r.Context(), a[1])
	if err == nil && v != nil {
		if profile := r.Header.Get("X-Profile-Id"); profile != "" && profile != strconv.FormatInt(v.ID, 10) {
			return nil, "", errors.New("profile mismatch")
		}
	}
	if err == nil && v != nil && i.host.ClientSeen != nil {
		i.host.ClientSeen(device(r).ID)
	}
	return v, a[1], err
}
func user(v *frontend.Viewer) map[string]any {
	role := "user"
	if v.IsOwner {
		role = "admin"
	}
	return map[string]any{"id": strconv.FormatInt(v.ID, 10), "username": v.Username, "email": v.Email, "role": role, "permissions": []string{}, "download_allowed": false, "password_change_required": false}
}
func (i *instance) login(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decode(r, &b) != nil || b.Username == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	token, err := i.host.Identity.SignIn(i.identityContext(r), b.Username, b.Password, device(r))
	if err != nil {
		if errors.Is(err, frontend.ErrRateLimited) {
			http.Error(w, "Too Many Requests", 429)
		} else {
			http.Error(w, "Unauthorized", 401)
		}
		return
	}
	v, err := i.host.Identity.Resolve(r.Context(), token)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	refresh, err := i.host.Identity.IssueRefresh(r.Context(), token, device(r))
	if err != nil {
		_ = i.host.Identity.Revoke(r.Context(), token)
		http.Error(w, "Unavailable", 503)
		return
	}
	reply(w, 200, map[string]any{"access_token": token, "refresh_token": refresh, "expires_in": 3600, "user": user(v)})
}
func (i *instance) authRoutes() error {
	for _, entry := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"POST", "/api/v2/auth/login", i.login},
		{"POST", "/api/v2/auth/refresh", i.refresh},
		{"POST", "/api/v2/auth/logout", i.logout},
		{"GET", "/api/v2/auth/device/capability", i.deviceCapability},
		{"POST", "/api/v2/auth/device/start", i.deviceStart},
		{"POST", "/api/v2/auth/device/poll", i.devicePoll},
	} {
		if err := i.router.Handle(entry.method, entry.path, entry.handler); err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) refresh(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Token string `json:"refresh_token"`
	}
	if decode(r, &b) != nil || b.Token == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	access, refresh, err := i.host.Identity.Refresh(i.identityContext(r), b.Token, device(r))
	if err != nil {
		if errors.Is(err, frontend.ErrRateLimited) {
			http.Error(w, "Too Many Requests", 429)
		} else {
			http.Error(w, "Unauthorized", 401)
		}
		return
	}
	reply(w, 200, map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": 3600})
}
func (i *instance) logout(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Identity.Revoke(r.Context(), token) != nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) deviceCapability(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"state": "available", "protocol_versions": []int{2}, "remote_playback_handoff": false, "cancel": false, "opened_signal": false})
}
func (i *instance) deviceStart(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name      string `json:"device_name"`
		Platform  string `json:"device_platform"`
		Purpose   string `json:"client_purpose"`
		Temporary bool   `json:"temporary"`
	}
	if decode(r, &b) != nil || b.Temporary {
		http.Error(w, "Bad Request", 400)
		return
	}
	d := device(r)
	d.DeviceName = b.Name
	d.Platform = b.Platform
	if i.host.ViewerPortalURL == nil {
		http.Error(w, "Portal unavailable", 503)
		return
	}
	verify, err := i.host.ViewerPortalURL(r.Context(), r)
	if err != nil || verify == "" {
		http.Error(w, "Portal unavailable", 503)
		return
	}
	code, secret, err := i.host.Identity.QuickConnectInitiate(i.identityContext(r), d)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	complete := verify + "?code=" + url.QueryEscape(code)
	reply(w, 201, map[string]any{"device_code": secret, "user_code": code, "match_code": code, "verification_uri": verify, "verification_uri_complete": complete, "expires_at": time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339), "expires_in": 600, "interval": 5, "device_name": b.Name, "device_platform": b.Platform, "client_purpose": "device_login", "temporary": false})
}
func (i *instance) devicePoll(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Code string `json:"device_code"`
	}
	if decode(r, &b) != nil || b.Code == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	ctx := i.identityContext(r)
	details, err := i.host.Identity.QuickConnectDetails(ctx, b.Code)
	if err != nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if !details.Approved {
		reply(w, 200, map[string]any{"status": "pending", "poll_after": 5, "opened": false, "profile_id": "", "profile_token": "", "temporary": false})
		return
	}
	access, err := i.host.Identity.QuickConnectPoll(ctx, b.Code)
	if err != nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	v, err := i.host.Identity.Resolve(ctx, access)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	refresh, err := i.host.Identity.IssueRefresh(ctx, access, details.Device)
	if err != nil {
		_ = i.host.Identity.Revoke(ctx, access)
		http.Error(w, "Unavailable", 503)
		return
	}
	reply(w, 200, map[string]any{"status": "approved", "poll_after": 5, "profile_id": "", "profile_token": "", "temporary": false, "tokens": map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": 3600, "user": user(v)}})
}
