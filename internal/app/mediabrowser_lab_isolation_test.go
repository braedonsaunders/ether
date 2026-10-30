//go:build plexparity && conformlab

package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform"
)

// New isolation/cache baseline. The surrogate 21-title listing and generated
// media reproduce the diagnostic source setup, not a frozen live reference.
// Infuse captured-ID alignment remains a separate unresolved harness task; the
// live conformance profile list still includes it with no mismatch suppression.
// Phase two exercises live target ordering only; it makes no reference calls.
func TestLabIsolatedProfilesOffline(t *testing.T) {
	if os.Getenv("LAB_ENV") != "" {
		t.Fatal("offline gate requires LAB_ENV unset")
	}
	raw, err := os.ReadFile("testdata/lab-isolated-profile-v1-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer listing.Close()
	for _, phase := range []string{"recorded", "live_target_only"} {
		for _, spec := range []struct{ protocol, clients string }{{"jellyfin", "findroid,jellycon,jellyfin-androidtv,streamyfin,swiftfin"}, {"emby", "emby-sdk"}} {
			for _, name := range strings.Split(spec.clients, ",") {
				t.Run(phase+"/"+name, func(t *testing.T) {
					target := newLabProfileTarget(t, labReferenceData{base: listing.URL}, spec.protocol, name)
					defer target.close()
					cfg := target.app.cfg.Get()
					if cfg.Server.MaxStreams != 30 || cfg.Server.DefaultStreamLimit != 2 || target.viewer.StreamLimit != 0 {
						t.Fatal("existing fixture caps changed")
					}
					for _, s := range target.app.playback.Sessions() {
						if s.UserID != target.viewer.ID {
							t.Fatal("foreign viewer state")
						}
					}
					p := target.protocol
					client := &http.Client{Timeout: 15 * time.Second}
					var align conform.DiscoveredIDs
					if b, e := os.ReadFile(filepath.Join("..", "..", "testdata", "conform", spec.protocol, name, "_discovery.json")); e == nil {
						if json.Unmarshal(b, &align) != nil {
							t.Fatal("invalid recorded discovery")
						}
					}
					found := p.Discover(context.Background(), client, target.server.URL, target.values["access_token"], name, &align)
					for k, v := range target.values {
						found.Values[k] = v
					}
					infos, streams := 0, 0
					for n, req := range p.Corpus(name) {
						if req.FixtureOnly {
							continue
						}
						if phase == "recorded" {
							if _, e := conform.LoadFixture(filepath.Join("..", "..", "testdata", "conform", spec.protocol, name, p.FixtureName(name, req, true)+".json")); e != nil {
								t.Logf("request=%d saved_fixture_absent=true", n)
								continue
							}
						}
						res, e := p.Fetch(context.Background(), client, target.server.URL, target.values["access_token"], req, name, found.Values, true)
						if e != nil {
							t.Errorf("request=%d transport failed", n)
							continue
						}
						p.Observe(req, res, found.Values)
						if strings.Contains(req.Path, "PlaybackInfo") {
							infos++
							if res.Status != 200 {
								t.Errorf("PlaybackInfo status=%d", res.Status)
							}
						}
						if strings.Contains(req.Path, "Static=true") {
							streams++
							if (res.Status != 200 && res.Status != 206) || res.BodyLength == 0 {
								t.Errorf("static status=%d bytes=%d", res.Status, res.BodyLength)
							}
						}
						t.Logf("request=%d status=%d bytes=%d", n, res.Status, res.BodyLength)
					}
					if infos == 0 || streams == 0 {
						t.Errorf("actual playback not covered: info=%d streams=%d", infos, streams)
					}
					target.close()
					target.close() // Idempotent ownership closure, including t.Cleanup.
				})
			}
		}
	}
}

func TestLabCommandVariablesMemoryOnly(t *testing.T) {
	if os.Getenv("LAB_PIPE_HELPER") == "1" {
		for _, path := range []string{"/dev/fd/3", "/dev/fd/4"} {
			f, e := os.Open(path)
			if e != nil {
				os.Exit(3)
			}
			b, e := io.ReadAll(f)
			f.Close()
			if e != nil {
				os.Exit(4)
			}
			var values map[string]string
			if json.Unmarshal(b, &values) != nil || values["access_token"] != "synthetic-only" {
				os.Exit(5)
			}
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLabCommandVariablesMemoryOnly$")
	cmd.Env = append(os.Environ(), "LAB_PIPE_HELPER=1")
	out, e := labCommandVariables(cmd, map[string]string{"access_token": "synthetic-only"}, map[string]string{"access_token": "synthetic-only"})
	if e != nil || strings.Contains(string(out), "synthetic-only") {
		t.Fatal("memory-only subprocess transfer failed")
	}
}
