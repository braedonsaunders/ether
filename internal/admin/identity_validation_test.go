package admin_test

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
)

func identityFields(errs []admin.FieldError) map[string]bool {
	out := map[string]bool{}
	for _, e := range errs {
		out[e.Field] = true
	}
	return out
}

func TestValidateClientIdentityAcceptsPolicy(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	cfg.ClientIdentity = config.ClientIdentity{
		SearchTV:     "sonarr",
		SearchMovies: "radarr",
		GrabTV:       config.IdentitySame,
		GrabMovies:   config.IdentitySame,
		UserAgent:    "Custom/1.0",
		Headers:      map[string]string{"Accept": "application/xml", "X-Api-Key": "k"},
	}
	cfg.Indexers = []config.Indexer{
		{ID: "inherited", Name: "Inherited", URL: "https://a.example", Enabled: true},
		{ID: "override", Name: "Override", URL: "https://b.example", Enabled: true, SearchIdentity: "prowlarr"},
		{
			ID: "custom", Name: "Custom", URL: "https://c.example", Enabled: true,
			GrabIdentity: "custom", UserAgent: "Indexer/2.0",
			Headers: map[string]string{"Authorization": "Bearer x"},
		},
	}
	if errs := admin.ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("valid policy rejected: %v", errs)
	}
}

func TestValidateClientIdentityRejections(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*config.Config)
		field string
	}{
		{"same in a search value", func(c *config.Config) { c.ClientIdentity.SearchTV = config.IdentitySame }, "clientIdentity.searchTv"},
		{"inherit in the global policy", func(c *config.Config) { c.ClientIdentity.SearchMovies = "inherit" }, "clientIdentity.searchMovies"},
		{"unknown preset", func(c *config.Config) { c.ClientIdentity.GrabMovies = "wat" }, "clientIdentity.grabMovies"},
		{
			"custom without a user agent",
			func(c *config.Config) { c.ClientIdentity.SearchTV = "custom" },
			"clientIdentity.userAgent",
		},
		{
			"header value with a newline",
			func(c *config.Config) {
				c.ClientIdentity = config.ClientIdentity{UserAgent: "Custom/1.0", Headers: map[string]string{"X-Trace": "a\r\nInjected: b"}}
			},
			"clientIdentity.headers",
		},
		{
			"framing header",
			func(c *config.Config) {
				c.ClientIdentity = config.ClientIdentity{UserAgent: "Custom/1.0", Headers: map[string]string{"Host": "evil.example"}}
			},
			"clientIdentity.headers",
		},
		{
			"invalid header name",
			func(c *config.Config) {
				c.ClientIdentity = config.ClientIdentity{UserAgent: "Custom/1.0", Headers: map[string]string{"Bad Header": "x"}}
			},
			"clientIdentity.headers",
		},
		{
			"indexer grab override same",
			func(c *config.Config) {
				c.Indexers = []config.Indexer{{ID: "a", URL: "https://a.example", GrabIdentity: config.IdentitySame}}
			},
			"indexers[0].grabIdentity",
		},
		{
			"indexer custom without a user agent",
			func(c *config.Config) {
				c.Indexers = []config.Indexer{{ID: "a", URL: "https://a.example", SearchIdentity: "custom"}}
			},
			"indexers[0].userAgent",
		},
	}
	for _, tt := range tests {
		cfg := config.Defaults(t.TempDir())
		tt.apply(&cfg)
		if fields := identityFields(admin.ValidateConfig(cfg)); !fields[tt.field] {
			t.Errorf("%s: expected %s, got %v", tt.name, tt.field, fields)
		}
	}
}

func TestValidateClientIdentityDefaultsAccepted(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	if errs := admin.ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("default identity policy rejected: %v", errs)
	}
	// Headers kept from an earlier custom selection are dormant, not invalid.
	cfg.ClientIdentity.Headers = map[string]string{"X-Api-Key": "kept"}
	if errs := admin.ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("dormant custom headers rejected: %v", errs)
	}
}
