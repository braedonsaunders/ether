package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// Dial opens an NNTP connection. With the proxy off, or with Usenet left
// on the normal route, it dials addr directly. Otherwise it dials through
// the proxy and returns the proxy's error unchanged in kind: a failed
// gateway is never followed by a direct dial.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	p := Current()
	if !p.Enabled || !p.Usenet {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	if p.Proxy == nil || p.Proxy.Hostname() == "" {
		return nil, ErrNotConfigured
	}
	switch p.Proxy.Scheme {
	case "socks5", "socks5h":
		return dialSOCKS(ctx, p.Proxy, addr)
	case "http", "https":
		return dialHTTPConnect(ctx, p.Proxy, addr)
	default:
		return nil, ErrNotConfigured
	}
}

func dialSOCKS(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	// A direct dialer reaches the proxy itself. FromURL's error can echo
	// the URL, so it is replaced before it can include a password.
	d, err := proxy.FromURL(proxyURL, &net.Dialer{})
	if err != nil {
		return nil, ErrNotConfigured
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("outbound proxy does not support cancellable connections")
	}
	c, err := cd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, scrub(err, proxyURL)
	}
	return c, nil
}

func dialHTTPConnect(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	host := proxyURL.Hostname()
	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, scrub(err, proxyURL)
	}
	if proxyURL.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, scrub(err, proxyURL)
		}
		conn = tc
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(err error) (net.Conn, error) {
		stop()
		conn.Close()
		return nil, scrub(err, proxyURL)
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(conn); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fail(err)
	}
	if resp.Body != nil {
		resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("outbound proxy refused the tunnel (%d)", resp.StatusCode))
	}
	stop()
	return &readerConn{Conn: conn, r: br}, nil
}

// readerConn keeps bytes ReadResponse buffered past the tunnel response.
type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// scrub drops a proxy URL or password if a dial error included one.
func scrub(err error, proxyURL *url.URL) error {
	if err == nil || proxyURL == nil {
		return err
	}
	msg := err.Error()
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		if password != "" {
			msg = strings.ReplaceAll(msg, password, "xxxxx")
		}
		if user := proxyURL.User.Username(); user != "" {
			msg = strings.ReplaceAll(msg, user, "xxxxx")
		}
	}
	redacted := proxyURL.Redacted()
	msg = strings.ReplaceAll(msg, proxyURL.String(), redacted)
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}
