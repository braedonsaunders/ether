package plextv

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CertInfo describes the certificate Ether serves.
type CertInfo struct {
	Present         bool          `json:"present"`
	SelfSigned      bool          `json:"selfSigned"`
	Custom          bool          `json:"custom"` // owner-supplied files, never replaced
	Hash            string        `json:"hash,omitempty"`
	Subject         string        `json:"subject,omitempty"`
	DNSNames        []string      `json:"dnsNames,omitempty"`
	NotBefore       time.Time     `json:"notBefore,omitempty"`
	NotAfter        time.Time     `json:"notAfter,omitempty"`
	Fingerprint     string        `json:"fingerprint,omitempty"`
	CertFile        string        `json:"certFile,omitempty"`
	KeyFile         string        `json:"keyFile,omitempty"`
	LastAttempt     time.Time     `json:"lastAttempt,omitempty"` // last plex.tv request
	NextAttempt     time.Time     `json:"nextAttempt,omitempty"`
	RetryAfter      time.Duration `json:"retryAfter,omitempty"`
	ReissueRequired bool          `json:"reissueRequired,omitempty"`
	LastError       string        `json:"lastError,omitempty"`
	// Verified is the last scheduled trust check, not a per-poll result.
	Verified bool `json:"verified"`
	// VerifyReason is chain, identity, handshake, or dates when Verified is false.
	VerifyReason string `json:"verifyReason,omitempty"`
}

// CertManagerOptions configures a CertManager.
type CertManagerOptions struct {
	// Dir holds plexdirect.crt, plexdirect.key and cert-state.json.
	Dir string
	// Client must carry the server identity.
	Client *Client
	// CustomCertFile/CustomKeyFile, when set to files outside Dir, are an
	// owner-supplied certificate: loaded and reported, never replaced.
	CustomCertFile string
	CustomKeyFile  string
	// RenewBefore triggers renewal this long before expiry (default: a third
	// of the certificate's lifetime, at least 7 days).
	RenewBefore time.Duration
	// RetryAfter is the minimum gap between plex.tv certificate requests
	// after a failure (default 24h). plex.tv rate-limits CSR uploads per
	// device; PMS servers that retried daily ended up locked out for days.
	RetryAfter time.Duration
	// ClientErrorRetryAfter controls retries after non-429 4xx responses,
	// which usually indicate a malformed request. Default: 5 minutes.
	ClientErrorRetryAfter time.Duration
	// SelfSignedValidity for the fallback certificate (default 1 year).
	SelfSignedValidity time.Duration
	// Request overrides the download timing (tests).
	Request CertRequest
	// Shared, on a cluster node, holds the certificate for every node
	// (see certshared.go). Nil: a standalone server keeps it in Dir only.
	Shared SharedCertStore
	Logger *slog.Logger
	Now    func() time.Time
	// TrustRoots, when set, replaces the system roots for VerifyTrust.
	TrustRoots *x509.CertPool
}

// CertManager obtains, stores and renews the server's TLS certificate:
// a plex.direct certificate when plex.tv issues one, otherwise a
// self-signed fallback. It serves it through GetCertificate so renewals
// apply without restarting listeners.
type CertManager struct {
	o CertManagerOptions

	opMu sync.Mutex // serialises Ensure

	mu   sync.RWMutex
	cert *tls.Certificate
	info CertInfo

	trustRoots *x509.CertPool
	trustDial  trustDialFunc
	trust      certTrust

	// Cluster sharing (certshared.go), guarded by mu.
	allowRenew func() bool
	sharedVer  int64
	bus        EventBus
	unsubs     []func()
}

type certState struct {
	LastAttempt     time.Time     `json:"lastAttempt"`
	LastError       string        `json:"lastError"`
	Hash            string        `json:"hash"`
	RetryAfter      time.Duration `json:"retryAfter,omitempty"`
	ReissueRequired bool          `json:"reissueRequired,omitempty"`
}

