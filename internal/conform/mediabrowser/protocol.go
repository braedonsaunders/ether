// Package mediabrowser implements profile-driven Jellyfin and Emby probes.
package mediabrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
)

type Profile struct {
	Protocol string             `json:"protocol"`
	Client   string             `json:"client"`
	Headers  map[string]string  `json:"headers"`
	Flows    map[string][]Probe `json:"flows"`
}
type Probe struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body,omitempty"`
}
type Protocol struct {
	Dialect  string
	profiles map[string]Profile
}

// Load reads profiles from a directory supplied by the caller.
func Load(dialect, dir string) (*Protocol, error) {
	if dialect != "jellyfin" && dialect != "emby" && dialect != "silo" {
		return nil, fmt.Errorf("unknown protocol %q", dialect)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no profiles in %s", dir)
	}
	p := &Protocol{Dialect: dialect, profiles: map[string]Profile{}}
	for _, file := range files {
		b, e := os.ReadFile(file)
		if e != nil {
			return nil, e
		}
		var profile Profile
		if e = json.Unmarshal(b, &profile); e != nil {
			return nil, fmt.Errorf("%s: %w", file, e)
		}
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		if profile.Protocol != dialect || profile.Client == "" || !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`).MatchString(name) {
			return nil, fmt.Errorf("%s: invalid protocol or client", file)
		}
		if _, ok := p.profiles[name]; ok {
			return nil, fmt.Errorf("duplicate client %q", name)
		}
		p.profiles[name] = profile
	}
	return p, nil
}

var _ conform.Protocol = (*Protocol)(nil)

func (p *Protocol) Clients() map[string]conform.Client {
	out := map[string]conform.Client{}
	for name, profile := range p.profiles {
		out[name] = conform.Client{Name: profile.Client}
	}
	return out
}
func (p *Protocol) Corpus(profile string) []conform.Request {
	v, ok := p.profiles[profile]
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(v.Flows))
	for k := range v.Flows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	order := map[string]int{"connect": 0, "sign_in": 1, "home": 2, "library_browse": 3, "item_detail": 4, "play_direct": 5, "play_transcode": 6, "progress_reporting": 7, "mark_watched": 8, "stop": 9}
	sort.SliceStable(keys, func(i, j int) bool {
		a, aok := order[keys[i]]
		b, bok := order[keys[j]]
		if !aok {
			a = len(order)
		}
		if !bok {
			b = len(order)
		}
		return a < b
	})
	var out []conform.Request
	for _, k := range keys {
		for _, r := range v.Flows[k] {
			h := http.Header{}
			for key, value := range v.Headers {
				h.Set(key, value)
			}
			for key, value := range r.Headers {
				h.Set(key, value)
			}
			method := r.Method
			if method == "" {
				method = http.MethodGet
			}
			out = append(out, conform.Request{Name: k + "_" + r.Name, Method: method, Path: r.Path, Headers: map[string]http.Header{profile + "-json": h}, Body: r.Body})
		}
	}
	return out
}
func (p *Protocol) Formats() []bool { return []bool{true} }
func (p *Protocol) Discover(ctx context.Context, c *http.Client, base, token, profile string, align *conform.DiscoveredIDs) conform.DiscoveredIDs {
	// Provider identities are obtained from an item listing; absence is surfaced
	// as a warning rather than silently matching unrelated items.
	out := conform.DiscoveredIDs{Values: map[string]string{}}
	if p.Dialect == "silo" {
		out.Warnings = append(out.Warnings, "Silo item discovery is not yet configured")
		return out
	}
	q, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/Items?Recursive=true&Fields=ProviderIds", nil)
	if err != nil {
		out.Warnings = append(out.Warnings, err.Error())
		return out
	}
	if p.Dialect == "jellyfin" {
		q.Header.Set("Authorization", `MediaBrowser Client="conformdiff", Device="conformdiff", DeviceId="conformdiff", Version="1", Token="`+token+`"`)
	} else {
		q.Header.Set("X-Emby-Token", token)
	}
	resp, err := c.Do(q)
	if err != nil {
		out.Warnings = append(out.Warnings, "item discovery failed")
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out.Warnings = append(out.Warnings, fmt.Sprintf("item discovery returned HTTP %d", resp.StatusCode))
		return out
	}
	var data struct {
		Items []struct {
			ID          string            `json:"Id"`
			ProviderIDs map[string]string `json:"ProviderIds"`
		} `json:"Items"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&data); err != nil {
		out.Warnings = append(out.Warnings, "invalid item discovery response")
		return out
	}
	for _, item := range data.Items {
		for key, value := range item.ProviderIDs {
			key = strings.ToLower(key)
			if (key == "tmdb" || key == "imdb") && value != "" && item.ID != "" {
				out.Values["provider:"+key+":"+value] = item.ID
				if align == nil && out.Values["itemId"] == "" {
					out.Values["itemId"] = item.ID
					out.Values["item_id"] = item.ID
				}
			}
		}
	}
	if align != nil {
		for k, v := range align.Values {
			if strings.HasPrefix(k, "provider:") {
				if target, ok := out.Values[k]; ok {
					out.Values["id:"+v] = target
					if v == align.Values["itemId"] {
						out.Values["itemId"] = target
						out.Values["item_id"] = target
					}
				}
			}
		}
		if align.Values["itemId"] != "" && out.Values["itemId"] == "" {
			out.Warnings = append(out.Warnings, "reference item has no matching provider ID on target")
		}
	}
	return out
}
func (p *Protocol) Fetch(ctx context.Context, c *http.Client, base, token string, r conform.Request, profile string, vars map[string]string, jsonMode bool) (conform.Response, error) {
	path := r.Path
	for k, v := range vars {
		path = strings.ReplaceAll(path, "{{"+k+"}}", v)
		path = strings.ReplaceAll(path, "${"+k+"}", v)
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "{}") {
		return conform.Response{}, fmt.Errorf("unresolved or invalid request path %q", r.Path)
	}
	bodyText := string(r.Body)
	for key, value := range vars {
		escaped := strconv.Quote(value)
		escaped = escaped[1 : len(escaped)-1]
		bodyText = strings.ReplaceAll(bodyText, "${"+key+"}", escaped)
	}
	if strings.Contains(bodyText, "${") {
		return conform.Response{}, fmt.Errorf("unresolved request body template")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, strings.TrimRight(base, "/")+path, bytes.NewReader([]byte(bodyText)))
	if err != nil {
		return conform.Response{}, err
	}
	for k, values := range r.Headers[profile+"-json"] {
		for _, v := range values {
			for key, value := range vars {
				v = strings.ReplaceAll(v, "${"+key+"}", value)
				v = strings.ReplaceAll(v, "{{"+key+"}}", value)
			}
			v = strings.ReplaceAll(v, "${token}", token)
			v = strings.ReplaceAll(v, "{{token}}", token)
			if strings.Contains(v, "${") || strings.Contains(v, "{{") {
				return conform.Response{}, fmt.Errorf("unresolved header template %q", k)
			}
			req.Header.Add(k, v)
		}
	}
	if p.Dialect == "jellyfin" && token != "" {
		auth := req.Header.Get("Authorization")
		if strings.HasPrefix(auth, "MediaBrowser ") && !strings.Contains(strings.ToLower(auth), "token=") {
			if strings.ContainsAny(token, "\"\r\n") {
				return conform.Response{}, fmt.Errorf("invalid token")
			}
			req.Header.Set("Authorization", auth+`, Token="`+token+`"`)
		}
	}
	if token != "" && req.Header.Get("X-Emby-Token") == "" && req.Header.Get("Authorization") == "" {
		if p.Dialect == "silo" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else if p.Dialect == "jellyfin" {
			req.Header.Set("Authorization", `MediaBrowser Client="conformdiff", Device="conformdiff", DeviceId="conformdiff", Version="1", Token="`+token+`"`)
		} else {
			req.Header.Set("X-Emby-Token", token)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		return conform.Response{}, fmt.Errorf("request failed (%T)", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return conform.Response{}, err
	}
	if len(body) > 16<<20 {
		return conform.Response{}, fmt.Errorf("response exceeds 16 MiB limit")
	}
	return conform.Response{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}
func (p *Protocol) FixtureName(profile string, r conform.Request, jsonMode bool) string {
	return conform.FixtureName(profile, r.Name+"_"+r.Method+"_"+r.Path, jsonMode)
}
func (p *Protocol) Compare(a, b conform.Response) conform.Report { return conform.Compare(a, b) }

// Redact removes credentials and host-specific values before fixture storage.
func (p *Protocol) Redact(r conform.Response) conform.Response {
	r.Headers = r.Headers.Clone()
	for k := range r.Headers {
		n := strings.ToLower(k)
		if strings.Contains(n, "token") || strings.Contains(n, "authorization") || strings.Contains(n, "cookie") || strings.Contains(n, "server-id") || n == "location" || n == "content-location" {
			r.Headers.Del(k)
		}
	}
	var v any
	if json.Unmarshal(r.Body, &v) == nil {
		scrub(v)
		if b, err := json.Marshal(v); err == nil {
			r.Body = b
		}
	} else {
		r.Body = redactText(conform.Redact(r.Body))
	}
	return r
}
func scrub(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			n := strings.ToLower(k)
			if strings.Contains(n, "token") || strings.Contains(n, "password") || n == "pw" || n == "username" || n == "serverid" || n == "server_id" || strings.HasSuffix(n, "path") || strings.HasSuffix(n, "url") || strings.HasSuffix(n, "uri") || strings.HasPrefix(n, "date") || strings.HasSuffix(n, "date") {
				x[k] = "[REDACTED]"
			} else {
				scrub(val)
			}
		}
	case []any:
		for _, val := range x {
			scrub(val)
		}
	}
}

var secretQuery = regexp.MustCompile(`(?i)([?&](?:api_key|access_token|token|authorization)=)[^&\s"']+`)

func redactText(b []byte) []byte { return secretQuery.ReplaceAll(b, []byte("${1}[REDACTED]")) }
