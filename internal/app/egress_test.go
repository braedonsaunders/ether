package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/config"
)

func egressNode(id, ip string, live bool) cluster.Node {
	return cluster.Node{ID: id, Name: id, Live: live, Caps: cluster.Caps{EgressIP: ip}}
}

func TestEgressWarning(t *testing.T) {
	if w := egressWarning([]cluster.Node{egressNode("a", "203.0.113.1", true), egressNode("b", "203.0.113.1", true), egressNode("c", "", true)}); w != "" {
		t.Fatalf("same address warned: %s", w)
	}
	if w := egressWarning([]cluster.Node{egressNode("a", "203.0.113.1", true), egressNode("dead", "198.51.100.9", false)}); w != "" {
		t.Fatalf("dead node counted: %s", w)
	}
	w := egressWarning([]cluster.Node{egressNode("b", "198.51.100.2", true), egressNode("a", "203.0.113.1", true), egressNode("c", "203.0.113.1", true)})
	want := "nodes reach providers from different public addresses (198.51.100.2: b; 203.0.113.1: a, c); a debrid account can be limited when it is used from more than one address"
	if w != want {
		t.Fatalf("warning %q", w)
	}
}

func TestClusterStatusReportsEgressWarning(t *testing.T) {
	fm := &fakeMembers{Solo: cluster.NewSolo(cluster.Local{Identity: cluster.Identity{NodeID: "a", Name: "a"}}, nil)}
	fm.nodes = []cluster.Node{egressNode("a", "203.0.113.1", true), egressNode("b", "198.51.100.2", true)}
	a := &App{cluster: fm, log: quietLog()}
	w := httptest.NewRecorder()
	a.clusterHandler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	var st clusterStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "different public addresses") {
		t.Fatalf("warnings %v", st.Warnings)
	}
}

func TestLeaderLogsEgressWarningOncePerChange(t *testing.T) {
	var buf bytes.Buffer
	a := &App{cluster: cluster.NewSolo(cluster.Local{Identity: cluster.Identity{NodeID: "a"}}, nil), log: slog.New(slog.NewTextHandler(&buf, nil))}
	split := []cluster.Node{egressNode("a", "203.0.113.1", true), egressNode("b", "198.51.100.2", true)}
	last := ""
	a.noteEgress(split, &last)
	a.noteEgress(split, &last)
	if n := strings.Count(buf.String(), "different public addresses"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, buf.String())
	}
	a.noteEgress(split[:1], &last)
	if !strings.Contains(buf.String(), "one public address") {
		t.Fatalf("recovery not logged:\n%s", buf.String())
	}
}

type fakeWAN struct {
	ip    string
	calls int
}

func (f *fakeWAN) Detect(context.Context) (net.IP, error) {
	f.calls++
	if f.ip == "" {
		return nil, errors.New("offline")
	}
	return net.ParseIP(f.ip), nil
}

func TestDetectEgressUsesSourcePath(t *testing.T) {
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: cfg, log: quietLog()}
	wan := &fakeWAN{ip: "203.0.113.7"}
	if ip := a.detectEgress(context.Background(), wan); ip != "203.0.113.7" || wan.calls != 1 {
		t.Fatalf("source lookup %q (%d calls)", ip, wan.calls)
	}
	wan.ip = ""
	if ip := a.detectEgress(context.Background(), wan); ip != "" {
		t.Fatalf("failed lookup returned %q", ip)
	}
	a.egress = &egressIP{}
	a.egress.set("203.0.113.7")
	if a.nodeCaps().EgressIP != "203.0.113.7" {
		t.Fatal("egress address not heartbeated")
	}
}
