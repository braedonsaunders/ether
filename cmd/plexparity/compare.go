package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// compareCorpus compares literal response bytes. No response fields are currently
// normalized; volatile values remain visible as differences until an explicit,
// reviewed normalizer is implemented.
func compareCorpus(ctx context.Context, baseline, candidate string, out io.Writer) (int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	ids := protocol.Discover(ctx, client, baseline, "parity-owner", "web", nil)
	if len(ids.Warnings) > 0 {
		return 0, fmt.Errorf("baseline discovery incomplete: %s", strings.Join(ids.Warnings, "; "))
	}
	other := protocol.Discover(ctx, client, candidate, "parity-owner", "web", nil)
	if len(other.Warnings) > 0 {
		return 0, fmt.Errorf("candidate discovery incomplete: %s", strings.Join(other.Warnings, "; "))
	}
	for k, v := range ids.Values {
		if other.Values[k] != v {
			return 0, fmt.Errorf("fixture discovery differs for %s: %q vs %q", k, v, other.Values[k])
		}
	}
	profiles := protocol.Clients()
	web := profiles["web"]
	for _, p := range []struct{ name, product, platform, device string }{{"androidtv", "Plex for Android", "Android", "Android TV"}, {"desktop", "Plex for Windows", "Windows", "Windows"}} {
		h := web.Headers.Clone()
		h.Set("X-Plex-Product", p.product)
		h.Set("X-Plex-Platform", p.platform)
		h.Set("X-Plex-Device", p.device)
		profiles[p.name] = protocol.Client{Name: p.product, Headers: h}
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	diffs := 0
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			for _, jsonMode := range []bool{false, true} {
				for _, probe := range protocol.Corpus() {
					h := profiles[name].Headers.Clone()
					if jsonMode {
						h.Set("Accept", "application/json")
					} else {
						h.Set("Accept", "application/xml")
					}
					probe.Headers = map[string]http.Header{name: h, name + "-json": h}
					a, err := protocol.Fetch(ctx, client, baseline, "parity-"+viewer, probe, name, ids.Values, jsonMode)
					if err != nil {
						return diffs, fmt.Errorf("baseline %s %s: %w", viewer, probe.Path, err)
					}
					b, err := protocol.Fetch(ctx, client, candidate, "parity-"+viewer, probe, name, ids.Values, jsonMode)
					if err != nil {
						return diffs, fmt.Errorf("candidate %s %s: %w", viewer, probe.Path, err)
					}
					if a.Status != b.Status || !bytes.Equal(a.Body, b.Body) || a.Headers.Get("Content-Type") != b.Headers.Get("Content-Type") {
						diffs++
						fmt.Fprintf(out, "viewer=%s client=%s json=%t route=%s\nbaseline status=%d content-type=%q body=%q\ncandidate status=%d content-type=%q body=%q\n", viewer, name, jsonMode, probe.Path, a.Status, a.Headers.Get("Content-Type"), a.Body, b.Status, b.Headers.Get("Content-Type"), b.Body)
					}
				}
			}
		}
	}
	return diffs, nil
}
