package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/faillog"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/jackc/pgx/v5"
)

// DefaultLockKey is the first half of the leader advisory lock
// ("EthC"); the second half is hashtext(current_schema()), so nodes sharing
// a schema contend for one lock while unrelated schemas (test runs) never
// collide.
const DefaultLockKey int32 = 0x45746843

// DefaultChannel is the LISTEN/NOTIFY channel for the public schema; other
// schemas get a channel derived from their name (NOTIFY is database-wide).
const DefaultChannel = "ether_cluster"

// Options configure the PostgreSQL runtime. Zero durations take the
// defaults in parentheses.
type Options struct {
	// DB is the node's pool on the shared database (store "ether-pgx").
	DB    *sql.DB
	Local Local
	// ConnConfig is used for the dedicated leader-lock and LISTEN
	// connections; nil copies the configuration of a pool connection, so
	// they reach the same server, database and search_path.
	ConnConfig *pgx.ConnConfig
	Logger     *slog.Logger
	Now        func() time.Time

	HeartbeatInterval time.Duration // heartbeat upsert (5s)
	LiveAfter         time.Duration // a node is live if seen within this (20s)
	PruneAfter        time.Duration // rows dead this long are deleted (24h)
	ElectionInterval  time.Duration // lock attempt / leader probe (2s)
	ProbeTimeout      time.Duration // bound on each probe query (3s)
	// StepDownHold keeps a node that just lost leadership out of the
	// election for a while so a healthy node takes over (5s).
	StepDownHold time.Duration
	// IdleSessionTimeout is set as idle_session_timeout on the lock
	// connection: if this node freezes or is partitioned from the database,
	// the server ends the session and releases the lock after this long
	// rather than waiting for TCP keepalives (15s; must exceed
	// ElectionInterval).
	IdleSessionTimeout time.Duration
	ListenPing         time.Duration // ping an idle LISTEN connection (15s)
	MinBackoff         time.Duration // bus reconnect backoff (500ms)
	MaxBackoff         time.Duration // (30s)
	AddrTimeout        time.Duration // bound on Local.Addrs (30s)
	// IdentityPath names this node's node.json in duplicate-node errors.
	IdentityPath string

	LockKey int32  // DefaultLockKey
	Channel string // derived from the schema when empty

	// Availability, when set, receives every heartbeat's outcome: the
	// node's shared view of database reachability, which lets deferrable
	// writes queue at once during an outage and replay as soon as a
	// heartbeat succeeds again.
	Availability *store.Availability

	// OnFatal is called once, from a runtime goroutine, when this node
	// must stop serving as a cluster member: another live process
	// heartbeats under the same node ID (a cloned machine or copied data
	// directory). The runtime has already stopped heartbeating, released
	// leadership and left the bus when it is called. The error wraps
	// ErrDuplicateNode.
	OnFatal func(error)
}

// ErrDuplicateNode means another running process uses this node's ID: its
// node.json was copied from another node.
var ErrDuplicateNode = errors.New("cluster: another running node uses this node's identity")

// duplicateNodeError is the owner-facing explanation; path is this node's
// node.json.
func duplicateNodeError(nodeID, path string) error {
	return fmt.Errorf("%w (node ID %s). This happens when a machine is cloned or a data directory is copied: "+
		"stop this server, delete %s on the copy and start it again so it joins as a new node", ErrDuplicateNode, shortID(nodeID), path)
}

