package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/store"
)

const clusterPGBase = "postgres://parity:parity@parity-postgres:5432/"

// runClusterPair imports the same real seeded SQLite fixture once through the
// baseline's cluster API, then PostgreSQL clones its database into independent
// baseline and candidate worlds before either pair of nodes is started.
func runClusterPair(ctx context.Context, baseline, candidate, seedDir string, out io.Writer) error {
	dir, err := os.MkdirTemp("", "ether-plexparity-cluster-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	db, err := sql.Open(store.PostgresDriverName, clusterPGBase+"postgres?sslmode=disable")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, `CREATE DATABASE parity_seed`); err != nil {
		return fmt.Errorf("create fixture seed database: %w", err)
	}
	bootstrap := filepath.Join(dir, "bootstrap")
	if err = copyFixture(seedDir, bootstrap); err != nil {
		return err
	}
	cfg, err := config.Open(bootstrap)
	if err != nil {
		return err
	}
	if err = cfg.Update(func(c *config.Config) error {
		c.Plex.Token = "parity-owner"
		c.Plex.OwnerPlexID = 1
		return nil
	}); err != nil {
		return err
	}
	initCmd := exec.CommandContext(ctx, baseline, "cluster", "init", "--data-dir", bootstrap, "--database-url", clusterPGBase+"parity_seed?sslmode=disable")
	if data, err := initCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("import seeded world into PostgreSQL: %w: %s", err, data)
	}
	for _, name := range []string{"parity_baseline", "parity_candidate"} {
		if _, err := db.ExecContext(ctx, `CREATE DATABASE `+name+` TEMPLATE parity_seed`); err != nil {
			return fmt.Errorf("clone seeded fixture database %s: %w", name, err)
		}
	}
	tmdbTracker := &fixtureTracker{}
	tmdb := httptest.NewServer(trackedTMDBFixture(tmdbTracker))
	defer tmdb.Close()
	plexTracker := &fixtureTracker{}
	plexTV := httptest.NewServer(trackedPlexTVFixture(plexTracker))
	defer plexTV.Close()
	var transcripts []transcript
	for i, side := range []struct{ bin, database string }{{baseline, "parity_baseline"}, {candidate, "parity_candidate"}} {
		fmt.Fprintf(out, "cluster side %d: starting two nodes with shared database %s\n", i, side.database)
		group := filepath.Join(dir, fmt.Sprintf("side-%d", i))
		if err := os.Mkdir(group, 0700); err != nil {
			return err
		}
		dbURL := clusterPGBase + side.database + "?sslmode=disable"
		var nodes []*parityClusterNode
		for n, id := range []string{"11111111111111111111111111111111", "22222222222222222222222222222222"} {
			nodeDir := filepath.Join(group, fmt.Sprintf("node-%d", n))
			if err := configureClusterNode(bootstrap, nodeDir, dbURL, id, 32400+n); err != nil {
				closeClusterNodes(nodes)
				return err
			}
			node, err := startParityClusterNode(ctx, side.bin, nodeDir, 32400+n, tmdb.URL, plexTV.URL)
			if err != nil {
				closeClusterNodes(nodes)
				return err
			}
			nodes = append(nodes, node)
		}
		captured, err := captureClusterFailover(ctx, nodes)
		closeClusterNodes(nodes)
		if err != nil {
			return fmt.Errorf("cluster side %d: %w", i, err)
		}
		transcripts = append(transcripts, captured)
	}
	if missing := tmdbTracker.failures(); len(missing) > 0 {
		return fmt.Errorf("cluster TMDB fixture does not implement %v", missing)
	}
	if missing := plexTracker.failures(); len(missing) > 0 {
		return fmt.Errorf("cluster Plex.tv fixture does not implement %v", missing)
	}
	n, err := diffTranscripts(transcripts[0], transcripts[1], out)
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("%d unapproved cluster response differences", n)
	}
	return nil
}

type parityClusterNode struct {
	url    string
	cancel context.CancelFunc
	done   chan error
	log    *os.File
	closed bool
}

