package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
)

// Capture metadata is read separately from requests. Neither the archived
// requests nor the recorded responses are rewritten by binding a replay.
type capturedMetadata struct {
	user, client, version, device string
	items                         map[string]capturedItem
	ambiguous                     map[string]bool
}
type capturedItem struct {
	ID          string            `json:"Id"`
	Type        string            `json:"Type"`
	ProviderIDs map[string]string `json:"ProviderIds"`
}
type capturedRun struct {
	user    string
	values  map[string]string
	missing map[string]string
}

var capturedID = regexp.MustCompile(`^\$\{(id_[0-9]+)\}$`)
var capturedSlot = regexp.MustCompile(`\$\{(id_[0-9]+)\}`)

// LoadCapturedBindings must be called before discovery. The fixture directory
// is explicitly selected by the harness, never guessed from a working directory.
// Only successful, complete responses attached to this profile's exact requests
// contribute evidence. Missing fixtures remain errors, not fallback identities.
func (p *Protocol) LoadCapturedBindings(dir, profile string) error {
	if p.profiles[profile].Source != "captured" {
		return nil
	}
	m := &capturedMetadata{items: map[string]capturedItem{}, ambiguous: map[string]bool{}}
	for _, r := range p.Corpus(profile) {
		resp, err := conform.LoadFixture(filepath.Join(dir, profile, p.FixtureName(profile, r, true)+".json"))
		if err != nil {
			return fmt.Errorf("capture metadata %s: %w", r.Name, err)
		}
		if resp.Status != 200 || resp.Truncated {
			continue
		}
		if r.Method == http.MethodPost && r.Path == "/Users/AuthenticateByName" {
			var login struct {
				User struct {
					ID string `json:"Id"`
				}
				SessionInfo struct {
					UserID                                 string `json:"UserId"`
					Client, ApplicationVersion, DeviceName string
				}
			}
			if json.Unmarshal(resp.Body, &login) == nil && capturedID.MatchString(login.User.ID) && login.User.ID == login.SessionInfo.UserID {
				s := login.SessionInfo
				if m.user != "" && (m.user != login.User.ID || m.client != s.Client || m.version != s.ApplicationVersion || m.device != s.DeviceName) {
					return fmt.Errorf("ambiguous captured login identity")
				}
				m.user, m.client, m.version, m.device = login.User.ID, s.Client, s.ApplicationVersion, s.DeviceName
			}
		}
		var v any
		if json.Unmarshal(resp.Body, &v) != nil {
			continue
		}
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				b, _ := json.Marshal(x)
				var item capturedItem
				if json.Unmarshal(b, &item) == nil && capturedID.MatchString(item.ID) && item.Type != "" && len(item.ProviderIDs) > 0 {
					item.ProviderIDs = canonicalProviders(item.ProviderIDs)
					if old, ok := m.items[item.ID]; ok {
						a, _ := json.Marshal(old)
						b, _ := json.Marshal(item)
						if string(a) != string(b) {
							m.ambiguous[item.ID] = true
						}
					}
					m.items[item.ID] = item
				}
				for _, v := range x {
					walk(v)
				}
			case []any:
				for _, v := range x {
					walk(v)
				}
			}
		}
		walk(v)
	}
	// A provider identifier cannot distinguish multiple captured editions/copies.
	for a, x := range m.items {
		for b, y := range m.items {
			if a != b && x.Type == y.Type && sharedProvider(x, y) {
				m.ambiguous[a] = true
				m.ambiguous[b] = true
			}
		}
	}
	if p.captures == nil {
		p.captures = map[string]*capturedMetadata{}
	}
	p.captures[profile] = m
	return nil
}
func canonicalProviders(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}
func sharedProvider(a, b capturedItem) bool {
	for k, v := range a.ProviderIDs {
		if b.ProviderIDs[k] == v {
			return true
		}
	}
	return false
}
func matchingItem(a, b capturedItem) bool {
	if a.Type != b.Type || !sharedProvider(a, b) {
		return false
	}
	for k, v := range a.ProviderIDs {
		if w := b.ProviderIDs[k]; w != "" && w != v {
			return false
		}
	}
	return true
}
func captureScope(base, token, profile string) [32]byte {
	return sha256.Sum256([]byte(strings.TrimRight(base, "/") + "\x00" + profile + "\x00" + token))
}