// Postgres is the clustered runtime.
//
// Heartbeats: every node upserts its cluster_nodes row every
// HeartbeatInterval with last_seen = its own clock in Unix milliseconds; a
// node is live while now-last_seen <= LiveAfter on the reader's clock.
// Timestamps are bound Go values (portable SQL, no server now()), so the
// scheme tolerates clock skew up to LiveAfter-HeartbeatInterval (15s by
// default); Start warns when this node's clock is more than a quarter of
// LiveAfter away from the database server's. Nodes should run NTP.
//
// Leadership: a dedicated connection holds pg_try_advisory_lock; the holder
// probes it with SELECT 1 every ElectionInterval and steps down the moment
// a probe fails (subscribers and RunAsLeader contexts are cancelled
// immediately). Each acquisition increments cluster_leader.epoch: a leader
// that was frozen past IdleSessionTimeout may believe it leads for up to
// one ElectionInterval after waking, so singleton writes that must never
// overlap should be fenced on Epoch.
//
// Bus: a dedicated connection LISTENs on one channel; Publish delivers to
// local subscribers immediately and sends pg_notify through the pool. The
// sender's own echo is ignored. After the LISTEN connection is
// re-established TopicResync is delivered locally, because notifications
// sent while it was down are lost; it is also delivered after the first
// LISTEN, covering events sent before this node joined the bus.
type Postgres struct {
	o     Options
	db    *sql.DB
	local Local
	log   *slog.Logger
	inc   string // incarnation: distinguishes this process's own echoes
	bus   *localBus
	lead  *leadership
	// beatFail and busFail rate-limit outage warnings from the heartbeat
	// and LISTEN loops (first failure, a summary a minute, the recovery).
	beatFail *faillog.Tracker
	busFail  *faillog.Tracker

	runMu   sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	connCfg *pgx.ConnConfig
	channel atomic.Pointer[string]

	beatWake chan struct{}
	stepWake chan struct{} // StepDown: run an election step now
	busReady chan struct{}
	busOnce  sync.Once

	mu           sync.Mutex
	self         Node
	addrs        []string
	beatAddrs    []string // addrs in the last successful heartbeat
	beatDraining bool     // draining flag in the last successful heartbeat
	resigned     bool     // StepDown: never lead again in this process
	joined       bool
	lastPrune    time.Time
	holdUntil    time.Time
	busUp        bool
	lastSkew     time.Duration
	lastBeatOK   time.Time
	fatal        error
}

var _ Cluster = (*Postgres)(nil)

// NewPostgres validates options; call Start to join the cluster.
func NewPostgres(o Options) (*Postgres, error) {
	if o.DB == nil {
		return nil, errors.New("cluster: postgres runtime needs DB")
	}
	if o.Local.Identity.NodeID == "" {
		return nil, errors.New("cluster: postgres runtime needs a node ID")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.HeartbeatInterval, 5*time.Second)
	def(&o.LiveAfter, 20*time.Second)
	def(&o.PruneAfter, 24*time.Hour)
	def(&o.ElectionInterval, 2*time.Second)
	def(&o.ProbeTimeout, 3*time.Second)
	def(&o.StepDownHold, 5*time.Second)
	def(&o.IdleSessionTimeout, 15*time.Second)
	def(&o.ListenPing, 15*time.Second)
	def(&o.MinBackoff, 500*time.Millisecond)
	def(&o.MaxBackoff, 30*time.Second)
	def(&o.AddrTimeout, 30*time.Second)
	if o.IdleSessionTimeout <= o.ElectionInterval {
		o.IdleSessionTimeout = 3 * o.ElectionInterval
	}
	if o.LockKey == 0 {
		o.LockKey = DefaultLockKey
	}
	if o.IdentityPath == "" {
		o.IdentityPath = NodeFile
	}
	log := o.Logger.With("component", "cluster", "node", shortID(o.Local.Identity.NodeID))
	p := &Postgres{
		o: o, db: o.DB, local: o.Local, log: log, inc: newID()[:12],
		bus: newLocalBus(log), lead: newLeadership(),
		beatWake: make(chan struct{}, 1), stepWake: make(chan struct{}, 1), busReady: make(chan struct{}),
		beatFail: faillog.New(log, "cluster: heartbeat"),
		busFail:  faillog.New(log, "cluster: event bus connection"),
	}
	p.self = Node{
		ID: o.Local.Identity.NodeID, Name: o.Local.Identity.Name, Version: o.Local.Version,
		StartedAt: o.Now(), Live: true,
	}
	return p, nil
}

func (p *Postgres) Mode() string { return ModePostgres }

// Self is this node as of its last heartbeat.
func (p *Postgres) Self() Node {
	p.mu.Lock()
	n := p.self
	n.Addrs = slices.Clone(n.Addrs)
	p.mu.Unlock()
	n.Leader = p.IsLeader()
	return n
}

func (p *Postgres) IsLeader() bool { return p.lead.isLeader() }
func (p *Postgres) Epoch() int64   { return p.lead.currentEpoch() }

func (p *Postgres) OnLeaderChange(fn func(bool)) func() { return p.lead.subscribe(fn) }

func (p *Postgres) RunAsLeader(ctx context.Context, name string, fn func(context.Context)) {
	runAsLeader(ctx, p.log, name, p.lead, fn)
}

func (p *Postgres) Subscribe(topic string, fn func([]byte)) func() { return p.bus.subscribe(topic, fn) }

