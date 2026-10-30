package catalog

import (
	"reflect"

	"github.com/braedonsaunders/ether/internal/config"
)

// MatchesConfig reports whether the catalog actually installed the config
// captured by a held config-policy lease. UpdateConfig can reject invalid
// visibility rules and retain the previous config; checking the Store alone
// therefore does not prove the catalog read used that snapshot. This method is
// a prework check, not an authorization or a durable epoch fence.
func (c *Catalog) MatchesConfig(expected config.Catalog) bool {
	if c == nil {
		return false
	}
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return reflect.DeepEqual(c.cfg, expected)
}
