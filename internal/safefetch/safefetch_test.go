package safefetch

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.5.0.9", "::1", // loopback
		"10.0.0.1", "172.16.0.1", "172.31.255.1", "192.168.1.1", "fd00::1", "fc00::1", // private
		"169.254.169.254", "169.254.0.1", "fe80::1", // link-local / cloud metadata
		"100.64.0.1", "100.127.255.1", // CGNAT (repo treats it as local)
		"224.0.0.1", "ff02::1", // multicast
		"0.0.0.0", "::", // unspecified
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", // v4-in-v6 forms of blocked v4
		"not-an-ip", "",
	}
	for _, s := range blocked {
		ip, _ := netip.ParseAddr(s)
		if IsPublicIP(ip) {
			t.Errorf("IsPublicIP(%q) = true, want false", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range allowed {
		ip, err := netip.ParseAddr(s)
		if err != nil || !IsPublicIP(ip) {
			t.Errorf("IsPublicIP(%q) = false (err %v), want true", s, err)
		}
	}
}

func TestValidatePublicURL(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://127.0.0.1:8080/admin",
		"https://192.168.0.1/x",
		"http://[::1]/x",
		"ftp://8.8.8.8/x",
		"file:///etc/passwd",
		"http:///no-host",
		"8.8.8.8/no-scheme",
	} {
		if err := ValidatePublicURL(ctx, raw); !errors.Is(err, ErrBlocked) {
			t.Errorf("ValidatePublicURL(%q) = %v, want ErrBlocked", raw, err)
		}
	}
	if err := ValidatePublicURL(ctx, "https://8.8.8.8/x"); err != nil {
		t.Errorf("ValidatePublicURL(public literal) = %v", err)
	}
	// A host name that resolves privately (rebinding shape) is refused.
	g := &Guard{LookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
	}}
	if err := g.ValidatePublicURL(ctx, "http://rebind.test/x"); !errors.Is(err, ErrBlocked) {
		t.Errorf("ValidatePublicURL(rebinding host) = %v, want ErrBlocked", err)
	}
}

// fakeNetGuard maps public.test to a public IP and internal.test to
// loopback, and dials validated public IPs at the local test server.
// internal.test must never be dialed at all.
func fakeNetGuard(t *testing.T, serverAddr string, dialed *atomic.Int32) *Guard {
	t.Helper()
	return &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "public.test":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			case "internal.test":
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("unknown host %q", host)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed.Add(1)
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if host != "93.184.216.34" {
				return nil, fmt.Errorf("guard dialed unvalidated address %q", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, serverAddr)
		},
	}
}

func TestGuardedClientPublicFakeAndInternalBlocked(t *testing.T) {
	var secretHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "public-ok")
		case "/redir":
			http.Redirect(w, r, "http://internal.test/secret", http.StatusFound)
		case "/secret":
			secretHits.Add(1)
			fmt.Fprint(w, "metadata-secret")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	var dialed atomic.Int32
	guard := fakeNetGuard(t, srv.Listener.Addr().String(), &dialed)
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: nil}})

	// Public (fake) host is reachable; the body comes from the test server.
	resp, err := client.Get("http://public.test:" + port + "/ok")
	if err != nil {
		t.Fatalf("public fetch: %v", err)
	}
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if string(body[:n]) != "public-ok" {
		t.Fatalf("public fetch body = %q", body[:n])
	}

	// Direct private destination: refused before any dial or hit.
	if _, err := client.Get("http://internal.test:" + port + "/secret"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("internal fetch err = %v, want ErrBlocked", err)
	}
	if _, err := client.Get("http://169.254.169.254/latest/meta-data"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("metadata fetch err = %v, want ErrBlocked", err)
	}

	// Redirect from a public host to an internal one: refused on the hop.
	if _, err := client.Get("http://public.test:" + port + "/redir"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect fetch err = %v, want ErrBlocked", err)
	}
	if got := secretHits.Load(); got != 0 {
		t.Fatalf("internal handler hit %d times, want 0", got)
	}

	// Non-http(s) schemes are refused.
	if _, err := client.Get("file:///etc/passwd"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("file fetch err = %v, want ErrBlocked", err)
	}
}