// Channel is the NOTIFY channel (empty before Start).
func (p *Postgres) Channel() string {
	if c := p.channel.Load(); c != nil {
		return *c
	}
	return ""
}

// BusConnected reports whether the LISTEN connection is up.
func (p *Postgres) BusConnected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busUp
}

// Health is the runtime's own state for diagnostics.
type Health struct {
	Channel       string    `json:"channel"`
	BusConnected  bool      `json:"busConnected"`
	LastHeartbeat time.Time `json:"lastHeartbeat,omitempty"`
	// ClockSkew is this node's clock minus the database server's, measured
	// at Start.
	ClockSkew time.Duration `json:"clockSkewNs"`
}

// Health reports bus, heartbeat and clock state.
func (p *Postgres) Health() Health {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Health{Channel: p.Channel(), BusConnected: p.busUp, LastHeartbeat: p.lastBeatOK, ClockSkew: p.lastSkew}
}

// Publish delivers to local subscribers, then notifies the other nodes.
// Before Start the event is only delivered locally.
func (p *Postgres) Publish(ctx context.Context, topic string, payload []byte) error {
	msg, err := encodeEnvelope(topic, p.local.Identity.NodeID, p.inc, payload)
	if err != nil {
		return err
	}
	p.bus.deliver(topic, payload)
	ch := p.Channel()
	if ch == "" {
		return nil
	}
	if _, err := p.db.ExecContext(ctx, `SELECT pg_notify(?, ?)`, ch, msg); err != nil {
		return fmt.Errorf("cluster: publish %s: %w", topic, err)
	}
	return nil
}

// Start joins the cluster: first heartbeat, then the heartbeat, address,
// election and bus goroutines. It waits briefly for the bus to listen so
// events published right after Start are received.
func (p *Postgres) Start(ctx context.Context) error {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if p.started {
		return errors.New("cluster: already started")
	}
	if p.closed {
		return errors.New("cluster: closed")
	}
	// Bound the complete join, including connection/configuration and clock
	// checks, not just the final heartbeat write.
	joinCtx, stopJoin := context.WithTimeout(ctx, max(5*time.Second, 3*p.o.ProbeTimeout+3*p.o.HeartbeatInterval))
	defer stopJoin()
	cfg := p.o.ConnConfig
	if cfg == nil {
		var err error
		if cfg, err = connConfigOf(joinCtx, p.db); err != nil {
			return err
		}
	}
	p.connCfg = cfg
	ch := p.o.Channel
	if ch == "" {
		var schema string
		if err := p.db.QueryRowContext(joinCtx, `SELECT current_schema()`).Scan(&schema); err != nil {
			return fmt.Errorf("cluster: read schema: %w", err)
		}
		ch = channelFor(schema)
	}
	p.channel.Store(&ch)
	p.checkClock(joinCtx)
	// Do not start listeners or background work until this incarnation owns
	// a durable, versioned heartbeat. Every retry rechecks duplicate ownership.
	backoff := p.o.MinBackoff
	for {
		claim, err := p.checkDuplicate(joinCtx)
		if err == nil {
			err = p.beatClaim(joinCtx, &claim)
		}
		if err == nil {
			if err = joinCtx.Err(); err == nil {
				break
			}
		}
		if errors.Is(err, ErrDuplicateNode) {
			return err
		}
		p.log.Warn("cluster: first heartbeat failed; retrying", "err", err)
		timer := time.NewTimer(backoff)
		select {
		case <-joinCtx.Done():
			timer.Stop()
			p.abandonStart()
			return fmt.Errorf("cluster: initial heartbeat unavailable: %w", joinCtx.Err())
		case <-timer.C:
		}
		backoff = min(backoff*2, p.o.MaxBackoff)
	}
	if err := ctx.Err(); err != nil {
		p.abandonStart()
		return err
	}
	rctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.started = true
	p.wg.Add(4)
	go p.addrLoop(rctx)
	go p.heartbeatLoop(rctx)
	go p.electionLoop(rctx)
	go p.listenLoop(rctx)
	select {
	case <-p.busReady:
	case <-time.After(5 * time.Second):
		p.log.Warn("cluster: event bus not listening yet; continuing")
	case <-ctx.Done():
	}
	// busReady and cancellation may become ready together; always honor
	// cancellation rather than leaving a started runtime behind.
	if err := ctx.Err(); err != nil {
		p.halt()
		p.abandonStart()
		p.started = false
		p.cancel = nil
		p.busReady = make(chan struct{})
		p.busOnce = sync.Once{}
		return err
	}
	p.log.Info("cluster: joined", "name", p.local.Identity.Name, "channel", ch)
	return nil
}

