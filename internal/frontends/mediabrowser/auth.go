package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
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
	p, _ := authhdr.MediaBrowser(r.Header.Get("Authorization"))
	if len(p) == 0 {
		p, _ = authhdr.MediaBrowser(r.Header.Get("X-Emby-Authorization"))
	}
	get := func(key, header string) string {
		if value := p[key]; value != "" {
			return value
		}
		return r.Header.Get(header)
	}
	client := get("Client", "X-Emby-Client")
	return frontend.Device{ID: get("DeviceId", "X-Emby-Device-Id"), Client: client, Product: client, Version: get("Version", "X-Emby-Client-Version"), DeviceName: get("Device", "X-Emby-Device-Name")}

}
func (i *instance) viewer(r *http.Request) (*frontend.Viewer, string, error) {
	token := authhdr.MediaBrowserToken(r)
	if token == "" {
		return nil, "", errors.New("missing credential")
	}
	v, err := i.host.Identity.Resolve(r.Context(), token)
	if err == nil && v != nil {
		scoped := r.PathValue("userId")
		if scoped == "" {
			values := route.Query(r.URL.Query(), "userId")
			if len(values) > 1 {
				return nil, "", errors.New("ambiguous user")
			}
			if len(values) == 1 {
				scoped = values[0]
			}
		}
		if scoped != "" {
			id, e := ids.ParseGUID(ids.User, scoped)
			if e != nil || id != v.ID {
				return nil, "", errors.New("viewer mismatch")
			}
		}
	}
	if err == nil && v != nil && i.host.ClientSeen != nil {
		i.host.ClientSeen(device(r).ID)
	}
	return v, token, err
}
func userDTO(v *frontend.Viewer) map[string]any {
	id, _ := ids.GUID(ids.User, v.ID)
	return map[string]any{"Id": id, "Name": v.Username, "Policy": map[string]any{"IsAdministrator": v.IsOwner, "IsDisabled": false, "AuthenticationProviderId": "Ether", "PasswordResetProviderId": "Ether", "EnableMediaPlayback": true, "EnableAudioPlaybackTranscoding": true, "EnableVideoPlaybackTranscoding": true, "EnablePlaybackRemuxing": true}, "Configuration": map[string]any{}}
}
func (i *instance) result(w http.ResponseWriter, r *http.Request, token string) {
	v, err := i.host.Identity.Resolve(r.Context(), token)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	jsonReply(w, map[string]any{"AccessToken": token, "ServerId": i.info.ID, "User": userDTO(v), "SessionInfo": map[string]any{}})
}
func identityError(w http.ResponseWriter, err error) {
	if errors.Is(err, frontend.ErrRateLimited) {
		http.Error(w, "Too Many Requests", 429)
	} else {
		http.Error(w, "Unauthorized", 401)
	}
}
func (i *instance) quickEnabled() bool {
	if i.host.Config != nil {
		if value, ok := i.host.Config()["quickConnect"].(bool); ok {
			return value
		}
	}
	return true
}
func (i *instance) routes() error {
	add := func(method, path string, h http.HandlerFunc) error { return i.router.Handle(method, path, h) }
	routes := []struct {
		m, p string
		h    http.HandlerFunc
	}{
		{"GET", "/System/Info", func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			jsonReply(w, map[string]any{"Id": i.info.ID, "ServerName": i.info.ServerName, "Version": i.info.Version, "ProductName": i.info.ProductName, "StartupWizardCompleted": true, "WebSocketPortNumber": i.listenerPort()})
		}},
		{"POST", "/Sessions/Capabilities/Full", func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			w.WriteHeader(204)
		}},
		{"GET", "/socket", i.socket},
		{"POST", "/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Username, Pw string }
			if decode(r, &body) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			token, err := i.host.Identity.SignIn(i.identityContext(r), body.Username, body.Pw, device(r))
			if err != nil {
				identityError(w, err)
				return
			}
			i.result(w, r, token)
		}},
		{"GET", "/Users/Me", func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			jsonReply(w, userDTO(v))
		}},
		{"POST", "/Sessions/Logout", func(w http.ResponseWriter, r *http.Request) {
			_, token, err := i.viewer(r)
			if err == nil {
				err = i.host.Identity.Revoke(r.Context(), token)
			}
			if err != nil {
				identityError(w, err)
				return
			}
			w.WriteHeader(204)
		}},
		{"GET", "/QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) { jsonReply(w, i.quickEnabled()) }},
		{"POST", "/QuickConnect/Initiate", func(w http.ResponseWriter, r *http.Request) {
			if !i.quickEnabled() {
				http.Error(w, "Quick Connect disabled", 401)
				return
			}
			dev := device(r)
			code, secret, err := i.host.Identity.QuickConnectInitiate(i.identityContext(r), dev)
			if err != nil {
				identityError(w, err)
				return
			}
			response := map[string]any{"Code": code, "Secret": secret, "Authenticated": false, "DeviceId": dev.ID, "DeviceName": dev.DeviceName, "AppName": dev.Client, "AppVersion": dev.Version, "DateAdded": time.Now().UTC()}
			if i.host.ViewerPortalURL != nil {
				if portal, err := i.host.ViewerPortalURL(r.Context(), r); err == nil && portal != "" {
					response["VerificationUrl"] = portal
				}
			}
			jsonReply(w, response)
		}},
		{"GET", "/QuickConnect/Connect", func(w http.ResponseWriter, r *http.Request) {
			secret := route.Query(r.URL.Query(), "secret")
			if len(secret) != 1 || secret[0] == "" {
				http.Error(w, "Bad Request", 400)
				return
			}
			details, err := i.host.Identity.QuickConnectDetails(i.identityContext(r), secret[0])
			if err != nil {
				identityError(w, err)
				return
			}
			jsonReply(w, map[string]any{"Authenticated": details.Approved, "Secret": details.Secret, "Code": details.Code, "DeviceId": details.Device.ID, "DeviceName": details.Device.DeviceName, "AppName": details.Device.Client, "AppVersion": details.Device.Version, "DateAdded": details.CreatedAt})
		}},
		{"POST", "/QuickConnect/Authorize", func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			user := route.Query(r.URL.Query(), "userId")
			if len(user) > 0 {
				id, e := ids.ParseGUID(ids.User, user[0])
				if e != nil || id != v.ID {
					http.Error(w, "Forbidden", 403)
					return
				}
			}
			code := route.Query(r.URL.Query(), "code")
			if len(code) != 1 {
				http.Error(w, "Bad Request", 400)
				return
			}
			if err = i.host.Identity.QuickConnectApprove(i.identityContext(r), v, code[0]); err != nil {
				identityError(w, err)
				return
			}
			jsonReply(w, true)
		}},
		{"POST", "/Users/AuthenticateWithQuickConnect", func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Secret string }
			if decode(r, &body) != nil || body.Secret == "" {
				http.Error(w, "Bad Request", 400)
				return
			}
			token, err := i.host.Identity.QuickConnectPoll(i.identityContext(r), body.Secret)
			if err != nil {
				identityError(w, err)
				return
			}
			i.result(w, r, token)
		}},
	}
	for _, r := range routes {
		if err := add(r.m, r.p, r.h); err != nil {
			return err
		}
	}
	return nil
}