// As pools trusted/public são distintas; cada uma tem variante direta
// (Proxy=nil, dial guardado e pinado) e variante via-proxy (Proxy do
// dono preservado), com pools próprios.
func TestGuardedTransportsPreserveProxyAndSeparatePools(t *testing.T) {
	proxyURL, _ := url.Parse("http://proxy.test:8080")
	rt := (&Guard{}).Transport(&http.Transport{Proxy: http.ProxyURL(proxyURL)})
	tr, ok := rt.(transport)
	if !ok {
		t.Fatalf("Transport type = %T, want safefetch transport", rt)
	}
	pub, okPub := tr.public.(policyTransport)
	trust, okTrust := tr.trusted.(policyTransport)
	if !okPub || !okTrust {
		t.Fatalf("pool types = %T/%T, want two policyTransport", tr.public, tr.trusted)
	}
	if pub.direct == trust.direct || pub.proxied == trust.proxied || pub.direct == pub.proxied || trust.direct == trust.proxied {
		t.Fatal("pools trusted/public (ou direto/proxy) compartilham *http.Transport")
	}
	for name, pool := range map[string]policyTransport{"public": pub, "trusted": trust} {
		if pool.direct.Proxy != nil {
			t.Errorf("%s direct Proxy != nil; variante direta não usa proxy", name)
		}
		// Direto: DialContext guardado E DialTLSContext próprio (IP +
		// ServerName original da decisão); Dial/DialTLS legados fora.
		if pool.direct.DialContext == nil || pool.direct.DialTLSContext == nil || pool.direct.Dial != nil || pool.direct.DialTLS != nil {
			t.Errorf("%s direct dial hooks not fully guarded (DialContext=%v DialTLSContext=%v Dial=%v DialTLS=%v)",
				name, pool.direct.DialContext != nil, pool.direct.DialTLSContext != nil, pool.direct.Dial != nil, pool.direct.DialTLS != nil)
		}
		// O seletor do dono fica guardado à parte (chamado uma vez em
		// policyTransport.RoundTrip); a Proxy func interna só consome a
		// decisão do contexto, sem re-selecionar.
		if pool.selector == nil || pool.proxied.Proxy == nil {
			t.Errorf("%s proxied: seletor do dono não preservado", name)
			continue
		}
		selReq, _ := http.NewRequest(http.MethodGet, "http://public.test/ok", nil)
		if got, err := pool.selector(selReq); err != nil || got == nil || got.String() != proxyURL.String() {
			t.Errorf("%s selector = %v, %v; want %v", name, got, err, proxyURL)
		}
		decReq, _ := http.NewRequest(http.MethodGet, "http://93.184.216.34/ok", nil)
		decReq = decReq.WithContext(context.WithValue(decReq.Context(), routeDecisionKey{}, &routeDecision{proxy: proxyURL}))
		got, err := pool.proxied.Proxy(decReq)
		if err != nil || got == nil || got.String() != proxyURL.String() {
			t.Errorf("%s proxied.Proxy(decisão) = %v, %v; want %v", name, got, err, proxyURL)
		}
		bareReq, _ := http.NewRequest(http.MethodGet, "http://public.test/ok", nil)
		if _, err := pool.proxied.Proxy(bareReq); !errors.Is(err, ErrUnsupportedTransport) {
			t.Errorf("%s proxied.Proxy(sem decisão) err = %v, want ErrUnsupportedTransport (sem re-selecionar)", name, err)
		}
	}
}

// Casos do dono (v3): com proxy configurado, destino público passa
// PELO proxy (proxy hits=1, direct=0) — a rota do dono é preservada.
// Destino privado/metadata é recusado na validação prévia: nem proxy
// nem destino são tocados (hits=0). Mutação: forçar Proxy=nil na
// variante via-proxy faz o 1º caso falhar (hit direto, proxy 0).
func TestGuardedClientWithConfiguredProxy(t *testing.T) {
	var proxyHits, targetHits, directDials atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		fmt.Fprint(w, "proxied")
	}))
	defer proxySrv.Close()
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		fmt.Fprint(w, "public-ok")
	}))
	defer targetSrv.Close()
	_, targetPort, _ := net.SplitHostPort(strings.TrimPrefix(targetSrv.URL, "http://"))

	guard := &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "public.test":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			case "internal.test":
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("unknown host %q", host)
		},
		// Dial direto (pinado) só pode ser este hook; se a requisição
		// for direta, ele conecta no servidor-alvo real e conta.
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			directDials.Add(1)
			var d net.Dialer
			return d.DialContext(ctx, network, targetSrv.Listener.Addr().String())
		},
	}
	proxyURL, _ := url.Parse(proxySrv.URL)
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}})

	// 1) Destino público via proxy.
	resp, err := client.Get("http://public.test:" + targetPort + "/ok")
	if err != nil {
		t.Fatalf("public fetch with configured proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "proxied" {
		t.Fatalf("public fetch body = %q, want %q (via proxy)", body, "proxied")
	}
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("proxy hit %d times, want 1", got)
	}
	if got := directDials.Load(); got != 0 {
		t.Fatalf("direct dials = %d, want 0 (rota via proxy preservada)", got)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("target hit %d times directly, want 0", got)
	}

	// 2) Destino privado/metadata: recusado antes do proxy.
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://internal.test:" + targetPort + "/secret",
	} {
		if _, err := client.Get(raw); !errors.Is(err, ErrBlocked) {
			t.Fatalf("private fetch %q via configured proxy err = %v, want ErrBlocked", raw, err)
		}
	}
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("proxy hit %d times after private fetches, want 1 (só o público)", got)
	}
	if got := directDials.Load(); got != 0 {
		t.Fatalf("direct dials = %d after private fetches, want 0", got)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("target hit %d times, want 0", got)
	}
}