func startParityClusterNode(ctx context.Context, bin, dir string, port int, tmdb, plexTV string) (*parityClusterNode, error) {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cluster fixture port %s must be free: %w", addr, err)
	}
	ln.Close()
	log, err := os.Create(filepath.Join(dir, "parity-server.log"))
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, bin, "--data-dir", dir)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), "ETHER_PORT="+fmt.Sprint(port), "ETHER_PARITY_TMDB_URL="+tmdb+"/3", "ETHER_PARITY_PLEXTV_URL="+plexTV)
	if err := cmd.Start(); err != nil {
		cancel()
		log.Close()
		return nil, err
	}
	node := &parityClusterNode{url: "http://" + addr, cancel: cancel, done: make(chan error, 1), log: log}
	go func() { node.done <- cmd.Wait() }()
	client := &http.Client{Timeout: time.Second}
	deadline := time.NewTimer(120 * time.Second)
	defer deadline.Stop()
	for {
		r, err := client.Get(node.url + "/identity")
		if err == nil {
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				return node, nil
			}
		}
		select {
		case <-ctx.Done():
			node.close()
			return nil, ctx.Err()
		case <-deadline.C:
			node.close()
			return nil, fmt.Errorf("cluster node %s did not start", addr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (n *parityClusterNode) close() {
	if n == nil || n.closed {
		return
	}
	n.closed = true
	n.cancel()
	<-n.done
	n.log.Close()
}

func closeClusterNodes(nodes []*parityClusterNode) {
	for _, node := range nodes {
		node.close()
	}
}

func captureClusterFailover(ctx context.Context, nodes []*parityClusterNode) (transcript, error) {
	result := transcript{ids: map[string]string{}}
	client := &http.Client{Timeout: 30 * time.Second}
	ids := protocol.Discover(ctx, client, nodes[0].url, "parity-owner", "web", nil)
	if len(ids.Warnings) != 0 || ids.Values["movieKey"] == "" {
		return result, fmt.Errorf("cluster fixture discovery incomplete: %v", ids.Warnings)
	}
	result.ids = ids.Values
	const session = "parity-cluster-session"
	h := protocol.Clients()["web"].Headers.Clone()
	h.Set("X-Plex-Client-Identifier", "parity-cluster-client")
	h.Set("X-Plex-Session-Identifier", session)
	h.Set("Accept", "application/json")
	fetch := func(node *parityClusterNode, label, method, path string, want int) (protocol.Response, error) {
		rh := h.Clone()
		if label == "part-a" || label == "part-b" {
			rh.Set("Range", "bytes=0-4095")
		}
		req := protocol.Request{Method: method, Path: path, Headers: map[string]http.Header{"web-json": rh}}
		r, err := strictFetch(ctx, client, node.url, "parity-owner", req, "web", ids.Values, true)
		if err != nil || r.Status != want || (want == http.StatusPartialContent && len(r.Body) != 4096) {
			return r, fmt.Errorf("cluster %s: status=%d want=%d bytes=%d: %v", label, r.Status, want, len(r.Body), err)
		}
		result.captures = append(result.captures, capture{"owner", "web", "cluster:" + label, true, r})
		return r, nil
	}
	part := fmt.Sprintf("/library/parts/{{movieKey}}/%d/file.mp4", fixtureNow.Unix())
	timeline := "/:/timeline?ratingKey={{movieKey}}&duration=5400000&time=1200000&state="
	if _, err := fetch(nodes[0], "part-a", http.MethodGet, part, http.StatusPartialContent); err != nil {
		return result, err
	}
	if _, err := fetch(nodes[0], "start-a", http.MethodPost, timeline+"playing", http.StatusOK); err != nil {
		return result, err
	}
	if err := waitClusterSession(ctx, client, nodes[1].url, h, ids.Values, session, true); err != nil {
		return result, err
	}
	fmt.Fprintln(os.Stderr, "cluster proof: node B observed node A session")
	if _, err := fetch(nodes[1], "shared-on-b", http.MethodGet, "/status/sessions", http.StatusOK); err != nil {
		return result, err
	}
	if _, err := fetch(nodes[1], "stop-on-b", http.MethodPost, timeline+"stopped", http.StatusOK); err != nil {
		return result, err
	}
	if err := waitClusterSession(ctx, client, nodes[0].url, h, ids.Values, session, false); err != nil {
		return result, err
	}
	fmt.Fprintln(os.Stderr, "cluster proof: node B stop removed session on node A")
	if _, err := fetch(nodes[0], "stopped-on-a", http.MethodGet, "/status/sessions", http.StatusOK); err != nil {
		return result, err
	}
	if _, err := fetch(nodes[0], "restart-a", http.MethodPost, timeline+"playing", http.StatusOK); err != nil {
		return result, err
	}
	if err := waitClusterSession(ctx, client, nodes[1].url, h, ids.Values, session, true); err != nil {
		return result, err
	}
	// Cancel only our own first node, simulating its abrupt loss.
	nodes[0].close()
	fmt.Fprintln(os.Stderr, "cluster proof: node A stopped; reconnecting on node B")
	if _, err := fetch(nodes[1], "part-b", http.MethodGet, part, http.StatusPartialContent); err != nil {
		return result, err
	}
	if _, err := fetch(nodes[1], "failover-playing", http.MethodPost, timeline+"playing", http.StatusOK); err != nil {
		return result, err
	}
	if err := waitClusterSession(ctx, client, nodes[1].url, h, ids.Values, session, true); err != nil {
		return result, err
	}
	if _, err := fetch(nodes[1], "failover-on-b", http.MethodGet, "/status/sessions", http.StatusOK); err != nil {
		return result, err
	}
	if _, err := fetch(nodes[1], "failover-stop", http.MethodPost, timeline+"stopped", http.StatusOK); err != nil {
		return result, err
	}
	if err := waitClusterSession(ctx, client, nodes[1].url, h, ids.Values, session, false); err != nil {
		return result, err
	}
	fmt.Fprintln(os.Stderr, "cluster proof: node B failover session stopped and absent")
	if _, err := fetch(nodes[1], "metadata-after-failover", http.MethodGet, "/library/metadata/{{movieKey}}", http.StatusOK); err != nil {
		return result, err
	}
	return result, nil
}

func waitClusterSession(ctx context.Context, client *http.Client, base string, headers http.Header, vars map[string]string, session string, want bool) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		req := protocol.Request{Method: http.MethodGet, Path: "/status/sessions", Headers: map[string]http.Header{"web-json": headers}}
		r, err := strictFetch(ctx, client, base, "parity-owner", req, "web", vars, true)
		if err == nil && r.Status == http.StatusOK {
			present, parseErr := sessionListed(r.Body, session)
			if parseErr != nil {
				return parseErr
			}
			if present == want {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("cluster session %q present did not become %v", session, want)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
