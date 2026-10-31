package plextv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/remote"
)

const (
	// CertReasonChain means the leaf did not chain to a trusted root.
	CertReasonChain = "chain"
	// CertReasonIdentity means the leaf is not *.<this server's hash>.plex.direct.
	CertReasonIdentity = "identity"
	// CertReasonHandshake means a local plex.direct TLS connection did not succeed.
	CertReasonHandshake = "handshake"
	// CertReasonDates means the leaf is outside its validity window.
	CertReasonDates = "dates"
)

type trustDialFunc func(ctx context.Context, network, addr string, cfg *tls.Config) error

type certTrust struct {
	fingerprint string
	verified    bool
	reason      string
}

// setTrustDial replaces the TLS dial VerifyTrust uses. Tests only.
func (m *CertManager) setTrustDial(fn trustDialFunc) {
	m.mu.Lock()
	m.trustDial = fn
	m.mu.Unlock()
}

// VerifyTrust checks the installed certificate and remembers the result.
// Status polls read that memory; they do not call this.
func (m *CertManager) VerifyTrust(ctx context.Context, identityHash, dialAddr string) (bool, string) {
	m.mu.RLock()
	pair := m.cert
	roots := m.trustRoots
	dial := m.trustDial
	now := time.Now()
	if m.o.Now != nil {
		now = m.o.Now()
	}
	m.mu.RUnlock()
	if pair == nil || len(pair.Certificate) == 0 {
		return false, CertReasonChain
	}
	leaf := pair.Leaf
	if leaf == nil {
		parsed, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return false, CertReasonChain
		}
		leaf = parsed
	}
	var intermediates []*x509.Certificate
	for _, raw := range pair.Certificate[1:] {
		c, err := x509.ParseCertificate(raw)
		if err == nil {
			intermediates = append(intermediates, c)
		}
	}
	ok, reason := verifyPlexDirect(leaf, intermediates, roots, identityHash, now)
	if ok {
		var dialErr error
		for attempt := 0; attempt < 2; attempt++ {
			dialErr = handshakePlexDirect(ctx, dial, roots, dialAddr)
			if dialErr == nil {
				break
			}
		}
		if dialErr != nil {
			ok, reason = false, CertReasonHandshake
		}
	}
	fp := Fingerprint(leaf)
	m.mu.Lock()
	m.trust = certTrust{fingerprint: fp, verified: ok, reason: reason}
	m.mu.Unlock()
	return ok, reason
}

func verifyPlexDirect(leaf *x509.Certificate, intermediates []*x509.Certificate, roots *x509.CertPool, identityHash string, now time.Time) (bool, string) {
	identityHash = strings.ToLower(strings.TrimSpace(identityHash))
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return false, CertReasonDates
	}
	name := "x." + identityHash + ".plex.direct"
	if identityHash == "" || leaf.VerifyHostname(name) != nil {
		return false, CertReasonIdentity
	}
	pool := x509.NewCertPool()
	for _, c := range intermediates {
		pool.AddCert(c)
	}
	opts := x509.VerifyOptions{
		DNSName:       name,
		Intermediates: pool,
		Roots:         roots,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := leaf.Verify(opts); err != nil {
		return false, CertReasonChain
	}
	return true, ""
}

func handshakePlexDirect(ctx context.Context, dial trustDialFunc, roots *x509.CertPool, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return errors.New("plextv: no local plex.direct address")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: roots}
	if dial != nil {
		return dial(ctx, "tcp", addr, cfg)
	}
	d := &tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// localPlexDirectDial is host:port of a local https plex.direct connection.
func localPlexDirectDial(conns []remote.Connection) string {
	for _, c := range conns {
		if !c.Local || c.Protocol != "https" {
			continue
		}
		u, err := url.Parse(c.URI)
		if err != nil || u.Hostname() == "" {
			continue
		}
		if _, _, ok := remote.ParsePlexDirectHost(u.Hostname()); !ok {
			continue
		}
		return u.Host
	}
	return ""
}
