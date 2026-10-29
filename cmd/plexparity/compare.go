package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

type capture struct {
	viewer, client, path string
	json                 bool
	response             protocol.Response
}
type transcript struct {
	ids      map[string]string
	captures []capture
}

// captureCorpus retains literal response bytes. There are no normalizations.
func captureCorpus(ctx context.Context, base string) (transcript, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	ids := protocol.Discover(ctx, client, base, "parity-owner", "web", nil)
	if len(ids.Warnings) > 0 {
		return transcript{}, fmt.Errorf("discovery incomplete: %s", strings.Join(ids.Warnings, "; "))
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
	result := transcript{ids: ids.Values}
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			for _, jsonMode := range []bool{false, true} {
				for _, probe := range protocol.Corpus() {
					probe.Path = strings.ReplaceAll(probe.Path, "query=burbs", "query=fixture")
					h := profiles[name].Headers.Clone()
					if jsonMode {
						h.Set("Accept", "application/json")
					} else {
						h.Set("Accept", "application/xml")
					}
					probe.Headers = map[string]http.Header{name: h, name + "-json": h}
					response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, probe, name, ids.Values, jsonMode)
					if err != nil {
						return result, fmt.Errorf("%s %s: %w", viewer, probe.Path, err)
					}
					if len(response.Body) >= 16<<20 {
						return result, fmt.Errorf("response at corpus size limit: %s %s", viewer, probe.Path)
					}
					result.captures = append(result.captures, capture{viewer, name, probe.Path, jsonMode, response})
				}
			}
		}
	}
	return result, nil
}
func diffTranscripts(a, b transcript, out io.Writer) (int, error) {
	if !reflect.DeepEqual(a.ids, b.ids) {
		return 0, fmt.Errorf("fixture discovery differs: baseline %v candidate %v", a.ids, b.ids)
	}
	if len(a.captures) != len(b.captures) {
		return 0, fmt.Errorf("capture counts differ: %d and %d", len(a.captures), len(b.captures))
	}
	n := 0
	for i, x := range a.captures {
		y := b.captures[i]
		if x.viewer != y.viewer || x.client != y.client || x.path != y.path || x.json != y.json {
			return n, fmt.Errorf("corpus order mismatch at %d", i)
		}
		if x.response.Status != y.response.Status || !bytes.Equal(x.response.Body, y.response.Body) || x.response.Headers.Get("Content-Type") != y.response.Headers.Get("Content-Type") {
			n++
			fmt.Fprintf(out, "viewer=%s client=%s json=%t route=%s\nbaseline status=%d content-type=%q body=%q\ncandidate status=%d content-type=%q body=%q\n", x.viewer, x.client, x.json, x.path, x.response.Status, x.response.Headers.Get("Content-Type"), x.response.Body, y.response.Status, y.response.Headers.Get("Content-Type"), y.response.Body)
		}
	}
	return n, nil
}
func compareCorpus(ctx context.Context, baseline, candidate string, out io.Writer) (int, error) {
	a, err := captureCorpus(ctx, baseline)
	if err != nil {
		return 0, fmt.Errorf("baseline: %w", err)
	}
	b, err := captureCorpus(ctx, candidate)
	if err != nil {
		return 0, fmt.Errorf("candidate: %w", err)
	}
	return diffTranscripts(a, b, out)
}