// Caso do dono (v4-a): o proxy fake resolveria "public.test" para um
// servidor PRIVADO se recebesse o hostname. Com o rewrite para o IP
// validado, o proxy recebe o IP público literal (CONNECT/forma absoluta
// com IP), o privado nunca é alcançado e o caminho público via proxy
// funciona. Mutação: remover o rewrite faz este teste falhar (o proxy
// recebe o hostname, "resolve" para o privado e devolve o segredo).
func TestGuardedClientViaProxyGetsValidatedIPNotHostname(t *testing.T) {
	var privateHits, proxyHits, directDials atomic.Int32
	var requestLine, hostLine atomic.Value // string
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "public-ok")
	}))
	defer targetSrv.Close()
	_, targetPort, _ := net.SplitHostPort(strings.TrimPrefix(targetSrv.URL, "http://"))

	// Proxy cru (TCP): captura a request-line e o header Host como vão
	// na wire — um servidor Go (httptest) descartaria o Host em favor
	// do host da forma absoluta, e é justamente essa dupla que importa.
	// Se recebesse o hostname, este proxy o "resolveria" para um
	// servidor privado e devolveria o segredo; com o IP validado,
	// devolve o conteúdo público.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyLn.Close()
	go func() {
		for {
			conn, err := proxyLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				proxyHits.Add(1)
				br := bufio.NewReader(conn)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				requestLine.Store(strings.TrimSpace(line))
				for {
					h, err := br.ReadString('\n')
					if err != nil || h == "\r\n" {
						break
					}
					if strings.HasPrefix(strings.ToLower(h), "host:") {
						hostLine.Store(strings.TrimSpace(h))
					}
				}
				body := "public-ok"
				if strings.Contains(line, "public.test") {
					// A resolução (hostil) do proxy: hostname -> privado.
					privateHits.Add(1)
					body = "metadata-secret"
				}
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}(conn)
		}
	}()

	guard := &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "public.test" {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			return nil, fmt.Errorf("unknown host %q", host)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			directDials.Add(1)
			var d net.Dialer
			return d.DialContext(ctx, network, targetSrv.Listener.Addr().String())
		},
	}
	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}})

	resp, err := client.Get("http://public.test:" + targetPort + "/ok")
	if err != nil {
		t.Fatalf("public fetch via proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "public-ok" {
		t.Fatalf("body = %q, want public-ok (não o segredo do privado)", body)
	}
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("proxy hits = %d, want 1", got)
	}
	if got, want := requestLine.Load(), "GET http://93.184.216.34:"+targetPort+"/ok HTTP/1.1"; got != want {
		t.Fatalf("request-line no proxy = %v, want forma absoluta com IP validado %q (não o hostname)", got, want)
	}
	if got, want := hostLine.Load(), "Host: public.test:"+targetPort; got != want {
		t.Fatalf("Host na wire = %v, want Host original %q", got, want)
	}
	if got := privateHits.Load(); got != 0 {
		t.Fatalf("servidor privado alcançado %d vezes, want 0", got)
	}
	if got := directDials.Load(); got != 0 {
		t.Fatalf("direct dials = %d, want 0", got)
	}
}

