package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/updater"
)

func TestRefreshPeersDropsDrainingNode(t *testing.T) {
	now := time.Now()
	fm := &fakeMembers{Solo: cluster.NewSolo(cluster.Local{Identity: cluster.Identity{NodeID: "self", Name: "a"}}, nil)}
	a := &App{cluster: fm, peers: &peerSet{}, log: quietLog()}
	fm.nodes = []cluster.Node{
		{ID: "self", Name: "a", LastSeen: now},
		{ID: "n2", Name: "b", Addrs: []string{"http://10.0.0.2:32400"}, LastSeen: now},
		{ID: "n3", Name: "c", Addrs: []string{"http://10.0.0.3:32400"}, LastSeen: now},
	}
	a.refreshPeers(context.Background())
	if got := strings.Join(a.peers.get(), ","); got != "http://10.0.0.2:32400,http://10.0.0.3:32400" {
		t.Fatalf("peers %s", got)
	}
	fm.nodes[1].Draining = true // b drains before its update
	if !a.refreshPeers(context.Background()) {
		t.Fatal("draining must change the published union (triggering a republish)")
	}
	if got := strings.Join(a.peers.get(), ","); got != "http://10.0.0.3:32400" {
		t.Fatalf("draining node still published: %s", got)
	}
}

func TestVersionWarning(t *testing.T) {
	if w := versionWarning([]cluster.Node{{Name: "a", Version: "1", Live: true}, {Name: "b", Version: "1", Live: true}}); w != "" {
		t.Fatalf("same versions warned: %s", w)
	}
	w := versionWarning([]cluster.Node{{Name: "a", Version: "1", Live: true}, {Name: "b", Version: "2", Live: true}, {Name: "c", Version: "0", Live: false}})
	if !strings.Contains(w, "1: a") || !strings.Contains(w, "2: b") || strings.Contains(w, "0: c") {
		t.Fatalf("warning %q", w)
	}
}

// fakeFeed serves one binary release.
type fakeFeed struct {
	upd     *updater.Update
	payload []byte
}

func newFakeFeed(version string) *fakeFeed {
	payload := []byte("ether " + version)
	sum := sha256.Sum256(payload)
	return &fakeFeed{payload: payload, upd: &updater.Update{
		Manifest: &updater.Manifest{Version: version, Channel: updater.ChannelStable},
		Artifact: &updater.Artifact{Kind: updater.KindBinary, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Filename: "ether-" + version},
	}}
}

func (f *fakeFeed) Check(context.Context) (*updater.Update, error) { return f.upd, nil }
func (f *fakeFeed) Download(_ context.Context, upd *updater.Update, dir string, _ updater.ProgressFunc) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, upd.Artifact.Filename)
	return p, os.WriteFile(p, f.payload, 0o644)
}

type noFeed struct{}

func (noFeed) Check(context.Context) (*updater.Update, error) { return nil, nil }
func (noFeed) Download(context.Context, *updater.Update, string, updater.ProgressFunc) (string, error) {
	return "", errors.New("no update")
}

// installLog records installs across nodes.
type installLog struct {
	mu     sync.Mutex
	events []string
}

func (l *installLog) add(s string) {
	l.mu.Lock()
	l.events = append(l.events, s)
	l.mu.Unlock()
}

