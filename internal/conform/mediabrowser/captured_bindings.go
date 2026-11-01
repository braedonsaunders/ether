package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	// sizes holds a positive media-source byte size when every captured
	// response for that item agrees. Item-level Size is often absent.
	sizes map[string]int64
	// views are CollectionFolders. They have no provider identity. The
	// captured Name is often the fixture redaction marker, so CollectionType
	// is the identity that remains, checked against the name when the name
	// was actually recorded.
	views     map[string]capturedView
	viewClash map[string]bool
	// seasons are children of a captured series. The series alias must itself
	// bind; the season is then the one IndexNumber of that series.
	seasons     map[string]capturedSeason
	seasonClash map[string]bool
}
type capturedView struct {
	Name, CollectionType, SortName string
}
type capturedSeason struct {
	SeriesID, SeriesName string
	Index, Year          int
}
type discoveredItem struct {
	capturedItem
	SeriesID       string `json:"SeriesId"`
	SeriesName     string `json:"SeriesName"`
	IndexNumber    int    `json:"IndexNumber"`
	ProductionYear int    `json:"ProductionYear"`
}
type capturedItem struct {
	ID           string            `json:"Id"`
	Type         string            `json:"Type"`
	ProviderIDs  map[string]string `json:"ProviderIds"`
	Size         int64             `json:"Size,omitempty"`
	MediaSources []struct {
		Size int64 `json:"Size"`
	} `json:"MediaSources,omitempty"`
}
type capturedRun struct {
	user    string
	values  map[string]string
	missing map[string]string
}

var capturedID = regexp.MustCompile(`^\$\{(id_[0-9]+)\}$`)
var capturedSlot = regexp.MustCompile(`\$\{(id_[0-9]+)\}`)

// Only the exact string marker opts in. Other profiles have structured source
// provenance, historically ignored by replay, which must remain compatible.
func (p Profile) isCaptured() bool {
	var marker string
	return json.Unmarshal(p.Source, &marker) == nil && marker == "captured"
}