// Caso do dono (v4-b): seletor stateful — 1ª chamada devolve proxy, 2ª
// devolveria direto. A rota decidida uma vez (proxy) é a usada: o
// seletor roda exatamente 1 vez. Mutação: re-selecionar no transporte
// interno faz este teste falhar (2ª seleção = direto, proxy hits=0).
func TestGuardedClientSelectsProxyRouteOnce(t *testing.T) {
	var selectorCalls, proxyHits, directDials atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		fmt.Fprint(w, "proxied")
	}))
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "public-ok")
	}))
	defer targetSrv.Close()
	_, targetPort, _ := net.SplitHostPort(strings.TrimPrefix(targetSrv.URL, "http://"))

	guard := &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			directDials.Add(1)
			var d net.Dialer
			return d.DialContext(ctx, network, targetSrv.Listener.Addr().String())
		},
	}
	selector := func(*http.Request) (*url.URL, error) {
		if selectorCalls.Add(1) == 1 {
			return proxyURL, nil
		}
		return nil, nil // 2ª seleção mudaria a rota para direto
	}
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: selector}})

	resp, err := client.Get("http://public.test:" + targetPort + "/ok")
	if err != nil {
		t.Fatalf("fetch com seletor stateful: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "proxied" {
		t.Fatalf("body = %q, want proxied (rota da 1ª decisão)", body)
	}
	if got := selectorCalls.Load(); got != 1 {
		t.Fatalf("seletor chamado %d vezes, want 1 (decisão única por request)", got)
	}
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("proxy hits = %d, want 1", got)
	}
	if got := directDials.Load(); got != 0 {
		t.Fatalf("direct dials = %d, want 0", got)
	}
}

func TestGuardedClientPinsSOCKSToValidatedIP(t *testing.T) {
	got := make(chan byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		if _, err := io.ReadFull(c, buf[:2]); err != nil {
			return
		}
		if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
			return
		}
		_, _ = c.Write([]byte{5, 0})
		if _, err := io.ReadFull(c, buf[:4]); err != nil {
			return
		}
		got <- buf[3]
		if buf[3] == 1 {
			_, _ = io.ReadFull(c, buf[:4])
			if net.IP(buf[:4]).String() != "93.184.216.34" {
				got <- 0
			}
		}
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
	}()
	var directDials atomic.Int32
	guard := &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			directDials.Add(1)
			return nil, errors.New("must not dial")
		},
	}
	socks, _ := url.Parse("socks5://" + ln.Addr().String())
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(socks)}})
	if _, err := client.Get("http://public.test/ok"); err == nil {
		t.Fatal("refused socks tunnel succeeded")
	}
	select {
	case atyp := <-got:
		if atyp != 1 {
			t.Fatalf("socks address type %#x, want IPv4", atyp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("socks proxy was not contacted")
	}
	if got := directDials.Load(); got != 0 {
		t.Fatalf("direct dials = %d, want 0", got)
	}
}

func TestGuardedClientRejectsUnknownProxyScheme(t *testing.T) {
	guard := &Guard{LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}}
	ftp, _ := url.Parse("ftp://127.0.0.1:21")
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(ftp)}})
	if _, err := client.Get("http://public.test/ok"); !errors.Is(err, ErrUnsupportedTransport) {
		t.Fatalf("ftp proxy err = %v, want ErrUnsupportedTransport", err)
	}
}

// HTTPS direto: o dial vai ao IP validado, mas o SNI/ServerName é o
// hostname original (DialTLSContext próprio lendo a decisão).
func TestGuardedClientDirectHTTPSUsesOriginalServerName(t *testing.T) {
	var sni atomic.Value // string
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "tls-ok")
	}))
	tlsSrv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni.Store(hello.ServerName)
		return nil, nil
	}}
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	_, tlsPort, _ := net.SplitHostPort(strings.TrimPrefix(tlsSrv.URL, "https://"))

	guard := &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil || host != "93.184.216.34" {
				return nil, fmt.Errorf("guard dialed unvalidated address %q", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, tlsSrv.Listener.Addr().String())
		},
	}
	client := guard.Client(&http.Client{Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test fixture; SNI é o que importa
	}})
	resp, err := client.Get("https://public.test:" + tlsPort + "/ok")
	if err != nil {
		t.Fatalf("direct https fetch: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tls-ok" {
		t.Fatalf("body = %q, want tls-ok", body)
	}
	if got := sni.Load(); got != "public.test" {
		t.Fatalf("SNI = %v, want public.test (hostname original, não o IP)", got)
	}
}

// HTTPS via proxy: o CONNECT vai ao IP validado (o proxy não resolve o
// hostname) e o TLS dentro do túnel usa o hostname original como SNI
// (clone de transporte por requisição com ServerName original).
func TestGuardedClientHTTPSViaProxyConnectsToValidatedIP(t *testing.T) {
	var sni, connectDest atomic.Value // string
	var proxyHits atomic.Int32
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "tls-proxied-ok")
	}))
	tlsSrv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni.Store(hello.ServerName)
		return nil, nil
	}}
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	_, tlsPort, _ := net.SplitHostPort(strings.TrimPrefix(tlsSrv.URL, "https://"))

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "want CONNECT", http.StatusMethodNotAllowed)
			return
		}
		proxyHits.Add(1)
		connectDest.Store(r.Host)
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		clientConn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", tlsSrv.Listener.Addr().String())
		if err != nil {
			clientConn.Close()
			return
		}
		_, _ = bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = bufrw.Flush()
		go func() { _, _ = io.Copy(upstream, clientConn) }()
		go func() { _, _ = io.Copy(clientConn, upstream) }()
	}))
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)

	guard := &Guard{LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}}
	client := guard.Client(&http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test fixture; SNI/CONNECT são o que importa
	}})
	resp, err := client.Get("https://public.test:" + tlsPort + "/ok")
	if err != nil {
		t.Fatalf("https via proxy fetch: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tls-proxied-ok" {
		t.Fatalf("body = %q, want tls-proxied-ok", body)
	}
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("proxy CONNECT hits = %d, want 1", got)
	}
	if got, want := connectDest.Load(), "93.184.216.34:"+tlsPort; got != want {
		t.Fatalf("CONNECT destino = %v, want IP validado %v (não o hostname)", got, want)
	}
	if got := sni.Load(); got != "public.test" {
		t.Fatalf("SNI = %v, want public.test (hostname original, não o IP)", got)
	}
}