func (l *installLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// rollingManager builds a node's update manager wired like initUpdates on
// a clustered self-applying node, with fast timings and a fake install.
func rollingManager(t *testing.T, a *App, name, version string, feed updater.Checker, boot updater.StartupResult, log *installLog, restarted chan<- string) *updater.Manager {
	t.Helper()
	dir := filepath.Join(a.opts.DataDir, "updates")
	return updater.NewManager(updater.ManagerOptions{
		NewClient: func(config.Updates) (updater.Checker, error) { return feed, nil },
		Config: func() config.Updates {
			return config.Updates{Enabled: true, AutoUpdate: true, MaintenanceStart: "00:00", MaintenanceEnd: "00:00"}
		},
		CurrentVersion: version,
		Dir:            dir,
		Target:         filepath.Join(a.opts.DataDir, "ether-bin"),
		SelfApply:      true,
		ActiveSessions: a.activeSessionCount,
		RequestRestart: func(string) { restarted <- name },
		Boot:           boot,
		Coordinator:    a.newClusterUpdates(),
		NodeName:       name,
		ApplyBinary: func(newPath, target string) error {
			log.add(name + " installs")
			return nil
		},
		CleanupOld:      func(string) error { return nil },
		FirstCheckDelay: time.Millisecond,
		Tick:            20 * time.Millisecond,
		HealthPeriod:    700 * time.Millisecond,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// TestRollingUpdateTwoNodesPostgres stages one release on two real
// cluster nodes: exactly one drains (leaving the published union) and
// installs; the other waits its turn until the first node's new version
// has confirmed its health and released the update lease.
func TestRollingUpdateTwoNodesPostgres(t *testing.T) {
	a, b := twoClusterApps(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log installLog
	restarted := make(chan string, 4)
	feed := newFakeFeed("2.0.0")
	ma := rollingManager(t, a, "a", "1.0.0", feed, updater.StartupResult{}, &log, restarted)
	mb := rollingManager(t, b, "b", "1.0.0", feed, updater.StartupResult{}, &log, restarted)
	go ma.Run(ctx)
	var first string
	select {
	case first = <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("no node installed: a=%+v", ma.Status())
	}
	if first != "a" {
		t.Fatalf("first install by %s", first)
	}
	// a is draining: its heartbeat says so and b no longer publishes it.
	if !a.nodeDraining() || !a.playback.Draining() {
		t.Fatal("installing node is not draining")
	}
	until(t, "b sees a draining", func() bool {
		nodes, _ := b.cluster.AllNodes(ctx)
		for _, n := range nodes {
			if n.ID == a.cluster.Self().ID {
				return n.Draining && n.Migrations["cluster"] >= 2
			}
		}
		return false
	})
	if _, err := a.playback.Prepare(ctx, &core.User{ID: 1, Username: "u"}, core.MovieKey(1), core.ClientCaps{ClientID: "c"}); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("draining node admitted a new session: %v", err)
	}

	go mb.Run(ctx)
	time.Sleep(500 * time.Millisecond)
	if got := log.get(); len(got) != 1 {
		t.Fatalf("b installed while a held the lease: %v", got)
	}
	if st := mb.Status(); st.State != updater.StateWaitingTurn || st.Cluster == nil || st.Cluster.LeaseHolder != a.cluster.Self().Name {
		t.Fatalf("b status %+v %+v", st.State, st.Cluster)
	}

	// a's restarted process (2.0.0) confirms health, then releases.
	var p updater.Pending
	raw, err := os.ReadFile(updater.PendingPath(filepath.Join(a.opts.DataDir, "updates")))
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	(&clusterUpdates{a: a}).SetDraining(false) // the new process starts in rotation
	ma2 := rollingManager(t, a, "a", "2.0.0", noFeed{}, updater.StartupResult{Confirming: &p}, &log, restarted)
	confirmStart := time.Now()
	go ma2.Run(ctx)
	select {
	case who := <-restarted:
		if who != "b" {
			t.Fatalf("second install by %s", who)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("b never got its turn: %+v", mb.Status())
	}
	if time.Since(confirmStart) < 700*time.Millisecond {
		t.Fatal("b installed before a confirmed its new version")
	}
	if got := strings.Join(log.get(), ","); got != "a installs,b installs" {
		t.Fatalf("installs %s", got)
	}
	lease := &cluster.UpdateLease{DB: a.db, NodeID: "x"}
	if h, err := lease.Holder(ctx); err != nil || h == nil || h.NodeID != b.cluster.Self().ID {
		t.Fatalf("lease holder after b's install: %+v %v", h, err)
	}
}

func TestOlderNodesGatePostgres(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("set " + store.TestPostgresEnv + " to run PostgreSQL tests")
	}
	db := store.OpenTest(t)
	ctx := context.Background()
	now := time.Now()
	gate := olderNodesGate("self", 20*time.Second, func() time.Time { return now })
	pending := []string{"state/3"}
	run := func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		return gate(ctx, tx, pending)
	}
	if err := run(); err != nil {
		t.Fatalf("no other nodes: %v", err)
	}
	ins := func(id, migs string, seen time.Time) {
		if _, err := db.ExecContext(ctx, `INSERT INTO cluster_nodes(id,name,version,started_at,last_seen,migrations) VALUES(?,?,?,?,?,?)`,
			id, id, "v-"+id, seen.UnixMilli(), seen.UnixMilli(), migs); err != nil {
			t.Fatal(err)
		}
	}
	ins("new", `{"state":3}`, now)
	ins("dead", `{"state":2}`, now.Add(-time.Hour))
	if err := run(); err != nil {
		t.Fatalf("only up-to-date or dead nodes: %v", err)
	}
	ins("old", `{"state":2}`, now)
	if err := run(); !errors.Is(err, store.ErrMigrationDeferred) || !strings.Contains(err.Error(), "old (v-old)") {
		t.Fatalf("older live node: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE cluster_nodes SET migrations='' WHERE id='old'`); err != nil {
		t.Fatal(err)
	}
	if err := run(); !errors.Is(err, store.ErrMigrationDeferred) {
		t.Fatalf("node without a migration report counts as older: %v", err)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
