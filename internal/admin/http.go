package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxBodyBytes = 4 << 20 // 4 MiB: config documents are small

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// writeError emits the uniform API error shape:
//
//	{"error":{"code":"...","message":"...","fields":[{"field","message"}]}}
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func writeValidationError(w http.ResponseWriter, fields []FieldError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]any{
			"code": "validation", "message": "configuration is invalid", "fields": fields,
		},
	})
}

var errBadJSON = errors.New("invalid JSON body")

// decodeJSON reads one JSON document, rejecting unknown fields so typos in
// the UI surface as errors instead of being silently dropped.
func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errBadJSON
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %s", errBadJSON, err)
	}
	// Reject trailing documents.
	if dec.More() {
		return errBadJSON
	}
	return nil
}

// decodeJSONLenient is decodeJSON without unknown-field rejection, for
// test payloads that may carry extra client-side bookkeeping fields.
func decodeJSONLenient(r *http.Request, dst any) error {
	if r.Body == nil {
		return errBadJSON
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes+1))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %s", errBadJSON, err)
	}
	return nil
}

func isMutating(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// binaryUploads are the endpoints that take a raw file body
// (application/octet-stream) instead of JSON.
var binaryUploads = map[string]bool{
	"/api/adoption/backups/restore": true,
}

// csrfOK guards mutating requests: state changes require the custom
// X-Ether-Request header (which cross-site forms cannot set) and, when a
// body is present, a JSON content type, or for the binaryUploads a raw
// octet stream from a same-origin page. Authentication itself uses the
// X-Plex-Token header, never cookies, so there is no ambient credential
// for a CSRF attack to ride on.
func csrfOK(r *http.Request) bool {
	if !isMutating(r) {
		return true
	}
	if r.Header.Get("X-Ether-Request") != "1" {
		return false
	}
	if r.ContentLength > 0 {
		ct := r.Header.Get("Content-Type")
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		ct = strings.TrimSpace(ct)
		if strings.EqualFold(ct, "application/json") {
			return true
		}
		return r.Method == http.MethodPost && binaryUpload(r.URL.Path) &&
			strings.EqualFold(ct, "application/octet-stream") && sameOrigin(r)
	}
	return true
}

// binaryUpload reports whether path accepts a raw file body. Live TV
// playlist and guide uploads carry the source id in the path, so they
// cannot be listed as a single map key.
func binaryUpload(path string) bool {
	if binaryUploads[path] {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/api/livetv/sources/")
	if !ok {
		return false
	}
	id, part, ok := strings.Cut(rest, "/")
	return ok && id != "" && !strings.Contains(part, "/") && (part == "playlist" || part == "guide")
}

// sameOrigin reports whether a browser request's Origin, when it sent
// one, is this server as the browser addressed it.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}
