package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/mcp"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestOAuthAuthorizationDetailsIncludeRedirectHost(t *testing.T) {
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,is_owner,active,created_at,updated_at) VALUES(1,100,1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grants, err := identity.NewGrants(db, testMembers{db}, 24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	p, err := mcp.NewPairing(mcp.PairingOptions{
		DB:     db,
		Grants: grants,
		Authorize: func(context.Context) (int64, error) {
			return 1, nil
		},
		ClientIP: func(r *http.Request) (netip.Addr, bool) {
			if v := r.Header.Get("X-Client-IP"); v != "" {
				ip, err := netip.ParseAddr(v)
				return ip, err == nil
			}
			return netip.Addr{}, false
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	oauth, console := p.OAuthHandler(), p.Handler()
	call := func(h http.Handler, method, path, body, clientIP string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1:32400"+path, strings.NewReader(body))
		req.RemoteAddr = "10.0.0.1:1234"
		if clientIP != "" {
			req.Header.Set("X-Client-IP", clientIP)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Register an external https client and inspect authorizationDetails.
	reg := call(oauth, "POST", "/ether/oauth/register", `{"client_name":"External App","redirect_uris":["https://app.example.com/cb"]}`, "10.0.0.2")
	if reg.Code != 201 {
		t.Fatalf("register: %d %s", reg.Code, reg.Body)
	}
	var regDoc map[string]any
	_ = json.Unmarshal(reg.Body.Bytes(), &regDoc)
	clientID := regDoc["client_id"].(string)

	verifier := strings.Repeat("v", 43)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"https://app.example.com/cb"},
		"response_type":         {"code"},
		"scope":                 {"full"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {"http://127.0.0.1:32400/ether/mcp"},
	}
	auth := call(oauth, "GET", "/ether/oauth/authorize?"+q.Encode(), "", "10.0.0.2")
	if auth.Code != 303 {
		t.Fatalf("authorize: %d %s", auth.Code, auth.Body)
	}
	loc, _ := url.Parse(auth.Header().Get("Location"))
	reqID := loc.Query().Get("request")

	det := call(console, "GET", "/authorization?request="+reqID, "", "10.0.0.2")
	if det.Code != 200 {
		t.Fatalf("details: %d %s", det.Code, det.Body)
	}
	var doc map[string]any
	_ = json.Unmarshal(det.Body.Bytes(), &doc)
	if doc["redirectHost"] != "app.example.com" {
		t.Fatalf("redirectHost = %v, want app.example.com", doc["redirectHost"])
	}
	if doc["redirectLocal"] != false {
		t.Fatalf("redirectLocal = %v, want false", doc["redirectLocal"])
	}

	// Distinct ClientIPs behind the same RemoteAddr have independent rate-limit buckets.
	for i := 0; i < 10; i++ {
		if r := call(oauth, "POST", "/ether/oauth/register", `{"redirect_uris":["http://127.0.0.1:1234/cb"]}`, "10.0.0.50"); r.Code != 201 {
			t.Fatalf("attempt %d for 10.0.0.50 = %d, want 201", i, r.Code)
		}
	}
	if r := call(oauth, "POST", "/ether/oauth/register", `{"redirect_uris":["http://127.0.0.1:1234/cb"]}`, "10.0.0.50"); r.Code != 429 {
		t.Fatalf("11th attempt for 10.0.0.50 = %d, want 429", r.Code)
	}
	if r := call(oauth, "POST", "/ether/oauth/register", `{"redirect_uris":["http://127.0.0.1:1234/cb"]}`, "10.0.0.51"); r.Code != 201 {
		t.Fatalf("1st attempt for 10.0.0.51 = %d, want 201", r.Code)
	}
}
