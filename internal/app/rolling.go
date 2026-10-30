package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/updater"
)

// Rolling updates (HA phase 5a).
//
// A clustered node that can update itself installs a release only while it
// holds the cluster update lease (cluster.UpdateLease), so nodes restart
// one at a time. Before installing it drains: its heartbeat says draining,
// so the leader drops its addresses from the published connections (and a
// draining leader hands leadership over); playback admits no new sessions
// on it (clients retry and reach another node); and the updater waits for
// this node's own sessions to end, up to Updates.MaxDrain. At shutdown the
// node releases the leader lock first so another node takes over at once.
// The restarted node renews the lease while confirming its health and
// releases it once confirmed (updater.Coordinator).
//
// Schema: migrations are applied under PostgreSQL's cluster-wide advisory
// lock (store.migratePostgres) before the node joins the cluster, and are
// additive by convention, so nodes still on the previous release keep
// working on the newer schema. Each node heartbeats the migrations its
// binary knows; a node that finds migrations it does not know (the
// database was migrated by a newer node) logs a warning and continues.
// Owners who prefer not to migrate past running older nodes set
// database.deferMigrations; the node then waits at startup until no live
// node lacks the pending migrations. The first Prism identity transition always
// defers past pre-Prism nodes, independent of that setting (see startupMigrationsGate).

// leaseOpTimeout bounds each update-lease query.
const leaseOpTimeout = 5 * time.Second

// clusterUpdates is the updater.Coordinator of a clustered node.
type clusterUpdates struct {
	a     *App
	lease *cluster.UpdateLease
}

func (a *App) newClusterUpdates() *clusterUpdates {
	return &clusterUpdates{a: a, lease: &cluster.UpdateLease{DB: a.db, NodeID: a.cluster.Self().ID}}
}

func (c *clusterUpdates) AcquireUpdate(ctx context.Context, version string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, leaseOpTimeout)
	defer cancel()
	// Another node's turn: report it. Otherwise only take the turn if a
	// peer stays in service while this node restarts.
	if h, err := c.lease.Holder(ctx); err == nil && h != nil && h.NodeID != "" &&
		h.NodeID != c.a.cluster.Self().ID && time.Now().Before(h.ExpiresAt) {
		return false, c.a.nodeName(ctx, h.NodeID), nil
	}
	if c.mustKeepServing(ctx) {
		return false, "", nil
	}
	ok, err := c.lease.Acquire(ctx, version)
	if errors.Is(err, cluster.ErrVersionFailed) {
		return false, "", fmt.Errorf("%w: %v", updater.ErrClusterRolledBack, err)
	}
	if err != nil || ok {
		return ok, c.a.cluster.Self().Name, err
	}
	h, err := c.lease.Holder(ctx)
	if err != nil || h == nil {
		return false, "", err
	}
	return false, c.a.nodeName(ctx, h.NodeID), nil
}

func (c *clusterUpdates) ReleaseUpdate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, leaseOpTimeout)
	defer cancel()
	return c.lease.Release(ctx)
}

func (c *clusterUpdates) ReportFailed(ctx context.Context, version string) error {
	ctx, cancel := context.WithTimeout(ctx, leaseOpTimeout)
	defer cancel()
	return c.lease.ReportFailed(ctx, version)
}

// SetDraining takes this node out of rotation for new sessions: playback
// refuses them and the next heartbeat (sent now) marks the node draining,
// so the leader republishes the connection union without it.
func (c *clusterUpdates) SetDraining(draining bool) {
	c.a.draining.Store(draining)
	if c.a.playback != nil {
		c.a.playback.SetDraining(draining)
	}
	if pg, ok := c.a.cluster.(*cluster.Postgres); ok {
		pg.Beat()
	}
}

// nodeName is a node's display name, or its ID when unknown.
func (a *App) nodeName(ctx context.Context, id string) string {
	if nodes, err := a.cluster.AllNodes(ctx); err == nil {
		for _, n := range nodes {
			if n.ID == id && n.Name != "" {
				return n.Name
			}
		}
	}
	return id
}

// nodeDraining is this node's heartbeated draining flag.
func (a *App) nodeDraining() bool { return a.draining.Load() }

// stepDownForShutdown releases leadership at the start of a graceful
// shutdown so another node takes over within an election interval.
func (a *App) stepDownForShutdown() {
	pg, ok := a.cluster.(*cluster.Postgres)
	if !ok || !pg.IsLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.log.Info("cluster: stepping down as leader before shutdown")
	pg.StepDown(ctx)
}

// warnNewerSchema logs when the shared database holds migrations this
// binary does not know: another node runs a newer release.
func (a *App) warnNewerSchema(ctx context.Context) {
	unknown, err := store.UnknownApplied(ctx, a.db)
	if err != nil || len(unknown) == 0 {
		return
	}
	a.log.Warn("database: the shared schema was migrated by a newer Ether release; continuing because migrations are additive — update this node",
		"version", a.opts.Version, "unknownMigrations", strings.Join(unknown, ","))
}

