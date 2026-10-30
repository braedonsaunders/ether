package remote

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
)

// Test-scoped output avoids changing the process-wide default logger.
type gdmDiagnosticWriter struct{ t *testing.T }

func (w gdmDiagnosticWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// TestGDMRunnerSocketDiagnostics records capability evidence, not a conformance
// pass. It probes the same nil-interface multicast/fallback sequence as listenGDM
// on a separate ephemeral port and closes every socket. Neither test runs in
// parallel. Never infer the responder's actual branch solely from this
// probe: correlate it with that test's Debug errors and retained-socket reply.
func TestGDMRunnerSocketDiagnostics(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("enumerate interfaces: %v", err)
	}
	for _, iface := range interfaces {
		// No addresses, MACs, environment variables or host identifiers.
		t.Logf("interface index=%d flags=%s mtu=%d", iface.Index, iface.Flags, iface.MTU)
	}
	logResult := func(phase, branch string, c *net.UDPConn, err error) {
		var errno syscall.Errno
		hasErrno := errors.As(err, &errno)
		t.Logf("phase=%s branch=%s success=%t err_type=%T errno_present=%t errno=%d err=%v", phase, branch, c != nil && err == nil, err, hasErrno, errno, err)
	}
	open := func(phase string, port int) *net.UDPConn {
		c, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(GDMMulticastGroup), Port: port})
		logResult(phase, "multicast", c, err)
		if err != nil {
			c, err = net.ListenUDP("udp4", &net.UDPAddr{Port: port})
			logResult(phase, "fallback", c, err)
		}
		return c
	}
	first := open("initial", 0)
	if first == nil {
		t.Fatal("diagnostic could not open either socket type")
	}
	defer first.Close()
	port := first.LocalAddr().(*net.UDPAddr).Port
	if second := open("old-still-open", port); second != nil {
		_ = second.Close()
	}
	_ = first.Close()
	// Only this independent probe closes before opening; production is untouched.
	if afterClose := open("old-closed", port); afterClose != nil {
		_ = afterClose.Close()
	}
}
