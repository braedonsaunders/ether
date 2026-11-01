package capture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
)

func TestPlexProxyPreservesDecisionQueryOrder(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer generalDecisionCode="1000"/>`))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("plex", "web", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(rec)
	defer proxy.Close()
	req, _ := http.NewRequest("GET", proxy.URL+"/video/:/transcode/universal/decision?hasMDE=1&path=%2Flibrary%2Fmetadata%2F11974&directPlay=0&protocol=hls&X-Plex-Client-Identifier=owner-client-id&X-Plex-Session-Identifier=owner-session-id&session=owner-session&X-Plex-Device-Name=Braedons-MacBook", nil)
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("X-Plex-Product", "Plex Web")
	req.Header.Set("X-Plex-Client-Profile-Extra", "add-limitation(scope=videoCodec&scopeName=hevc&type=upperBound&name=video.bitDepth&value=8)")
	req.Header.Set("X-Plex-Token", "secret-token")
	req.Header.Set("X-Plex-Client-Identifier", "owner-client-id")
	req.Header.Set("X-Plex-Device-Name", "Braedons-MacBook")
	req.Header.Set("X-Plex-Model", "MacBookPro18,3")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entry := readEntry(t, dir, 1)
	if entry.Protocol != "plex" {
		t.Fatalf("protocol %q", entry.Protocol)
	}
	if !strings.Contains(entry.Request.Path, "hasMDE=1&path=") || !strings.Contains(entry.Request.Path, "&directPlay=0&protocol=hls") {
		t.Fatalf("query order lost: %s", entry.Request.Path)
	}
	if !strings.Contains(entry.Request.Path, "{{ratingKey}}") {
		t.Fatalf("library path not templated: %s", entry.Request.Path)
	}
	if strings.Contains(entry.Request.Path, "11974") || strings.Contains(entry.Request.Headers.Get("X-Plex-Token"), "secret") {
		t.Fatalf("leak in capture: %+v", entry.Request)
	}
	if entry.Request.Headers.Get("X-Plex-Client-Profile-Extra") == "" {
		t.Fatal("profile extra dropped")
	}
	out := filepath.Join(t.TempDir(), "corpus.json")
	if err = DecisionsFromCapture(dir, "web", out); err != nil {
		t.Fatal(err)
	}
	c, err := conformplex.LoadDecisionCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 1 || c.Records[0].Request.Query[0][0] != "hasMDE" || c.Records[0].Request.Query[2][0] != "directPlay" {
		t.Fatalf("corpus: %+v", c.Records)
	}
	if c.Records[0].Request.Headers[0][0] != "Accept" {
		t.Fatalf("header order: %v", c.Records[0].Request.Headers)
	}
	hasExtra := false
	for _, h := range c.Records[0].Request.Headers {
		if h[0] == "X-Plex-Client-Profile-Extra" && h[1] != "" {
			hasExtra = true
		}
	}
	if !hasExtra {
		t.Fatalf("corpus dropped Profile-Extra: %v", c.Records[0].Request.Headers)
	}
}

func TestPlexProxyRedactsOwnerIdentifiers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer/>`))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("plex", "web", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(rec)
	defer proxy.Close()
	req, _ := http.NewRequest("GET", proxy.URL+"/library/sections?X-Plex-Client-Identifier=owner-client-id&X-Plex-Session-Identifier=owner-session-id&session=owner-session&X-Plex-Device-Name=Braedons-MacBook", nil)
	req.Header.Set("X-Plex-Client-Identifier", "owner-client-id")
	req.Header.Set("X-Plex-Device-Name", "Braedons-MacBook")
	req.Header.Set("X-Plex-Model", "MacBookPro18,3")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entry := readEntry(t, dir, 1)
	leaks := []string{"owner-client-id", "owner-session-id", "owner-session", "Braedons-MacBook", "MacBookPro18,3"}
	blob := entry.Request.Path + " " + entry.Request.Headers.Get("X-Plex-Client-Identifier") + " " + entry.Request.Headers.Get("X-Plex-Device-Name") + " " + entry.Request.Headers.Get("X-Plex-Model")
	for _, leak := range leaks {
		if strings.Contains(blob, leak) {
			t.Fatalf("owner identifier leaked %q: path=%s headers=%v", leak, entry.Request.Path, entry.Request.Headers)
		}
	}
	for _, placeholder := range []string{"${client_identifier}", "${session_identifier}", "${session}", "${device_name}"} {
		if !strings.Contains(entry.Request.Path, placeholder) {
			t.Fatalf("query missing %s: %s", placeholder, entry.Request.Path)
		}
	}
	if entry.Request.Headers.Get("X-Plex-Client-Identifier") != "${client_identifier}" {
		t.Fatalf("client identifier header: %q", entry.Request.Headers.Get("X-Plex-Client-Identifier"))
	}
	if entry.Request.Headers.Get("X-Plex-Device-Name") != "${device_name}" {
		t.Fatalf("device name header: %q", entry.Request.Headers.Get("X-Plex-Device-Name"))
	}
	if entry.Request.Headers.Get("X-Plex-Model") != "${model}" {
		t.Fatalf("model header: %q", entry.Request.Headers.Get("X-Plex-Model"))
	}
}

func TestDecisionsFromCaptureRequiresReferenceOrigin(t *testing.T) {
	dir := t.TempDir()
	entry := Entry{
		Sequence: 1, Protocol: "plex", Client: "web",
		Request:  Request{Method: "GET", Path: conformplex.DecisionPath + "?path=/library/metadata/1"},
		Response: conform.Response{Status: 200},
	}
	b, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%08d.json", 1)), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := DecisionsFromCapture(dir, "web", filepath.Join(t.TempDir(), "c.json")); err == nil {
		t.Fatal("ambiguous origin accepted")
	}
}
