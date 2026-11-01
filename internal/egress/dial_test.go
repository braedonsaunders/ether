package egress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/usenet"
)

func TestSOCKSKeepsProviderHostname(t *testing.T) {
	got := make(chan string, 1)
	ln := socksServer(t, func(host string, port int) net.Conn {
		got <- net.JoinHostPort(host, strconv.Itoa(port))
		return nil
	})
	u, err := url.Parse("socks5://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("ether", "secret-pass")
	prev := Current()
	Set(Policy{Enabled: true, Proxy: u, Usenet: true})
	t.Cleanup(func() { Set(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Dial(ctx, "tcp", "news.example.invalid:563")
	if err == nil {
		t.Fatal("expected the refused tunnel to fail")
	}
	if stringsContains(err.Error(), "secret-pass") {
		t.Fatalf("dial error leaked a secret: %v", err)
	}
	select {
	case addr := <-got:
		if addr != "news.example.invalid:563" {
			t.Fatalf("socks target %q", addr)
		}
	case <-ctx.Done():
		t.Fatal("socks server saw no request")
	}
}

func TestHTTPConnectDoesNotDialProviderDirectly(t *testing.T) {
	provider, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	accepted := make(chan struct{})
	go func() {
		c, err := provider.Accept()
		if err == nil {
			c.Close()
			close(accepted)
		}
	}()
	var connectHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectHost = r.Host
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer proxy.Close()
	withUsenetProxy(t, proxy.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Dial(ctx, "tcp", provider.Addr().String())
	if err == nil {
		t.Fatal("refused tunnel succeeded")
	}
	if connectHost != provider.Addr().String() {
		t.Fatalf("CONNECT host %q", connectHost)
	}
	select {
	case <-accepted:
		t.Fatal("provider accepted a direct connection")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestUsenetTLSThroughSOCKSVerifiesProviderName(t *testing.T) {
	good, pool := nntpTLS(t, "news.example")
	bad, _ := nntpTLS(t, "other.example")
	ln := socksServer(t, func(host string, port int) net.Conn {
		if host != "news.example" {
			t.Errorf("socks host %q", host)
			return nil
		}
		if port == 563 {
			return good
		}
		return bad
	})
	withUsenetProxy(t, "socks5://"+ln.Addr().String())
	opts := usenet.DialOptions{
		Host: "news.example", TLS: true, Timeout: 2 * time.Second,
		DialContext: Dial,
		TLSConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	c, err := usenet.Dial(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	opts.Port = 119
	if _, err := usenet.Dial(context.Background(), opts); err == nil {
		t.Fatal("certificate for another name was accepted")
	}
}

func withUsenetProxy(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	prev := Current()
	Set(Policy{Enabled: true, Proxy: u, Usenet: true})
	t.Cleanup(func() { Set(prev) })
}

func stringsContains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || len(s) > 0 && contains(s, sub))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// socksServer answers SOCKS5 CONNECT. dial returns the upstream connection,
// or nil to refuse. The greeting and request are read before dial runs.
func socksServer(t *testing.T, dial func(host string, port int) net.Conn) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 300)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
					return
				}
				if _, err := c.Write([]byte{5, 0}); err != nil {
					return
				}
				if _, err := io.ReadFull(c, buf[:4]); err != nil {
					return
				}
				var host string
				switch buf[3] {
				case 1:
					if _, err := io.ReadFull(c, buf[:4]); err != nil {
						return
					}
					host = net.IP(buf[:4]).String()
				case 3:
					if _, err := io.ReadFull(c, buf[:1]); err != nil {
						return
					}
					n := int(buf[0])
					if _, err := io.ReadFull(c, buf[:n]); err != nil {
						return
					}
					host = string(buf[:n])
				default:
					_, _ = c.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				port := int(buf[0])<<8 | int(buf[1])
				up := dial(host, port)
				if up == nil {
					_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				done := make(chan struct{})
				go func() { _, _ = io.Copy(up, c); close(done) }()
				_, _ = io.Copy(c, up)
				<-done
			}(c)
		}
	}()
	return ln
}

func nntpTLS(t *testing.T, name string) (net.Conn, *x509.CertPool) {
	t.Helper()
	cert, pool := certFor(t, name)
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		tlsSrv := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err := tlsSrv.Handshake(); err != nil {
			return
		}
		_, _ = tlsSrv.Write([]byte("200 test greeting\r\n"))
		buf := make([]byte, 64)
		_, _ = tlsSrv.Read(buf)
	}()
	return client, pool
}

func certFor(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return cert, pool
}
