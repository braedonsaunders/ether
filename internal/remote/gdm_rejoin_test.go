package remote

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestGDMBackoffGrowsAndCaps(t *testing.T) {
	var d time.Duration
	var seq []time.Duration
	for i := 0; i < 12; i++ {
		d = nextGDMBackoff(d)
		seq = append(seq, d)
	}
	if seq[0] != gdmErrBackoffMin || seq[1] != 2*gdmErrBackoffMin {
		t.Fatalf("start of sequence %v", seq[:2])
	}
	if seq[len(seq)-1] != gdmErrBackoffMax {
		t.Fatalf("not capped: %v", seq)
	}
}

func TestGDMRejoinKeepsAnswering(t *testing.T) {
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	info := GDMInfo{Name: "Ether", MachineIdentifier: "m", Port: 32400, Version: "1"}
	logger := slog.New(slog.NewTextHandler(gdmDiagnosticWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g := NewGDMResponder(GDMOptions{Info: func() GDMInfo { return info }, Ports: []int{port}, rejoinInterval: 100 * time.Millisecond, Logger: logger})
	if err := g.Start(context.Background()); err != nil {
		t.Skipf("cannot bind GDM socket here: %v", err)
	}
	defer g.Stop()
	g.mu.Lock()
	first := g.conns[0]
	g.mu.Unlock()

	// Wait for an observed rejoin rather than assuming the serving goroutine
	// ran during a fixed sleep on a loaded race runner.
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		current := g.conns[0]
		g.mu.Unlock()
		if current != first {
			break
		}
		if time.Now().After(deadline) {
			// Diagnostic only: retain the original failure even if the old socket answers.
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			reps, err := DiscoverGDM(ctx, fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
			cancel()
			valid := len(reps) == 1 && reps[0].MachineIdentifier == "m"
			t.Logf("GDM diagnostic retained socket: replies=%d expected_identity=%t err=%v", len(reps), valid, err)
			t.Fatal("socket was not reopened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// UDP discovery can lose a datagram while the old socket is replaced.
	// Require a real response after rejoin, but do not require that a single
	// packet survives continuous 100ms rejoins. A broken responder still fails
	// the bounded deadline below.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		reps, err := DiscoverGDM(ctx, fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err != nil {
			t.Fatalf("after rejoin: %v", err)
		}
		if len(reps) > 0 {
			if len(reps) != 1 || reps[0].MachineIdentifier != "m" {
				t.Fatalf("unexpected response: %v", reps)
			}
			return
		}
	}
	t.Fatal("rejoined socket did not answer discovery before deadline")
}
