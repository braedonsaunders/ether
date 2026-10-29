// Package mediabrowser implements profile-driven Jellyfin and Emby probes.
package mediabrowser

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
)

type Profile struct {
	Protocol      string             `json:"protocol"`
	Client        string             `json:"client"`
	Headers       map[string]string  `json:"headers"`
	Flows         map[string][]Probe `json:"flows"`
	ProfileStatus string             `json:"profile_status"`
}
type Probe struct {
	Name           string            `json:"name"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Headers        map[string]string `json:"headers"`
	Body           json.RawMessage   `json:"body,omitempty"`
	BodyBase64     string            `json:"body_base64,omitempty"`
	RangeKind      string            `json:"range_kind,omitempty"`
	Binary         bool              `json:"binary,omitempty"`
	FixtureOnly    bool              `json:"fixture_only,omitempty"`
	ExpectedStatus int               `json:"expected_status,omitempty"`
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
		for _, requests := range profile.Flows {
			for _, request := range requests {
				if request.BodyBase64 != "" {
					if len(request.Body) > 0 {
						return nil, fmt.Errorf("%s: request has two bodies", file)
					}
					if _, err := base64.StdEncoding.DecodeString(request.BodyBase64); err != nil {
						return nil, fmt.Errorf("%s: invalid body_base64", file)
					}
				}
			}
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
			body := []byte(r.Body)
			if r.BodyBase64 != "" {
				decoded, err := base64.StdEncoding.DecodeString(r.BodyBase64)
				if err != nil {
					continue
				}
				body = decoded
			}
			base := conform.Request{Name: k + "_" + r.Name, Method: method, Path: r.Path, Headers: map[string]http.Header{profile + "-json": h}, Body: body, FixtureOnly: r.FixtureOnly, ExpectedStatus: r.ExpectedStatus}
			if r.RangeKind != "" {
				base.RangeKind = r.RangeKind
				out = append(out, base)
			} else if r.Binary || binaryPath(method, r.Path) {
				for _, kind := range []string{"start", "middle", "suffix"} {
					item := base
					item.Name += "_" + kind
					item.RangeKind = kind
					out = append(out, item)
				}
			} else {
				out = append(out, base)
			}
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
	if r.FixtureOnly {
		return conform.Response{}, fmt.Errorf("fixture-only request cannot be replayed")
	}
	if p.Dialect == "silo" && (r.Path == "/api/v2/playback/start" || (r.Method == http.MethodDelete && strings.HasPrefix(r.Path, "/api/v2/playback/"))) {
		if vars == nil {
			return conform.Response{}, fmt.Errorf("Silo playback requires mutable run variables")
		}
		id, err := newUUID()
		if err != nil {
			return conform.Response{}, err
		}
		if r.Path == "/api/v2/playback/start" {
			vars["playback_attempt_id"] = id
			for _, key := range []string{"playback_attempt_id_direct", "playback_attempt_id_transcode"} {
				if strings.Contains(string(r.Body), "${"+key+"}") {
					vars[key] = id
				}
			}
		} else {
			vars["stop_id"] = id
		}
	}
	path := r.Path
	for k, v := range vars {
		path = strings.ReplaceAll(path, "{{"+k+"}}", v)
		path = strings.ReplaceAll(path, "${"+k+"}", v)
	}
	path = strings.ReplaceAll(path, "${access_token}", url.QueryEscape(token))
	path = strings.ReplaceAll(path, "${token}", url.QueryEscape(token))
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "{}") {
		return conform.Response{}, fmt.Errorf("unresolved or invalid request path %q", r.Path)
	}
	bodyText := string(r.Body)
	for key, value := range vars {
		escaped := strconv.Quote(value)
		escaped = escaped[1 : len(escaped)-1]
		if strings.HasPrefix(strings.ToLower(r.Headers[profile+"-json"].Get("Content-Type")), "application/x-www-form-urlencoded") {
			escaped = url.QueryEscape(value)
		}
		bodyText = strings.ReplaceAll(bodyText, "${"+key+"}", escaped)
	}
	if strings.Contains(bodyText, "${") {
		return conform.Response{}, fmt.Errorf("unresolved request body template")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, strings.TrimRight(base, "/")+path, bytes.NewReader([]byte(bodyText)))
	if err != nil {
		return conform.Response{}, fmt.Errorf("invalid request URL")
	}
	for k, values := range r.Headers[profile+"-json"] {
		for _, v := range values {
			for key, value := range vars {
				v = strings.ReplaceAll(v, "${"+key+"}", value)
				v = strings.ReplaceAll(v, "{{"+key+"}}", value)
			}
			if strings.Contains(v, "${websocket_key}") {
				var key [16]byte
				if _, err := rand.Read(key[:]); err != nil {
					return conform.Response{}, err
				}
				v = strings.ReplaceAll(v, "${websocket_key}", base64.StdEncoding.EncodeToString(key[:]))
			}
			v = strings.ReplaceAll(v, "${access_token}", token)
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
	var rangeValue string
	if r.RangeKind != "" {
		switch r.RangeKind {
		case "captured":
			rangeValue = req.Header.Get("Range")
			if !boundedRange(rangeValue) {
				return conform.Response{}, fmt.Errorf("captured range is not bounded to 65536 bytes")
			}
		case "start":
			rangeValue = "bytes=0-65535"
		case "suffix":
			rangeValue = "bytes=-65536"
		case "middle":
			preflight := req.Clone(ctx)
			preflight.Header = preflight.Header.Clone()
			preflight.Header.Set("Range", "bytes=0-0")
			probe, e := c.Do(preflight)
			if e != nil {
				return conform.Response{}, fmt.Errorf("range preflight failed (%T)", e)
			}
			var first, last, total int64
			_, parseErr := fmt.Sscanf(probe.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &total)
			probe.Body.Close()
			if probe.StatusCode != http.StatusPartialContent || parseErr != nil || first != 0 || last != 0 || total <= 0 {
				return conform.Response{}, fmt.Errorf("range preflight returned invalid Content-Range")
			}
			start := total / 2
			end := start + 65535
			if end >= total {
				end = total - 1
			}
			rangeValue = fmt.Sprintf("bytes=%d-%d", start, end)
		default:
			return conform.Response{}, fmt.Errorf("invalid range kind")
		}
		req.Header.Set("Range", rangeValue)
	}
	resp, err := c.Do(req)
	if err != nil {
		return conform.Response{}, fmt.Errorf("request failed (%T)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return conform.Response{Status: resp.StatusCode, Headers: resp.Header.Clone(), Kind: "websocket"}, nil
	}
	limit := int64((16 << 20) + 1)
	if r.RangeKind != "" {
		limit = 65537
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return conform.Response{}, err
	}
	if r.RangeKind != "" {
		var defect *conform.ReferenceDefect
		truncated := false
		observedLength := len(body)
		if len(body) > 65536 {
			var first, last, total int64
			_, parseErr := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &total)
			if p.Dialect != "emby" || r.RangeKind != "suffix" || resp.StatusCode != http.StatusPartialContent || len(body) != 65537 || resp.Header.Get("Content-Length") != "65537" || parseErr != nil || first != 0 || last != 65536 || total <= 65537 {
				return conform.Response{}, fmt.Errorf("range response exceeds 65536 bytes")
			}
			truncated = true
			body = body[:65536]
			defect = &conform.ReferenceDefect{ID: "emby-suffix-range-ignored", Detail: "bytes=-65536 was served as bytes 0-65536", Observed: "Emby 4.10.2"}
		}
		headers := http.Header{}
		for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Cache-Control"} {
			if v := resp.Header.Values(k); len(v) > 0 {
				if k == "ETag" {
					headers.Set("ETag", "[PRESENT]")
				} else {
					headers[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
				}
			}
		}
		sum := sha256.Sum256(body)
		return conform.Response{Status: resp.StatusCode, Headers: headers, Kind: "binary-range", RangeKind: r.RangeKind, BodyLength: int64(observedLength), SHA256: hex.EncodeToString(sum[:]), Truncated: truncated, ReferenceDefect: defect, RequestRange: rangeValue}, nil
	}
	if len(body) > 16<<20 {
		return conform.Response{}, fmt.Errorf("response exceeds 16 MiB limit")
	}
	return conform.Response{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}
func (p *Protocol) FixtureName(profile string, r conform.Request, jsonMode bool) string {
	return conform.FixtureName(profile, r.Name+"_"+r.Method+"_"+r.Path, jsonMode)
}
func (p *Protocol) Compare(a, b conform.Response) conform.Report {
	if bytes.HasPrefix(bytes.TrimSpace(a.Body), []byte("#EXTM3U")) || bytes.HasPrefix(bytes.TrimSpace(b.Body), []byte("#EXTM3U")) {
		return conform.ComparePlaylist(a, b)
	}
	if a.Kind == "websocket" || b.Kind == "websocket" {
		return conform.CompareHeaders(a, b, []string{"Connection", "Upgrade"})
	}
	if a.Kind == "binary-range" || b.Kind == "binary-range" {
		return conform.CompareBinary(a, b)
	}
	report := conform.Compare(a, b)
	if p.Dialect == "emby" && emptyEmbyReferenceAddresses(a, b) {
		changed := false
		for n := range report.Differences {
			d := &report.Differences[n]
			switch d.Path {
			case "$/LocalAddress", "$/WanAddress", "$/LocalAddresses[]", "$/RemoteAddresses[]":
				if d.Kind == conform.KindExtra {
					d.Kind = conform.KindValue
					d.Blocking = false
					changed = true
				}
			}
		}
		if changed {
			report.ReferenceDefects = append(report.ReferenceDefects, conform.ReferenceDefect{ID: "emby-lab-unconfigured-addresses", Detail: "The lab has no configured advertised addresses; a target may advertise reachable addresses.", Observed: "Emby 4.10.2 lab"})
		}
	}
	return report
}

// CompareRequest treats the target's extra ETag on direct range streams as
// informational. Ether uses that validator for correct If-Range resumption.
func (p *Protocol) CompareRequest(req conform.Request, reference, target conform.Response) conform.Report {
	report := p.Compare(reference, target)
	path, _, _ := strings.Cut(req.Path, "?")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && (strings.EqualFold(parts[0], "jellyfin") || strings.EqualFold(parts[0], "emby")) {
		parts = parts[1:]
	}
	if req.Method != http.MethodGet || req.RangeKind == "" || reference.Kind != "binary-range" || target.Kind != "binary-range" || len(parts) != 3 || !strings.EqualFold(parts[0], "Videos") || parts[1] == "" || !(strings.EqualFold(parts[2], "stream") || strings.HasPrefix(strings.ToLower(parts[2]), "stream.")) {
		return report
	}
	for index := range report.Differences {
		diff := &report.Differences[index]
		if diff.Path == "$headers.ETag" && diff.Kind == conform.KindHeader && diff.Reference == false && diff.Target == true {
			diff.Kind = conform.KindValue
			diff.Blocking = false
		}
	}
	return report
}

func emptyEmbyReferenceAddresses(a, b conform.Response) bool {
	if a.Status != 200 || b.Status != 200 {
		return false
	}
	var ref, target map[string]any
	if json.Unmarshal(a.Body, &ref) != nil || json.Unmarshal(b.Body, &target) != nil {
		return false
	}
	for _, m := range []map[string]any{ref, target} {
		if _, ok := m["Id"].(string); !ok {
			return false
		}
		if _, ok := m["ServerName"].(string); !ok {
			return false
		}
		if _, ok := m["Version"].(string); !ok {
			return false
		}
	}
	for _, k := range []string{"LocalAddress", "WanAddress"} {
		if v, ok := ref[k]; ok && v != nil && v != "" {
			return false
		}
		v, ok := target[k].(string)
		if !ok || v == "" {
			return false
		}
	}
	for _, k := range []string{"LocalAddresses", "RemoteAddresses"} {
		if v, ok := ref[k]; ok && v != nil {
			list, ok := v.([]any)
			if !ok || len(list) != 0 {
				return false
			}
		}
		list, ok := target[k].([]any)
		if !ok || len(list) == 0 {
			return false
		}
		for _, v := range list {
			if s, ok := v.(string); !ok || s == "" {
				return false
			}
		}
	}
	return true
}

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
		v = scrub(v)
		if b, err := json.Marshal(v); err == nil {
			r.Body = b
		}
	} else {
		r.Body = redactText(conform.Redact(r.Body))
	}
	return r
}
func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			n := strings.ToLower(k)
			if strings.Contains(n, "token") || strings.Contains(n, "password") || n == "pw" || n == "username" || n == "serverid" || n == "server_id" || n == "servername" || n == "friendlyname" || n == "machinename" || strings.HasSuffix(n, "path") || strings.HasSuffix(n, "url") || strings.HasSuffix(n, "uri") || strings.HasPrefix(n, "date") || strings.HasSuffix(n, "date") {
				x[k] = "[REDACTED]"
			} else {
				x[k] = scrub(val)
			}
		}
	case []any:
		for i, val := range x {
			x[i] = scrub(val)
		}
	case string:
		if isoDate.MatchString(x) {
			return "[REDACTED]"
		}
		return string(redactText(conform.Redact([]byte(x))))
	}
	return v
}

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?:T|$)`)