// NewCertManager returns a manager and loads any certificate on disk.
func NewCertManager(o CertManagerOptions) (*CertManager, error) {
	if o.Dir == "" && o.CustomCertFile == "" {
		return nil, errors.New("plextv: cert manager needs a directory")
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 24 * time.Hour
	}
	if o.ClientErrorRetryAfter <= 0 {
		o.ClientErrorRetryAfter = 5 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &CertManager{o: o, trustRoots: o.TrustRoots}
	if o.Dir != "" {
		if err := os.MkdirAll(o.Dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := m.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		o.Logger.Warn("stored certificate unusable", "err", err)
	}
	if o.Shared != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := m.SyncShared(ctx); err != nil {
			o.Logger.Warn("shared certificate unavailable; using the local copy", "err", err)
		}
		cancel()
	}
	return m, nil
}

func (m *CertManager) certPath() string  { return filepath.Join(m.o.Dir, "plexdirect.crt") }
func (m *CertManager) keyPath() string   { return filepath.Join(m.o.Dir, "plexdirect.key") }
func (m *CertManager) statePath() string { return filepath.Join(m.o.Dir, "cert-state.json") }

func (m *CertManager) custom() bool {
	if m.o.CustomCertFile == "" || m.o.CustomKeyFile == "" {
		return false
	}
	if m.o.Dir == "" {
		return true
	}
	abs, err := filepath.Abs(m.o.CustomCertFile)
	dir, err2 := filepath.Abs(m.o.Dir)
	if err != nil || err2 != nil {
		return true
	}
	return !strings.HasPrefix(abs, dir+string(filepath.Separator))
}

func (m *CertManager) load() error {
	certFile, keyFile := m.certPath(), m.keyPath()
	if m.custom() {
		certFile, keyFile = m.o.CustomCertFile, m.o.CustomKeyFile
	}
	var st certState
	if m.o.Dir != "" {
		if b, err := os.ReadFile(m.statePath()); err == nil {
			_ = json.Unmarshal(b, &st)
		}
	}
	m.mu.Lock()
	m.info.LastAttempt, m.info.LastError, m.info.RetryAfter = st.LastAttempt, st.LastError, st.RetryAfter
	m.info.ReissueRequired = st.ReissueRequired
	m.mu.Unlock()
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	pair.Leaf = leaf
	m.install(&pair, leaf, certFile, keyFile, m.custom())
	return nil
}

func (m *CertManager) install(pair *tls.Certificate, leaf *x509.Certificate, certFile, keyFile string, custom bool) {
	info := CertInfo{
		Present:     true,
		Custom:      custom,
		SelfSigned:  isSelfSigned(leaf),
		Subject:     leaf.Subject.CommonName,
		DNSNames:    leaf.DNSNames,
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Fingerprint: Fingerprint(leaf),
		CertFile:    certFile,
		KeyFile:     keyFile,
	}
	for _, n := range append([]string{leaf.Subject.CommonName}, leaf.DNSNames...) {
		if mm := plexDirectRe.FindStringSubmatch(n); mm != nil {
			info.Hash = strings.ToLower(mm[1])
			break
		}
	}
	m.mu.Lock()
	info.LastAttempt, info.LastError, info.RetryAfter = m.info.LastAttempt, m.info.LastError, m.info.RetryAfter
	info.ReissueRequired = m.info.ReissueRequired
	m.cert = pair
	m.info = info
	m.mu.Unlock()
}

func isSelfSigned(c *x509.Certificate) bool {
	return c.Issuer.String() == c.Subject.String() &&
		c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

// Info returns the current certificate state.
func (m *CertManager) Info() CertInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info := m.info
	info.DNSNames = append([]string(nil), m.info.DNSNames...)
	if m.trust.fingerprint != "" && m.trust.fingerprint == info.Fingerprint {
		info.Verified = m.trust.verified
		info.VerifyReason = m.trust.reason
	}
	if !info.LastAttempt.IsZero() && info.LastError != "" {
		d := info.RetryAfter
		if d <= 0 {
			d = m.o.RetryAfter
		}
		info.NextAttempt = info.LastAttempt.Add(d)
	}
	return info
}

// GetCertificate is a tls.Config.GetCertificate callback.
func (m *CertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, errors.New("no certificate")
	}
	return m.cert, nil
}

// TLSConfig returns a server TLS config backed by GetCertificate.
func (m *CertManager) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: m.GetCertificate}
}

