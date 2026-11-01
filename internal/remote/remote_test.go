package remote

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFilterAddrs(t *testing.T) {
	mk := func(cidr string) net.Addr {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	ifs := []ifaceInfo{
		{name: "lo0", flags: net.FlagUp | net.FlagLoopback, addrs: []net.Addr{mk("127.0.0.1/8")}},
		{name: "en0", flags: net.FlagUp, addrs: []net.Addr{mk("fe80::1/64"), mk("192.168.1.5/24"), mk("2001:db8::5/64")}},
		{name: "en1", flags: 0, addrs: []net.Addr{mk("192.168.2.5/24")}}, // down
		{name: "docker0", flags: net.FlagUp, addrs: []net.Addr{mk("172.17.0.1/16")}},
		{name: "eth1", flags: net.FlagUp, addrs: []net.Addr{mk("169.254.3.4/16"), mk("10.0.0.7/8"), mk("100.100.1.1/10")}},
		{name: "eth2", flags: net.FlagUp, addrs: []net.Addr{mk("203.0.113.9/24")}},
	}
	got := filterAddrs(ifs, LANOptions{})
	var ips []string
	for _, a := range got {
		ips = append(ips, a.IP.String())
	}
	want := "192.168.1.5,10.0.0.7,203.0.113.9"
	if strings.Join(ips, ",") != want {
		t.Fatalf("got %v want %s", ips, want)
	}
	got = filterAddrs(ifs, LANOptions{IncludeIPv6: true, IncludeVirtual: true})
	ips = nil
	for _, a := range got {
		ips = append(ips, a.IP.String())
	}
	if strings.Join(ips, ",") != "192.168.1.5,172.17.0.1,10.0.0.7,203.0.113.9,2001:db8::5" {
		t.Fatalf("with v6+virtual: %v", ips)
	}
	if _, err := LANAddresses(LANOptions{}); err != nil {
		t.Fatalf("real interfaces: %v", err)
	}
}

func TestPlexDirectHost(t *testing.T) {
	h := PlexDirectHost(net.ParseIP("192.168.1.5"), "ABCDEF0123")
	if h != "192-168-1-5.abcdef0123.plex.direct" {
		t.Fatal(h)
	}
	ip, hash, ok := ParsePlexDirectHost(h)
	if !ok || !ip.Equal(net.ParseIP("192.168.1.5")) || hash != "abcdef0123" {
		t.Fatal(ip, hash, ok)
	}
	h6 := PlexDirectHost(net.ParseIP("2001:db8::1"), "aa")
	if h6 != "2001-db8-0-0-0-0-0-1.aa.plex.direct" {
		t.Fatal(h6)
	}
	ip, _, ok = ParsePlexDirectHost(h6)
	if !ok || !ip.Equal(net.ParseIP("2001:db8::1")) {
		t.Fatal(ip, ok)
	}
	for _, bad := range []string{"example.com", "x.plex.direct", "a.b.c.plex.direct", "zz-1.aa.plex.direct"} {
		if _, _, ok := ParsePlexDirectHost(bad); ok {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestBuildConnections(t *testing.T) {
	lan := []net.IP{net.ParseIP("192.168.1.5")}
	wan := net.ParseIP("203.0.113.7")
	cs := BuildConnections(BuildOptions{LAN: lan, WAN: wan, LocalPort: 32400, PublicPort: 40000, CertHash: "hash",
		CustomURLs: []string{"https://plex.example.com/", "ftp://bad", "http://10.0.0.2:8080"}})
	want := []string{
		"https://192-168-1-5.hash.plex.direct:32400",
		"http://192.168.1.5:32400",
		"https://203-0-113-7.hash.plex.direct:40000",
		"http://203.0.113.7:40000",
		"https://plex.example.com",
		"http://10.0.0.2:8080",
	}
	if got := URIs(cs); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
	if !cs[0].Local || cs[2].Local || cs[2].Port != 40000 || cs[0].Address != "192.168.1.5" {
		t.Fatalf("flags: %+v", cs)
	}
	if !cs[4].Custom || cs[4].Port != 443 || cs[4].Local || !cs[5].Local {
		t.Fatalf("custom: %+v %+v", cs[4], cs[5])
	}

	req := URIs(BuildConnections(BuildOptions{LAN: lan, CertHash: "hash", Secure: SecureRequired}))
	if len(req) != 1 || !strings.HasPrefix(req[0], "https://") {
		t.Fatalf("required: %v", req)
	}
	dis := URIs(BuildConnections(BuildOptions{LAN: lan, CertHash: "hash", Secure: SecureDisabled}))
	if len(dis) != 1 || dis[0] != "http://192.168.1.5:32400" {
		t.Fatalf("disabled: %v", dis)
	}
	// Required without a certificate still yields something usable.
	noCert := URIs(BuildConnections(BuildOptions{LAN: lan, Secure: SecureRequired}))
	if len(noCert) != 1 || noCert[0] != "http://192.168.1.5:32400" {
		t.Fatalf("required/no cert: %v", noCert)
	}
	self := URIs(BuildConnections(BuildOptions{LAN: lan, HTTPSOnPlainIP: true, Secure: SecureRequired}))
	if len(self) != 1 || self[0] != "https://192.168.1.5:32400" {
		t.Fatalf("self-signed: %v", self)
	}
	// Custom HTTPS URL satisfies SecureRequired without a plex.direct certificate hash.
	byo := URIs(BuildConnections(BuildOptions{
		LAN:        lan,
		Secure:     SecureRequired,
		CustomURLs: []string{"https://plex.example.com", "http://plex.example.com:32400"},
	}))
	if len(byo) != 1 || byo[0] != "https://plex.example.com" {
		t.Fatalf("byo https required: %v", byo)
	}
}

func TestParseCustomURLAndCGNATFilter(t *testing.T) {
	cgnat, err := ParseCustomURL("http://100.100.1.2:32400/")
	if err != nil {
		t.Fatalf("ParseCustomURL CGNAT: %v", err)
	}
	if !cgnat.Local || !cgnat.Custom || cgnat.URI != "http://100.100.1.2:32400" {
		t.Fatalf("CGNAT custom connection = %+v", cgnat)
	}
	if IsPublishableURI("http://100.100.1.2:32400") || IsPublishableURI("https://100-64-0-1.abcdef0123.plex.direct:32400") || IsPublishableURI("http://100.127.255.254:32400") {
		t.Fatal("CGNAT 100.64.0.0/10 URI reported publishable to plex.tv")
	}
	if !IsPublishableURI("http://100.63.255.254:32400") || !IsPublishableURI("http://100.128.0.1:32400") || !IsPublishableURI("http://192.168.1.5:32400") || !IsPublishableURI("https://plex.example.com") {
		t.Fatal("non-CGNAT URI reported unpublishable")
	}

	for _, junk := range []string{
		"",
		"[http://192.168.88.244:32420](http://192.168.88.244:32420)",
		"[plex](https://plex.example.com)",
		"http://192.168.88.244:32420](http://192.168.88.244:32420)",
		"192.168.88.244:32420",
		"ftp://plex.example.com",
		"http://user:pass@plex.example.com:32400",
		"https://plex.example.com/some/path",
		"https://plex.example.com?token=1",
		"https://plex.example.com#section",
		"http://192.168.1.999:32400",
		"http://..bad..:32400",
		"http://plex.example.com:70000",
		"http://plex.example.com:",
	} {
		if _, err := ParseCustomURL(junk); err == nil {
			t.Errorf("ParseCustomURL(%q) succeeded, want error", junk)
		}
		if IsPublishableURI(junk) {
			t.Errorf("IsPublishableURI(%q) = true, want false", junk)
		}
	}
}

func TestWANDetector(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 500)
	}))
	defer bad.Close()
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "192.168.1.1\n")
	}))
	defer private.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ip":"203.0.113.7"}`)
	}))
	defer good.Close()
	d := NewWANDetector(nil, nil, bad.URL, private.URL, good.URL)
	ip, err := d.Detect(context.Background())
	if err != nil || ip.String() != "203.0.113.7" {
		t.Fatal(ip, err)
	}
	d = NewWANDetector(nil, nil, bad.URL, private.URL)
	if _, err := d.Detect(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// fakeIGD is an httptest IGD with a WANIPConnection:1 service.
type fakeIGD struct {
	mu        sync.Mutex
	mappings  map[string]string // "TCP/32400" -> "client:port"
	leases    map[string]string
	permanent bool   // reject non-zero leases with 725
	conflict  string // internal client already holding every port
	strict    bool   // refuse to add over or delete any existing entry (606/718)
	calls     []string
	srv       *httptest.Server
}

func newFakeIGD(t *testing.T) *fakeIGD {
	f := &fakeIGD{mappings: map[string]string{}, leases: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/rootDesc.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <specVersion><major>1</major><minor>0</minor></specVersion>
 <device>
  <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  <friendlyName>Fake Router</friendlyName>
  <serviceList><service><serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType><controlURL>/l3f</controlURL></service></serviceList>
  <deviceList><device>
   <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
   <deviceList><device>
    <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
    <serviceList><service>
     <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
     <controlURL>/ctl/IPConn</controlURL>
    </service></serviceList>
   </device></deviceList>
  </device></deviceList>
 </device>
</root>`)
	})
	mux.HandleFunc("/ctl/IPConn", f.soap)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIGD) mapping(k string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.mappings[k]
	return v, ok
}

func (f *fakeIGD) lease(k string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leases[k]
}

func (f *fakeIGD) ncalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeIGD) nmappings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mappings)
}

func (f *fakeIGD) soap(w http.ResponseWriter, r *http.Request) {
	action := r.Header.Get("SOAPAction")
	body, _ := io.ReadAll(r.Body)
	b := string(body)
	arg := func(n string) string {
		i := strings.Index(b, "<"+n+">")
		j := strings.Index(b, "</"+n+">")
		if i < 0 || j < 0 {
			return ""
		}
		return b[i+len(n)+2 : j]
	}
	fault := func(code int, desc string) {
		w.WriteHeader(500)
		fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, desc)
	}
	ok := func(name, inner string) {
		fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%sResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">%s</u:%sResponse></s:Body></s:Envelope>`, name, inner, name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(action, `"urn:schemas-upnp-org:service:WANIPConnection:1#`) {
		fault(401, "Invalid Action")
		return
	}
	name := strings.TrimSuffix(strings.TrimPrefix(action, `"urn:schemas-upnp-org:service:WANIPConnection:1#`), `"`)
	f.calls = append(f.calls, name)
	key := arg("NewProtocol") + "/" + arg("NewExternalPort")
	switch name {
	case "GetExternalIPAddress":
		ok(name, "<NewExternalIPAddress>203.0.113.7</NewExternalIPAddress>")
	case "AddPortMapping":
		if f.permanent && arg("NewLeaseDuration") != "0" {
			fault(725, "OnlyPermanentLeasesSupported")
			return
		}
		if _, exists := f.mappings[key]; exists && f.strict {
			fault(718, "ConflictInMappingEntry")
			return
		}
		if f.conflict != "" {
			if _, exists := f.mappings[key]; !exists {
				f.mappings[key] = f.conflict + ":32400"
			}
		}
		if cur, exists := f.mappings[key]; exists && !strings.HasPrefix(cur, arg("NewInternalClient")+":") {
			fault(718, "ConflictInMappingEntry")
			return
		}
		f.mappings[key] = arg("NewInternalClient") + ":" + arg("NewInternalPort")
		f.leases[key] = arg("NewLeaseDuration")
		ok(name, "")
	case "DeletePortMapping":
		if f.strict {
			fault(606, "Action not authorized")
			return
		}
		if _, exists := f.mappings[key]; !exists {
			fault(714, "NoSuchEntryInArray")
			return
		}
		delete(f.mappings, key)
		ok(name, "")
	case "GetSpecificPortMappingEntry":
		cur, exists := f.mappings[key]
		if !exists {
			fault(714, "NoSuchEntryInArray")
			return
		}
		host, port, _ := strings.Cut(cur, ":")
		ok(name, fmt.Sprintf("<NewInternalPort>%s</NewInternalPort><NewInternalClient>%s</NewInternalClient><NewEnabled>1</NewEnabled><NewPortMappingDescription>x</NewPortMappingDescription><NewLeaseDuration>0</NewLeaseDuration>", port, host))
	default:
		fault(401, "Invalid Action")
	}
}

// ssdpResponder answers M-SEARCH on loopback with location.
func ssdpResponder(t *testing.T, location string) string {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !strings.HasPrefix(string(buf[:n]), "M-SEARCH") || !strings.Contains(string(buf[:n]), "InternetGatewayDevice:1") {
				continue
			}
			resp := "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nUSN: uuid:x::urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nLOCATION: " + location + "\r\n\r\n"
			c.WriteToUDP([]byte(resp), src)
			c.WriteToUDP([]byte("garbage"), src)
		}
	}()
	return c.LocalAddr().String()
}

