package app

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/remote"
)

// Public (egress) addresses in a cluster.
//
// Debrid providers, and some Usenet providers, flag an account used from
// several public addresses at once. Each clustered node therefore
// heartbeats the public address it reaches the internet from
// (cluster.Caps.EgressIP) and the cluster status warns the owner when live
// nodes differ; the leader also logs the warning once per change.
//
// The address is the one source traffic leaves from: through the outbound
// proxy when indexers, addons, debrid or Usenet are routed, otherwise the
// direct public address. It is not the UPnP or plex.tv published address.
// Nodes with no known address are ignored.

// egressInterval is how often a node re-detects its public address.
const egressInterval = 10 * time.Minute

// egressIP caches this node's public address for heartbeats, which must
// not wait on the network.
type egressIP struct {
	mu sync.Mutex
	ip string
}

func (e *egressIP) get() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ip
}

func (e *egressIP) set(ip string) {
	e.mu.Lock()
	e.ip = ip
	e.mu.Unlock()
}

// detectEgress finds the public address source traffic leaves from
// ("" when the lookup fails).
func (a *App) detectEgress(ctx context.Context, wan interface {
	Detect(context.Context) (net.IP, error)
}) string {
	if wan == nil {
		return ""
	}
	ip, err := wan.Detect(ctx)
	if err != nil {
		a.log.Debug("cluster: detect source address", "err", err)
		return ""
	}
	return ip.String()
}

// runEgress keeps this node's heartbeated public address current
// (clustered only).
func (a *App) runEgress(ctx context.Context) {
	if !a.clustered() {
		return
	}
	wan := remote.NewWANDetector(egress.Client(egress.CapProbe, 10*time.Second), a.log.With("component", "cluster"))
	t := time.NewTicker(egressInterval)
	defer t.Stop()
	for {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ip := a.detectEgress(dctx, wan)
		cancel()
		if ctx.Err() != nil {
			return
		}
		a.egress.set(ip)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// egressWarning is the owner warning for live nodes reaching the internet
// from different public addresses, "" when they agree (or are unknown).
func egressWarning(nodes []cluster.Node) string {
	byIP := map[string][]string{}
	for _, n := range nodes {
		if !n.Live || n.Caps.EgressIP == "" {
			continue
		}
		name := n.Name
		if name == "" {
			name = n.ID
		}
		byIP[n.Caps.EgressIP] = append(byIP[n.Caps.EgressIP], name)
	}
	if len(byIP) < 2 {
		return ""
	}
	ips := make([]string, 0, len(byIP))
	for ip := range byIP {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	parts := make([]string, 0, len(ips))
	for _, ip := range ips {
		names := slices.Sorted(slices.Values(byIP[ip]))
		parts = append(parts, fmt.Sprintf("%s: %s", ip, strings.Join(names, ", ")))
	}
	return "nodes reach providers from different public addresses (" + strings.Join(parts, "; ") +
		"); a debrid account can be limited when it is used from more than one address"
}

// noteEgress logs the egress warning on the leader when it changes.
func (a *App) noteEgress(nodes []cluster.Node, last *string) {
	if !a.cluster.IsLeader() {
		*last = ""
		return
	}
	w := egressWarning(nodes)
	if w == *last {
		return
	}
	switch {
	case w != "":
		a.log.Warn("cluster: " + w)
	case *last != "":
		a.log.Info("cluster: nodes now reach the internet from one public address")
	}
	*last = w
}