// abandonStart relinquishes only this incarnation. A failed delete remains
// safely owned and may be retried by the same instance on a later Start.
func (p *Postgres) abandonStart() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := p.db.ExecContext(ctx, `DELETE FROM cluster_nodes WHERE id=? AND incarnation=?`, p.local.Identity.NodeID, p.inc); err == nil {
		p.mu.Lock()
		p.joined = false
		p.mu.Unlock()
	}
}

// Close leaves the cluster.
func (p *Postgres) Close() error {
	p.runMu.Lock()
	if p.closed {
		p.runMu.Unlock()
		return nil
	}
	p.closed = true
	started := p.started
	p.runMu.Unlock()
	if !started {
		return nil
	}
	p.halt()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `DELETE FROM cluster_nodes WHERE id=? AND incarnation=?`, p.local.Identity.NodeID, p.inc)
	if err == nil {
		_ = p.Publish(ctx, TopicMembers, []byte(p.local.Identity.NodeID))
	}
	p.log.Info("cluster: left")
	return err
}

// halt stops every goroutine (releasing leadership) without removing the
// heartbeat row — a crash, as far as other nodes can tell.
func (p *Postgres) halt() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	p.lead.set(false, 0)
}

// ---- membership --------------------------------------------------------------

// Beat heartbeats now instead of at the next interval (after this node's
// draining state changed, so the leader republishes promptly).
func (p *Postgres) Beat() { p.wakeBeat() }

func (p *Postgres) wakeBeat() {
	select {
	case p.beatWake <- struct{}{}:
	default:
	}
}

func (p *Postgres) addrLoop(ctx context.Context) {
	defer p.wg.Done()
	if p.local.Addrs == nil {
		return
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		actx, cancel := context.WithTimeout(ctx, p.o.AddrTimeout)
		addrs := p.local.Addrs(actx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		changed := !slices.Equal(addrs, p.addrs)
		p.addrs = slices.Clone(addrs)
		p.mu.Unlock()
		if changed {
			p.wakeBeat()
		}
		t.Reset(p.o.HeartbeatInterval)
	}
}

func (p *Postgres) heartbeatLoop(ctx context.Context) {
	defer p.wg.Done()
	t := time.NewTicker(p.o.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.beatWake:
		}
		bctx, cancel := context.WithTimeout(ctx, p.o.HeartbeatInterval)
		err := p.beat(bctx)
		cancel()
		switch {
		case ctx.Err() != nil:
		case err != nil:
			p.beatFail.Fail(err)
			p.o.Availability.Report(err)
		default:
			p.beatFail.OK()
			p.o.Availability.OK()
		}
	}
}

// beat upserts this node's row, announces membership changes and prunes
// long-dead rows.
func (p *Postgres) beat(ctx context.Context) error { return p.beatClaim(ctx, nil) }

