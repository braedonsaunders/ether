// Package mediabrowser implements the Jellyfin and Emby protocol surfaces.
package mediabrowser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// PublicInfo is supplied by the host, so the server identity remains stable
// across restarts and is distinct for each protocol surface.
type PublicInfo struct {
	ID, ServerName, LocalAddress, Version, ProductName string
}

// PublicRoutes registers connection endpoints that do not require a grant.
// A user picker is intentionally not exposed until the host can supply its
// authorized list of visible viewers.
func PublicRoutes(r *route.Router, info PublicInfo) error {
	return publicRoutes(r, func() PublicInfo { return info })
}
func publicRoutes(r *route.Router, getInfo func() PublicInfo) error {
	handlers := []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"GET", "/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
			info := getInfo()
			address := info.LocalAddress
			if address == "" && r.Host != "" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				address = scheme + "://" + r.Host
			}
			if info.ProductName == "Emby Server" {
				jsonReply(w, map[string]any{"Id": info.ID, "ServerName": info.ServerName, "Version": info.Version, "LocalAddress": address, "LocalAddresses": []string{address}, "RemoteAddresses": []string{address}, "WanAddress": address})
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(struct {
				ID                     string `json:"Id"`
				ServerName             string `json:"ServerName"`
				LocalAddress           string `json:"LocalAddress"`
				Version                string `json:"Version"`
				ProductName            string `json:"ProductName"`
				OperatingSystem        string `json:"OperatingSystem"`
				StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
			}{info.ID, info.ServerName, address, info.Version, info.ProductName, "", true})
		}},
		{"GET", "/Branding/Configuration", func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, map[string]any{}) }},
		{"POST", "/System/Ping", func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, getInfo().ProductName) }},
		{"GET", "/web", webPage},
		{"GET", "/web/{path...}", webPage},
		{"GET", "/Users/Public", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte("[]"))
		}},
		{"GET", "/System/Ping", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(getInfo().ProductName)
		}},
	}
	for _, h := range handlers {
		if err := r.Handle(h.method, h.path, h.handler); err != nil {
			return err
		}
	}
	return nil
}

type prefixKey struct{}

// StripPrefix accepts the optional protocol prefix while preserving the rest
// of the request for the router. It does not mutate the incoming request.
func StripPrefix(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= len(prefix) && strings.EqualFold(r.URL.Path[:len(prefix)], prefix) && (len(r.URL.Path) == len(prefix) || r.URL.Path[len(prefix)] == '/') {
			clone := r.Clone(context.WithValue(r.Context(), prefixKey{}, prefix))
			u := *r.URL
			clone.URL = &u
			clone.URL.Path = r.URL.Path[len(prefix):]
			if clone.URL.Path == "" {
				clone.URL.Path = "/"
			}
			clone.URL.RawPath = ""
			r = clone
		}
		next.ServeHTTP(w, r)
	})
}

func webPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Vary", "Accept-Language")
	type copy struct{ heading, instruction string }
	translations := map[string]copy{
		"en": {"Connect to Ether", "Open a native Jellyfin or Emby app, add this server's address, and sign in with your Ether viewer account or Quick Connect."},
		"fr": {"Se connecter à Ether", "Ouvrez une application Jellyfin ou Emby native, ajoutez l’adresse de ce serveur et connectez-vous avec votre compte Ether ou Quick Connect."},
		"es": {"Conectar a Ether", "Abra una aplicación nativa de Jellyfin o Emby, agregue la dirección de este servidor e inicie sesión con su cuenta de Ether o Quick Connect."},
		"de": {"Mit Ether verbinden", "Öffnen Sie eine native Jellyfin- oder Emby-App, fügen Sie die Serveradresse hinzu und melden Sie sich mit Ihrem Ether-Konto oder Quick Connect an."},
	}
	lang := "en"
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		tag = strings.SplitN(tag, "-", 2)[0]
		if _, ok := translations[tag]; ok {
			lang = tag
			break
		}
	}
	c := translations[lang]
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="%s"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Ether</title><main><h1>%s</h1><p>%s</p></main></html>`, lang, c.heading, c.instruction)
}
