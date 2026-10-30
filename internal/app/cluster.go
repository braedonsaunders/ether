package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Cluster membership as seen by the plex.tv publisher.
const (
	// memberPoll is how often the leader re-reads membership for the
	// published connection union (join/leave events also trigger a read).
	memberPoll = 10 * time.Second
	// memberGrace keeps a node's addresses published this long after its
	// last heartbeat, so a brief stall does not churn plex.tv.
	memberGrace = 60 * time.Second
	// healthSampleInterval matches notify.Service.Run's health sampling.
	healthSampleInterval = 30 * time.Second
	// clusterLiveAfter matches cluster.Options.LiveAfter's default.
	clusterLiveAfter = 20 * time.Second
)

// buildCluster loads this node's identity and selects the runtime for the
// database: Solo for SQLite (always leader, nothing shared), Postgres for a
// shared PostgreSQL database.
func (a *App) buildCluster(dialect store.Dialect) error {
	id, err := cluster.LoadIdentity(a.opts.DataDir)
	if err != nil {
		return err
	}
	local := cluster.Local{Identity: id, Version: a.opts.Version, Addrs: a.nodeAddrs, Caps: a.nodeCaps, Load: a.nodeLoad,
		Draining: a.nodeDraining, Migrations: store.KnownMigrations()}
	log := a.log.With("component", "cluster")
	a.peers = &peerSet{}
	a.egress = &egressIP{}
	if dialect != store.Postgres {
		a.cluster = cluster.NewSolo(local, log)
		return nil
	}
	a.clusterFatal = make(chan error, 1)
	a.dbAvail = store.NewAvailability(nil)
	pg, err := cluster.NewPostgres(cluster.Options{
		DB: a.db, Local: local, Logger: log, Availability: a.dbAvail,
		IdentityPath: filepath.Join(a.opts.DataDir, cluster.NodeFile),
		// A clone of this node (copied node.json) is running: stop serving.
		OnFatal: func(err error) {
			select {
			case a.clusterFatal <- err:
			default:
			}
		},
	})
	if err != nil {
		return err
	}
	a.cluster = pg
	// Registered after the database closer, so it runs first: leave the
	// cluster (drop the heartbeat row, release leadership) while the
	// database is still open.
	a.closers = append(a.closers, func() { _ = pg.Close() })
	return nil
}

// clusterStartRetryMin / Max bound the backoff while joining the cluster
// fails at startup.
var (
	clusterStartRetryMin = time.Second
	clusterStartRetryMax = 30 * time.Second
	clusterStartBudget   = 2 * time.Minute
)

// startCluster joins the cluster, retrying failures other than a
// duplicate node identity (the database became unreachable between
// opening it and joining). A node that gave up after one failed attempt
// would serve without heartbeats, leadership or the event bus until it was
// restarted, so singleton work would never run on it; like the database
// wait in New, nothing listens meanwhile. A bounded startup budget provides
// a clear error if membership cannot be established.
func (a *App) startCluster(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, clusterStartBudget)
	defer cancel()
	wait := clusterStartRetryMin
	for attempt := 1; ; attempt++ {
		err := a.cluster.Start(ctx)
		if err == nil && ctx.Err() != nil {
			return fmt.Errorf("cluster: could not establish owned heartbeat: %w", ctx.Err())
		}
		if err == nil || errors.Is(err, cluster.ErrDuplicateNode) {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("cluster: could not establish owned heartbeat: %w", ctx.Err())
		}
		a.log.Warn("cluster: could not join; retrying", "attempt", attempt, "retry", wait, "err", err)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("cluster: could not establish owned heartbeat: %w", ctx.Err())
		case <-t.C:
		}
		wait = min(wait*2, clusterStartRetryMax)
	}
}

// clustered reports whether this node shares a PostgreSQL database with
// other nodes.
func (a *App) clustered() bool {
	return a.cluster != nil && a.cluster.Mode() == cluster.ModePostgres
}

// clusterNodeID is the ID components tag node-owned shared rows with when
// clustered; empty on a standalone server, which keeps their standalone
// behaviour.
func (a *App) clusterNodeID() string {
	if !a.clustered() {
		return ""
	}
	return a.cluster.Self().ID
}

// clusterNode is the cluster surface handed to components that share
// sessions, queues, notifications and cache invalidations; nil (an untyped
// nil interface) on a standalone server, which keeps them in-process.
func (a *App) clusterNode() clusterbus.Node {
	if !a.clustered() {
		return nil
	}
	return clusterbus.FromCluster(a.cluster)
}

// describeLiveGroup describes a live TV viewing unit for other nodes'
// status pages.
func (a *App) describeLiveGroup(group string) *core.PlaySession {
	if a.livetv == nil {
		return nil
	}
	for _, s := range a.livetv.ViewingSessions() {
		if s.ID == group {
			return s
		}
	}
	return nil
}

// nodeAddrs is this node's own publishable connection URIs (heartbeated).
func (a *App) nodeAddrs(ctx context.Context) []string {
	if a.registrar == nil {
		return nil
	}
	return a.registrar.NodeURIs(ctx)
}

func (a *App) nodeCaps() cluster.Caps {
	caps := cluster.Caps{Weight: a.cfg.Get().Server.ClusterWeight, EgressIP: a.egress.get()}
	if a.transcoder != nil {
		caps.Transcode, caps.HWAccel = true, a.transcoder.HWAccel().Name
	}
	return caps
}

