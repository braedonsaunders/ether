package remote

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocationMatchesResponder(t *testing.T) {
	src := &net.UDPAddr{IP: net.ParseIP("192.168.1.1"), Port: 1900}
	if !locationMatchesResponder("http://192.168.1.1:5000/rootDesc.xml", src) {
		t.Fatal("matching responder IP rejected")
	}
	if locationMatchesResponder("http://127.0.0.1:8080/rootDesc.xml", src) {
		t.Fatal("mismatched loopback location accepted")
	}
	if locationMatchesResponder("http://example.com/rootDesc.xml", src) {
		t.Fatal("hostname location accepted")
	}
}

func TestLoadIGDRejectsCrossHostControlURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <URLBase>http://192.168.99.99:9000</URLBase>
  <device>
    <friendlyName>Router</friendlyName>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
        <controlURL>http://10.0.0.9:8080/ctl</controlURL>
      </service>
    </serviceList>
  </device>
</root>`)
	}))
	defer srv.Close()
	if _, err := LoadIGD(context.Background(), srv.Client(), srv.URL+"/desc.xml"); err == nil {
		t.Fatal("expected cross-host controlURL to be rejected")
	}
}
