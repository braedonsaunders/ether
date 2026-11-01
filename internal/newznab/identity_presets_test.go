package newznab

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestIdentityPresetPersonas(t *testing.T) {
	want := map[string]string{
		IdentityProwlarr:  "Prowlarr/2.7.0.5723 (alpine 3.24.3)",
		IdentitySonarr:    "Sonarr/4.0.20.3014 (alpine 3.24.3)",
		IdentityRadarr:    "Radarr/6.4.4.10685 (alpine 3.24.3)",
		IdentityNZBHydra2: "NZBHydra2 9.2.0",
	}
	for name, ua := range want {
		id, _ := resolveIdentity(config.ClientIdentity{}, config.Indexer{SearchIdentity: name}, purposeSearch, mediaMovie, "1.2.3")
		if id.name != name || id.userAgent != ua {
			t.Errorf("%s identity = %q (%s)", name, id.userAgent, id.name)
		}
		if strings.Contains(id.userAgent, "Ether") {
			t.Errorf("%s persona leaks the Ether marker: %q", name, id.userAgent)
		}
	}
	for _, name := range []string{IdentityProwlarr, IdentitySonarr, IdentityRadarr} {
		id, _ := resolveIdentity(config.ClientIdentity{}, config.Indexer{SearchIdentity: name}, purposeSearch, mediaTV, "1.2.3")
		if id.headers["Accept"] != rssAccept["Accept"] {
			t.Errorf("%s Accept = %q, want the RSS accept header", name, id.headers["Accept"])
		}
	}
	hydra, _ := resolveIdentity(config.ClientIdentity{}, config.Indexer{SearchIdentity: IdentityNZBHydra2}, purposeSearch, mediaTV, "1.2.3")
	if hydra.headers["Accept"] != "application/xml" || hydra.headers["Content-Type"] != "application/xml" {
		t.Errorf("nzbhydra2 headers = %v", hydra.headers)
	}
	ether, _ := resolveIdentity(config.ClientIdentity{}, config.Indexer{}, purposeSearch, mediaTV, "1.2.3")
	if len(ether.headers) != 0 || ether.userAgent != "Ether/1.2.3" {
		t.Errorf("ether identity = %+v", ether)
	}
}

func TestIdentityEtherReportsBuildVersion(t *testing.T) {
	first, _ := resolveIdentity(config.ClientIdentity{}, config.Indexer{}, purposeSearch, mediaTV, "1.2.3")
	second, _ := resolveIdentity(config.ClientIdentity{}, config.Indexer{}, purposeSearch, mediaTV, "9.9.9")
	if first.userAgent != "Ether/1.2.3" || second.userAgent != "Ether/9.9.9" {
		t.Fatalf("build version not substituted: %q %q", first.userAgent, second.userAgent)
	}
	dev, _ := resolveIdentity(config.ClientIdentity{}, config.Indexer{}, purposeSearch, mediaTV, "")
	if dev.userAgent != "Ether/dev" {
		t.Fatalf("empty version = %q", dev.userAgent)
	}
}

func TestResolveIdentityPrecedence(t *testing.T) {
	policy := config.ClientIdentity{
		SearchTV:     IdentitySonarr,
		SearchMovies: IdentityRadarr,
		GrabTV:       config.IdentitySame,
		GrabMovies:   config.IdentitySame,
	}
	inherited := config.Indexer{}
	overridden := config.Indexer{SearchIdentity: IdentityProwlarr}
	partial := config.Indexer{GrabIdentity: IdentityNZBHydra2}

	cases := []struct {
		name    string
		policy  config.ClientIdentity
		indexer config.Indexer
		purpose identityPurpose
		media   identityMedia
		want    string
	}{
		{"inherited TV search", policy, inherited, purposeSearch, mediaTV, IdentitySonarr},
		{"inherited movie search", policy, inherited, purposeSearch, mediaMovie, IdentityRadarr},
		{"search override applies to TV", policy, overridden, purposeSearch, mediaTV, IdentityProwlarr},
		{"search override applies to movies", policy, overridden, purposeSearch, mediaMovie, IdentityProwlarr},
		{"partial override keeps grab inherited", policy, partial, purposeSearch, mediaTV, IdentitySonarr},
		{"partial override grab", policy, partial, purposeGrab, mediaTV, IdentityNZBHydra2},
		{"grab follows search TV", policy, inherited, purposeGrab, mediaTV, IdentitySonarr},
		{"grab follows search movie", policy, inherited, purposeGrab, mediaMovie, IdentityRadarr},
		{"zero policy is ether", config.ClientIdentity{}, inherited, purposeSearch, mediaMovie, IdentityEther},
		{"explicit grab preset", config.ClientIdentity{SearchTV: IdentitySonarr, GrabTV: IdentityProwlarr}, inherited, purposeGrab, mediaTV, IdentityProwlarr},
	}
	for _, tt := range cases {
		got, fellBack := resolveIdentity(tt.policy, tt.indexer, tt.purpose, tt.media, "1.0.0")
		if got.name != tt.want || fellBack {
			t.Errorf("%s: identity = %s (fallback %v), want %s", tt.name, got.name, fellBack, tt.want)
		}
	}
}

