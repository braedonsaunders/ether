package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
