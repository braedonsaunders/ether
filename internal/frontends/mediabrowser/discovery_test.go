package mediabrowser

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDiscovery(t *testing.T) {
	d := &discovery{info: PublicInfo{ID: "server-id", ServerName: "Ether"}, port: 8096}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	port := d.conn.LocalAddr().(*net.UDPAddr).Port
	d.mu.Unlock()
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = c.Write([]byte("Who is JellyfinServer?")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err = json.Unmarshal(buf[:n], &result); err != nil || result["Id"] != "server-id" || result["Name"] != "Ether" || !strings.HasSuffix(result["Address"], ":8096") {
		t.Fatal(result, err)
	}
}
