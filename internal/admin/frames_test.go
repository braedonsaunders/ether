package admin_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
)

func TestDashboardFramePolicy(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	check := func(wantCSP, wantXFO string) {
		t.Helper()
		r := req(http.MethodGet, "/ether/", "")
		r.Header.Set("Origin", "https://untrusted.example")
		r.Header.Set("Referer", "https://untrusted.example/")
		rec := serve(h, r)
		csp := rec.Header().Get("Content-Security-Policy")
		if rec.Code != 200 || !strings.HasPrefix(csp, wantCSP+"; script-src 'self' ") || rec.Header().Get("X-Frame-Options") != wantXFO {
			t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
		}
	}
	check("frame-ancestors 'none'", "DENY")
	if err := store.Update(func(c *config.Config) error {
		c.Server.WebFrameOrigins = []string{"http://dashboard.example:8090/", "https://dashboard.example"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check("frame-ancestors http://dashboard.example:8090 https://dashboard.example", "")
	r := req(http.MethodGet, "/ether/api/config", "")
	if rec := serve(h, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("framing bypassed auth: %d", rec.Code)
	}
	r = req(http.MethodGet, "/ether/api/config", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	if rec := serve(h, r); rec.Code != http.StatusOK {
		t.Fatalf("owner access failed: %d", rec.Code)
	}
	for _, bad := range []string{"*", "https://*.example.com", "javascript:alert(1)", "https://user:pass@example.com", "https://example.com/path", "https://example.com?q=x", "https://example.com#", "https://example.com; frame-ancestors *", "https://example.com\r\nX-Evil: yes", "https://example.com:99999", "https://example.com:", "https://example.com?", "https://example.com/%2f"} {
		t.Run(bad, func(t *testing.T) {
			if err := store.Update(func(c *config.Config) error { c.Server.WebFrameOrigins = []string{bad}; return nil }); err != nil {
				t.Fatal(err)
			}
			check("frame-ancestors 'none'", "DENY")
			found := false
			for _, e := range admin.ValidateConfig(store.Get()) {
				if e.Field == "server.webFrameOrigins" {
					found = true
				}
			}
			if !found {
				t.Fatal("invalid origin accepted by config API")
			}
		})
	}
	if err := store.Update(func(c *config.Config) error {
		c.Server.WebFrameOrigins = strings.Fields(strings.Repeat("https://example.com ", 17))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check("frame-ancestors 'none'", "DENY")
}

// Every inline script the embedded console ships is allowed by hash, and
// nothing else inline is.
func TestConsoleScriptPolicyMatchesServedIndex(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	rec := serve(h, req(http.MethodGet, "/ether/", ""))
	if rec.Code != 200 {
		t.Skipf("embedded UI not built: %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	var scriptSrc string
	for _, d := range strings.Split(csp, ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == "script-src" {
			scriptSrc = strings.Join(f[1:], " ")
		}
	}
	if strings.Contains(scriptSrc, "unsafe-inline") || strings.Contains(scriptSrc, "unsafe-eval") || !strings.HasPrefix(scriptSrc, "'self'") {
		t.Fatalf("script-src %q", scriptSrc)
	}
	inline := regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	hashes := 0
	for _, m := range inline.FindAllStringSubmatch(rec.Body.String(), -1) {
		if strings.Contains(strings.ToLower(m[1]), "src=") {
			continue
		}
		sum := sha256.Sum256([]byte(m[2]))
		want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(scriptSrc, want) {
			t.Fatalf("inline script %q not allowed by %q", m[2], scriptSrc)
		}
		hashes++
	}
	if got := strings.Count(scriptSrc, "'sha256-"); got != hashes {
		t.Fatalf("policy allows %d inline scripts, page has %d", got, hashes)
	}
}
