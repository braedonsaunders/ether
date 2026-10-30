package playback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// twoNodes builds two Managers sharing one database (on PostgreSQL: one
// schema) and an in-memory cluster bus, as two cluster nodes would.
func twoNodes(t *testing.T, tweak func(h *harness, o *Options)) (*harness, *harness, *clusterbus.Hub) {
	t.Helper()
	db := store.OpenTest(t)
	hub := clusterbus.NewHub()
	rels := []*fakeRelease{mkv("A", 10000, time.Minute)}
	mk := func(name string) *harness {
		return newHarness(t, rels, []string{"A"}, func(h *harness, o *Options) {
			h.db, h.node = db, hub.Node(name)
			o.SessionHeartbeat = 50 * time.Millisecond
			if tweak != nil {
				tweak(h, o)
			}
		})
	}
	return mk("a"), mk("b"), hub
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClusterUserLimitSpansNodes(t *testing.T) {
	a, b, _ := twoNodes(t, func(h *harness, o *Options) { h.cfg.Server.DefaultStreamLimit = 1 })
	ctx := context.Background()
	ps := a.prepare(friend, movieKey, "tv")
	if _, err := b.m.Prepare(ctx, friend, movie2, defaultCaps("phone")); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("second stream on another node admitted: %v", err)
	}
	// Other users are unaffected (no server limit).
	b.prepare(owner, movie2, "owner-tv")
	// The same client moving to the other node replaces its stream.
	moved := b.prepare(friend, movie3, "tv")
	b.m.Stop(moved.ID)
	a.m.Stop(ps.ID)
	eventually(t, "slot freed cluster-wide", func() bool {
		_, err := b.m.Prepare(ctx, friend, movie2, defaultCaps("phone"))
		return err == nil
	})
}

func TestClusterCollidingPlexAndBrokerScopesConsumeSeparateSlots(t *testing.T) {
	for _, brokerFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("brokerFirst=%t", brokerFirst), func(t *testing.T) {
			a, b, _ := twoNodes(t, func(h *harness, _ *Options) { h.cfg.Server.MaxStreams = 1 })
			ctx := context.Background()
			caps := defaultCaps("8:jellyfincolliding-device")
			caps.Protocol = "plex"
			other := caps
			other.Protocol = "jellyfin"
			var first *core.PlaySession
			var err error
			if brokerFirst {
				first, err = a.m.PrepareSurface(ctx, friend, movieKey, other, "", false)
			} else {
				first, err = a.m.Prepare(ctx, friend, movieKey, caps)
			}
			if err != nil {
				t.Fatal(err)
			}
			if brokerFirst {
				_, err = b.m.Prepare(ctx, friend, movieKey, caps)
			} else {
				_, err = b.m.PrepareSurface(ctx, friend, movieKey, other, "", false)
			}
			if !errors.Is(err, core.ErrStreamLimit) {
				t.Fatalf("remote collision bypassed stream limit: %v", err)
			}
			a.m.Stop(first.ID)
		})
	}
}

func TestClusterServerLimitAndLiveTVUnits(t *testing.T) {
	a, b, _ := twoNodes(t, func(h *harness, o *Options) { h.cfg.Server.MaxStreams = 2 })
	ctx := context.Background()
	a.prepare(owner, movieKey, "tv")
	// A live TV viewing unit on node B takes the second slot...
	if err := b.m.BeginChannel(friend, defaultCaps("iptv"), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	// ...its programme sources do not take more...
	if _, err := b.m.PrepareChannel(ctx, friend, movie2, defaultCaps("prog"), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	// ...and the server is full everywhere.
	if _, err := a.m.Prepare(ctx, friend, movie3, defaultCaps("other")); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("server limit exceeded across nodes: %v", err)
	}
	if err := a.m.BeginChannel(owner, defaultCaps("iptv2"), "fedcba9876543210fedcba9876543210"); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("live TV unit admitted past the server limit: %v", err)
	}
}