func (p *Postgres) beatClaim(ctx context.Context, claim *nodeClaim) error {
	now := p.o.Now()
	p.mu.Lock()
	addrs := slices.Clone(p.addrs)
	started := p.self.StartedAt
	p.mu.Unlock()
	if addrs == nil {
		addrs = []string{}
	}
	caps, load, draining := p.local.caps(), p.local.load(), p.local.draining()
	addrsJSON, _ := json.Marshal(addrs)
	capsJSON, _ := json.Marshal(caps)
	migJSON := ""
	if p.local.Migrations != nil {
		b, _ := json.Marshal(p.local.Migrations)
		migJSON = string(b)
	}
	drainFlag := 0
	if draining {
		drainFlag = 1
	}
	id := p.local.Identity.NodeID
	p.mu.Lock()
	joined, fatal := p.joined, p.fatal
	p.mu.Unlock()
	if fatal != nil {
		return fatal
	}
	if joined {
		// Once joined, only this process may write the row: an update that
		// finds another incarnation's row means a second live process
		// shares our node ID.
		res, err := p.db.ExecContext(ctx, `UPDATE cluster_nodes SET name=?, version=?, addrs=?, caps=?, active_sessions=?, started_at=?, last_seen=?,
draining=?, migrations=?
WHERE id=? AND incarnation=?`,
			p.local.Identity.Name, p.local.Version, string(addrsJSON), string(capsJSON), load,
			started.UnixMilli(), now.UnixMilli(), drainFlag, migJSON, id, p.inc)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var other string
			err := p.db.QueryRowContext(ctx, `SELECT incarnation FROM cluster_nodes WHERE id=?`, id).Scan(&other)
			switch {
			case err == nil && other != p.inc:
				p.fail(duplicateNodeError(id, p.o.IdentityPath))
				return ErrDuplicateNode
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return err
			}
			// The row is gone (pruned while this node was stalled): re-insert
			// it below.
			joined = false
		}
	}
	if !joined {
		query := `INSERT INTO cluster_nodes(id,name,version,addrs,caps,active_sessions,started_at,last_seen,incarnation,draining,migrations)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO NOTHING`
		args := []any{id, p.local.Identity.Name, p.local.Version, string(addrsJSON), string(capsJSON), load, started.UnixMilli(), now.UnixMilli(), p.inc, drainFlag, migJSON}
		if claim != nil && claim.exists {
			query = `INSERT INTO cluster_nodes(id,name,version,addrs,caps,active_sessions,started_at,last_seen,incarnation,draining,migrations)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, version=excluded.version, addrs=excluded.addrs, caps=excluded.caps,
active_sessions=excluded.active_sessions, started_at=excluded.started_at, last_seen=excluded.last_seen, incarnation=excluded.incarnation,
draining=excluded.draining, migrations=excluded.migrations
WHERE cluster_nodes.incarnation=? AND cluster_nodes.last_seen=?`
			args = append(args, claim.incarnation, claim.lastSeen)
		}
		res, err := p.db.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			err := duplicateNodeError(id, p.o.IdentityPath)
			if claim == nil {
				p.fail(err)
			}
			return err
		}
	}
	p.mu.Lock()
	p.self.Addrs, p.self.Caps, p.self.Load, p.self.LastSeen = addrs, caps, load, now
	p.self.Draining, p.self.Migrations = draining, p.local.Migrations
	announce := !p.joined || !slices.Equal(addrs, p.beatAddrs) || draining != p.beatDraining
	p.joined, p.beatAddrs, p.lastBeatOK, p.beatDraining = true, addrs, now, draining
	prune := now.Sub(p.lastPrune) > time.Hour
	if prune {
		p.lastPrune = now
	}
	p.mu.Unlock()
	if announce {
		if err := p.Publish(ctx, TopicMembers, []byte(id)); err != nil {
			p.log.Debug("cluster: announce membership", "err", err)
		}
	}
	if prune {
		cut := now.Add(-p.o.PruneAfter).UnixMilli()
		if res, err := p.db.ExecContext(ctx, `DELETE FROM cluster_nodes WHERE last_seen < ?`, cut); err != nil {
			p.log.Debug("cluster: prune dead nodes", "err", err)
		} else if n, _ := res.RowsAffected(); n > 0 {
			p.log.Info("cluster: pruned dead nodes", "rows", n)
		}
	}
	return nil
}

// checkDuplicate refuses to join while another live process heartbeats
// under this node ID. A row left by this node's own previous process (a
// crash or kill) also looks live for up to LiveAfter, so a recent row with
// another incarnation is watched for two heartbeat intervals: a live
// process advances its last_seen, a dead one does not.
type nodeClaim struct {
	incarnation string
	lastSeen    int64
	exists      bool
}

func (p *Postgres) checkDuplicate(ctx context.Context) (nodeClaim, error) {
	id := p.local.Identity.NodeID
	read := func() (inc string, seen int64, ok bool, err error) {
		err = p.db.QueryRowContext(ctx, `SELECT incarnation, last_seen FROM cluster_nodes WHERE id=?`, id).Scan(&inc, &seen)
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false, nil
		}
		return inc, seen, err == nil, err
	}
	inc, seen, ok, err := read()
	if err != nil {
		return nodeClaim{}, err
	}
	if !ok {
		return nodeClaim{}, nil
	}
	claim := nodeClaim{incarnation: inc, lastSeen: seen, exists: true}
	if inc == p.inc {
		return claim, nil
	}
	if p.o.Now().Sub(time.UnixMilli(seen)) > p.o.LiveAfter {
		return claim, nil // long dead: claim only if this exact row is unchanged
	}
	p.log.Info("cluster: this node's heartbeat row is recent; checking that no other process uses this node ID", "wait", 2*p.o.HeartbeatInterval)
	deadline := time.Now().Add(2*p.o.HeartbeatInterval + p.o.HeartbeatInterval/2)
	step := max(p.o.HeartbeatInterval/4, 10*time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nodeClaim{}, ctx.Err()
		case <-time.After(step):
		}
		inc2, seen2, ok2, err := read()
		if err != nil {
			return nodeClaim{}, err
		}
		if !ok2 {
			return nodeClaim{}, nil
		}
		if inc2 != inc || seen2 != seen {
			return nodeClaim{}, duplicateNodeError(id, p.o.IdentityPath)
		}
	}
	return claim, nil
}

