package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

// DefaultWANEndpoints are queried in order. The first is the endpoint PMS
// itself uses (GET https://v4.plex.tv/pms/:/ip in observed PMS traffic);
// the rest are well-known plain-text fallbacks.
var DefaultWANEndpoints = []string{
	"https://v4.plex.tv/pms/:/ip",
	"https://api.ipify.org",
	"https://icanhazip.com",
	"https://ifconfig.me/ip",
}

// WANDetector finds this network's public IPv4 address.
type WANDetector struct {
	Endpoints []string
	Client    *http.Client
	Logger    *slog.Logger
}

// NewWANDetector returns a detector over endpoints (DefaultWANEndpoints when
// empty) with a short per-request timeout.
func NewWANDetector(client *http.Client, logger *slog.Logger, endpoints ...string) *WANDetector {
	if len(endpoints) == 0 {
		endpoints = DefaultWANEndpoints
	}
	if client == nil {
		client = egress.Client(egress.CapDirect, 10*time.Second)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WANDetector{Endpoints: endpoints, Client: client, Logger: logger}
}

// ErrNoWANIP is returned when no endpoint produced a public address.
var ErrNoWANIP = errors.New("remote: could not detect public IP")

// Detect returns the first public IP any endpoint reports.
func (d *WANDetector) Detect(ctx context.Context) (net.IP, error) {
	var errs []error
	for _, ep := range d.Endpoints {
		ip, err := d.query(ctx, ep)
		if err == nil {
			return ip, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d.Logger.Debug("wan ip endpoint failed", "endpoint", ep, "err", err)
		errs = append(errs, fmt.Errorf("%s: %w", ep, err))
	}
	return nil, errors.Join(append([]error{ErrNoWANIP}, errs...)...)
}

func (d *WANDetector) query(ctx context.Context, endpoint string) (net.IP, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain, application/json;q=0.9")
	req.Header.Set("User-Agent", "Ether")
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	ip := parseIPBody(body)
	if ip == nil {
		return nil, fmt.Errorf("no IP in response %q", truncate(string(body), 64))
	}
	if !IsPublicIP(ip) {
		return nil, fmt.Errorf("endpoint returned non-public address %s", ip)
	}
	return ip, nil
}

func parseIPBody(body []byte) net.IP {
	s := strings.TrimSpace(string(body))
	if strings.HasPrefix(s, "{") {
		var v struct {
			IP      string `json:"ip"`
			Address string `json:"address"`
		}
		if json.Unmarshal(body, &v) == nil {
			if v.IP == "" {
				v.IP = v.Address
			}
			s = v.IP
		}
	}
	ip := net.ParseIP(strings.TrimSpace(s))
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
