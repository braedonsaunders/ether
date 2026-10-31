package remote

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

// Reachability is the result of a remote-access self-test.
type Reachability struct {
	Checked   time.Time `json:"checked"`
	Reachable bool      `json:"reachable"`
	// URI is the first URI that answered with this server's identity.
	URI string `json:"uri,omitempty"`
	// Results has one entry per URI tried.
	Results []ProbeResult `json:"results"`
}

// ProbeResult is one URI's outcome.
type ProbeResult struct {
	URI     string `json:"uri"`
	OK      bool   `json:"ok"`
	Latency string `json:"latency,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ReachabilityChecker probes connection URIs for this server's /identity.
//
// Probing the WAN address from inside the LAN relies on the router's NAT
// loopback (hairpinning). Routers without it make a correctly forwarded
// port look unreachable, so a negative result is reported as such and
// callers should present it as "could not verify", not as a hard failure.
type ReachabilityChecker struct {
	Client  *http.Client
	Timeout time.Duration
}

// NewReachabilityChecker uses client (a 10s-timeout client when nil).
func NewReachabilityChecker(client *http.Client) *ReachabilityChecker {
	if client == nil {
		client = egress.Client(egress.CapDirect, 10*time.Second)
	}
	return &ReachabilityChecker{Client: client, Timeout: 8 * time.Second}
}

// Check probes uris concurrently and reports whether any answered with
// machineID.
func (r *ReachabilityChecker) Check(ctx context.Context, uris []string, machineID string) Reachability {
	res := Reachability{Checked: time.Now(), Results: make([]ProbeResult, len(uris))}
	var wg sync.WaitGroup
	for i, u := range uris {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			err := r.probe(ctx, u, machineID)
			pr := ProbeResult{URI: u, OK: err == nil, Latency: time.Since(start).Round(time.Millisecond).String()}
			if err != nil {
				pr.Error = err.Error()
			}
			res.Results[i] = pr
		}()
	}
	wg.Wait()
	for _, pr := range res.Results {
		if pr.OK {
			res.Reachable = true
			res.URI = pr.URI
			break
		}
	}
	return res
}

func (r *ReachabilityChecker) probe(ctx context.Context, base, machineID string) error {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/identity", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, application/xml;q=0.9")
	req.Header.Set("X-Plex-Product", "Ether Reachability")
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	got := identityMachineID(body)
	if got == "" {
		return errors.New("response is not a Plex identity")
	}
	if !strings.EqualFold(got, machineID) {
		return fmt.Errorf("answered by a different server (%s)", got)
	}
	return nil
}

// identityMachineID extracts machineIdentifier from a PMS /identity reply
// in JSON or XML form.
func identityMachineID(body []byte) string {
	trim := strings.TrimSpace(string(body))
	if strings.HasPrefix(trim, "{") {
		var v struct {
			MediaContainer struct {
				MachineIdentifier string `json:"machineIdentifier"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(body, &v) == nil {
			return v.MediaContainer.MachineIdentifier
		}
		return ""
	}
	var v struct {
		MachineIdentifier string `xml:"machineIdentifier,attr"`
	}
	if xml.Unmarshal(body, &v) == nil {
		return v.MachineIdentifier
	}
	return ""
}
