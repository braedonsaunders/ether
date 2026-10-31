package catalog

import (
	"reflect"
	"slices"

	"github.com/braedonsaunders/ether/internal/config"
)

// Non-zero size gives every live configuration incarnation a distinct identity.
type configVersion struct{ marker byte }

// ConfigEvidence is an opaque, non-authorizing observation of the exact
// installed config incarnation. It detects change-and-revert as well as a
// currently different value. It is valid only for the issuing Catalog.
type ConfigEvidence struct {
	owner   *Catalog
	version *configVersion
}

func cloneCatalogConfig(cfg config.Catalog) config.Catalog {
	cfg.Exclusions = slices.Clone(cfg.Exclusions)
	for i := range cfg.Exclusions {
		cfg.Exclusions[i].Rules = slices.Clone(cfg.Exclusions[i].Rules)
		for j := range cfg.Exclusions[i].Rules {
			cfg.Exclusions[i].Rules[j].Value = slices.Clone(cfg.Exclusions[i].Rules[j].Value)
		}
	}
	return cfg
}

// CaptureConfigEvidence compares while holding cfgMu. The caller must own or
// synchronize expected for the duration of the call.
func (c *Catalog) CaptureConfigEvidence(expected config.Catalog) (ConfigEvidence, bool) {
	if c == nil {
		return ConfigEvidence{}, false
	}
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	if c.cfgVersion == nil || !reflect.DeepEqual(c.cfg, expected) {
		return ConfigEvidence{}, false
	}
	return ConfigEvidence{owner: c, version: c.cfgVersion}, true
}

// Current is a postwork check; it must not be treated as a durable policy
// fence or used instead of checking the policy epoch transaction.
func (e ConfigEvidence) Current() bool {
	if e.owner == nil || e.version == nil {
		return false
	}
	e.owner.cfgMu.RLock()
	defer e.owner.cfgMu.RUnlock()
	return e.owner.cfgVersion == e.version
}

// MatchesConfig reports whether the catalog actually installed the config
// captured by a held config-policy lease. UpdateConfig can reject invalid
// visibility rules and retain the previous config; checking the Store alone
// therefore does not prove the catalog read used that snapshot. This method is
// a prework check, not an authorization or a durable epoch fence.
func (c *Catalog) MatchesConfig(expected config.Catalog) bool {
	_, ok := c.CaptureConfigEvidence(expected)
	return ok
}