// fail stops this node's cluster membership after a fatal conflict and
// reports it once through OnFatal.
func (p *Postgres) fail(err error) {
	p.mu.Lock()
	if p.fatal != nil {
		p.mu.Unlock()
		return
	}
	p.fatal = err
	p.mu.Unlock()
	p.log.Error("cluster: leaving the cluster", "err", err)
	if p.cancel != nil {
		p.cancel() // stops heartbeats, election (releasing the lock) and the bus
	}
	p.lead.set(false, 0)
	if p.o.OnFatal != nil {
		go p.o.OnFatal(err)
	}
}

// Err is the fatal error that ended this node's membership, if any.
func (p *Postgres) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fatal
}

// Nodes lists live members.
func (p *Postgres) Nodes(ctx context.Context) ([]Node, error) {
	all, err := p.AllNodes(ctx)
	if err != nil {
		return nil, err
	}
	live := all[:0]
	for _, n := range all {
		if n.Live {
			live = append(live, n)
		}
	}
	return live, nil
}

// AllNodes lists every recorded member with its liveness.
func (p *Postgres) AllNodes(ctx context.Context) ([]Node, error) {
	var leaderID string
	if err := p.db.QueryRowContext(ctx, `SELECT node_id FROM cluster_leader WHERE id=1`).Scan(&leaderID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT id,name,version,addrs,caps,active_sessions,started_at,last_seen,draining,migrations FROM cluster_nodes ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := p.o.Now()
	selfLeads := p.IsLeader()
	var out []Node
	for rows.Next() {
		var (
			n                 Node
			addrs, caps, migs string
			startedMS, seenMS int64
			draining          int
		)
		if err := rows.Scan(&n.ID, &n.Name, &n.Version, &addrs, &caps, &n.Load, &startedMS, &seenMS, &draining, &migs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(addrs), &n.Addrs)
		_ = json.Unmarshal([]byte(caps), &n.Caps)
		if migs != "" {
			_ = json.Unmarshal([]byte(migs), &n.Migrations)
		}
		n.Draining = draining != 0
		n.StartedAt, n.LastSeen = time.UnixMilli(startedMS), time.UnixMilli(seenMS)
		n.Live = now.Sub(n.LastSeen) <= p.o.LiveAfter
		if n.ID == p.local.Identity.NodeID {
			n.Leader = selfLeads
		} else {
			n.Leader = !selfLeads && n.Live && n.ID == leaderID
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// checkClock warns when this node's clock is far from the server's.
func (p *Postgres) checkClock(ctx context.Context) {
	var dbMS int64
	before := p.o.Now()
	if err := p.db.QueryRowContext(ctx, `SELECT CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)`).Scan(&dbMS); err != nil {
		return
	}
	after := p.o.Now()
	local := before.Add(after.Sub(before) / 2)
	skew := local.Sub(time.UnixMilli(dbMS))
	p.mu.Lock()
	p.lastSkew = skew
	p.mu.Unlock()
	if skew < 0 {
		skew = -skew
	}
	if skew > p.o.LiveAfter/4 {
		p.log.Warn("cluster: this node's clock differs from the database server's; heartbeat liveness assumes synchronised clocks (enable NTP)",
			"skew", skew.Round(time.Millisecond))
	}
}

// ---- leadership --------------------------------------------------------------

func (p *Postgres) electionLoop(ctx context.Context) {
	defer p.wg.Done()
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			conn.Close(context.Background()) // ends the session: releases the lock
		}
		p.lead.set(false, 0)
	}()
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.stepWake:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		conn = p.electionStep(ctx, conn)
		t.Reset(p.o.ElectionInterval)
	}
}

func (p *Postgres) stepDown(conn *pgx.Conn, reason string, err error) {
	if conn != nil {
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn.Close(cctx)
		cancel()
	}
	if p.lead.isLeader() {
		p.log.Warn("cluster: leadership lost", "reason", reason, "err", err)
		p.mu.Lock()
		p.holdUntil = p.o.Now().Add(p.o.StepDownHold)
		p.mu.Unlock()
	}
	p.lead.set(false, 0)
}

// electionStep probes the held lock or tries to take it; it returns the
// lock connection to keep (nil after a failure).
func (p *Postgres) electionStep(ctx context.Context, conn *pgx.Conn) *pgx.Conn {
	leading := p.lead.isLeader()
	p.mu.Lock()
	resigned := p.resigned
	p.mu.Unlock()
	if resigned {
		if conn != nil || leading {
			p.stepDown(conn, "stepping down for shutdown", nil)
		}
		return nil
	}
	if p.local.draining() && p.othersCanLead(ctx) {
		// About to restart for an update: let a node that stays up lead
		// (and publish connections without this node's addresses).
		if leading {
			p.stepDown(conn, "draining for an update; handing leadership over", nil)
			return nil
		}
		return conn
	}
	if !leading {
		p.mu.Lock()
		hold := p.o.Now().Before(p.holdUntil)
		p.mu.Unlock()
		if hold {
			return conn
		}
	}
	if conn == nil {
		c, err := p.connect(ctx, "leader", map[string]string{
			"idle_session_timeout": strconv.FormatInt(p.o.IdleSessionTimeout.Milliseconds(), 10),
		})
		if err != nil {
			if ctx.Err() == nil {
				p.log.Debug("cluster: leader connection failed", "err", err)
			}
			return nil
		}
		conn = c
	}
	qctx, cancel := context.WithTimeout(ctx, p.o.ProbeTimeout)
	defer cancel()
	if leading {
		if _, err := conn.Exec(qctx, `SELECT 1`); err != nil {
			if ctx.Err() != nil {
				return conn // shutting down; the deferred close releases the lock
			}
			p.stepDown(conn, "database session lost", err)
			return nil
		}
		return conn
	}
	var got bool
	if err := conn.QueryRow(qctx, `SELECT pg_try_advisory_lock($1, hashtext(current_schema()))`, p.o.LockKey).Scan(&got); err != nil {
		p.stepDown(conn, "lock attempt failed", err)
		return nil
	}
	if !got {
		return conn
	}
	var epoch int64
	err := conn.QueryRow(qctx, `INSERT INTO cluster_leader(id, epoch, node_id, acquired_at) VALUES (1, 1, $1, $2)
ON CONFLICT (id) DO UPDATE SET epoch = cluster_leader.epoch + 1, node_id = excluded.node_id, acquired_at = excluded.acquired_at
RETURNING epoch`, p.local.Identity.NodeID, p.o.Now().UnixMilli()).Scan(&epoch)
	if err != nil {
		// Without a recorded epoch, give the lock back for another node.
		p.log.Warn("cluster: record leader epoch", "err", err)
		p.stepDown(conn, "epoch update failed", err)
		return nil
	}
	p.log.Info("cluster: acquired leadership", "epoch", epoch)
	p.lead.set(true, epoch)
	return conn
}

// othersCanLead reports whether another live node that is not draining
// could take leadership. On a database error it answers false, so a lone
// or isolated node keeps its role.
func (p *Postgres) othersCanLead(ctx context.Context) bool {
	qctx, cancel := context.WithTimeout(ctx, p.o.ProbeTimeout)
	defer cancel()
	var n int
	err := p.db.QueryRowContext(qctx, `SELECT COUNT(*) FROM cluster_nodes WHERE id<>? AND draining=0 AND last_seen>=?`,
		p.local.Identity.NodeID, p.o.Now().Add(-p.o.LiveAfter).UnixMilli()).Scan(&n)
	return err == nil && n > 0
}

// StepDown gives up leadership for good (until the process restarts) and
// releases the leader lock at once, so another node takes over within an
// election interval instead of waiting for this node to leave the cluster.
// Call it at the start of a graceful shutdown. It waits (bounded by ctx)
// for the lock to be released.
func (p *Postgres) StepDown(ctx context.Context) {
	p.mu.Lock()
	p.resigned = true
	p.mu.Unlock()
	if !p.IsLeader() {
		return
	}
	done := make(chan struct{})
	unsub := p.lead.subscribe(func(leader bool) {
		if !leader {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	})
	defer unsub()
	select {
	case p.stepWake <- struct{}{}:
	default:
	}
	if !p.IsLeader() {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ---- event bus ---------------------------------------------------------------

func (p *Postgres) setBusUp(up bool) {
	p.mu.Lock()
	p.busUp = up
	p.mu.Unlock()
}

func (p *Postgres) listenLoop(ctx context.Context) {
	defer p.wg.Done()
	defer p.setBusUp(false)
	backoff := p.o.MinBackoff
	first := true
	sleep := func() bool {
		t := time.NewTimer(backoff)
		defer t.Stop()
		backoff = min(backoff*2, p.o.MaxBackoff)
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for ctx.Err() == nil {
		conn, err := p.connect(ctx, "bus", nil)
		if err == nil {
			if _, err = conn.Exec(ctx, `LISTEN `+pgx.Identifier{p.Channel()}.Sanitize()); err != nil {
				conn.Close(context.Background())
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.busFail.Fail(err, "retry", backoff)
			if !sleep() {
				return
			}
			continue
		}
		p.setBusUp(true)
		p.busFail.OK()
		// Resync after every LISTEN, the first one included: components
		// fill their caches before the node joins (and Start may give up
		// waiting for the bus or be retried), so events other nodes sent
		// before this node first listened were never received either. It
		// is delivered before Start returns.
		if first {
			p.log.Debug("cluster: event bus listening; resynchronising caches")
		} else {
			p.log.Debug("cluster: event bus reconnected; resynchronising caches")
		}
		p.bus.deliver(TopicResync, nil)
		p.busOnce.Do(func() { close(p.busReady) })
		first = false
		backoff = p.o.MinBackoff
		err = p.receive(ctx, conn)
		conn.Close(context.Background())
		p.setBusUp(false)
		if ctx.Err() != nil {
			return
		}
		p.busFail.Fail(err)
		if !sleep() {
			return
		}
	}
}

// receive dispatches notifications until the connection fails.
func (p *Postgres) receive(ctx context.Context, conn *pgx.Conn) error {
	for {
		wctx, cancel := context.WithTimeout(ctx, p.o.ListenPing)
		n, err := conn.WaitForNotification(wctx)
		timedOut := wctx.Err() != nil
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !timedOut || conn.IsClosed() {
				return err
			}
			// Idle: make sure the connection is still alive.
			pctx, cancel := context.WithTimeout(ctx, p.o.ProbeTimeout)
			err = conn.Ping(pctx)
			cancel()
			if err != nil {
				return err
			}
			continue
		}
		if n != nil {
			p.dispatch(n.Payload)
		}
	}
}

func (p *Postgres) dispatch(raw string) {
	env, payload, err := decodeEnvelope(raw)
	if err != nil {
		p.log.Debug("cluster: ignoring malformed event", "err", err)
		return
	}
	if env.Inc == p.inc {
		return // our own Publish, already delivered locally
	}
	if env.Topic == TopicResync {
		return // local-only
	}
	p.bus.deliver(env.Topic, payload)
}

// ---- connections -------------------------------------------------------------

// connect opens a dedicated connection with server-side keepalives so a
// half-open session is noticed by the server too.
func (p *Postgres) connect(ctx context.Context, role string, params map[string]string) (*pgx.Conn, error) {
	cfg := p.connCfg.Copy()
	// A config copied from a live connection carries that connection's
	// notification buffer hook; pgx installs a fresh one when nil.
	cfg.OnNotification = nil
	rp := map[string]string{}
	for k, v := range cfg.RuntimeParams {
		rp[k] = v
	}
	rp["application_name"] = "ether-" + role + "/" + shortID(p.local.Identity.NodeID)
	rp["tcp_keepalives_idle"] = "10"
	rp["tcp_keepalives_interval"] = "5"
	rp["tcp_keepalives_count"] = "3"
	for k, v := range params {
		rp[k] = v
	}
	cfg.RuntimeParams = rp
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return pgx.ConnectConfig(cctx, cfg)
}

// connConfigOf copies the configuration of one of db's connections.
func connConfigOf(ctx context.Context, db *sql.DB) (*pgx.ConnConfig, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("cluster: database connection: %w", err)
	}
	defer conn.Close()
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(dc any) error {
		pc, ok := dc.(pgxConnection)
		if !ok {
			return errors.New("cluster: the database is not PostgreSQL")
		}
		cfg = pc.PgxConn().Config()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// channelFor is the NOTIFY channel for a schema.
func channelFor(schema string) string {
	if schema == "" || schema == "public" {
		return DefaultChannel
	}
	h := fnv.New32a()
	h.Write([]byte(schema))
	return fmt.Sprintf("%s_%08x", DefaultChannel, h.Sum32())
}
