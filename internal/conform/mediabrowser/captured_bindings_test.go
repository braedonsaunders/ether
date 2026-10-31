package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
)

// These are synthetic offline peers, not playback evidence or replacement goldens.
func captureTestProtocol(t *testing.T) *Protocol {
	t.Helper()
	p, err := Load("jellyfin", filepath.Join("..", "profiles", "jellyfin"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.LoadCapturedBindings(filepath.Join("..", "..", "..", "testdata", "conform", "jellyfin"), "infuse-jellyfin"); err != nil {
		t.Fatal(err)
	}
	return p
}
func captureTestServer(t *testing.T, items []capturedItem, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users/Me":
			if r.Header.Get("X-Emby-Token") != "owned-token" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"Id":"owned-user"}`))
		case "/Items":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": items})
		default:
			*calls++
			if !strings.Contains(r.Header.Get("Authorization"), `Version="8.5.9"`) || !strings.Contains(r.Header.Get("Authorization"), `DeviceId="run-device"`) || !strings.Contains(r.Header.Get("Authorization"), `Token="owned-token"`) {
				t.Error("lost request identity")
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
}
func captureRequest(p *Protocol, sequence string) conform.Request {
	for _, r := range p.Corpus("infuse-jellyfin") {
		if r.Name == "captured_captured-"+sequence {
			return r
		}
	}
	panic("missing archived request")
}
func TestCapturedBindingsArchiveProvenance(t *testing.T) {
	p := captureTestProtocol(t)
	m := p.captures["infuse-jellyfin"]
	if m.user != "${id_1}" || m.version != "8.5.9" || m.client != "Infuse-Direct" || m.device != "Apple TV" {
		t.Fatal("archived login provenance changed")
	}
	for _, key := range []string{"${id_27}", "${id_49}", "${id_57}"} {
		if m.ambiguous[key] || m.sizes[key] == 0 {
			t.Fatalf("edition size evidence lost: %s ambiguous=%v size=%d", key, m.ambiguous[key], m.sizes[key])
		}
	}
	if m.sizes["${id_27}"] == m.sizes["${id_49}"] || m.sizes["${id_49}"] == m.sizes["${id_57}"] || m.sizes["${id_27}"] == m.sizes["${id_57}"] {
		t.Fatal("edition sizes were not distinct")
	}
	for _, key := range []string{"${id_2}", "${id_3}", "${id_8}"} {
		if _, ok := m.items[key]; ok {
			t.Fatalf("new metadata needs review: %s", key)
		}
	}
	calls := 0
	s := captureTestServer(t, nil, &calls)
	defer s.Close()
	p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
	vars := map[string]string{"user_id": "owned-user", "device_id": "run-device"}
	req := captureRequest(p, "000002")
	before, _ := json.Marshal(req)
	got, err := p.Fetch(context.Background(), s.Client(), s.URL, "owned-token", req, "infuse-jellyfin", vars, true)
	if err != nil || got.Status != 200 || calls != 1 {
		t.Fatalf("owned user fetch: %v status=%d calls=%d", err, got.Status, calls)
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) || len(vars) != 2 {
		t.Fatal("binding mutated recorded request or caller variables")
	}
}
func TestCapturedEditionSizeBindsOnlyTheMatchingCopy(t *testing.T) {
	p := captureTestProtocol(t)
	m := p.captures["infuse-jellyfin"]
	size := m.sizes["${id_49}"]
	if size == 0 {
		t.Fatal("missing archived edition size")
	}
	calls := 0
	s := captureTestServer(t, []capturedItem{{ID: "four-k", Type: "Movie", ProviderIDs: map[string]string{"Tmdb": "10378", "Imdb": "tt1254207"}, MediaSources: []struct {
		Size int64 `json:"Size"`
	}{{Size: size}}}}, &calls)
	defer s.Close()
	p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
	if _, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", captureRequest(p, "000036"), map[string]string{"device_id": "run-device"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", captureRequest(p, "000038"), map[string]string{"device_id": "run-device"}); err == nil {
		t.Fatal("a different edition size bound to the 4K copy")
	}
}

func TestCapturedBindingsRejectUnprovenScope(t *testing.T) {
	for _, tc := range []struct{ name, key, value, token, sequence string }{
		{"other user", "user_id", "other-user", "owned-token", "000002"},
		{"other credential", "access_token", "other-token", "owned-token", "000002"},
		{"other token alias", "token", "other-token", "owned-token", "000002"},
		{"unobserved credential", "", "", "other-token", "000002"},
		{"missing device", "device_id", "", "owned-token", "000002"},
		{"injected device", "device_id", "bad\"device", "owned-token", "000002"},
		{"conflicting user alias", "id_1", "other-user", "owned-token", "000002"},
		{"invented library", "id_2", "invented-library", "owned-token", "000005"},
		{"ambiguous edition", "id_49", "invented-item", "owned-token", "000039"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := captureTestProtocol(t)
			calls := 0
			s := captureTestServer(t, nil, &calls)
			defer s.Close()
			p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
			vars := map[string]string{"user_id": "owned-user", "device_id": "run-device"}
			if tc.key != "" {
				vars[tc.key] = tc.value
			}
			_, err := p.Fetch(context.Background(), s.Client(), s.URL, tc.token, captureRequest(p, tc.sequence), "infuse-jellyfin", vars, true)
			if err == nil || calls != 0 {
				t.Fatalf("unsafe dispatch: err=%v calls=%d", err, calls)
			}
		})
	}
}
func TestCapturedBindingsTypedProviderAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []capturedItem
		want  bool
	}{
		{"unique", []capturedItem{{ID: "owned-movie", Type: "Movie", ProviderIDs: map[string]string{"Tmdb": "45745"}}}, true},
		{"conflicting provider casing", []capturedItem{{ID: "case-conflict", Type: "Movie", ProviderIDs: map[string]string{"Tmdb": "45745", "tmdb": "other"}}}, false},
		{"wrong type", []capturedItem{{ID: "wrong-series", Type: "Series", ProviderIDs: map[string]string{"Tmdb": "45745"}}}, false},
		{"conflicting provider", []capturedItem{{ID: "wrong-movie", Type: "Movie", ProviderIDs: map[string]string{"Tmdb": "45745", "Imdb": "tt-wrong"}}}, false},
		{"two editions", []capturedItem{{ID: "one", Type: "Movie", ProviderIDs: map[string]string{"Tmdb": "45745"}}, {ID: "two", Type: "Movie", ProviderIDs: map[string]string{"Tmdb": "45745"}}}, false},
		{"missing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := captureTestProtocol(t)
			calls := 0
			s := captureTestServer(t, tc.items, &calls)
			defer s.Close()
			p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
			req := captureRequest(p, "000126")
			_, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", req, map[string]string{"device_id": "run-device"})
			if (err == nil) != tc.want {
				t.Fatalf("binding=%v want success=%v", err, tc.want)
			}
			if _, _, err = p.bindCaptured(s.URL+"/other", "owned-token", "infuse-jellyfin", req, map[string]string{"device_id": "run-device"}); err == nil {
				t.Fatal("cross-server mapping accepted")
			}
		})
	}
}
func TestCapturedBindingsMissingMetadataAndFailedRediscovery(t *testing.T) {
	p := captureTestProtocol(t)
	calls := 0
	s := captureTestServer(t, nil, &calls)
	defer s.Close()
	p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
	req := captureRequest(p, "000002")
	vars := map[string]string{"device_id": "run-device"}
	// Re-discovery after metadata loss must revoke the old run binding.
	delete(p.captures, "infuse-jellyfin")
	p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
	if _, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", req, vars); err == nil {
		t.Fatal("missing metadata accepted")
	}
}

func TestCapturedBindingsFiniteArchiveClassification(t *testing.T) {
	p := captureTestProtocol(t)
	calls := 0
	// Same single movie provider identity as the isolated app seed. No server
	// playback and no inference that three captured editions are interchangeable.
	s := captureTestServer(t, []capturedItem{{ID: "seed-movie", Type: "Movie", ProviderIDs: map[string]string{"Tmdb": "10378", "Imdb": "tt1254207"}}}, &calls)
	defer s.Close()
	ids := p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
	if !strings.Contains(strings.Join(ids.Warnings, "\n"), "id_49: missing or ambiguous current typed provider identity") {
		t.Fatal("missing explicit size mismatch diagnostic")
	}
	total, resolved := 0, 0
	for _, r := range p.Corpus("infuse-jellyfin") {
		if r.FixtureOnly {
			continue
		}
		total++
		_, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", r, map[string]string{"user_id": "owned-user", "device_id": "run-device"})
		if err == nil {
			resolved++
		}
	}
	if total != 47 || resolved != 11 {
		t.Fatalf("binding-only classification: total=%d resolved=%d unresolved=%d", total, resolved, total-resolved)
	}
	if calls != 0 {
		t.Fatal("classification dispatched a corpus request")
	}
	t.Log("immutable archive: 47 replayable templates; 11 identity-only bindings; 36 explicitly unresolved against isolated one-movie seed; no playback evidence")
}

func TestCapturedBindingsVersionProvenanceCannotBecomeToken(t *testing.T) {
	for _, mutation := range []string{"missing version", "different client", "different user agent"} {
		t.Run(mutation, func(t *testing.T) {
			p := captureTestProtocol(t)
			calls := 0
			s := captureTestServer(t, nil, &calls)
			defer s.Close()
			p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
			req := captureRequest(p, "000001")
			switch mutation {
			case "missing version":
				p.captures["infuse-jellyfin"].version = ""
			case "different client":
				p.captures["infuse-jellyfin"].client = "Other"
			case "different user agent":
				req.Headers["infuse-jellyfin-json"].Set("User-Agent", "Other/1")
			}
			_, err := p.Fetch(context.Background(), s.Client(), s.URL, "owned-token", req, "infuse-jellyfin", map[string]string{"device_id": "run-device"}, true)
			if err == nil || calls != 0 {
				t.Fatalf("unsafe version substitution: %v", err)
			}
		})
	}
}

func TestCapturedBindingsDiscardPartialDiscovery(t *testing.T) {
	for _, body := range []string{
		`{"Items":[{"Id":"partial-item","Type":"Movie","ProviderIds":{"Tmdb":"45745"}},{"Id":17}]}`,
		`{"Items":[{"Id":"partial-item","Type":"Movie","ProviderIds":{"Tmdb":"45745"}}`,
		`{"Items":[{"Id":"partial-item","Type":"Movie","ProviderIds":{"Tmdb":"45745"}}]}junk`,
	} {
		t.Run(body, func(t *testing.T) {
			p := captureTestProtocol(t)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/Users/Me" {
					_, _ = w.Write([]byte(`{"Id":"owned-user"}`))
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer s.Close()
			p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
			_, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", captureRequest(p, "000126"), map[string]string{"device_id": "run-device"})
			if err == nil {
				t.Fatal("partial or malformed discovery accepted")
			}
		})
	}
}

func TestCapturedBindingsProviderCaseCollision(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := canonicalProviders(map[string]string{"Tmdb": "45745", "tmdb": "other"}); got != nil {
			t.Fatalf("conflicting case selected a value: %v", got)
		}
	}
	if got := canonicalProviders(map[string]string{"Tmdb": "45745", "tmdb": "45745"}); got["tmdb"] != "45745" {
		t.Fatal("identical aliases should agree")
	}
	// New synthetic provenance fixture; never modify the shipped archive.
	p := &Protocol{Dialect: "jellyfin", profiles: map[string]Profile{"case-test": {Source: json.RawMessage(`"captured"`), Flows: map[string][]Probe{"captured": {{Name: "one", Method: "GET", Path: "/Items"}}}}}}
	dir := t.TempDir()
	r := p.Corpus("case-test")[0]
	err := conform.SaveFixture(filepath.Join(dir, "case-test"), p.FixtureName("case-test", r, true), conform.Response{Status: 200, Body: []byte(`{"Items":[{"Id":"${id_9}","Type":"Movie","ProviderIds":{"Tmdb":"45745","tmdb":"other"}}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.LoadCapturedBindings(dir, "case-test"); err != nil {
		t.Fatal(err)
	}
	if !p.captures["case-test"].ambiguous["${id_9}"] {
		t.Fatal("archived provider conflict was not marked ambiguous")
	}
}

func TestCapturedBindingsSourceMarkerCompatibility(t *testing.T) {
	for _, dialect := range []string{"jellyfin", "emby", "silo"} {
		t.Run(dialect, func(t *testing.T) {
			p, err := Load(dialect, filepath.Join("..", "profiles", dialect))
			if err != nil {
				t.Fatal(err)
			}
			for name, profile := range p.profiles {
				if profile.isCaptured() {
					if !(dialect == "jellyfin" && name == "infuse-jellyfin") && !(dialect == "silo" && name == "silo-appletv") {
						t.Fatalf("unexpected captured marker: %s/%s", dialect, name)
					}
				} else if err := p.LoadCapturedBindings("nonexistent-fixture-root", name); err != nil {
					t.Fatalf("noncaptured source must not load fixtures: %v", err)
				}
			}
		})
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`"captured"`, true}, {`"\u0063aptured"`, true},
		{`{"kind":"captured"}`, false}, {`["captured"]`, false}, {`true`, false}, {`17`, false}, {`null`, false},
		{`"Captured"`, false}, {`"captured "`, false}, {``, false}, {`"captured`, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := (Profile{Source: json.RawMessage(tc.raw)}).isCaptured(); got != tc.want {
				t.Fatalf("captured marker=%v want=%v", got, tc.want)
			}
		})
	}
}

// A peer that accepts only the session credential (Authorization token plus this
// run's DeviceId) must prove /Users/Me. A caller-supplied user id is not proof.
// Token-only discovery stays unproven, which is the recorded Infuse failure.
func TestCapturedUsersMeProvesRunCredential(t *testing.T) {
	p := captureTestProtocol(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")
		owned := r.Header.Get("X-Emby-Token") == "owned-token" && strings.Contains(auth, `Token="owned-token"`) && strings.Contains(auth, `DeviceId="run-device"`) && strings.Contains(auth, `Client="Infuse-Direct"`) && strings.Contains(auth, `Device="Apple TV"`) && !strings.Contains(auth, "${")
		if r.URL.Path == "/Users/Me" {
			if !owned {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "server-user", "Name": "viewer", "Policy": map[string]any{}})
			return
		}
		if !owned {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
	}))
	defer s.Close()
	align := &conform.DiscoveredIDs{Values: map[string]string{"device_id": "run-device", "user_id": "spoofed-user", "id_1": "spoofed-user"}}
	p.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", align)
	info := captureRequest(p, "000001")
	if _, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", info, map[string]string{"device_id": "run-device", "user_id": "spoofed-user"}); err == nil || !strings.Contains(err.Error(), "cross-owner user variable") {
		t.Fatal("caller user id was treated as ownership", err)
	}
	if _, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", info, map[string]string{"device_id": "run-device", "user_id": "server-user"}); err != nil {
		t.Fatal(err)
	}
	segments := captureRequest(p, "000036")
	if _, _, err := p.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", segments, map[string]string{"device_id": "run-device", "user_id": "server-user"}); err == nil || strings.Contains(err.Error(), "ownership unproven") || !strings.Contains(err.Error(), "missing or ambiguous current typed provider identity") {
		t.Fatal("MediaSegments did not get past ownership", err)
	}
	bare := captureTestProtocol(t)
	bare.Discover(context.Background(), s.Client(), s.URL, "owned-token", "infuse-jellyfin", nil)
	if _, _, err := bare.bindCaptured(s.URL, "owned-token", "infuse-jellyfin", info, map[string]string{"device_id": "run-device", "user_id": "server-user"}); err == nil || !strings.Contains(err.Error(), "current credential ownership unproven") {
		t.Fatal("token-only discovery proved ownership", err)
	}
}
