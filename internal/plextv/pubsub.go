package plextv

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/braedonsaunders/ether/internal/egress"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Presence on plex.tv.
//
// A server is "present" (resources presence=true, shown online in the apps)
// while it holds a Server-Sent-Events stream open to Plex's pubsub service.
// Observed from PMS 1.43.4 and verified live:
//
//   - GET https://plex.tv/services/pubsub/servers (server token) lists the
//     pubsub hosts: <Servers><Server host="pubsubNN.pop.<region>.plex.bz" region="…"/>…
//   - GET https://<host>/sub/eventsource/<ownerPlexID>/<machineIdentifier>?X-Plex-Token=<server token>
//     returns text/event-stream with ": -1" keep-alive comments.
//   - The *.plex.bz hosts present a *.plex.tv certificate, so TLS must be
//     verified against pubsub.plex.tv while dialling the .plex.bz host.
//   - Events are XML in data lines, e.g.
//     <Message command="notifyConnectivity" asyncIdentifier="…" connectivity="0|1" address="…" port="32400"/>,
//     the answer to PUT /api/servers/{id}/connectivity. Other commands PMS
//     handles: notifyResetToken, notifyAccountChange, notifySharingChange,
//     notifyHomeChange, refreshCertificate.

// PubsubTLSName is the name pubsub hosts' certificates are valid for.
const PubsubTLSName = "pubsub.plex.tv"

// PubsubMessage is one event pushed to the server.
type PubsubMessage struct {
	Command         string `xml:"command,attr"`
	AsyncIdentifier string `xml:"asyncIdentifier,attr"`
	Connectivity    string `xml:"connectivity,attr"`
	Address         string `xml:"address,attr"`
	Port            string `xml:"port,attr"`
	Raw             string `xml:"-"`
}

// Reachable reports a notifyConnectivity result.
func (m PubsubMessage) Reachable() bool { return m.Connectivity == "1" }

// PubsubServers lists pubsub hosts, best region first as plex.tv orders them.
func (c *Client) PubsubServers(ctx context.Context, serverToken string) ([]string, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/services/pubsub/servers",
		token:  serverToken,
		accept: "application/xml",
	})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Servers []struct {
			Host string `xml:"host,attr"`
		} `xml:"Server"`
	}
	if err := xml.Unmarshal(resp.body, &doc); err != nil {
		return nil, fmt.Errorf("plextv: pubsub servers: %w", err)
	}
	var hosts []string
	for _, s := range doc.Servers {
		if h := strings.TrimSpace(s.Host); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil, errors.New("plextv: pubsub servers: empty list")
	}
	return hosts, nil
}

// PresenceOptions configure a Presence.
type PresenceOptions struct {
	Client *Client
	// Credentials returns the owner's plex.tv id, this server's machine
	// identifier and server token; ok=false while unclaimed.
	Credentials func() (ownerID int64, machineID, token string, ok bool)
	OnMessage   func(PubsubMessage)
	// StreamClient performs the SSE request; defaults to one verifying TLS
	// against PubsubTLSName.
	StreamClient *http.Client
	// StreamURL builds the stream URL for a host (tests use http).
	StreamURL              func(host string, ownerID int64, machineID, token string) string
	MinBackoff, MaxBackoff time.Duration
	// IdleTimeout reconnects when the stream delivers no bytes for this long
	// (default 5m). Pubsub sends ": -1" keep-alive comments, so silence
	// means a half-open connection (NAT timeout, sleep/wake) that would
	// otherwise block the reader forever and leave the server offline.
	IdleTimeout time.Duration
}