func TestUPnPDiscoverAndMap(t *testing.T) {
	f := newFakeIGD(t)
	target := ssdpResponder(t, f.srv.URL+"/rootDesc.xml")
	ctx := context.Background()
	igd, err := DiscoverIGD(ctx, DiscoverOptions{Target: target, Wait: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if igd.ServiceType != ServiceWANIPConnection1 || igd.ControlURL != f.srv.URL+"/ctl/IPConn" || igd.FriendlyName != "Fake Router" {
		t.Fatalf("%+v", igd)
	}
	if !igd.LocalIP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("local ip %v", igd.LocalIP)
	}
	ip, err := igd.ExternalIP(ctx)
	if err != nil || ip.String() != "203.0.113.7" {
		t.Fatal(ip, err)
	}
	if err := igd.AddPortMapping(ctx, "tcp", 32400, 32400, igd.LocalIP, "Ether", time.Hour); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.mapping("TCP/32400"); v != "127.0.0.1:32400" || f.lease("TCP/32400") != "3600" {
		t.Fatal(v, f.lease("TCP/32400"))
	}
	err = igd.DeletePortMapping(ctx, "tcp", 1)
	ue, ok := err.(*UPnPError)
	if !ok || ue.Code != 714 {
		t.Fatalf("want 714 got %v", err)
	}
}

func TestPortMapper(t *testing.T) {
	f := newFakeIGD(t)
	f.permanent = true
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	discovers := 0
	m := NewPortMapper(PortMapperOptions{
		DiscoverIGD: func(ctx context.Context) (*IGD, error) {
			discovers++
			return LoadIGD(ctx, nil, f.srv.URL+"/rootDesc.xml")
		},
		Gateway: func() (net.IP, error) { return nil, fmt.Errorf("no nat-pmp") },
		Now:     clock,
	})
	ctx := context.Background()
	st, err := m.Ensure(ctx, 32400, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Active || st.Method != MethodUPnP || st.Lease != "permanent" || st.ExternalIP != "203.0.113.7" || st.ExternalPort != 32400 {
		t.Fatalf("%+v", st)
	}
	if f.lease("TCP/32400") != "0" {
		t.Fatal("expected permanent fallback")
	}
	// Second call within the renew window does nothing.
	n := f.ncalls()
	if _, err := m.Ensure(ctx, 32400, 0); err != nil || f.ncalls() != n {
		t.Fatal("unexpected gateway calls")
	}
	// After the renew point it re-adds without rediscovery.
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	if _, err := m.Ensure(ctx, 32400, 0); err != nil || discovers != 1 {
		t.Fatalf("renew: err=%v discovers=%d", err, discovers)
	}
	// Changing the external port removes the old mapping.
	if _, err := m.Ensure(ctx, 32400, 40000); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.mapping("TCP/32400"); ok {
		t.Fatal("old mapping kept")
	}
	if v, _ := f.mapping("TCP/40000"); v != "127.0.0.1:32400" {
		t.Fatal(v)
	}
	if got := m.Status(); got.ExternalPort != 40000 {
		t.Fatalf("status %+v", got)
	}
	m.Close(ctx)
	if f.nmappings() != 0 || m.Status().Active {
		t.Fatal("close did not unmap")
	}
}

func TestPortMapperConflict(t *testing.T) {
	f := newFakeIGD(t)
	f.conflict = "192.168.1.99"
	m := NewPortMapper(PortMapperOptions{
		DiscoverIGD: func(ctx context.Context) (*IGD, error) { return LoadIGD(ctx, nil, f.srv.URL+"/rootDesc.xml") },
		Gateway:     func() (net.IP, error) { return nil, fmt.Errorf("none") },
	})
	st, err := m.Ensure(context.Background(), 32400, 0)
	if err == nil || st.Active || !strings.Contains(st.LastError, "already mapped to 192.168.1.99") {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	// Backoff: an immediate retry does not hit the gateway.
	n := f.ncalls()
	if _, err := m.Ensure(context.Background(), 32400, 0); err == nil || f.ncalls() != n {
		t.Fatal("expected backoff")
	}
}

// A gateway that will not replace or delete an existing entry still
// works when the entry already forwards to this host and port (left by an
// earlier run of this node); one forwarding elsewhere is a conflict.
func TestPortMapperAdoptsExistingOwnMapping(t *testing.T) {
	f := newFakeIGD(t)
	f.strict = true
	f.mappings["TCP/32400"] = "127.0.0.1:32400"
	f.mappings["TCP/32401"] = "127.0.0.1:9999"
	newMapper := func() *PortMapper {
		return NewPortMapper(PortMapperOptions{
			DiscoverIGD: func(ctx context.Context) (*IGD, error) { return LoadIGD(ctx, nil, f.srv.URL+"/rootDesc.xml") },
			Gateway:     func() (net.IP, error) { return nil, fmt.Errorf("none") },
		})
	}
	st, err := newMapper().Ensure(context.Background(), 32400, 0)
	if err != nil || !st.Active || st.ExternalPort != 32400 || st.InternalPort != 32400 || st.ExternalIP != "203.0.113.7" {
		t.Fatalf("existing own mapping not adopted: st=%+v err=%v", st, err)
	}
	if st.RenewAt.IsZero() {
		t.Fatalf("adopted mapping never re-checked: %+v", st)
	}
	// Same host, different internal port: not ours to adopt.
	st, err = newMapper().Ensure(context.Background(), 32400, 32401)
	if err == nil || st.Active {
		t.Fatalf("mapping to another port adopted: st=%+v err=%v", st, err)
	}
}

// fakeNATPMP serves RFC 6886 on loopback.
func fakeNATPMP(t *testing.T) (*net.UDPConn, func() []string) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var mu sync.Mutex
	log := &[]string{}
	go func() {
		buf := make([]byte, 64)
		first := true
		for {
			n, src, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if first {
				first = false // drop the first packet to exercise retransmission
				continue
			}
			mu.Lock()
			switch {
			case n == 2 && buf[1] == 0:
				*log = append(*log, "ext")
				resp := []byte{0, 128, 0, 0, 0, 0, 0, 1, 203, 0, 113, 7}
				c.WriteToUDP(resp, src)
			case n == 12 && (buf[1] == 1 || buf[1] == 2):
				internal := binary.BigEndian.Uint16(buf[4:6])
				ext := binary.BigEndian.Uint16(buf[6:8])
				lease := binary.BigEndian.Uint32(buf[8:12])
				*log = append(*log, fmt.Sprintf("map %d->%d %d", ext, internal, lease))
				resp := make([]byte, 16)
				resp[1] = 128 + buf[1]
				binary.BigEndian.PutUint16(resp[8:10], internal)
				if lease > 0 {
					binary.BigEndian.PutUint16(resp[10:12], ext+1) // gateway picks a different port
					binary.BigEndian.PutUint32(resp[12:16], 1800)
				}
				c.WriteToUDP(resp, src)
			default:
				c.WriteToUDP([]byte{0, 128 + buf[1], 0, 5, 0, 0, 0, 0}, src)
			}
			mu.Unlock()
		}
	}()
	return c, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), (*log)...) }
}

