package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestClusterStatusStandalone(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Update(func(c *config.Config) error { c.Transcode.FFmpegPath = "/not-installed/ffmpeg"; return nil }); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id, err := cluster.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.clustered() || a.clusterNodeID() != "" {
		t.Fatal("SQLite must run standalone")
	}
	r := httptest.NewRequest("GET", "http://localhost/ether/api/cluster/", nil)
	r.RemoteAddr = "127.0.0.1:1024"
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var st clusterStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode != cluster.ModeSolo || st.Self.ID != id.NodeID || !st.Self.Leader || st.Leader != id.NodeID ||
		len(st.Nodes) != 1 || !st.Nodes[0].Live || st.Health != nil {
		t.Fatalf("status %+v", st)
	}
}

// fakeMembers is a clustered runtime whose membership the test controls.
type fakeMembers struct {
	*cluster.Solo
	nodes []cluster.Node
}

func (f *fakeMembers) Mode() string { return cluster.ModePostgres }
func (f *fakeMembers) AllNodes(context.Context) ([]cluster.Node, error) {
	return f.nodes, nil
}

func TestRefreshPeersPublishesUnionWithGrace(t *testing.T) {
	now := time.Now()
	fm := &fakeMembers{Solo: cluster.NewSolo(cluster.Local{Identity: cluster.Identity{NodeID: "self", Name: "a"}}, nil)}
	a := &App{cluster: fm, peers: &peerSet{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	fm.nodes = []cluster.Node{
		{ID: "self", Name: "a", Addrs: []string{"http://10.0.0.1:32400"}, LastSeen: now},
		{ID: "n2", Name: "b", Addrs: []string{"http://10.0.0.2:32400"}, LastSeen: now.Add(-30 * time.Second)},
	}
	if !a.refreshPeers(context.Background()) {
		t.Fatal("first membership read should change the peer set")
	}
	if got := strings.Join(a.peers.get(), ","); got != "http://10.0.0.2:32400" {
		t.Fatalf("peers %s", got)
	}
	if a.refreshPeers(context.Background()) {
		t.Fatal("unchanged membership reported as a change")
	}
	// A node silent for more than memberGrace is dropped.
	fm.nodes[1].LastSeen = now.Add(-memberGrace - time.Second)
	if !a.refreshPeers(context.Background()) || len(a.peers.get()) != 0 {
		t.Fatalf("expired node still published: %v", a.peers.get())
	}
}

// TestClusterWiringPostgres builds a node on a throwaway PostgreSQL schema
// and joins the cluster without running the (network-bound) services.
func TestClusterWiringPostgres(t *testing.T) {
	raw := os.Getenv(store.TestPostgresEnv)
	if raw == "" {
		t.Skip("set " + store.TestPostgresEnv + " to run PostgreSQL wiring tests")
	}
	ctx := context.Background()
	schemaDB, err := store.OpenTestPostgres(ctx, raw) // owns (and drops) the schema
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { schemaDB.Close() })
	var schema string
	if err := schemaDB.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Setenv(config.DatabaseURLEnv, u.String())

	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Update(func(c *config.Config) error {
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		c.Plex.RemoteAccess = false // node addresses stay offline (no WAN lookup)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.clustered() || a.clusterNodeID() == "" {
		t.Fatal("PostgreSQL must run clustered")
	}
	if err := a.cluster.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !a.cluster.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("sole node never became leader")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := httptest.NewRequest("GET", "http://localhost/ether/api/cluster/", nil)
	r.RemoteAddr = "127.0.0.1:1024"
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	var st clusterStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != 200 {
		t.Fatalf("status %d %s %v", w.Code, w.Body.String(), err)
	}
	if st.Mode != cluster.ModePostgres || st.Leader != a.clusterNodeID() || st.Epoch < 1 || len(st.Nodes) != 1 ||
		!st.Nodes[0].Live || st.Health == nil || !st.Health.BusConnected {
		t.Fatalf("status %+v", st)
	}
}

// flakyStart is a clustered runtime whose Start fails a few times first.
type flakyStart struct {
	*cluster.Solo
	fails  int
	starts int
	err    error
}

func (f *flakyStart) Start(context.Context) error {
	f.starts++
	if f.starts <= f.fails {
		return f.err
	}
	return nil
}

func TestStartClusterRetriesTransientFailures(t *testing.T) {
	defer func(lo, hi, budget time.Duration) {
		clusterStartRetryMin, clusterStartRetryMax, clusterStartBudget = lo, hi, budget
	}(clusterStartRetryMin, clusterStartRetryMax, clusterStartBudget)
	clusterStartRetryMin, clusterStartRetryMax, clusterStartBudget = time.Millisecond, 2*time.Millisecond, time.Second
	solo := cluster.NewSolo(cluster.Local{Identity: cluster.Identity{NodeID: "self"}}, nil)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A database blip while joining must not leave the node outside the
	// cluster (no heartbeat, never leader) for the rest of the process.
	f := &flakyStart{Solo: solo, fails: 2, err: errors.New("cluster: database connection: connection refused")}
	a := &App{cluster: f, log: quiet}
	if err := a.startCluster(context.Background()); err != nil || f.starts != 3 {
		t.Fatalf("err %v after %d starts", err, f.starts)
	}

	// A duplicate node identity is final.
	dup := &flakyStart{Solo: solo, fails: 5, err: fmt.Errorf("%w (node ID x)", cluster.ErrDuplicateNode)}
	a = &App{cluster: dup, log: quiet}
	if err := a.startCluster(context.Background()); !errors.Is(err, cluster.ErrDuplicateNode) || dup.starts != 1 {
		t.Fatalf("duplicate: err %v after %d starts", err, dup.starts)
	}

	// Shutdown while waiting ends the retries.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	down := &flakyStart{Solo: solo, fails: 1 << 30, err: errors.New("unreachable")}
	a = &App{cluster: down, log: quiet}
	if err := a.startCluster(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: err %v", err)
	}

	clusterStartBudget = 10 * time.Millisecond
	if err := a.startCluster(context.Background()); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "owned heartbeat") {
		t.Fatalf("unavailable database: %v", err)
	}
}
