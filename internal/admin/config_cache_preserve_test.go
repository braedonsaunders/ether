package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
)

// A full-document save from another section must not copy the effective
// shared policy (what GET returns) into the node-local cache.
func TestPutConfigWithoutCacheKeepsStoredCachePolicies(t *testing.T) {
	nasDir := t.TempDir()
	s := newStore(t, func(c *config.Config) {
		seededConfig(c)
		c.Cache.Storage = "shared"
		c.Cache.Dir = nasDir
		c.Cache.BudgetBytes = 1 << 30
		p := c.Cache
		p.Dir, p.Storage = "", ""
		p.BudgetBytes = 100 << 30
		c.SharedCache = &p
	})
	h := admin.NewHandler(admin.Deps{Config: s, IsOwnerToken: storedOwner(s)})
	rec := serve(h, req(http.MethodGet, "/ether/api/config", ""))
	var env struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	decode(t, rec, &env)
	delete(env.Config, "cache")
	env.Config["server"] = mustReplace(t, env.Config["server"], "friendlyName", "Den")
	raw, _ := json.Marshal(env.Config)
	if rec := serve(h, req(http.MethodPut, "/ether/api/config", string(raw))); rec.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", rec.Code, rec.Body)
	}
	got := s.Get()
	if got.Server.FriendlyName != "Den" {
		t.Fatalf("section not saved: %q", got.Server.FriendlyName)
	}
	if got.Cache.BudgetBytes != 1<<30 || got.Cache.Dir != nasDir || got.Cache.Storage != "shared" {
		t.Fatalf("node-local cache changed: %+v", got.Cache)
	}
	if got.SharedCache == nil || got.SharedCache.BudgetBytes != 100<<30 {
		t.Fatalf("shared policy changed: %+v", got.SharedCache)
	}
}

func mustReplace(t *testing.T, raw json.RawMessage, key string, value any) json.RawMessage {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	obj[key] = value
	out, _ := json.Marshal(obj)
	return out
}
