// Package stremio searches Stremio stream addons for torrent releases.
//
// A stream addon answers GET {base}/stream/{type}/{id}.json with streams
// for an IMDb title (type "movie", id "tt0816692") or episode (type
// "series", id "tt0903747:1:2"). Ether reads each stream's info hash, file
// name, size, seeders and any debrid cached marker, and streams the torrent
// through the owner's own debrid accounts; it never calls an addon's
// playback links. Ether ships no addon URLs: the owner pastes the install
// URL of an addon they chose, with or without the addon's own settings.
package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// userAgent identifies Ether honestly. An addon that serves only the
// Stremio app may refuse it; Ether never poses as a browser or as Stremio.
const userAgent = "Ether (+https://github.com/braedonsaunders/ether)"

const (
	defaultTimeout = 20 * time.Second
	maxManifest    = 1 << 20
	maxStreams     = 8 << 20
)

// BaseURL returns an addon's base URL from its install URL: a stremio://
// link becomes https://, and a trailing /manifest.json or /configure is
// removed. The path keeps the addon's settings segment, if any.
func BaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(raw, "stremio://"); ok {
		raw = "https://" + rest
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("stremio: the addon URL must be an http(s) or stremio:// install link")
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	p := strings.TrimRight(u.EscapedPath(), "/")
	for _, suffix := range []string{"/manifest.json", "/configure"} {
		p = strings.TrimSuffix(p, suffix)
	}
	u.RawPath, u.Path = "", ""
	if p != "" {
		if u.Path, err = url.PathUnescape(p); err != nil {
			return nil, errors.New("stremio: the addon URL path is not valid")
		}
		u.RawPath = p
	}
	return u, nil
}

// Manifest is the part of an addon manifest Ether reads.
type Manifest struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Types         []string          `json:"types"`
	IDPrefixes    []string          `json:"idPrefixes"`
	Resources     []json.RawMessage `json:"resources"`
	BehaviorHints struct {
		ConfigurationRequired bool `json:"configurationRequired"`
	} `json:"behaviorHints"`
}

// resource is a manifest resource given as an object; a plain string
// resource inherits the manifest's types and id prefixes.
type resource struct {
	Name       string   `json:"name"`
	Types      []string `json:"types"`
	IDPrefixes []string `json:"idPrefixes"`
}

// streamResource returns the addon's stream resource, if it has one.
func (m Manifest) streamResource() (resource, bool) {
	for _, raw := range m.Resources {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			if name == "stream" {
				return resource{Name: name, Types: m.Types, IDPrefixes: m.IDPrefixes}, true
			}
			continue
		}
		var r resource
		if json.Unmarshal(raw, &r) == nil && r.Name == "stream" {
			if r.Types == nil {
				r.Types = m.Types
			}
			if r.IDPrefixes == nil {
				r.IDPrefixes = m.IDPrefixes
			}
			return r, true
		}
	}
	return resource{}, false
}

// Check reports why Ether cannot search the addon, or nil.
func (m Manifest) Check() error {
	if m.BehaviorHints.ConfigurationRequired {
		return errors.New("the addon must be configured first: open its configure page and paste the install link it gives you")
	}
	r, ok := m.streamResource()
	if !ok {
		return errors.New("the addon does not provide streams")
	}
	if !contains(r.Types, "movie") && !contains(r.Types, "series") {
		return errors.New("the addon does not provide movie or series streams")
	}
	if !m.takesIDs("tt") && !m.takesIDs("tmdb") {
		anime := false
		for _, db := range animeDatabases {
			anime = anime || m.takesIDs(db)
		}
		if !anime {
			return errors.New("the addon does not look titles up by IMDb, TMDB, Kitsu, MyAnimeList, AniList or AniDB id")
		}
	}
	return nil
}

// takesIDs reports whether the addon's streams are looked up by ids with
// prefix ("tt", "tmdb", "kitsu"). An addon that lists no prefixes is
// asked by IMDb id only, as before prefixes were read.
func (m Manifest) takesIDs(prefix string) bool {
	r, ok := m.streamResource()
	if !ok {
		return false
	}
	if len(r.IDPrefixes) == 0 {
		return prefix == "tt"
	}
	for _, p := range r.IDPrefixes {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(p), ":"), prefix) {
			return true
		}
	}
	return false
}

// serves reports whether the addon answers stream requests of a type.
func (m Manifest) serves(kind string) bool {
	r, ok := m.streamResource()
	return ok && (len(r.Types) == 0 || contains(r.Types, kind))
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// FetchManifest reads and checks the manifest of the addon at rawURL.
func FetchManifest(ctx context.Context, client *http.Client, rawURL string) (Manifest, error) {
	base, err := BaseURL(rawURL)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := getJSON(ctx, client, base, "/manifest.json", maxManifest, &m); err != nil {
		return Manifest{}, err
	}
	if m.ID == "" && len(m.Resources) == 0 {
		return Manifest{}, fmt.Errorf("stremio: %s did not answer with an addon manifest", base.Host)
	}
	return m, m.Check()
}

// httpError is a non-2xx addon answer.
type httpError struct {
	host   string
	status int
	retry  string
}

func (e *httpError) Error() string {
	if e.status == http.StatusForbidden || e.status == http.StatusUnauthorized {
		return fmt.Sprintf("stremio: %s refused Ether (HTTP %d); some public addon instances serve only the Stremio app, so use an instance that allows servers or host the addon yourself", e.host, e.status)
	}
	return fmt.Sprintf("stremio: %s answered HTTP %d", e.host, e.status)
}

// getJSON decodes base+suffix into out. Errors name only the addon host:
// an addon URL may carry the owner's settings, including keys.
func getJSON(ctx context.Context, client *http.Client, base *url.URL, suffix string, limit int64, out any) error {
	u := *base
	u.Path += suffix
	if u.RawPath != "" {
		u.RawPath += suffix
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("stremio: %s: invalid request", base.Host)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("stremio: %s: %w", base.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &httpError{host: base.Host, status: resp.StatusCode, retry: resp.Header.Get("Retry-After")}
	}
	body := io.LimitReader(resp.Body, limit+1)
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("stremio: %s: %w", base.Host, err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("stremio: %s answered with more than %d bytes", base.Host, limit)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("stremio: %s did not answer with JSON", base.Host)
	}
	return nil
}