func TestClusterAdmissionIsAtomic(t *testing.T) {
	a, b, _ := twoNodes(t, func(h *harness, o *Options) { h.cfg.Server.MaxStreams = 1 })
	groups := []string{
		"00000000000000000000000000000001", "00000000000000000000000000000002",
		"00000000000000000000000000000003", "00000000000000000000000000000004",
		"00000000000000000000000000000005", "00000000000000000000000000000006",
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i, g := range groups {
		m := a.m
		if i%2 == 1 {
			m = b.m
		}
		wg.Add(1)
		go func(m *Manager, g string, uid int64) {
			defer wg.Done()
			if err := m.BeginChannel(&core.User{ID: uid, Username: g}, defaultCaps(g), g); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(m, g, int64(10+i))
	}
	wg.Wait()
	if admitted != 1 {
		t.Fatalf("admitted %d streams with a server limit of 1", admitted)
	}
}

func TestClusterRemoteSessionsAndDeadNodes(t *testing.T) {
	a, b, hub := twoNodes(t, func(h *harness, o *Options) { h.cfg.Server.MaxStreams = 1 })
	ps := a.prepare(friend, movieKey, "tv")
	var remote []*core.PlaySession
	eventually(t, "B sees A's session", func() bool {
		remote = b.m.RemoteSessions(context.Background())
		return len(remote) == 1
	})
	if r := remote[0]; r.ID != ps.ID || r.Node != "a" || r.TitleName != "The Signal (1999)" || r.Username != "friend" || r.Source != nil {
		t.Fatalf("remote session %+v", r)
	}
	if got := a.m.RemoteSessions(context.Background()); len(got) != 0 {
		t.Fatalf("A lists its own session as remote: %v", got)
	}
	if _, err := b.m.Prepare(context.Background(), owner, movie2, defaultCaps("x")); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("limit not applied: %v", err)
	}
	// Node A dies: its streams stop counting as soon as the cluster sees it.
	hub.Kill("a")
	time.Sleep(liveNodesTTL + 100*time.Millisecond)
	if _, err := b.m.Prepare(context.Background(), owner, movie2, defaultCaps("x")); err != nil {
		t.Fatalf("dead node's stream still counted: %v", err)
	}
	if got := b.m.RemoteSessions(context.Background()); len(got) != 0 {
		t.Fatalf("dead node's sessions listed: %v", got)
	}
}

// Rows an old process of this node left behind are removed at startup;
// other nodes' rows are not touched.
func TestClusterStartupClearsOnlyOwnRows(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	for _, row := range []struct{ id, node string }{{"old-a", "a"}, {"live-b", "b"}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO playback_live_sessions(id,node_id,user_id,kind,updated_at) VALUES(?,?,1,'session',?)`,
			row.id, row.node, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	hub := clusterbus.NewHub()
	newHarness(t, nil, nil, func(h *harness, o *Options) { h.db, h.node = db, hub.Node("a") })
	var ids []string
	rows, err := db.QueryContext(ctx, `SELECT id FROM playback_live_sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != "live-b" {
		t.Fatalf("rows after startup: %v", ids)
	}
}

// A database outage never stops playback: admission fails open.
func TestClusterAdmissionFailsOpen(t *testing.T) {
	h := newHarness(t, nil, nil)
	broken := store.OpenTest(t)
	broken.Close()
	r := newRegistry(h.m, broken, clusterbus.NewHub().Node("x"))
	id := identity{userID: 1, clientID: "c", key: movieKey}
	err := r.admit(context.Background(), owner, id, unitRow{id: "p", user: 1, kind: unitPending}, 0, 0, 1, 1)
	if err != nil {
		t.Fatalf("admission failed closed: %v", err)
	}
}

// Standalone managers (no Cluster) never touch the shared table.
func TestStandaloneWritesNoSessionRows(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	h.prepare(owner, movieKey, "tv")
	if h.m.reg != nil || h.m.RemoteSessions(context.Background()) != nil {
		t.Fatal("standalone manager has a registry")
	}
}

// An owner change made on one node drops the warm detail-page entries of
// every node.
func TestInvalidateTitleReachesEveryNode(t *testing.T) {
	a, b, hub := twoNodes(t, nil)
	warm := func(h *harness, key core.RatingKey) {
		h.m.mu.Lock()
		h.m.warm[identity{userID: 1, clientID: "c", key: key}] = &warmEntry{}
		h.m.mu.Unlock()
	}
	count := func(h *harness) int {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return len(h.m.warm)
	}
	warm(a, movieKey)
	warm(b, movieKey)
	warm(b, movie2)
	if n := a.m.InvalidateTitleEverywhere(movieKey); n != 1 {
		t.Fatalf("local entries dropped %d", n)
	}
	eventually(t, "B drops the title", func() bool { return count(b) == 1 })
	b.m.mu.Lock()
	_, kept := b.m.warm[identity{userID: 1, clientID: "c", key: movie2}]
	b.m.mu.Unlock()
	if !kept {
		t.Fatal("B dropped another title")
	}
	// A bus resync drops every warm entry.
	hub.Resync("b")
	eventually(t, "resync clears", func() bool { return count(b) == 0 })
}