var secretQuery = regexp.MustCompile(`(?i)([?&](?:api_key|access_token|token|authorization|st)=)[^&\s"']+`)

func redactText(b []byte) []byte { return secretQuery.ReplaceAll(b, []byte("${1}[REDACTED]")) }

func (p *Protocol) IgnorePaths() []string { return nil }

func binaryPath(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	path = strings.ToLower(strings.SplitN(path, "?", 2)[0])
	return path == "${segment_url}" || strings.HasSuffix(path, "/stream") || strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".m4s") || strings.HasSuffix(path, ".mp4")
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func boundedRange(value string) bool {
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return false
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < 0 {
		return false
	}
	if parts[0] == "" {
		return end > 0 && end <= 65536
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	return err == nil && start >= 0 && end >= start && end-start < 65536
}

// Observe carries the most recent successful playback negotiation into the
// subsequent stream and timeline probes for this profile. Failed negotiations
// clear old IDs so a stale session is never mistaken for the new one.
func (*Protocol) Observe(request conform.Request, response conform.Response, vars map[string]string) {
	path := strings.SplitN(request.Path, "?", 2)[0]
	if !strings.EqualFold(request.Method, http.MethodPost) || !strings.HasSuffix(strings.ToLower(path), "/playbackinfo") {
		return
	}
	delete(vars, "play_session_id")
	delete(vars, "media_source_id")
	if response.Status < 200 || response.Status >= 300 {
		return
	}
	var result struct {
		PlaySessionID string `json:"PlaySessionId"`
		MediaSources  []struct {
			ID string `json:"Id"`
		} `json:"MediaSources"`
	}
	if json.Unmarshal(response.Body, &result) != nil {
		return
	}
	if safeObservedID(result.PlaySessionID) {
		vars["play_session_id"] = result.PlaySessionID
	}
	if len(result.MediaSources) > 0 && safeObservedID(result.MediaSources[0].ID) {
		vars["media_source_id"] = result.MediaSources[0].ID
	}
}
func safeObservedID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

var _ conform.ResponseObserver = (*Protocol)(nil)