// Discovery is credential scoped. Neither caller-supplied aliases nor a previous
// server's discovery can confer ownership of a captured user or item.
func (p *Protocol) discoverCaptured(ctx context.Context, c *http.Client, base, token, profile string) conform.DiscoveredIDs {
	out := conform.DiscoveredIDs{Values: map[string]string{}}
	run := capturedRun{values: map[string]string{}, missing: map[string]string{}}
	defer func() {
		p.captureMu.Lock()
		defer p.captureMu.Unlock()
		if p.captureRuns == nil {
			p.captureRuns = map[[32]byte]capturedRun{}
		}
		p.captureRuns[captureScope(base, token, profile)] = run
	}()
	m := p.captures[profile]
	if m == nil {
		out.Warnings = append(out.Warnings, "capture metadata unavailable")
		return out
	}
	get := func(path string, v any) bool {
		if token == "" || strings.ContainsAny(token, "\"\r\n") {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
		if err != nil {
			return false
		}
		req.Header.Set("X-Emby-Token", token)
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200 && json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v) == nil
	}
	var user struct {
		ID string `json:"Id"`
	}
	if !get("/Users/Me", &user) || !safeObservedID(user.ID) {
		out.Warnings = append(out.Warnings, "capture run user ownership unavailable")
		return out
	}
	run.user = user.ID
	if m.user != "" {
		run.values[strings.TrimSuffix(strings.TrimPrefix(m.user, "${"), "}")] = user.ID
	}
	var listing struct{ Items []capturedItem }
	if !get("/Items?Recursive=true&Fields=ProviderIds", &listing) {
		out.Warnings = append(out.Warnings, "capture item discovery unavailable")
	}
	for i := range listing.Items {
		listing.Items[i].ProviderIDs = canonicalProviders(listing.Items[i].ProviderIDs)
	}
	for alias, item := range m.items {
		key := capturedID.FindStringSubmatch(alias)[1]
		if m.ambiguous[alias] {
			run.missing[key] = "ambiguous captured provider identity"
			continue
		}
		matches := map[string]bool{}
		for _, candidate := range listing.Items {
			if safeObservedID(candidate.ID) && matchingItem(item, candidate) {
				matches[candidate.ID] = true
			}
		}
		if len(matches) != 1 {
			run.missing[key] = "missing or ambiguous current typed provider identity"
			continue
		}
		for id := range matches {
			run.values[key] = id
		}
	}
	keys := map[string]bool{}
	for _, r := range p.Corpus(profile) {
		if !r.FixtureOnly {
			for _, s := range capturedSlot.FindAllStringSubmatch(r.Path, -1) {
				keys[s[1]] = true
			}
		}
	}
	var diagnostics []string
	for key := range keys {
		if run.values[key] != "" {
			diagnostics = append(diagnostics, "capture binding "+key+": resolved")
		} else {
			if run.missing[key] == "" {
				run.missing[key] = "no archived identity binding"
			}
			diagnostics = append(diagnostics, "capture binding "+key+": "+run.missing[key])
		}
	}
	sort.Strings(diagnostics)
	out.Warnings = append(out.Warnings, diagnostics...)
	return out
}

func (p *Protocol) bindCaptured(base, token, profile string, r conform.Request, vars map[string]string) (conform.Request, map[string]string, error) {
	fail := func(reason string) (conform.Request, map[string]string, error) {
		return r, nil, fmt.Errorf("captured request binding: %s", reason)
	}
	m := p.captures[profile]
	if m == nil {
		return fail("metadata unavailable")
	}
	p.captureMu.Lock()
	run, ok := p.captureRuns[captureScope(base, token, profile)]
	p.captureMu.Unlock()
	if !ok || run.user == "" {
		return fail("current credential ownership unproven")
	}
	if v := vars["user_id"]; v != "" && v != run.user {
		return fail("cross-owner user variable")
	}
	if v := vars["token"]; v != "" && v != token {
		return fail("cross-owner credential variable")
	}
	if v := vars["access_token"]; v != "" && v != token {
		return fail("cross-owner credential variable")
	}
	if !safeObservedID(vars["device_id"]) {
		return fail("missing or invalid run device identity")
	}
	bound := map[string]string{}
	for k, v := range vars {
		bound[k] = v
	}
	for k, v := range run.values {
		if old := vars[k]; old != "" && old != v {
			return fail("conflicting alias " + k)
		}
		bound[k] = v
	}
	// Arbitrary variables must never fill a missing/ambiguous captured identity.
	check := func(text string) error {
		for _, s := range capturedSlot.FindAllStringSubmatch(text, -1) {
			if run.values[s[1]] == "" {
				reason := run.missing[s[1]]
				if reason == "" {
					reason = "no archived identity binding"
				}
				return fmt.Errorf("%s: %s", s[1], reason)
			}
		}
		return nil
	}
	if err := check(r.Path + string(r.Body)); err != nil {
		return fail(err.Error())
	}
	if m.device == "" || strings.ContainsAny(m.device, "\"\r\n${}") {
		return fail("missing captured device name")
	}
	bound["device"] = m.device
	headers := map[string]http.Header{}
	for k, h := range r.Headers {
		headers[k] = h.Clone()
	}
	r.Headers = headers
	for k, vs := range headers[profile+"-json"] {
		for i, v := range vs {
			if err := check(v); err != nil {
				return fail(err.Error())
			}
			if strings.Contains(v, `Version="${access_token}"`) {
				if headers[profile+"-json"].Get("User-Agent") != m.client+"/"+m.version || m.version == "" || strings.ContainsAny(m.version, "\"\r\n${}") || !strings.Contains(v, `Client="`+m.client+`"`) {
					return fail("missing or conflicting captured client/version provenance")
				}
				v = strings.ReplaceAll(v, `Version="${access_token}"`, `Version="`+m.version+`"`)
			}
			headers[profile+"-json"][k][i] = v
		}
	}
	return r, bound, nil
}
