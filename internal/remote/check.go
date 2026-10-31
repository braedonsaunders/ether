package remote

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

// EgressCheck is what the owner console shows next to the proxy address.
// Errors are safe to display: they do not contain a proxy password.
type EgressCheck struct {
	SourceIP       string `json:"sourceIp,omitempty"`
	SourceError    string `json:"sourceError,omitempty"`
	PublishedIP    string `json:"publishedIp,omitempty"`
	PublishedError string `json:"publishedError,omitempty"`
	RemoteAccess   bool   `json:"remoteAccess"`
	ProxyEnabled   bool   `json:"proxyEnabled"`
}

// CheckEgress asks a public-IP service which address source traffic leaves
// from, and which address a direct connection sees. endpoints defaults to
// the same list remote access uses. The direct lookup is what Plex publishes
// when no public host name is set; the handler may replace it with that name.
func CheckEgress(ctx context.Context, p egress.Policy, endpoints []string) EgressCheck {
	if len(endpoints) == 0 {
		endpoints = DefaultWANEndpoints
	}
	res := EgressCheck{ProxyEnabled: p.Enabled}
	source := NewWANDetector(p.SnapshotClient(egress.CapProbe, 10*time.Second), nil, endpoints...)
	if ip, err := source.Detect(ctx); err != nil {
		res.SourceError = safeErr(err)
	} else {
		res.SourceIP = ip.String()
	}
	if ctx.Err() != nil {
		return res
	}
	direct := NewWANDetector(p.SnapshotClient(egress.CapDirect, 10*time.Second), nil, endpoints...)
	if ip, err := direct.Detect(ctx); err != nil {
		res.PublishedError = safeErr(err)
	} else {
		res.PublishedIP = ip.String()
	}
	return res
}

func safeErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
