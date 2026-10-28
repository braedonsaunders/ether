package remote

import (
	"context"
	"fmt"
	"net"
	"testing"
)

func TestPortMapperPreservesAnotherServiceOnSameHost(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			f := newFakeIGD(t)
			f.strict = strict
			f.mappings["TCP/32400"] = "127.0.0.1:32400"
			gatewayCalls := 0
			m := NewPortMapper(PortMapperOptions{
				DiscoverIGD: func(ctx context.Context) (*IGD, error) { return LoadIGD(ctx, nil, f.srv.URL+"/rootDesc.xml") },
				Gateway: func() (net.IP, error) {
					gatewayCalls++
					return nil, fmt.Errorf("NAT-PMP must not bypass an occupied port")
				},
			})
			st, err := m.Ensure(context.Background(), 32450, 32400)
			if err == nil || st.Active {
				t.Errorf("occupied port accepted: status=%+v err=%v", st, err)
			}
			if got, _ := f.mapping("TCP/32400"); got != "127.0.0.1:32400" {
				t.Errorf("PMS mapping changed to %q", got)
			}
			if gatewayCalls != 0 {
				t.Errorf("NAT-PMP fallback attempted %d times after known port conflict", gatewayCalls)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, call := range f.calls {
				if call == "AddPortMapping" || call == "DeletePortMapping" {
					t.Errorf("mutated occupied mapping with %s", call)
				}
			}
		})
	}
}

func TestPortMapperClosePreservesReassignedMapping(t *testing.T) {
	f := newFakeIGD(t)
	m := NewPortMapper(PortMapperOptions{
		DiscoverIGD: func(ctx context.Context) (*IGD, error) { return LoadIGD(ctx, nil, f.srv.URL+"/rootDesc.xml") },
		Gateway:     func() (net.IP, error) { return nil, fmt.Errorf("no NAT-PMP") },
	})
	if _, err := m.Ensure(context.Background(), 32450, 40000); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.mappings["TCP/40000"] = "127.0.0.1:32400"
	f.calls = nil
	f.mu.Unlock()
	m.Close(context.Background())
	if got, _ := f.mapping("TCP/40000"); got != "127.0.0.1:32400" {
		t.Fatalf("reassigned PMS mapping changed to %q", got)
	}
}