// LoadCapturedBindings must be called before discovery. The fixture directory
// is explicitly selected by the harness, never guessed from a working directory.
// Only successful, complete responses attached to this profile's exact requests
// contribute evidence. Missing fixtures remain errors, not fallback identities.
func (p *Protocol) LoadCapturedBindings(dir, profile string) error {
	if !p.profiles[profile].isCaptured() {
		return nil
	}
	m := &capturedMetadata{items: map[string]capturedItem{}, ambiguous: map[string]bool{}, sizes: map[string]int64{}, views: map[string]capturedView{}, viewClash: map[string]bool{}, seasons: map[string]capturedSeason{}, seasonClash: map[string]bool{}}
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
				recordCapturedFolder(m, x)
				recordCapturedSeason(m, x)
				b, _ := json.Marshal(x)
				var item capturedItem
				if json.Unmarshal(b, &item) == nil && capturedID.MatchString(item.ID) && item.Type != "" && len(item.ProviderIDs) > 0 {
					size := editionSize(item)
					item.Size = 0
					item.MediaSources = nil
					item.ProviderIDs = canonicalProviders(item.ProviderIDs)
					if item.ProviderIDs == nil {
						m.ambiguous[item.ID] = true
					}
					if old, ok := m.items[item.ID]; ok {
						a, _ := json.Marshal(old)
						b, _ := json.Marshal(item)
						if string(a) != string(b) {
							m.ambiguous[item.ID] = true
						}
					}
					m.items[item.ID] = item
					if size > 0 {
						if prev, ok := m.sizes[item.ID]; ok && prev != size {
							m.ambiguous[item.ID] = true
							delete(m.sizes, item.ID)
						} else if !m.ambiguous[item.ID] {
							m.sizes[item.ID] = size
						}
					}
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
	// Distinct positive media-source sizes can: the lab's three Big Buck Bunny
	// items share TMDB 10378 and are separated only by MediaSources[].Size.
	for a, x := range m.items {
		for b, y := range m.items {
			if a != b && x.Type == y.Type && sharedProvider(x, y) && !distinctEditionSize(m, a, b) {
				m.ambiguous[a] = true
				m.ambiguous[b] = true
				delete(m.sizes, a)
				delete(m.sizes, b)
			}
		}
	}
	for id, item := range m.items {
		if size, ok := m.sizes[id]; ok && !m.ambiguous[id] {
			item.Size = size
			m.items[id] = item
		}
	}
	if p.captures == nil {
		p.captures = map[string]*capturedMetadata{}
	}
	p.captures[profile] = m
	return nil
}
func recordCapturedFolder(m *capturedMetadata, raw map[string]any) {
	id, _ := raw["Id"].(string)
	kind, _ := raw["Type"].(string)
	collection, _ := raw["CollectionType"].(string)
	if !capturedID.MatchString(id) || kind != "CollectionFolder" || collection == "" {
		return
	}
	view := capturedView{Name: stringField(raw["Name"]), CollectionType: collection, SortName: stringField(raw["SortName"])}
	if old, ok := m.views[id]; ok && old != view {
		m.viewClash[id] = true
	}
	m.views[id] = view
}

func recordCapturedSeason(m *capturedMetadata, raw map[string]any) {
	id, _ := raw["Id"].(string)
	kind, _ := raw["Type"].(string)
	series, _ := raw["SeriesId"].(string)
	index := jsonInt(raw["IndexNumber"])
	if !capturedID.MatchString(id) || kind != "Season" || !capturedID.MatchString(series) || index <= 0 {
		return
	}
	season := capturedSeason{SeriesID: series, SeriesName: stringField(raw["SeriesName"]), Index: index, Year: jsonInt(raw["ProductionYear"])}
	if old, ok := m.seasons[id]; ok && old != season {
		m.seasonClash[id] = true
	}
	m.seasons[id] = season
}

func stringField(v any) string {
	s, _ := v.(string)
	return s
}

func jsonInt(v any) int {
	n, ok := v.(float64)
	if !ok || n != float64(int(n)) {
		return 0
	}
	return int(n)
}

func redactedCapture(s string) bool { return strings.Contains(s, "[REDACTED]") }

func canonicalProviders(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if v != "" {
			key := strings.ToLower(k)
			if old, ok := out[key]; ok && old != v {
				return nil
			}
			out[key] = v
		}
	}
	return out
}
func editionSize(item capturedItem) int64 {
	if item.Size > 0 {
		return item.Size
	}
	var size int64
	for _, source := range item.MediaSources {
		if source.Size <= 0 {
			continue
		}
		if size != 0 && size != source.Size {
			return 0
		}
		size = source.Size
	}
	return size
}

func distinctEditionSize(m *capturedMetadata, a, b string) bool {
	sa, sb := m.sizes[a], m.sizes[b]
	return sa > 0 && sb > 0 && sa != sb
}