func (a *App) nodeLoad() int {
	if a.playback == nil {
		return 0
	}
	return len(a.playback.Sessions())
}

// peerSet caches the other live nodes' URIs for the registrar, which reads
// it on every publish without touching the database.
type peerSet struct {
	mu   sync.Mutex
	uris []string
}

func (p *peerSet) get() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.uris)
}

// set stores uris and reports whether they changed.
func (p *peerSet) set(uris []string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if slices.Equal(p.uris, uris) {
		return false
	}
	p.uris = slices.Clone(uris)
	return true
}

// refreshPeers re-reads membership; on a database error the last known
// peers stay published.
func (a *App) refreshPeers(ctx context.Context) bool {
	nodes, err := a.cluster.AllNodes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Debug("cluster: read members", "err", err)
		}
		return false
	}
	return a.peers.set(cluster.PeerAddrs(nodes, a.cluster.Self().ID, time.Now(), memberGrace))
}

// runRegistrarAsLeader runs the plex.tv registrar (publication, presence,
// friend sync, certificate renewal, reachability) only on the leader. GDM
// discovery, the HTTP listener and port mapping (Registrar.RunPortMapping:
// each node is reached through its own port forward) stay on every node.
func (a *App) runRegistrarAsLeader(ctx context.Context) {
	a.cluster.RunAsLeader(ctx, "plex.tv registrar", func(ctx context.Context) {
		a.refreshPeers(ctx)
		if err := a.registrar.Start(ctx); err != nil {
			a.log.Warn("plex.tv registrar", "err", err)
			return
		}
		defer a.registrar.Stop()
		// A new leader republishes at once: the union may have changed
		// while another node was publishing.
		a.registrar.RequestPublish()
		a.watchMembers(ctx)
	})
}

// watchMembers republishes when the peer URI union changes: on join/leave
// events and on a slow poll that also catches crashed nodes expiring.
func (a *App) watchMembers(ctx context.Context) {
	poke := make(chan struct{}, 1)
	defer a.cluster.Subscribe(cluster.TopicMembers, func([]byte) {
		select {
		case poke <- struct{}{}:
		default:
		}
	})()
	t := time.NewTicker(memberPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-poke:
		}
		if a.refreshPeers(ctx) {
			a.registrar.RequestPublish()
		}
	}
}

// sampleHealth raises this node's provider and cache alerts. Clustered
// nodes need it because notification delivery (which samples too) runs
// only on the leader; sampling is edge-triggered, so the leader sampling
// twice is harmless.
func (a *App) sampleHealth(ctx context.Context) {
	t := time.NewTicker(healthSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = a.notifications.SampleHealth(ctx)
		}
	}
}

// clusterStatus is GET /ether/api/cluster/.
type clusterStatus struct {
	Mode   string          `json:"mode"`
	Self   cluster.Node    `json:"self"`
	Leader string          `json:"leader,omitempty"` // leader node ID, when known
	Epoch  int64           `json:"epoch"`            // this node's leader epoch (0 unless leading)
	Nodes  []cluster.Node  `json:"nodes"`            // every recorded node, with liveness
	Health *cluster.Health `json:"health,omitempty"`
	Error  string          `json:"error,omitempty"`
	// Warnings are cluster conditions the owner should act on.
	Warnings []string `json:"warnings,omitempty"`
	// Database is this node's shared-database connectivity (clustered
	// nodes only).
	Database *databaseHealth `json:"database,omitempty"`
}

func (a *App) clusterHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		st := clusterStatus{Mode: a.cluster.Mode(), Self: a.cluster.Self(), Epoch: a.cluster.Epoch()}
		st.Self.Draining = a.nodeDraining()
		if a.cfg != nil {
			st.Self.Caps.Weight = a.cfg.Get().Server.ClusterWeight
		}
		// Bounded so the page still answers (with the database state)
		// while the database is unreachable.
		nctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		nodes, err := a.cluster.AllNodes(nctx)
		cancel()
		if err != nil {
			st.Error = err.Error()
		}
		st.Nodes = nodes
		if st.Nodes == nil {
			st.Nodes = []cluster.Node{}
		}
		for _, n := range st.Nodes {
			if n.Leader {
				st.Leader = n.ID
			}
		}
		if w := egressWarning(st.Nodes); w != "" {
			st.Warnings = append(st.Warnings, w)
		}
		if w := versionWarning(st.Nodes); w != "" {
			st.Warnings = append(st.Warnings, w)
		}
		if pg, ok := a.cluster.(*cluster.Postgres); ok {
			h := pg.Health()
			st.Health = &h
			db := a.databaseHealth()
			st.Database = &db
			if db.Message != "" {
				st.Warnings = append(st.Warnings, db.Message)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.Handle("/node", a.clusterManageHandler())
	setup := a.clusterSetupHandler()
	mux.Handle("/setup", setup)
	mux.Handle("/setup/", setup)
	settings := a.clusterSettingsHandler()
	mux.Handle("/settings", settings)
	mux.Handle("/join-tokens", settings)
	mux.Handle("/join-tokens/", settings)
	return mux
}

// clusterLoginLock serialises indexer-site logins across nodes, so one
// node logs in while the others wait and then reuse the shared session.
func (a *App) clusterLoginLock(ctx context.Context, key string) (func(), error) {
	unlock, err := store.SessionLock(ctx, a.db, "cardigann-login:"+key)
	if err != nil {
		return nil, err
	}
	if unlock == nil {
		return func() {}, nil
	}
	return unlock, nil
}