func TestResolveIdentityUnknownMediaFallsBackToEther(t *testing.T) {
	policy := config.ClientIdentity{SearchTV: IdentitySonarr, GrabTV: IdentitySonarr}
	indexer := config.Indexer{SearchIdentity: IdentityProwlarr, GrabIdentity: IdentityRadarr}
	got, fellBack := resolveIdentity(policy, indexer, purposeGrab, mediaUnknown, "1.0.0")
	if got.name != IdentityEther || !fellBack {
		t.Fatalf("unknown media grab = %s (fallback %v), want ether with a recorded fallback", got.name, fellBack)
	}
}

func TestResolveIdentityCustomPairs(t *testing.T) {
	policy := config.ClientIdentity{
		SearchMovies: IdentityCustom,
		GrabMovies:   config.IdentitySame,
		UserAgent:    "Global/1.0",
		Headers:      map[string]string{"X-Scope": "global"},
	}
	indexer := config.Indexer{UserAgent: "Indexer/2.0", Headers: map[string]string{"X-Scope": "indexer"}}

	global, _ := resolveIdentity(policy, indexer, purposeSearch, mediaMovie, "1.0.0")
	if global.userAgent != "Global/1.0" || global.headers["X-Scope"] != "global" {
		t.Fatalf("global custom = %+v", global)
	}
	grab, _ := resolveIdentity(policy, indexer, purposeGrab, mediaMovie, "1.0.0")
	if grab.userAgent != "Global/1.0" || grab.profile != profileServarr {
		t.Fatalf("same-as-search custom grab = %+v", grab)
	}

	overridden := config.Indexer{SearchIdentity: IdentityCustom, UserAgent: "Indexer/2.0", Headers: map[string]string{"X-Scope": "indexer"}}
	own, _ := resolveIdentity(policy, overridden, purposeSearch, mediaMovie, "1.0.0")
	if own.userAgent != "Indexer/2.0" || own.headers["X-Scope"] != "indexer" {
		t.Fatalf("indexer custom = %+v", own)
	}
}

func TestIdentitySummary(t *testing.T) {
	policy := config.ClientIdentity{
		SearchTV: IdentitySonarr, SearchMovies: IdentityRadarr,
		GrabTV: config.IdentitySame, GrabMovies: config.IdentitySame,
		UserAgent: "Custom/1.0", Headers: map[string]string{"X-Secret": "header-secret"},
	}
	summary := IdentitySummary(policy, []config.Indexer{
		{ID: "a", Name: "Fake", URL: "https://ix.example"},
		{ID: "b", Name: "Other", URL: "https://ix2.example", SearchIdentity: IdentityProwlarr},
	})
	for _, want := range []string{
		"TV search sonarr", "movie search radarr", "TV grab sonarr", "movie grab radarr",
		"Fake search inherit (TV sonarr, movies radarr), grab inherit (TV sonarr, movies radarr)",
		"Other search prowlarr (TV prowlarr, movies prowlarr)",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q lacks %q", summary, want)
		}
	}
	for _, secret := range []string{"header-secret", "Custom/1.0"} {
		if strings.Contains(summary, secret) {
			t.Errorf("summary leaks %q: %s", secret, summary)
		}
	}
}

func TestPresetUserAgent(t *testing.T) {
	if got := PresetUserAgent(IdentityEther, "1.2.3"); got != "Ether/1.2.3" {
		t.Fatalf("ether = %q", got)
	}
	if got := PresetUserAgent(IdentitySonarr, "1.2.3"); got != "Sonarr/4.0.20.3014 (alpine 3.24.3)" {
		t.Fatalf("sonarr = %q", got)
	}
	if got := PresetUserAgent(IdentityCustom, "1.2.3"); got != "" {
		t.Fatalf("custom = %q, want no fixed value", got)
	}
}

func TestIdentityPresetTableIsConsistent(t *testing.T) {
	for _, name := range IdentityPresets() {
		p, ok := identityPresets[name]
		if !ok {
			t.Fatalf("%s has no preset data", name)
		}
		if name == IdentityEther || name == IdentityCustom {
			continue
		}
		if p.userAgent == "" {
			t.Errorf("%s has no user agent", name)
		}
		if strings.Contains(p.userAgent, "Ether") {
			t.Errorf("%s persona contains the Ether marker", name)
		}
		if p.profile == profileCurrent {
			t.Errorf("%s persona kept the ether query profile", name)
		}
	}
}
