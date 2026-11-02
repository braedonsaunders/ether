package extend

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// TextResolver looks up a plugin string for a locale. It returns false when
// neither the locale nor English has the key.
type TextResolver func(plugin, key, locale string) (string, bool)

// Response is the body of GET /api/extend/registry.
type Response struct {
	Version     uint64       `json:"version"`
	Descriptors []Descriptor `json:"descriptors"`
}

// Handler serves the registry to the owner console. Query parameters:
// kinds (comma separated; all when absent) and locale (for plugin strings).
// The ETag changes with the registry version and the locale, so the console
// can revalidate cheaply after an extend.changed event.
func Handler(r *Registry, resolve TextResolver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
			return
		}
		var kinds []Kind
		for _, raw := range strings.Split(req.URL.Query().Get("kinds"), ",") {
			if raw = strings.TrimSpace(raw); raw == "" {
				continue
			}
			k := Kind(raw)
			if !Known(k) {
				writeError(w, http.StatusBadRequest, "invalid", "unknown descriptor kind "+strconv.Quote(raw))
				return
			}
			kinds = append(kinds, k)
		}
		locale := strings.TrimSpace(req.URL.Query().Get("locale"))
		version := r.Version()
		etag := `"` + strconv.FormatUint(version, 10) + "-" + locale + "-" + req.URL.Query().Get("kinds") + `"`
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if match := req.Header.Get("If-None-Match"); match != "" && match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		ds := r.List(kinds...)
		for i := range ds {
			ds[i].Label = resolveText(ds[i].Label, locale, resolve)
			ds[i].Hint = resolveText(ds[i].Hint, locale, resolve)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(Response{Version: version, Descriptors: ds})
	})
}

func resolveText(t Text, locale string, resolve TextResolver) Text {
	if t.Plugin == "" || t.Key == "" || resolve == nil {
		return t
	}
	if s, ok := resolve(t.Plugin, t.Key, locale); ok {
		t.Resolved = s
	}
	return t
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}
