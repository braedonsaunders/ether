package vela

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

func viewerWire(v *frontend.Viewer) object {
	id, _ := ids.GUID(ids.User, v.ID)
	// The viewer value includes current policy; revisions change when those facts do.
	return object{"id": id, "name": v.Username, "policyRevision": hashJSON(v)}
}
func (i *instance) tokens(w http.ResponseWriter, r *http.Request, access, refresh string, d frontend.Device) {
	resolver, ok := i.host.Identity.(frontend.DeviceIdentity)
	if !ok {
		fail(w, frontend.ErrUnauthorized)
		return
	}
	v, expires, err := resolver.ResolveDevice(r.Context(), access, d)
	if err != nil || v == nil {
		fail(w, frontend.ErrUnauthorized)
		return
	}
	i.tokenResult(w, r, access, refresh, d, v, expires)
}
func (i *instance) auth(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Username, Password, RefreshToken, Code, Browser, ExistingToken, UserCode, DeviceCode, Secret string
		Device                                                                                       device `json:"device"`
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	ctx := i.identityContext(r)
	d := b.Device.neutral()
	path := strings.TrimPrefix(r.URL.Path, "/vela/v0/auth/")
	switch path {
	case "sign-in":
		a, e := i.host.Identity.SignIn(ctx, b.Username, b.Password, d)
		if e != nil {
			fail(w, e)
			return
		}
		i.tokens(w, r, a, "", d)
	case "refresh":
		a, f, e := i.host.Identity.Refresh(ctx, b.RefreshToken, d)
		if e != nil {
			fail(w, e)
			return
		}
		i.tokens(w, r, a, f, d)
	case "revoke":
		_, token, e := i.viewer(r)
		if e != nil {
			fail(w, e)
			return
		}
		if e = i.host.Identity.Revoke(ctx, token); e != nil {
			fail(w, e)
			return
		}
		send(w, 200, object{"accepted": true})
	case "web-code/redeem":
		service, ok := i.host.Identity.(interface {
			RedeemDeviceWebCode(context.Context, string, frontend.Device) (string, error)
		})
		if !ok {
			i.unavailable(w, r)
			return
		}
		a, e := service.RedeemDeviceWebCode(ctx, b.Code, d)
		if e != nil {
			fail(w, e)
			return
		}
		i.tokens(w, r, a, "", d)
	case "device", "quick-connect":
		if path == "device" && i.host.ViewerPortalURL == nil {
			i.unavailable(w, r)
			return
		}
		code, secret, e := i.host.Identity.QuickConnectInitiate(ctx, d)
		if e != nil {
			fail(w, e)
			return
		}
		details, e := i.host.Identity.QuickConnectDetails(ctx, secret)
		if e != nil {
			fail(w, e)
			return
		}
		if path == "quick-connect" {
			send(w, 200, object{"secret": secret, "code": code, "expiresAt": details.ExpiresAt.UTC().Format(time.RFC3339), "pollIntervalSeconds": 5})
			return
		}
		if i.host.ViewerPortalURL == nil {
			i.unavailable(w, r)
			return
		}
		url, e := i.host.ViewerPortalURL(ctx, r)
		if e != nil {
			fail(w, e)
			return
		}
		send(w, 200, object{"deviceCode": secret, "userCode": code, "verificationUrl": url, "expiresAt": details.ExpiresAt.UTC().Format(time.RFC3339), "pollIntervalSeconds": 5})
	case "device/poll", "quick-connect/poll":
		secret := b.Secret
		if path == "device/poll" {
			secret = b.DeviceCode
		}
		service, ok := i.host.Identity.(interface {
			QuickConnectPollDevice(context.Context, string, frontend.Device) (string, error)
		})
		if !ok {
			i.unavailable(w, r)
			return
		}
		a, e := service.QuickConnectPollDevice(ctx, secret, d)
		if errors.Is(e, frontend.ErrQuickConnectPending) {
			send(w, 200, object{"status": "pending"})
			return
		}
		if e != nil {
			fail(w, e)
			return
		}
		// Use a recorder so the token response keeps the same authoritative expiry.
		out := httptest.NewRecorder()
		i.tokens(out, r, a, "", d)
		if out.Code != 200 {
			send(w, out.Code, json.RawMessage(out.Body.Bytes()))
			return
		}
		var tokens object
		_ = json.Unmarshal(out.Body.Bytes(), &tokens)
		send(w, 200, object{"status": "approved", "tokens": tokens})
	case "device/approve", "quick-connect/approve":
		v, _, e := i.viewer(r)
		if e != nil {
			fail(w, e)
			return
		}
		code := b.Code
		if path == "device/approve" {
			code = b.UserCode
		}
		if e = i.host.Identity.QuickConnectApprove(ctx, v, code); e != nil {
			fail(w, e)
			return
		}
		send(w, 200, object{"accepted": true})
	}
}