type countingRoundTripper struct{ hits *atomic.Int32 }

func (c countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.hits.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("custom-ok")),
		Request:    req,
	}, nil
}

// RoundTripper custom/wrapped não é clonável: o cliente guardado deve
// falhar com "safefetch: unsupported transport" sem chamar o original
// (sem fallback sem enforcement). Mutação: restaurar o fallback faz
// este teste falhar (custom é chamado e responde 200).
func TestGuardedClientRejectsUnsupportedTransport(t *testing.T) {
	var hits atomic.Int32
	guard := &Guard{LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}}
	client := guard.Client(&http.Client{Transport: countingRoundTripper{hits: &hits}})
	_, err := client.Get("http://public.test/ok")
	if !errors.Is(err, ErrUnsupportedTransport) {
		t.Fatalf("custom transport err = %v, want ErrUnsupportedTransport", err)
	}
	if !strings.Contains(err.Error(), "safefetch: unsupported transport") {
		t.Fatalf("custom transport err = %q, want clear unsupported message", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("custom RoundTripper called %d times, want 0", got)
	}
}

// Regression (owner review, bypass 2): a connection opened to a
// private trusted origin lives in the trusted pool only. A later
// untrusted request to the very same host:port must be blocked — with
// a shared pool it would reuse the open connection and skip the dial
// (and its address check) entirely.
func TestGuardTrustedConnectionNotReusedByUntrustedRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "lan-ok")
	}))
	defer srv.Close()

	client := (&Guard{}).Client(&http.Client{})

	// Trusted fetch opens (and pools, in the trusted pool) a connection.
	ctx := WithTrustedOrigin(context.Background(), srv.URL)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("trusted LAN fetch: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// Untrusted request to the same host:port: blocked, no new hit.
	if _, err := client.Get(srv.URL + "/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("untrusted fetch to trusted host err = %v, want ErrBlocked", err)
	}
	// A context trusting a *different* origin does not unlock it either.
	otherCtx := WithTrustedOriginFor(context.Background(), "http://127.0.0.1:1", srv.URL+"/x")
	req, _ = http.NewRequestWithContext(otherCtx, http.MethodGet, srv.URL+"/x", nil)
	if _, err := client.Do(req); !errors.Is(err, ErrBlocked) {
		t.Fatalf("fetch trusting another origin err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (only the trusted fetch)", got)
	}

	// The trusted pool still works afterwards.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("second trusted LAN fetch: %v", err)
	}
	resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits = %d, want 2 (trusted fetches only)", got)
	}
}

func TestGuardTrustedOriginAllowsOnlyThatOrigin(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "lan-ok")
	}))
	defer srv.Close()

	client := (&Guard{}).Client(&http.Client{})
	ctx := WithTrustedOrigin(context.Background(), srv.URL)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("trusted LAN fetch: %v", err)
	}
	resp.Body.Close()

	// The same private host on a different port is not the trusted origin.
	other := strings.Replace(srv.URL, srv.Listener.Addr().String(), "127.0.0.1:1", 1)
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, other, nil)
	if _, err := client.Do(req); !errors.Is(err, ErrBlocked) {
		t.Fatalf("other private origin err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (only the trusted fetch)", got)
	}
}
