package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

func TestFixturesFromCapture(t *testing.T) {
	dir := t.TempDir()
	profiles := t.TempDir()
	fixtures := t.TempDir()
	entries := []Entry{
		{Sequence: 1, Protocol: "jellyfin", Client: "infuse", Request: Request{Method: "GET", Path: "/optional"}, Response: conform.Response{Status: 404}, ResponseOrigin: "reference"},
		{Sequence: 2, Protocol: "jellyfin", Client: "infuse", Request: Request{Method: "GET", Path: "/optional"}, Response: conform.Response{Status: 404}, ResponseOrigin: "reference"},
		{Sequence: 3, Protocol: "jellyfin", Client: "infuse", Request: Request{Method: "GET", Path: "/failed"}, Response: conform.Response{Status: 502}, ResponseOrigin: "proxy-error"},
		{Sequence: 4, Protocol: "jellyfin", Client: "infuse", Request: Request{Method: "POST", Path: "/progress"}, Response: conform.Response{Status: 204}, ResponseOrigin: "reference"},
	}
	for i, e := range entries {
		b, _ := json.Marshal(e)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%08d.json", i+1)), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := FixturesFromCapture(dir, "jellyfin", "infuse", profiles, fixtures); err != nil {
		t.Fatal(err)
	}
	p, err := mediabrowser.Load("jellyfin", filepath.Join(profiles, "jellyfin"))
	if err != nil {
		t.Fatal(err)
	}
	reqs := p.Corpus("infuse")
	if len(reqs) != 2 || reqs[0].ExpectedStatus != 404 || !reqs[1].FixtureOnly {
		t.Fatalf("requests: %+v", reqs)
	}
	for i, req := range reqs {
		r, err := conform.LoadFixture(filepath.Join(fixtures, "jellyfin", "infuse", p.FixtureName("infuse", req, true)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != entries[[]int{0, 3}[i]].Response.Status {
			t.Fatal(r.Status)
		}
	}
	if _, err = p.Fetch(context.Background(), http.DefaultClient, "http://invalid", "", reqs[1], "infuse", nil, true); err == nil {
		t.Fatal("fixture-only replay allowed")
	}
	entries[0].ResponseOrigin = ""
	b, _ := json.Marshal(entries[0])
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%08d.json", 1)), b, 0600)
	if err := FixturesFromCapture(dir, "jellyfin", "infuse", profiles, fixtures); err == nil {
		t.Fatal("ambiguous origin accepted")
	}
}

func TestRedactObjectKeysPreservesNumbers(t *testing.T) {
	body := []byte(`{"ImageBlurHashes":{"0123456789abcdef0123456789abcdef":"hash"},"ticks":638996512345678901}`)
	got := redactObjectKeys(body, map[string]string{})
	if bytes.Contains(got, []byte("0123456789abcdef0123456789abcdef")) || !bytes.Contains(got, []byte("638996512345678901")) || !bytes.Contains(got, []byte("${capture_key_1}")) {
		t.Fatalf("unexpected body: %s", got)
	}
}

func TestTicketRedaction(t *testing.T) {
	h := safeRequestHeaders(http.Header{"Sec-Websocket-Protocol": []string{"silo.playback-control.v2, silo.ticket.secret_value"}, "X-Silo-Device-Id": []string{"private-device"}}, nil)
	if strings.Contains(h.Get("Sec-Websocket-Protocol"), "secret_value") {
		t.Fatal("websocket ticket leaked")
	}
	if h.Get("X-Silo-Device-Id") != "${device_id}" {
		t.Fatal("device header presence not safely retained")
	}
	r := (&mediabrowser.Protocol{}).Redact(conform.Response{Body: []byte(`{"ticket":"secret_value","device_code":"secret_value","user_code":"secret_value","match_code":"secret_value","protocol":"silo.playback-control.v2"}`)})
	if bytes.Contains(r.Body, []byte("secret_value")) {
		t.Fatal("response ticket leaked")
	}
}