func TestNATPMPFallback(t *testing.T) {
	c, log := fakeNATPMP(t)
	port := c.LocalAddr().(*net.UDPAddr).Port
	m := NewPortMapper(PortMapperOptions{
		DiscoverIGD: func(ctx context.Context) (*IGD, error) { return nil, ErrNoIGD },
		Gateway:     func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil },
		NATPMPPort:  port,
		Lease:       time.Hour,
	})
	st, err := m.Ensure(context.Background(), 32400, 32400)
	if err != nil {
		t.Fatal(err)
	}
	if st.Method != MethodNATPMP || st.ExternalPort != 32401 || st.ExternalIP != "203.0.113.7" || st.Lease != "30m0s" {
		t.Fatalf("%+v", st)
	}
	m.Close(context.Background())
	got := strings.Join(log(), ";")
	if !strings.Contains(got, "map 32400->32400 3600") || !strings.Contains(got, "map 0->32400 0") {
		t.Fatal(got)
	}
}

func TestNATPMPErrorCode(t *testing.T) {
	c, _ := fakeNATPMP(t)
	n := &NATPMP{Gateway: c.LocalAddr().String()}
	_, err := n.call(context.Background(), []byte{0, 9}, 8)
	if e, ok := err.(*NATPMPError); !ok || e.Code != 5 {
		t.Fatalf("got %v", err)
	}
}