func (m *CertManager) renewBefore(leaf CertInfo) time.Duration {
	if m.o.RenewBefore > 0 {
		return m.o.RenewBefore
	}
	d := leaf.NotAfter.Sub(leaf.NotBefore) / 3
	if d < 7*24*time.Hour {
		d = 7 * 24 * time.Hour
	}
	return d
}

// NeedsRenewal reports whether the current certificate should be replaced.
func (m *CertManager) NeedsRenewal() bool {
	info := m.Info()
	if !info.Present {
		return true
	}
	if info.Custom {
		return false
	}
	if info.ReissueRequired {
		return true
	}
	if info.SelfSigned {
		return true // always prefer a plex.direct certificate when one becomes available
	}
	return m.o.Now().After(info.NotAfter.Add(-m.renewBefore(info)))
}

// Ensure makes sure a usable certificate is installed. With a server token
// and a missing, self-signed or expiring certificate it requests a
// plex.direct certificate (at most once per RetryAfter after a failure,
// unless force). If none can be obtained and there is no valid certificate,
// it installs a self-signed one for hosts. It returns the resulting info;
// the error reports the plex.tv failure, if any, even when a fallback is
// in place.
func (m *CertManager) Ensure(ctx context.Context, serverToken string, hosts []string, force bool) (CertInfo, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.custom() {
		if err := m.load(); err != nil {
			return m.Info(), fmt.Errorf("plextv: custom certificate: %w", err)
		}
		return m.Info(), nil
	}
	if m.o.Shared != nil {
		// Another node may have renewed (or failed and backed off) since.
		if _, err := m.syncSharedLocked(ctx); err != nil {
			m.o.Logger.Warn("sync shared certificate", "err", err)
		}
	}
	now := m.o.Now()
	cur := m.Info()
	var reqErr error
	needPlex := cur.ReissueRequired || !cur.Present || cur.SelfSigned || now.After(cur.NotAfter.Add(-m.renewBefore(cur)))
	// Local expiry does not establish that the current device record owns the
	// certificate. A claim may have recreated that record since issuance.
	if !needPlex && serverToken != "" && m.o.Client != nil {
		_, hash, err := m.o.Client.CertificateSubject(ctx, serverToken)
		if err != nil {
			return cur, fmt.Errorf("plextv: check device certificate subject: %w", err)
		}
		needPlex = cur.Hash == "" || !strings.EqualFold(cur.Hash, hash)
	}
	retryAfter := cur.RetryAfter
	if retryAfter <= 0 {
		retryAfter = m.o.RetryAfter
	}
	throttled := !force && cur.LastError != "" && now.Before(cur.LastAttempt.Add(retryAfter))
	switch {
	case !needPlex:
	case serverToken == "" || m.o.Client == nil:
		reqErr = errors.New("plextv: not claimed; cannot request a plex.direct certificate")
	case !m.renewalAllowed():
		reqErr = ErrRenewalDeferred
	case throttled:
		reqErr = fmt.Errorf("plextv: certificate request deferred until %s after: %s", cur.LastAttempt.Add(retryAfter).Format(time.RFC3339), cur.LastError)
	default:
		reqErr = m.requestPlexDirect(ctx, serverToken, cur, now)
	}
	if ctx.Err() != nil {
		return m.Info(), ctx.Err()
	}
	cur = m.Info()
	valid := cur.Present && now.Before(cur.NotAfter)
	if !valid {
		if err := m.installSelfSigned(hosts, cur.Hash); err != nil {
			return m.Info(), errors.Join(reqErr, err)
		}
	}
	return m.Info(), reqErr
}

