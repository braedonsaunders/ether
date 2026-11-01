package app

import (
	"encoding/json"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/egress"
)

// applyOutbound installs the node-local proxy and, once Usenet is up,
// drops warm NNTP connections when the route they dial changes.
func (a *App) applyOutbound(c config.Config) {
	egress.Use(c.Outbound, a.log)
	key := outboundDialKey(c.Outbound)
	changed := a.outboundDial != "" && a.outboundDial != key
	a.outboundDial = key
	if changed && a.fetcher != nil {
		a.fetcher.Redial()
	}
}

func outboundDialKey(o config.Outbound) string {
	b, _ := json.Marshal(struct {
		Enabled  bool     `json:"enabled"`
		URL      string   `json:"url"`
		Username string   `json:"username"`
		Password string   `json:"password"`
		Usenet   bool     `json:"usenet"`
		Bypass   []string `json:"bypass"`
	}{o.Enabled, o.URL, o.Username, o.Password, o.Usenet, o.Bypass})
	return string(b)
}
