package playback

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// TestDatabaseOutageKeepsStreamsAndProgress runs a clustered node's
// playback and watch state through a PostgreSQL outage, simulated by an
// in-process TCP proxy between the node and the server that is cut (or
// black-holed) for about three seconds mid-stream. Stream reads must be
// unaffected, timeline writes must never block for long, a new session must
// still start (cluster-wide admission fails open), and everything reported
// during the outage must be in the database once it is back.
func TestDatabaseOutageKeepsStreamsAndProgress(t *testing.T) {
	raw := os.Getenv(store.TestPostgresEnv)
	if raw == "" {
		t.Skip("set " + store.TestPostgresEnv + " to run PostgreSQL outage tests")
	}
	for _, mode := range []string{"cut", "pause"} {
		t.Run(mode, func(t *testing.T) { runOutage(t, raw, mode) })
	}
}

func runOutage(t *testing.T, raw, mode string) {
	ctx := context.Background()
	direct, err := store.OpenTestPostgres(ctx, raw) // owns the schema; never proxied
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { direct.Close() })
	var schema string
	if err := direct.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	proxy, err := store.NewTestProxy(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.Close() })
	q := u.Query()
	q.Set("search_path", schema)
	q.Set("connect_timeout", "2")
	u.RawQuery = q.Encode()
	pu, _ := proxy.URL(u.String())
	db, err := sql.Open(store.PostgresDriverName, pu)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(6)
	t.Cleanup(func() { db.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	avail := store.NewAvailability(nil)
	ws := state.New(db, state.Options{Availability: avail, Logger: quiet})
	t.Cleanup(func() { ws.Close(context.Background()) })
	const size = 4 << 20
	h := newHarness(t, []*fakeRelease{mkv("A", size, 100*time.Second), mkv("B", size, 100*time.Second)}, []string{"A", "B"},
		func(h *harness, o *Options) {
			h.db, h.node, h.avail = db, clusterbus.NewHub().Node("a"), avail
			h.cfg.Server.DefaultStreamLimit = 5 // admission consults the cluster
			o.SessionHeartbeat = 50 * time.Millisecond
		})
	ps := h.prepare(owner, movieKey, "tv")
	srv := directPlayServer(t, h.m, ps.ID)
	read := func(off int64) {
		t.Helper()
		const n = 64 << 10
		resp, body, err := get(t, srv.URL, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", off, off+n-1)})
		if err != nil || resp.StatusCode != 206 || !bytes.Equal(body, expected("A", off, n)) {
			t.Fatalf("read at %d: status %d err %v len %d", off, resp.StatusCode, err, len(body))
		}
	}
	dur := 100 * time.Minute
	if err := ws.Progress(ctx, owner.ID, movieKey, time.Minute, dur, "playing"); err != nil {
		t.Fatal(err)
	}
	read(0)

	if mode == "cut" {
		proxy.Cut()
	} else {
		proxy.Pause()
	}
	outageStart := time.Now()
	var last time.Duration
	var off int64
	var started bool
	// One report has to clear the 5% start threshold (5 minutes of this
	// 100 minute item). A paused database can consume the whole window on
	// the first write, and a 2 minute report would correctly be stored as
	// no resume point.
	for i := 6; time.Since(outageStart) < 3*time.Second; i++ {
		last = time.Duration(i) * time.Minute
		t0 := time.Now()
		if err := ws.Progress(ctx, owner.ID, movieKey, last, dur, "playing"); err != nil {
			t.Fatalf("progress during outage: %v", err)
		}
		if d := time.Since(t0); d > 2500*time.Millisecond {
			t.Fatalf("timeline write blocked %v during the outage", d)
		}
		off = (off + 300<<10) % (size - 64<<10)
		read(off)
		if !started && time.Since(outageStart) > time.Second {
			// A new viewer on this node still starts: the cluster-wide
			// limit check fails open and the resume record is held.
			t0 := time.Now()
			if _, err := h.m.Prepare(ctx, friend, movie2, defaultCaps("phone")); err != nil {
				t.Fatalf("new session during outage: %v", err)
			}
			if d := time.Since(t0); d > 5*time.Second {
				t.Fatalf("new session waited %v on the outage", d)
			}
			started = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !avail.Down() {
		t.Fatal("outage not detected")
	}
	if st, _ := ws.OutboxStatus(); st.Depth != 1 {
		t.Fatalf("held progress %+v", st)
	}
	proxy.Restore()

	eventually(t, "held writes replayed", func() bool {
		st, _ := ws.OutboxStatus()
		rs, _ := h.m.ResumeOutboxStatus()
		return st.Depth == 0 && rs.Depth == 0
	})
	var offNS int64
	if err := direct.QueryRowContext(ctx, `SELECT view_offset_ns FROM state_watch WHERE user_id=? AND rating_key=?`,
		owner.ID, int64(movieKey)).Scan(&offNS); err != nil {
		t.Fatal(err)
	}
	if time.Duration(offNS) != last {
		t.Fatalf("persisted offset %v, want %v (the last report during the outage)", time.Duration(offNS), last)
	}
	var n int
	if err := direct.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_resume WHERE user_id=? AND rating_key=?`,
		friend.ID, int64(movie2)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("resume record of the session started during the outage: %d %v", n, err)
	}
	// The session registry catches up on its next heartbeat.
	eventually(t, "registry rows refreshed", func() bool {
		var rows int
		_ = direct.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_live_sessions WHERE kind='session'`).Scan(&rows)
		return rows == 2
	})
	read(0)
	if avail.Down() {
		t.Fatal("availability not restored")
	}
}