func (m *CertManager) requestPlexDirect(ctx context.Context, token string, cur CertInfo, now time.Time) error {
	req := m.o.Request
	req.Key = nil
	req.Reason = "missing"
	req.InvalidIn = 0
	if cur.Present && !cur.SelfSigned {
		req.Reason = "expiring"
		req.InvalidIn = int(cur.NotAfter.Sub(now) / time.Second)
	}
	m.o.Logger.Info("requesting plex.direct certificate", "reason", req.Reason)
	bundle, err := m.o.Client.RequestPlexDirectCertificate(ctx, token, req)
	st := certState{LastAttempt: now, Hash: cur.Hash, RetryAfter: m.o.RetryAfter, ReissueRequired: cur.ReissueRequired}
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		st.LastError = err.Error()
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests {
			st.RetryAfter = m.o.ClientErrorRetryAfter
		}
		m.saveState(st)
		m.mu.Lock()
		m.info.LastAttempt, m.info.LastError, m.info.RetryAfter = st.LastAttempt, st.LastError, st.RetryAfter
		m.mu.Unlock()
		m.o.Logger.Warn("plex.direct certificate request failed", "err", err)
		m.shareState(ctx, st)
		return err
	}
	st.Hash = bundle.Hash
	if err := m.write(bundle); err != nil {
		st.LastError = err.Error()
		m.saveState(st)
		m.mu.Lock()
		m.info.LastAttempt, m.info.LastError, m.info.RetryAfter = st.LastAttempt, st.LastError, st.RetryAfter
		m.mu.Unlock()
		m.shareState(ctx, st)
		return err
	}
	st.ReissueRequired = false
	if err := m.persistState(st); err != nil {
		return fmt.Errorf("plextv: persist certificate reissue state: %w", err)
	}
	m.mu.Lock()
	m.info.LastAttempt, m.info.LastError, m.info.RetryAfter = st.LastAttempt, "", 0
	m.info.ReissueRequired = false
	m.mu.Unlock()
	m.shareState(ctx, st)
	m.o.Logger.Info("plex.direct certificate installed", "subject", bundle.Subject, "expires", bundle.Leaf.NotAfter)
	return nil
}

func (m *CertManager) installSelfSigned(hosts []string, hash string) error {
	b, err := GenerateSelfSigned(hosts, hash, m.o.SelfSignedValidity)
	if err != nil {
		return err
	}
	m.o.Logger.Info("installed self-signed certificate", "expires", b.Leaf.NotAfter)
	return m.write(b)
}

// Install stores and activates a bundle (used by Ensure; exported so the
// spike can install a certificate it obtained by hand).
func (m *CertManager) Install(b *CertBundle) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.write(b); err != nil {
		return err
	}
	if !isSelfSigned(b.Leaf) {
		m.shareState(context.Background(), m.currentState())
	}
	return nil
}

// shareState stores the certificate and request state for the cluster;
// failures are logged (the local copy keeps serving).
func (m *CertManager) shareState(ctx context.Context, st certState) {
	if m.o.Shared == nil {
		return
	}
	if err := m.pushSharedLocked(ctx, st); err != nil {
		m.o.Logger.Warn("store certificate for the cluster", "err", err)
	}
}

func (m *CertManager) write(b *CertBundle) error {
	if m.o.Dir == "" {
		return errors.New("plextv: no certificate directory")
	}
	pair, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return err
	}
	pair.Leaf = b.Leaf
	if err := writeFileAtomic(m.keyPath(), b.KeyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(m.certPath(), b.CertPEM, 0o644); err != nil {
		return err
	}
	m.install(&pair, b.Leaf, m.certPath(), m.keyPath(), false)
	return nil
}

func (m *CertManager) saveState(st certState) {
	if err := m.persistState(st); err != nil {
		m.o.Logger.Warn("save certificate state", "err", err)
	}
}

func (m *CertManager) persistState(st certState) error {
	if m.o.Dir == "" {
		return errors.New("plextv: no certificate state directory")
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(m.statePath(), b, 0o600)
}

// RequireReissue records a claim lifecycle change without touching certificate files.
func (m *CertManager) RequireReissue(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.custom() {
		return nil
	}
	if m.o.Shared != nil {
		if _, err := m.syncSharedLocked(ctx); err != nil {
			return err
		}
	}
	st := m.currentState()
	st.ReissueRequired = true
	st.LastError = ""
	st.LastAttempt = time.Time{}
	if err := m.persistState(st); err != nil {
		return err
	}
	m.mu.Lock()
	m.info.ReissueRequired = true
	m.info.LastError = ""
	m.info.LastAttempt = time.Time{}
	m.mu.Unlock()
	return m.pushSharedLocked(ctx, st)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// keyFromPEM parses a PKCS#8/PKCS#1/EC private key (tests and Install
// callers holding PEM).
func keyFromPEM(b []byte) (crypto.Signer, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM key")
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	return nil, errors.New("unsupported private key")
}
