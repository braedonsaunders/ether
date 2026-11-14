package usenet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/sockerr"
)

// Problem kinds reported by TestProvider.
const (
	ProblemConfig      = "config"      // missing or invalid settings
	ProblemDNS         = "dns"         // host name does not resolve
	ProblemUnreachable = "unreachable" // refused, no route, reset
	ProblemTimeout     = "timeout"     // no answer in time
	ProblemTLS         = "tls"         // handshake or certificate failure
	ProblemAuth        = "auth"        // credentials rejected
	ProblemUnavailable = "unavailable" // server refused service (e.g. too many connections)
	ProblemProtocol    = "protocol"    // not an NNTP server, or unexpected replies
)

// ProviderError is TestProvider's failure: a stable Kind for the UI and a
// human-readable Message suitable for showing to the owner as-is.
type ProviderError struct {
	Kind    string
	Message string
	Err     error // underlying error
}

func (e *ProviderError) Error() string { return e.Message }
func (e *ProviderError) Unwrap() error { return e.Err }

// TestProvider connects to p, logs in and runs DATE, using the fetcher's
// TLS and dialer options. A successful check re-enables the matching pool.
func (f *Fetcher) TestProvider(ctx context.Context, p config.UsenetProvider) error {
	err := testProvider(ctx, p, &f.o)
	if err == nil {
		for _, pool := range f.order() {
			// A draft with changed credentials must never unlock the saved account.
			if pool.cfg.ID == p.ID && pool.cfg.Host == p.Host && pool.cfg.Port == p.Port &&
				pool.cfg.TLS == p.TLS && pool.cfg.Username == p.Username && pool.cfg.Password == p.Password {
				pool.brk.authRecovered()
			}
		}
	}
	return err
}

// TestProvider is the setup wizard's check with default options.
func TestProvider(ctx context.Context, p config.UsenetProvider) error {
	var o Options
	o.withDefaults()
	return testProvider(ctx, p, &o)
}

func testProvider(ctx context.Context, p config.UsenetProvider, o *Options) error {
	if strings.TrimSpace(p.Host) == "" {
		return &ProviderError{Kind: ProblemConfig, Message: "Enter the server host name."}
	}
	if p.Port < 0 || p.Port > 65535 {
		return &ProviderError{Kind: ProblemConfig, Message: fmt.Sprintf("Port %d is not valid.", p.Port)}
	}
	if p.Username == "" && p.Password != "" {
		return &ProviderError{Kind: ProblemConfig, Message: "A password was given without a username."}
	}
	c, err := Dial(ctx, DialOptions{
		Host: p.Host, Port: p.Port, TLS: p.TLS,
		Username: p.Username, Password: p.Password,
		TLSConfig: o.TLSConfig, Timeout: o.DialTimeout, CommandTimeout: o.DialTimeout,
		DialContext: o.DialContext,
	})
	if err != nil {
		return describeError(p, err)
	}
	defer c.Quit()
	if _, err := c.Date(ctx); err != nil {
		var pe *ProtocolError
		if !errors.As(err, &pe) || errors.Is(err, ErrAuthRequired) {
			return describeError(p, err)
		}
		// Some servers lack DATE; the login worked, which is what matters.
	}
	return nil
}

func describeError(p config.UsenetProvider, err error) error {
	port := p.Port
	if port == 0 {
		port = 119
		if p.TLS {
			port = 563
		}
	}
	addr := net.JoinHostPort(p.Host, strconv.Itoa(port))
	pe := func(kind, msg string) error { return &ProviderError{Kind: kind, Message: msg, Err: err} }

	var (
		dnsErr   *net.DNSError
		recErr   tls.RecordHeaderError
		unkAuth  x509.UnknownAuthorityError
		hostErr  x509.HostnameError
		certErr  x509.CertificateInvalidError
		protoErr *ProtocolError
		alertErr tls.AlertError
	)
	switch {
	case errors.As(err, &dnsErr):
		return pe(ProblemDNS, fmt.Sprintf("Could not find the server %q. Check the host name.", p.Host))
	case errors.Is(err, ErrAuthFailed), errors.Is(err, ErrAuthRequired):
		return pe(ProblemAuth, "The server rejected the username or password.")
	case errors.As(err, &recErr):
		return pe(ProblemTLS, fmt.Sprintf("SSL is enabled but %s did not answer with SSL/TLS. Use the provider's SSL port (usually 563) or turn SSL off.", addr))
	case errors.As(err, &unkAuth):
		return pe(ProblemTLS, fmt.Sprintf("The certificate of %s is not issued by a trusted authority.", addr))
	case errors.As(err, &hostErr):
		return pe(ProblemTLS, fmt.Sprintf("The certificate of %s is for a different host name: %v.", addr, hostErr))
	case errors.As(err, &certErr):
		return pe(ProblemTLS, fmt.Sprintf("The certificate of %s is invalid: %v.", addr, certErr))
	case errors.As(err, &alertErr), strings.Contains(err.Error(), "tls handshake"):
		return pe(ProblemTLS, fmt.Sprintf("The SSL/TLS handshake with %s failed: %v.", addr, errors.Unwrap(err)))
	case errors.Is(err, ErrServiceUnavailable):
		msg := "The server refused service"
		if errors.As(err, &protoErr) && protoErr.Msg != "" {
			msg += ": " + protoErr.Msg
		}
		return pe(ProblemUnavailable, msg+". The account may be over its connection limit; try again or lower the connection count.")
	case sockerr.IsConnRefused(err):
		return pe(ProblemUnreachable, fmt.Sprintf("%s refused the connection. Check the host and port.", addr))
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		hint := ""
		if !p.TLS {
			hint = " If this is an SSL port, turn SSL on."
		}
		return pe(ProblemTimeout, fmt.Sprintf("%s did not respond in time. Check the host, port and your firewall.%s", addr, hint))
	case errors.As(err, &protoErr), strings.Contains(err.Error(), "malformed reply"):
		hint := ""
		if !p.TLS {
			hint = " If this is an SSL port, turn SSL on."
		}
		return pe(ProblemProtocol, fmt.Sprintf("%s does not look like a Usenet (NNTP) server: %v.%s", addr, err, hint))
	case isNetworkError(err):
		return pe(ProblemUnreachable, fmt.Sprintf("Could not connect to %s: %v.", addr, err))
	}
	return pe(ProblemProtocol, fmt.Sprintf("Testing %s failed: %v.", addr, err))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