// PresenceStatus is reported in the registrar status.
type PresenceStatus struct {
	Connected   bool      `json:"connected"`
	Host        string    `json:"host,omitempty"`
	Since       time.Time `json:"since,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastMessage string    `json:"lastMessage,omitempty"`
}

// Presence keeps the pubsub stream open, reconnecting with backoff and
// rotating hosts after repeated failures.
type Presence struct {
	o  PresenceOptions
	mu sync.Mutex
	st PresenceStatus
}

func NewPresence(o PresenceOptions) *Presence {
	if o.StreamClient == nil {
		tr := egress.Transport(egress.CapDirect)
		tr.TLSClientConfig = &tls.Config{ServerName: PubsubTLSName, MinVersion: tls.VersionTLS12}
		tr.ForceAttemptHTTP2 = false
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		o.StreamClient = &http.Client{Transport: tr} // no timeout: the stream is long-lived
	}
	if o.StreamURL == nil {
		o.StreamURL = func(host string, ownerID int64, machineID, token string) string {
			return "https://" + host + "/sub/eventsource/" + strconv.FormatInt(ownerID, 10) + "/" +
				url.PathEscape(machineID) + "?X-Plex-Token=" + url.QueryEscape(token)
		}
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = 5 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 2 * time.Minute
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 5 * time.Minute
	}
	return &Presence{o: o}
}

func (p *Presence) Status() PresenceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

func (p *Presence) set(fn func(*PresenceStatus)) {
	p.mu.Lock()
	fn(&p.st)
	p.mu.Unlock()
}

// Run holds the stream open until ctx ends.
func (p *Presence) Run(ctx context.Context) {
	// Run may be restarted (a clustered node runs presence only while it
	// leads), so a stopped stream must not keep reporting connected.
	defer p.set(func(s *PresenceStatus) { s.Connected = false })
	backoff := p.o.MinBackoff
	var hosts []string
	failures := 0
	for ctx.Err() == nil {
		owner, machine, token, ok := p.o.Credentials()
		if !ok {
			p.set(func(s *PresenceStatus) { *s = PresenceStatus{LastError: "not claimed"} })
			if !sleepCtx(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if len(hosts) == 0 || failures >= 3 {
			h, err := p.o.Client.PubsubServers(ctx, token)
			if err != nil {
				p.fail(err)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, p.o.MaxBackoff)
				continue
			}
			if failures >= 3 && len(hosts) > 0 && len(h) > 1 && h[0] == hosts[0] {
				h = append(h[1:], h[0]) // rotate away from the failing host
			}
			hosts, failures = h, 0
		}
		start := time.Now()
		err := p.stream(ctx, hosts[0], owner, machine, token)
		if ctx.Err() != nil {
			return
		}
		p.fail(err)
		if time.Since(start) > time.Minute {
			backoff, failures = p.o.MinBackoff, 0 // a long-lived stream that dropped: reconnect promptly
		} else {
			failures++
		}
		p.o.Client.log.Debug("pubsub stream ended; reconnecting", "host", hosts[0], "err", err, "in", backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, p.o.MaxBackoff)
	}
}

func (p *Presence) fail(err error) {
	msg := "stream closed"
	if err != nil {
		msg = err.Error()
	}
	p.set(func(s *PresenceStatus) { s.Connected, s.LastError = false, msg })
}

// errPubsubIdle reports a stream dropped by the idle watchdog.
var errPubsubIdle = errors.New("no data or keep-alive within the idle timeout")

func (p *Presence) stream(ctx context.Context, host string, owner int64, machine, token string) error {
	// The idle watchdog cancels the request when no line (event or
	// keep-alive comment) arrives within IdleTimeout.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(p.o.IdleTimeout, func() { cancel(errPubsubIdle) })
	defer watchdog.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.o.StreamURL(host, owner, machine, token), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "PlexMediaServer-EventSourceClient/"+p.o.Client.id.Version)
	resp, err := p.o.StreamClient.Do(req)
	if err != nil {
		return fmt.Errorf("pubsub %s: %w", host, redactToken(err, token))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pubsub %s: HTTP %d", host, resp.StatusCode)
	}
	p.set(func(s *PresenceStatus) {
		s.Connected, s.Host, s.Since, s.LastError = true, host, time.Now(), ""
	})
	p.o.Client.log.Info("plex.tv presence connected", "host", host)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var data strings.Builder
	for sc.Scan() {
		watchdog.Reset(p.o.IdleTimeout)
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				p.dispatch(data.String())
				data.Reset()
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if data.Len() > 0 {
		p.dispatch(data.String())
	}
	if errors.Is(context.Cause(ctx), errPubsubIdle) {
		return fmt.Errorf("pubsub %s: %w", host, errPubsubIdle)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("pubsub %s: %w", host, err)
	}
	return nil
}

func (p *Presence) dispatch(payload string) {
	var m PubsubMessage
	if err := xml.Unmarshal([]byte(payload), &m); err != nil || m.Command == "" {
		p.o.Client.log.Debug("pubsub: unhandled event", "bytes", len(payload))
		return
	}
	m.Raw = payload
	p.set(func(s *PresenceStatus) { s.LastMessage = m.Command })
	if p.o.OnMessage != nil {
		p.o.OnMessage(m)
	}
}

func redactToken(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "REDACTED"))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
