package remote

import (
	"context"
	"errors"
	"net"
	"runtime"
	"syscall"
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

// These tests exercise the real timed rejoin loop and UDP responder. The
// controlled listener removes the need for a multicast-capable interface;
// successful multicast membership renewal still requires environment-dependent
// integration validation.
func TestGDMRejoinKeepsAnswering(t *testing.T) {
	testGDMRejoinLifecycle(t, true)
}

func TestGDMFailedRejoinKeepsAnswering(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows forbids the overlapping UDP bind this test uses to force a failed rejoin")
	}
	testGDMRejoinLifecycle(t, false)
}

func testGDMRejoinLifecycle(t *testing.T, replace bool) {
	t.Helper()
	type attempt struct {
		port    int
		conn    *net.UDPConn
		err     error
		oldOpen bool
	}
	attempted := make(chan attempt, 1)
	var first *net.UDPConn
	firstAttempt := true
	listen := func(port int) (*net.UDPConn, error) {
		if first == nil {
			var err error
			first, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			return first, err
		}
		// Check the old socket is still open when replacement is attempted.
		// Control does not change socket state or consume a packet.
		raw, err := first.SyscallConn()
		oldOpen := err == nil && raw.Control(func(uintptr) {}) == nil
		bindPort := port
		if replace && firstAttempt {
			// A distinct real UDP port allows both sockets to coexist without
			// multicast or platform-specific reuse options. We still verify
			// that rejoin requested the OLD port. This tests lifecycle, not
			// same-port multicast binding or interface membership.
			bindPort = 0
		}
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bindPort})
		if firstAttempt {
			attempted <- attempt{port: port, conn: c, err: err, oldOpen: oldOpen}
			firstAttempt = false
		}
		// Subsequent attempts bind the current occupied port and fail,
		// leaving a stable socket for the live UDP assertion.
		return c, err
	}
	info := GDMInfo{Name: "Ether", MachineIdentifier: "m", Port: 32400, Version: "1"}
	g := NewGDMResponder(GDMOptions{
		Info: func() GDMInfo { return info }, Ports: []int{0},
		rejoinInterval: 100 * time.Millisecond, listen: listen,
	})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start UDP responder: %v", err)
	}
	defer g.Stop()
	firstPort := first.LocalAddr().(*net.UDPAddr).Port
	var a attempt
	select {
	case a = <-attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("serving loop did not attempt rejoin")
	}
	if a.port != firstPort || !a.oldOpen {
		t.Fatalf("rejoin must open before closing old socket: requested_port=%d want=%d old_open=%t", a.port, firstPort, a.oldOpen)
	}
	want := first
	if replace {
		if a.err != nil || a.conn == nil || a.conn == first {
			t.Fatalf("replacement listener: conn=%p err=%v", a.conn, a.err)
		}
		want = a.conn
	} else if a.conn != nil || !errors.Is(a.err, syscall.EADDRINUSE) {
		t.Fatalf("expected actual overlapping UDP bind failure: conn=%p err=%v", a.conn, a.err)
	}
	// Observe the installed connection, not merely a successful listener call.
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		current := g.conns[0]
		g.mu.Unlock()
		if current == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement socket was not installed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A reply proves the serving loop finished the rejoin attempt and continues
	// reading from the expected socket, including after a failed bind.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answered := false
	for ctx.Err() == nil {
		reps, err := DiscoverGDM(ctx, want.LocalAddr().String(), 100*time.Millisecond)
		if err != nil {
			t.Fatalf("after rejoin attempt: %v", err)
		}
		if len(reps) > 0 {
			if len(reps) != 1 || reps[0].MachineIdentifier != "m" {
				t.Fatalf("unexpected response: %v", reps)
			}
			answered = true
			break
		}
	}
	if !answered {
		t.Fatal("socket did not answer discovery after rejoin attempt")
	}
	g.mu.Lock()
	current := g.conns[0]
	g.mu.Unlock()
	if current != want {
		t.Fatal("responder did not retain the expected socket")
	}
	if replace {
		if err := first.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("old socket was not closed after replacement: %v", err)
		}
	}
}