// startupMigrationsGate makes the first Prism identity transition a stop-all
// upgrade even when optional deferral is disabled. The epoch cannot serialize
// membership writers in released binaries that predate auth/5. This prevents
// migrating past a live old node; revoking old writer credentials before the
// transition also prevents an old binary restarting after it (see the runbook).
func startupMigrationsGate(selfID string, deferAll bool, liveAfter time.Duration, now func() time.Time) store.MigrationGate {
	older := olderNodesGate(selfID, liveAfter, now)
	return func(ctx context.Context, q store.Querier, pending []string) error {
		if deferAll {
			return older(ctx, q, pending)
		}
		for _, migration := range pending {
			if migration == "auth/5" {
				return older(ctx, q, []string{"auth/5"})
			}
		}
		return nil
	}
}

// olderNodesGate defers this node's pending migrations while another live
// node runs a binary that does not know them (database.deferMigrations).
// Nodes that predate the migrations report count as not knowing them.
func olderNodesGate(selfID string, liveAfter time.Duration, now func() time.Time) store.MigrationGate {
	return func(ctx context.Context, q store.Querier, pending []string) error {
		var hasTable, hasReport bool
		if err := q.QueryRowContext(ctx, `SELECT to_regclass('cluster_nodes') IS NOT NULL`).Scan(&hasTable); err != nil || !hasTable {
			return err // a new cluster: nobody to wait for
		}
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) > 0 FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'cluster_nodes' AND column_name = 'migrations'`).Scan(&hasReport); err != nil {
			return err
		}
		cols := `name, version, ''`
		if hasReport {
			cols = `name, version, migrations`
		}
		rows, err := q.QueryContext(ctx, `SELECT `+cols+` FROM cluster_nodes WHERE id <> ? AND last_seen >= ?`,
			selfID, now().Add(-liveAfter).UnixMilli())
		if err != nil {
			return err
		}
		defer rows.Close()
		var older []string
		for rows.Next() {
			var name, version, raw string
			if err := rows.Scan(&name, &version, &raw); err != nil {
				return err
			}
			known := map[string]int{}
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &known)
			}
			for _, p := range pending {
				comp, v, _ := strings.Cut(p, "/")
				if n, _ := strconv.Atoi(v); known[comp] < n {
					older = append(older, fmt.Sprintf("%s (%s)", name, version))
					break
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(older) > 0 {
			return fmt.Errorf("%w: live node(s) %s run a release without migrations %s (migration deferral required); waiting for them to update or stop",
				store.ErrMigrationDeferred, strings.Join(older, ", "), strings.Join(pending, ","))
		}
		return nil
	}
}

// versionWarning names the releases running in the cluster when they
// differ (a rolling update in progress, or a node left behind).
func versionWarning(nodes []cluster.Node) string {
	versions := map[string][]string{}
	var order []string
	for _, n := range nodes {
		if !n.Live {
			continue
		}
		if _, ok := versions[n.Version]; !ok {
			order = append(order, n.Version)
		}
		versions[n.Version] = append(versions[n.Version], n.Name)
	}
	if len(order) < 2 {
		return ""
	}
	parts := make([]string, 0, len(order))
	for _, v := range order {
		parts = append(parts, v+": "+strings.Join(versions[v], ", "))
	}
	return "Nodes run different Ether releases (" + strings.Join(parts, "; ") + "). This is expected during a rolling update; otherwise update the older nodes."
}

// mustKeepServing reports whether this node has to stay up for now because
// restarting it would leave no other node serving clients: other nodes are
// recorded but none is live and accepting sessions (a two-node cluster
// whose peer is down, or whose peer is itself draining). A node alone in
// the cluster may update, and a node already holding the lease (renewing
// through its own update) is never held back.
func (c *clusterUpdates) mustKeepServing(ctx context.Context) bool {
	self := c.a.cluster.Self().ID
	if h, err := c.lease.Holder(ctx); err == nil && h != nil && h.NodeID == self {
		return false
	}
	nodes, err := c.a.cluster.AllNodes(ctx)
	if err != nil {
		return true // membership unknown: do not risk taking the last node down
	}
	if othersNotServing(nodes, self) {
		c.a.log.Info("updater: waiting to update until another cluster node is serving")
		return true
	}
	return false
}

// othersNotServing reports whether other nodes are recorded but none of
// them is live and accepting new sessions.
func othersNotServing(nodes []cluster.Node, self string) bool {
	others, serving := 0, 0
	for _, n := range nodes {
		if n.ID == self {
			continue
		}
		others++
		if n.Live && !n.Draining {
			serving++
		}
	}
	return others > 0 && serving == 0
}