func TestGDMRoundTrip(t *testing.T) {
	info := GDMInfo{Name: "Ether\r\nInjected: x", MachineIdentifier: "abc123", Port: 32400, Version: "1.42.2", UpdatedAt: time.Unix(1700000000, 0)}
	g := NewGDMResponder(GDMOptions{Info: func() GDMInfo { return info }, ListenAddrs: []string{"127.0.0.1:0", "127.0.0.1:0"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := g.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(ctx); err == nil {
		t.Fatal("double start")
	}
	addrs := g.Addrs()
	if len(addrs) != 2 {
		t.Fatal(addrs)
	}
	for _, a := range addrs {
		reps, err := DiscoverGDM(ctx, a.String(), 500*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(reps) != 1 {
			t.Fatalf("replies %d", len(reps))
		}
		r := reps[0]
		if r.ContentType != "plex/media-server" || r.MachineIdentifier != "abc123" || r.Port != 32400 ||
			r.Version != "1.42.2" || r.Name != "Ether  Injected: x" || r.UpdatedAt.Unix() != 1700000000 {
			t.Fatalf("%+v", r)
		}
		if r.Headers.Get("Injected") != "" {
			t.Fatal("header injection")
		}
	}
	// Non-search packets are ignored.
	conn, err := net.Dial("udp4", addrs[0].String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("HELLO * HTTP/1.0\r\n\r\n"))
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := conn.Read(make([]byte, 100)); err == nil {
		t.Fatal("answered a non-search packet")
	}
	conn.Close()
	g.Stop()
	g.Stop()
	if reps, _ := DiscoverGDM(context.Background(), addrs[0].String(), 200*time.Millisecond); len(reps) != 0 {
		t.Fatal("answered after stop")
	}
}

func TestGDMMessages(t *testing.T) {
	hello := string(GDMHello(GDMInfo{Name: "E", MachineIdentifier: "m", Port: 1, Version: "v"}, false))
	if !strings.HasPrefix(hello, "HELLO * HTTP/1.0\r\nContent-Type: plex/media-server\r\nResource-Identifier: m\r\n") || !strings.HasSuffix(hello, "\r\n\r\n") {
		t.Fatalf("%q", hello)
	}
	if !strings.HasPrefix(string(GDMHello(GDMInfo{}, true)), "BYE * HTTP/1.0") {
		t.Fatal("bye")
	}
	if _, err := ParseGDMReply([]byte("HTTP/1.0 404 Not Found\r\n\r\n")); err == nil {
		t.Fatal("parsed 404")
	}
}

func TestReachability(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/identity" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><MediaContainer size="0" claimed="1" machineIdentifier="MID" version="1.42"/>`)
	}))
	defer good.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"MediaContainer":{"machineIdentifier":"OTHER"}}`)
	}))
	defer other.Close()
	rc := NewReachabilityChecker(nil)
	res := rc.Check(context.Background(), []string{other.URL, "http://127.0.0.1:1", good.URL}, "mid")
	if !res.Reachable || res.URI != good.URL || len(res.Results) != 3 {
		t.Fatalf("%+v", res)
	}
	if res.Results[0].OK || !strings.Contains(res.Results[0].Error, "different server") || res.Results[1].OK {
		t.Fatalf("%+v", res.Results)
	}
	res = rc.Check(context.Background(), []string{other.URL}, "mid")
	if res.Reachable {
		t.Fatal("reachable via other server")
	}
}

func TestParseProcRoute(t *testing.T) {
	data := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	ip, err := parseProcRoute(bufio.NewScanner(strings.NewReader(data)))
	if err != nil || ip.String() != "192.168.1.1" {
		t.Fatal(ip, err)
	}
}

// After this host's LAN address changes (DHCP), renewing through the cached
// gateway must forward the port to the new address, replacing our own
// entry for the old one, not keep re-asserting the stale forward.
func TestPortMapperFollowsLocalAddressChange(t *testing.T) {
	f := newFakeIGD(t)
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m := NewPortMapper(PortMapperOptions{
		DiscoverIGD: func(ctx context.Context) (*IGD, error) { return LoadIGD(ctx, nil, f.srv.URL+"/rootDesc.xml") },
		Gateway:     func() (net.IP, error) { return nil, fmt.Errorf("no nat-pmp") },
		Now:         clock,
	})
	ctx := context.Background()
	if _, err := m.Ensure(ctx, 32400, 0); err != nil {
		t.Fatal(err)
	}
	// Simulate the mapping having been made from the previous address.
	m.opMu.Lock()
	m.igd.LocalIP = net.ParseIP("192.168.1.77").To4()
	m.opMu.Unlock()
	f.mu.Lock()
	f.mappings["TCP/32400"] = "192.168.1.77:32400"
	f.mu.Unlock()

	mu.Lock()
	now = now.Add(3 * time.Hour) // past the renewal point
	mu.Unlock()
	st, err := m.Ensure(ctx, 32400, 0)
	if err != nil || !st.Active {
		t.Fatalf("renew: st=%+v err=%v", st, err)
	}
	if v, _ := f.mapping("TCP/32400"); v != "127.0.0.1:32400" {
		t.Fatalf("mapping still forwards to %q after the address change", v)
	}
	if st.InternalIP != "127.0.0.1" {
		t.Fatalf("status internal IP %q", st.InternalIP)
	}
}