func editionSizeRequired(m *capturedMetadata, id string) bool {
	item, ok := m.items[id]
	if !ok || m.sizes[id] == 0 {
		return false
	}
	for other, candidate := range m.items {
		if other != id && candidate.Type == item.Type && sharedProvider(item, candidate) && distinctEditionSize(m, id, other) {
			return true
		}
	}
	return false
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
	if a.Size > 0 && b.Size != a.Size {
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

// capturedCredential is the MediaBrowser header the rest of this harness sends
// with a user access token. A bare X-Emby-Token is not the credential that
// minted the session. DeviceId is included only for this run's own device.
// Caller item aliases are not part of the header.
func capturedCredential(token, deviceID, client, device, version string) string {
	field := func(fallback, value string) string {
		if value == "" || strings.ContainsAny(value, "\"\r\n") {
			return fallback
		}
		return value
	}
	parts := []string{
		`Client="` + field("conformdiff", client) + `"`,
		`Device="` + field("conformdiff", device) + `"`,
		`Version="` + field("1", version) + `"`,
		`Token="` + token + `"`,
	}
	if safeObservedID(deviceID) {
		parts = append(parts, `DeviceId="`+deviceID+`"`)
	}
	return "MediaBrowser " + strings.Join(parts, ", ")
}

// Discovery is credential scoped. Neither caller-supplied aliases nor a previous
// server's discovery can confer ownership of a captured user or item.
// deviceID is this run's device claim, not an item alias.
func (p *Protocol) discoverCaptured(ctx context.Context, c *http.Client, base, token, profile, deviceID string) conform.DiscoveredIDs {
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
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Emby-Token", token)
		auth := capturedCredential(token, deviceID, m.client, m.device, m.version)
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Emby-Authorization", auth)
		if safeObservedID(deviceID) {
			req.Header.Set("X-Emby-Device-Id", deviceID)
		}
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return false
		}
		decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
		if decoder.Decode(v) != nil {
			return false
		}
		var extra any
		return decoder.Decode(&extra) == io.EOF
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
	var listing struct {
		Items []discoveredItem
	}
	if !get("/Items?Recursive=true&Fields=ProviderIds,MediaSources,SeriesName,ProductionYear", &listing) {
		listing.Items = nil
		out.Warnings = append(out.Warnings, "capture item discovery unavailable")
	}
	for i := range listing.Items {
		size := editionSize(listing.Items[i].capturedItem)
		listing.Items[i].ProviderIDs = canonicalProviders(listing.Items[i].ProviderIDs)
		listing.Items[i].Size = size
		listing.Items[i].MediaSources = nil
	}
	for alias, item := range m.items {
		key := capturedID.FindStringSubmatch(alias)[1]
		if m.ambiguous[alias] {
			run.missing[key] = "ambiguous captured provider identity"
			continue
		}
		wanted := item
		if !editionSizeRequired(m, alias) {
			// A unique provider identity does not need a byte size. Size is
			// required only to keep sibling editions from collapsing.
			wanted.Size = 0
		}
		matches := map[string]bool{}
		for _, candidate := range listing.Items {
			if safeObservedID(candidate.ID) && matchingItem(wanted, candidate.capturedItem) {
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
	bindCapturedViews(&run, m, func(v any) bool {
		return get("/Users/"+url.PathEscape(run.user)+"/Views", v)
	})
	bindCapturedSeasons(&run, m, listing.Items)
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

func bindCapturedViews(run *capturedRun, m *capturedMetadata, get func(any) bool) {
	var page struct {
		Items []struct {
			ID             string `json:"Id"`
			Name           string `json:"Name"`
			Type           string `json:"Type"`
			CollectionType string `json:"CollectionType"`
		}
	}
	loaded := run.user != "" && get(&page)
	for alias, view := range m.views {
		key := capturedID.FindStringSubmatch(alias)[1]
		if m.viewClash[alias] || view.CollectionType == "" {
			run.missing[key] = "ambiguous captured view"
			continue
		}
		if !loaded {
			run.missing[key] = "missing or ambiguous current view"
			continue
		}
		matches := map[string]bool{}
		for _, candidate := range page.Items {
			if candidate.Type != "" && candidate.Type != "CollectionFolder" {
				continue
			}
			if !safeObservedID(candidate.ID) || candidate.CollectionType != view.CollectionType {
				continue
			}
			// The fixture redacts Name. A recorded name still has to match.
			if view.Name != "" && !redactedCapture(view.Name) && candidate.Name != view.Name {
				continue
			}
			matches[candidate.ID] = true
		}
		if len(matches) != 1 {
			run.missing[key] = "missing or ambiguous current view"
			continue
		}
		for id := range matches {
			run.values[key] = id
		}
		delete(run.missing, key)
	}
}

func bindCapturedSeasons(run *capturedRun, m *capturedMetadata, items []discoveredItem) {
	for alias, season := range m.seasons {
		key := capturedID.FindStringSubmatch(alias)[1]
		series := capturedID.FindStringSubmatch(season.SeriesID)
		if m.seasonClash[alias] || series == nil {
			run.missing[key] = "ambiguous captured season"
			continue
		}
		parent := run.values[series[1]]
		if parent == "" {
			run.missing[key] = "missing or ambiguous current typed provider identity"
			continue
		}
		matches := map[string]bool{}
		for _, candidate := range items {
			if candidate.Type != "Season" || candidate.IndexNumber != season.Index || candidate.SeriesID != parent {
				continue
			}
			if season.SeriesName != "" && !redactedCapture(season.SeriesName) && candidate.SeriesName != "" && candidate.SeriesName != season.SeriesName {
				continue
			}
			if season.Year > 0 && candidate.ProductionYear > 0 && candidate.ProductionYear != season.Year {
				continue
			}
			if !safeObservedID(candidate.ID) {
				continue
			}
			matches[candidate.ID] = true
		}
		if len(matches) != 1 {
			run.missing[key] = "missing or ambiguous current season"
			continue
		}
		for id := range matches {
			run.values[key] = id
		}
		delete(run.missing, key)
	}
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
