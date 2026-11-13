package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Issue #396, maintenance variant: the provider pins the failed node into
// every fresh link for as long as the download's link lives, so no API
// answer is a new route. The link path and token still stand on any edge of
// the provider (observed 2026-11-09), so the file must replay the SAME link
// on an edge the engine has seen answer live — same bytes, no second
// provider, no waiting for the link to expire.
func TestRerouteReplaysPinnedLinkOnAnotherEdge(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox")
	live := newMediaServer(t, 4<<20)
	h.cacheEverywhere(testHash)
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("pinned-node"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	deadToken := pl.URL[strings.LastIndex(pl.URL, "/")+1:]
	live.issue(deadToken) // the live edge will serve this link's token

	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()

	// The engine learns the live edge from any file it serves: a witness
	// title is already streaming there.
	witness := pl
	witness.URL = live.issue("witness")
	wsrc, err := e.Open(context.Background(), "witness-file", witness.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer wsrc.Close()
	wbuf := make([]byte, 4096)
	if _, err := wsrc.(readAtContexter).ReadAtContext(context.Background(), wbuf, 0); err != nil {
		t.Fatal(err)
	}

	var links atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		links.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": pl.URL}) // pinned forever
	}))
	defer api.Close()
	h.svc.entryByID("torbox-0").p = fixtureTorBox(api.URL)

	source, err := e.Open(context.Background(), "pinned-node", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	buf := make([]byte, 4096)
	if _, err := source.(readAtContexter).ReadAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	h.media.Close() // the node dies; the provider keeps pinning it

	for _, read := range []struct {
		name string
		off  int64
		fn   func(context.Context, []byte, int64) (int, error)
	}{
		{"background", 1 << 20, source.(*httpSource).ReadAtPrefetchContext},
		{"viewer", 3 << 20, source.(readAtContexter).ReadAtContext},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := read.fn(ctx, buf, read.off)
		cancel()
		if err != nil || n != len(buf) || !bytes.Equal(buf, live.data[read.off:read.off+int64(len(buf))]) {
			t.Fatalf("%s read after the pinned node died: n=%d err=%v", read.name, n, err)
		}
	}
	if got := links.Load(); got != 0 {
		t.Fatalf("requestdl calls=%d, want %d (all pinned; the replay itself must ask the API nothing)", got, 0)
	}
}

// The replay's edges belong to the provider account that minted the token:
// a file on another provider may stream from a fresher edge, yet that host
// must not be handed the signed path and query while the wave replays.
func TestRerouteNeverHandsTheTokenToAnotherProvidersEdge(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox", "realdebrid")
	live := newMediaServer(t, 4<<20)
	foreign := newMediaServer(t, 4<<20)
	h.provs["realdebrid-1"].media = foreign
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true // only TorBox holds this file; there is no second route but an edge
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("pinned-node"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	deadToken := pl.URL[strings.LastIndex(pl.URL, "/")+1:]
	live.issue(deadToken) // the live edge will serve this link's token

	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()

	// A witness title is already streaming on the live edge: a TorBox edge.
	witness := pl
	witness.URL = live.issue("witness")
	wsrc, err := e.Open(context.Background(), "witness-file", witness.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer wsrc.Close()
	wbuf := make([]byte, 4096)
	if _, err := wsrc.(readAtContexter).ReadAtContext(context.Background(), wbuf, 0); err != nil {
		t.Fatal(err)
	}

	source, err := e.Open(context.Background(), "pinned-node", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	buf := make([]byte, 4096)
	if _, err := source.(readAtContexter).ReadAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}

	// The other provider's file now completes a block on its own edge:
	// fresher than live, but not this link's provider.
	rd := pl
	rd.Provider = "realdebrid-1"
	rd.URL = foreign.issue("foreign")
	rsrc, err := e.Open(context.Background(), "foreign-file", rd.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer rsrc.Close()
	if _, err := rsrc.(readAtContexter).ReadAtContext(context.Background(), wbuf, 0); err != nil {
		t.Fatal(err)
	}
	foreignSeen := foreign.requests.Load()

	var links atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		links.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": pl.URL}) // pinned forever
	}))
	defer api.Close()
	h.svc.entryByID("torbox-0").p = fixtureTorBox(api.URL)
	h.media.Close() // the node dies; the provider keeps pinning it

	for _, read := range []struct {
		name string
		off  int64
		fn   func(context.Context, []byte, int64) (int, error)
	}{
		{"background", 1 << 20, source.(*httpSource).ReadAtPrefetchContext},
		{"viewer", 3 << 20, source.(readAtContexter).ReadAtContext},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := read.fn(ctx, buf, read.off)
		cancel()
		if err != nil || n != len(buf) || !bytes.Equal(buf, live.data[read.off:read.off+int64(len(buf))]) {
			t.Fatalf("%s read after the pinned node died: n=%d err=%v", read.name, n, err)
		}
	}
	if got := links.Load(); got != 0 {
		t.Fatalf("requestdl calls=%d, want %d (the replay itself must ask the API nothing)", got, 0)
	}
	if got := foreign.requests.Load() - foreignSeen; got != 0 {
		t.Fatalf("another account's edge answered %d replay requests; the signed path left its provider", got)
	}
}

// A probe is the evidence a route is taken on: only a whole 206 for the
// asked offset of the expected file size proves an edge carries the link.
func TestProbeLinkRejectsFalsePositiveAnswers(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox")
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()
	const off, size = int64(1 << 20), int64(4 << 20)
	exact := fmt.Sprintf("bytes %d-%d/%d", off, off+probeSample-1, size)
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		want    bool
	}{
		{"204 no content", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}, false},
		{"200 HTML error page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>gone</html>"))
		}, false},
		{"206 for another offset", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", probeSample-1, size))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, probeSample))
		}, false},
		{"206 for another file size", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+probeSample-1, size/4))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, probeSample))
		}, false},
		{"206 truncated short", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Range", exact)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("five"))
		}, false},
		{"302 to an unapproved host", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://127.0.0.1:1/hop")
			w.WriteHeader(http.StatusFound)
		}, false},
		{"a whole 206 wins", func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Range"); got != fmt.Sprintf("bytes=%d-%d", off, off+probeSample-1) {
				t.Errorf("probe asked for %s, want the failed read's offset", got)
			}
			w.Header().Set("Content-Range", exact)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, probeSample))
		}, true},
	} {
		srv := httptest.NewServer(c.handler)
		if got := e.probeLink(context.Background(), srv.URL+"/dl/link?auth=xyz", off, size); got != c.want {
			t.Errorf("%s: probeLink=%v, want %v", c.name, got, c.want)
		}
		srv.Close()
	}
}

// A drained edge answers headers promptly and then delivers the body in a
// dribble (observed on TorBox 2026-11-12/13: edges that had been taken out
// of service served ~0.1-0.6 MB/s until they refused connections). The
// probe's sample must be big enough that one probe budget measures flow,
// not just a willingness to answer headers: a sample delivered slower than
// probeSample/edgeProbeBudget is evidence to route away from.
func TestProbeLinkRejectsDribblingEdge(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox")
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()
	const off, size = int64(1 << 20), int64(4 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+probeSample-1, size))
		w.WriteHeader(http.StatusPartialContent)
		flusher, _ := w.(http.Flusher)
		chunk := make([]byte, min(64, probeSample))
		for written := int64(0); written < probeSample; written += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return // the probe gave up; the client hung up
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond): // ~6 KB/s: headers fast, flow dead
			}
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), edgeProbeBudget)
	defer cancel()
	if e.probeLink(ctx, srv.URL+"/dl/link?auth=xyz", off, size) {
		t.Fatal("probeLink approved an edge whose body never flowed within the probe budget")
	}
}

// TorBox keeps a link's token inside its own CDN: candidates are vetted
// against the host the link itself names, before any byte is sent.
func TestTorBoxApprovesCDNEdge(t *testing.T) {
	p := newTorBox(testKey, fastOpts("http://api.invalid"))
	raw := "http://nexus-096.torboxcdn.example/dl/abc?auth=1"
	coUk := "http://a.b.co.uk/dl/abc?auth=1"
	ipIssued := "http://127.0.0.1:8080/dl/abc?auth=1"
	for _, c := range []struct {
		name string
		raw  string
		host string
		want bool
	}{
		{"the incident's live edges", raw, "nexus-132.torboxcdn.example:443", true},
		{"any host under the registrable domain", raw, "deep.edge.torboxcdn.example", true},
		{"the parent names the same registrable domain", raw, "torboxcdn.example:443", true},
		{"another port on the link's own host", raw, "nexus-096.torboxcdn.example:8443", true},
		{"suffix-wearers of the parent", raw, "torboxcdn.example.evil.net:443", false},
		{"another registrable domain", raw, "another-cdn.net:443", false},
		{"an IP candidate shares no domain", raw, "127.0.0.1:9999", false},
		{"an empty candidate", raw, "", false},
		// A shared public suffix is not a shared registrable domain: two
		// tenants of a public suffix are strangers, not one CDN.
		{"co.uk tenants are strangers", coUk, "x.y.co.uk:443", false},
		{"co.uk siblings share one domain", coUk, "c.b.co.uk:443", true},
		// Bare IPs have no registrable domain.
		{"same IP and port has no registrable domain", ipIssued, "127.0.0.1:8080", false},
		{"same IP another port is not an edge", ipIssued, "127.0.0.1:8443", false},
		{"an IP issuer keeps its exclusivity", ipIssued, "edge.torboxcdn.example:443", false},
	} {
		if got := p.approvesCDNEdge(c.raw, c.host); got != c.want {
			t.Errorf("%s: approvesCDNEdge(%q, %q)=%v, want %v", c.name, c.raw, c.host, got, c.want)
		}
	}
}

// Edge evidence belongs to the account that watched the block complete:
// freshest-first, minus the failed host, minus stale and down hosts.
func TestRecentEdgesKeepEvidencePerProviderAccount(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	var r recentEdges
	r.record("torbox-0", "nexus-132:443", now.Add(-time.Minute))
	r.record("realdebrid-1", "spare-edge:443", now)
	if got := r.freshest("torbox-0", "", nil, now); len(got) != 1 || got[0] != "nexus-132:443" {
		t.Fatalf("another account's edge leaked into the candidate list: %v", got)
	}
	r.record("torbox-0", "nexus-096:443", now)
	if got := r.freshest("torbox-0", "nexus-096:443", func(string) bool { return false }, now); len(got) != 1 || got[0] != "nexus-132:443" {
		t.Fatalf("freshest-first or exclude broken: %v", got)
	}
	if got := r.freshest("torbox-0", "nexus-096:443", func(h string) bool { return h == "nexus-132:443" }, now); len(got) != 0 {
		t.Fatalf("a down host counted as a route on freshness alone: %v", got)
	}
	if got := r.freshest("torbox-0", "", nil, now.Add(recentEdgeTTL+time.Minute)); len(got) != 0 {
		t.Fatalf("stale edges outlived the TTL: %v", got)
	}
}

// A refresh can bump the generation while a probe is in flight and leave
// the file on the same dead host with a newer token. The stale probe's 206
// must not count as a win: the fresh link runs through the same usable
// predicate as the rest of the wave, and a still-pinned file gets its new
// link tried against the remembered edges rather than a claimed replay.
func TestResumeOnAnotherEdgeDiscardsProbeStaledByRefresh(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox")
	live := newMediaServer(t, 4<<20)
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("stale-probe"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	tok1 := pl.URL[strings.LastIndex(pl.URL, "/")+1:]
	live.issue(tok1) // the live edge carries the pinned link's token
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()

	// A witness title streams on the live edge: it is the remembered edge
	// for this account's files.
	witness := pl
	witness.URL = live.issue("witness")
	wsrc, err := e.Open(context.Background(), "witness-file", witness.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer wsrc.Close()
	wbuf := make([]byte, 4096)
	if _, err := wsrc.(readAtContexter).ReadAtContext(context.Background(), wbuf, 0); err != nil {
		t.Fatal(err)
	}

	h.svc.entryByID("torbox-0").p = fixtureTorBox("http://api.invalid") // the API is never asked here
	source, err := e.Open(context.Background(), "stale-probe", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	f := source.(*httpSource).f

	failed := pl.URL
	failedAddr := dialAddr(failed)
	usable := func(u string) bool {
		return u != "" && dialAddr(u) != failedAddr && !e.backoff.down(urlHost(u))
	}
	const off = int64(1 << 20)
	probeRange := fmt.Sprintf("bytes=%d-%d", off, off+probeSample-1)
	// The edge carries the old token but stalls its probes for this offset,
	// so the test can move the link while a probe is in flight.
	live.mu.Lock()
	live.stall[tok1+"|"+probeRange] = 400 * time.Millisecond
	live.mu.Unlock()
	res := make(chan [2]bool, 1)

	// Phase 1: the refresh re-pins the file to the dead host. No move, no
	// replay claim — and the fresh token does get its chance on the edge.
	inflight := func(before int64) {
		for now := time.Now(); time.Now().Before(now.Add(3 * time.Second)); time.Sleep(5 * time.Millisecond) {
			if live.requests.Load() > before {
				return
			}
		}
		t.Fatal("the probe never reached the live edge")
	}
	seen := live.requests.Load()
	go func() {
		moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
		res <- [2]bool{moved, replayed}
	}()
	inflight(seen)
	q := pl
	q.URL = h.media.URL + "/m/repinned" // the provider pins again: same host, newer token
	q.Expires = q.Expires.Add(time.Hour)
	f.adopt(q)
	if got := <-res; got != [2]bool{false, false} {
		t.Fatalf("moved/replayed=%v, want false/false: a stale-probed refresh that stayed pinned is no replay", got)
	}
	if p, _ := f.payload(); p.URL != q.URL {
		t.Fatalf("the helper overwrote the refresh: %s, want %s", p.URL, q.URL)
	}
	if got := live.requests.Load() - seen; got != 2 {
		t.Fatalf("probe requests=%d, want 2: the stale 206 discarded, then the fresh link tried on the edge once", got)
	}

	// Phase 2: a refresh moves the file to the live edge while the second
	// probe is in flight. The mover won; the helper reports the win without
	// claiming the replay.
	live.issue("repinned") // this time that token does answer on the edge
	live.mu.Lock()
	live.stall["repinned|"+probeRange] = 400 * time.Millisecond
	live.mu.Unlock()
	seen = live.requests.Load()
	go func() {
		moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
		res <- [2]bool{moved, replayed}
	}()
	inflight(seen)
	q2 := q
	q2.URL = live.issue("moved")
	q2.Expires = q2.Expires.Add(time.Hour)
	f.adopt(q2)
	if got := <-res; got != [2]bool{true, false} {
		t.Fatalf("moved/replayed=%v, want true/false: the mover won the move, not this replay", got)
	}
	if p, _ := f.payload(); p.URL != q2.URL {
		t.Fatalf("the helper overwrote the move: %s, want %s", p.URL, q2.URL)
	}
	if got := live.requests.Load() - seen; got != 1 {
		t.Fatalf("probe requests=%d, want 1: a won move asks the edges nothing more", got)
	}
}

// A generation change mid-probe can hand the wave a link from a different
// account. Only that account may vet and probe the next candidate: an old
// account's approver must not wave a new account's token onto an edge that
// never served it, and a non-opted-in account ends the wave there.
func TestResumeOnAnotherEdgeRebuildsApprovalAfterAccountSwitch(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox", "realdebrid")
	edgeA := newMediaServer(t, 4<<20) // second candidate: must not be asked
	edgeB := newMediaServer(t, 4<<20) // freshest: carries the pinned token, stalls the probe
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("account-switch"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	tok1 := pl.URL[strings.LastIndex(pl.URL, "/")+1:]
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()

	// Two TorBox edges are seeded straight into the ledger at distinct
	// times: edgeB is freshest, so the wave reaches for it first. No
	// witness streams — this test watches the helper, nothing else.
	e.recent.record("torbox-0", urlHost(edgeA.URL), e.now())
	e.recent.record("torbox-0", urlHost(edgeB.URL), e.now().Add(time.Minute))

	h.svc.entryByID("torbox-0").p = fixtureTorBox("http://api.invalid")
	source, err := e.Open(context.Background(), "account-switch", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	f := source.(*httpSource).f

	edgeB.issue(tok1)
	const off = int64(1 << 20)
	probeRange := fmt.Sprintf("bytes=%d-%d", off, off+probeSample-1)
	hold := make(chan struct{})
	defer func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}()
	edgeB.gate[tok1+"|"+probeRange] = hold // hold the first probe until the account has switched
	edgeA.issue("rd-token")                // a probe that reached edgeA would even succeed — the point is that it must not arrive
	failed := pl.URL
	failedAddr := dialAddr(failed)
	usable := func(u string) bool {
		return u != "" && dialAddr(u) != failedAddr && !e.backoff.down(urlHost(u))
	}
	seenB := edgeB.requests.Load()
	res := make(chan [2]bool, 1)
	go func() {
		moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
		res <- [2]bool{moved, replayed}
	}()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if edgeB.requests.Load() > seenB {
			break // the probe is held in flight
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never reached the freshest edge")
		}
	}
	q := pl
	q.Provider = "realdebrid-1" // an account that never opted in
	q.URL = h.media.URL + "/m/rd-token"
	q.Expires = q.Expires.Add(time.Hour)
	f.adopt(q)
	close(hold) // the probe may answer now: the file it learns of has already moved
	if got := <-res; got != [2]bool{false, false} {
		t.Fatalf("moved/replayed=%v, want false/false: the new account is not opted in", got)
	}
	if p, _ := f.payload(); p.URL != q.URL {
		t.Fatalf("the helper overwrote the switch: %s, want %s", p.URL, q.URL)
	}
	if got := edgeA.requests.Load(); got != 0 {
		t.Fatalf("the second edge took %d request(s), none expected: the wave probed on the old snapshot", got)
	}
}

// Adoption only frees the file if the edge then carries the real read.
// When it answers the full range with a redirect instead, following that
// hop would hand Go's Referer — the signed URL, token included — to a host
// the provider never approved: the read refuses the hop and the wave
// replaces the link.
func TestRerouteRedirectedReplayReadKeepsTheTokenHome(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox")
	live := newMediaServer(t, 4<<20)
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("redirected-replay"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	tok1 := pl.URL[strings.LastIndex(pl.URL, "/")+1:]
	live.issue(tok1)

	var hopReqs atomic.Int32
	var hopRef atomic.Pointer[string]
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hopReqs.Add(1)
		ref := r.Header.Get("Referer")
		hopRef.Store(&ref)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer hop.Close()

	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()

	witness := pl
	witness.URL = live.issue("witness")
	wsrc, err := e.Open(context.Background(), "witness-file", witness.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer wsrc.Close()
	if _, err := wsrc.(readAtContexter).ReadAtContext(context.Background(), make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}

	relief := live.issue("relief") // the refresh after a refused redirect returns a fresh provider link
	var links atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		links.Add(1) // replay itself now asks for no links before the redirected read
		u := relief
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": u})
	}))
	defer api.Close()
	h.svc.entryByID("torbox-0").p = fixtureTorBox(api.URL)
	source, err := e.Open(context.Background(), "redirected-replay", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	buf := make([]byte, 4096)
	if _, err := source.(readAtContexter).ReadAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}

	h.media.Close() // the pinned node dies; the replay begins like the happy path

	// The edge will answer the real read at the failed offset with a hop
	// — while its 64-byte probe stays honest, as it was at adoption time.
	// The first request at a fresh position asks for a whole run.
	runRange := fmt.Sprintf("bytes=%d-%d", int64(1<<20), int64(1<<20)+int64(freshRunBlocks)*testBlock-1)
	live.mu.Lock()
	live.redirect[tok1+"|"+runRange] = hop.URL + "/off-cdn"
	live.mu.Unlock()

	// One block, not a coalesced prefetch: the redirect is keyed to this
	// exact range, which is what a single-block fetch asks for.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	n, err := source.(readAtContexter).ReadAtContext(ctx, buf, 1<<20)
	cancel()
	if err != nil || n != len(buf) || !bytes.Equal(buf, live.data[1<<20:1<<20+int64(len(buf))]) {
		t.Fatalf("read after the redirected replay: n=%d err=%v", n, err)
	}
	if got := hopReqs.Load(); got != 0 {
		t.Fatalf("the redirect target took %d request(s) (Referer %q): the replayed read followed an unvetted hop", got, deref(hopRef.Load()))
	}
	p, _ := source.(*httpSource).f.payload()
	if p.URL != relief || p.OnReplayEdge {
		live.mu.Lock()
		rs := strings.Join(live.ranges, " ")
		live.mu.Unlock()
		t.Fatalf("the wave left the file on %s (replay=%v), want the provider's fresh link %s; live saw [%s]", p.URL, p.OnReplayEdge, relief, rs)
	}
	if got := links.Load(); got != 1 {
		t.Fatalf("requestdl calls=%d, want 1 (only the dead-link refresh for the redirected read)", got)
	}
	// Readback, the round-three way: relink copies the payload it is
	// handed, so a forced refresh must retire the replay marker at the
	// provider-link assignment, before the save — a stored link comes back
	// verbatim, marker and all. Hand the service a marked payload, as a
	// replayed file's refresh does, and read the row out after it.
	marked := pl
	marked.OnReplayEdge = true
	out, err := h.svc.Refresh(context.Background(), marked, true)
	if err != nil {
		t.Fatal(err)
	}
	if out.OnReplayEdge {
		t.Fatal("a forced refresh returned a fresh provider link wearing the replay marker")
	}
	if stored, ok := h.svc.storedPayload(context.Background(), marked.Key); !ok || stored.OnReplayEdge {
		t.Fatalf("the store read back a fresh provider link wearing the marker (found=%v)", ok)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	n, err = source.(readAtContexter).ReadAtContext(ctx2, buf, 3<<20)
	cancel2()
	if err != nil || n != len(buf) || !bytes.Equal(buf, live.data[3<<20:3<<20+int64(len(buf))]) {
		t.Fatalf("viewer read after the replaced link: n=%d err=%v", n, err)
	}
}

// A 3xx from an ordinary, provider-signed link is not a replayed read: a
// Location-less 302 comes back unanswered even to a client that follows
// redirects, and it must keep the treatment it always had — retry the same
// link, and if the answer never changes, fail as a source outage. Burning
// the link budget on those answers turns transient CDN oddities into
// ErrArticleMissing after four generations of refreshes.
func TestOrdinaryRedirectedReadKeepsItsLinkAndClass(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox")
	live := newMediaServer(t, 4<<20)
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("ordinary-redirect"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	tok := "ordinary-302"
	pl.URL = live.issue(tok) // the provider's link stands on this edge for the whole test
	raw = pl.Encode()

	var links atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		links.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": pl.URL})
	}))
	defer api.Close()
	h.svc.entryByID("torbox-0").p = fixtureTorBox(api.URL)

	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()
	source, err := e.Open(context.Background(), "ordinary-redirect", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	buf := make([]byte, 4096)
	if _, err := source.(readAtContexter).ReadAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}

	// The edge now answers every request for this link with a Location-less
	// 302 — Go hands it back unanswered even to the ordinary client. Read
	// past the blocks the first read cached, so the wire is really asked.
	live.mu.Lock()
	live.status[tok] = http.StatusFound
	live.mu.Unlock()
	const off = int64(3 << 20)
	if _, err := source.(readAtContexter).ReadAtContext(context.Background(), buf, off); !errors.Is(err, core.ErrSourceUnavailable) {
		t.Errorf("persistent Location-less 302: err=%v, want ErrSourceUnavailable as before the 3xx branch (not a burned ArticleMissing)", err)
	}
	if got := links.Load(); got != 0 {
		t.Errorf("requestdl calls=%d, want 0: an unmarked link's redirect must not burn refreshes", got)
	}
	if p, _ := source.(*httpSource).f.payload(); p.URL != pl.URL || p.OnReplayEdge {
		t.Errorf("the file ended on %s (replay=%v), want the unrefreshed signed link %s", p.URL, p.OnReplayEdge, pl.URL)
	}
}

// A generation change during a FAILED probe: the wave must re-check the
// link before spending the old snapshot's remaining candidates. When the
// switch moved the file onto a route that is already usable, the mover's
// win is reported — the way a won SUCCESSFUL probe reports it — instead of
// the wave probing on and falsely claiming no route (and its cooldown).
func TestResumeOnAnotherEdgeSwitchDuringFailedProbe(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox", "torbox")
	edgeB := newMediaServer(t, 4<<20) // the freshest A edge: holds the probe, then proves it stale with a 204
	edgeA := newMediaServer(t, 4<<20) // an honest A candidate: must not be asked after the switch
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("switch-failed-probe"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	tok1 := pl.URL[strings.LastIndex(pl.URL, "/")+1:]
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()

	e.recent.record("torbox-0", urlHost(edgeA.URL), e.now())
	e.recent.record("torbox-0", urlHost(edgeB.URL), e.now().Add(time.Minute)) // freshest
	h.svc.entryByID("torbox-0").p = fixtureTorBox("http://api.invalid")
	h.svc.entryByID("torbox-1").p = fixtureTorBox("http://api.invalid")
	source, err := e.Open(context.Background(), "switch-failed-probe", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	f := source.(*httpSource).f

	edgeB.issue(tok1)
	edgeB.status[tok1] = http.StatusNoContent // every probe of A's token here fails; only the held first one is gated
	const off = int64(1 << 20)
	probeRange := fmt.Sprintf("bytes=%d-%d", off, off+probeSample-1)
	hold := make(chan struct{})
	defer func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}()
	edgeB.gate[tok1+"|"+probeRange] = hold
	edgeA.issue("honest") // a probe reaching edgeA would even 403 (unknown token): the point is that none arrives
	failed := pl.URL
	failedAddr := dialAddr(failed)
	q := pl
	q.Provider = "torbox-1" // a second opted-in TorBox account
	q.URL = edgeB.issue("fresh-b")
	q.Expires = q.Expires.Add(time.Hour)
	usable := func(u string) bool {
		return u != "" && dialAddr(u) != failedAddr && !e.backoff.down(urlHost(u))
	}
	seenB := edgeB.requests.Load()
	res := make(chan [2]bool, 1)
	go func() {
		moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
		res <- [2]bool{moved, replayed}
	}()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if edgeB.requests.Load() > seenB {
			break // the probe is held in flight
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never reached the freshest edge")
		}
	}
	f.adopt(q)
	close(hold) // the probe may fail now: the link it will report on has moved
	if got := <-res; got != [2]bool{true, false} {
		t.Fatalf("moved/replayed=%v, want true/false: the switch moved the file to a usable route; a failed stale probe must not probe on and deny it", got)
	}
	if p, _ := f.payload(); p.URL != q.URL {
		t.Fatalf("the helper overwrote the switch: %s, want %s", p.URL, q.URL)
	}
	if got := edgeA.requests.Load(); got != 0 {
		t.Fatalf("edgeA took %d request(s), want 0: the wave probed the old ledger after the account switch", got)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// The ledger's evidence must name the account whose link the request
// actually fetched: while an adopt loop flips the file between accounts on
// two different edges, every recorded (provider, host) pair has to belong
// to that provider's edge — never a URL from one snapshot credited to the
// other's account.
func TestRecentEdgesAttributeToTheRequestSnapshot(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox", "realdebrid")
	torHostEdge := newMediaServer(t, 4<<20) // serves torbox-0's links
	rdEdge := newMediaServer(t, 4<<20)      // serves realdebrid-1's links
	h.cacheEverywhere(testHash)
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("flip"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	rd := base
	rd.Provider = "realdebrid-1"
	rd.URL = rdEdge.issue("flip-open")
	const flipBlock = 4 << 10
	e := NewEngine(h.svc, EngineOptions{BlockSize: flipBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()
	source, err := e.Open(context.Background(), "flip", rd.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	f := source.(*httpSource).f

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for tick := 1; ; tick++ {
			select {
			case <-stop:
				return
			default:
			}
			q := base
			q.Expires = time.Now().Add(time.Hour + time.Duration(tick)*time.Minute)
			if tick%2 == 0 {
				q.Provider, q.URL = "torbox-0", torHostEdge.issue(fmt.Sprintf("flip%d", tick))
			} else {
				q.Provider, q.URL = "realdebrid-1", rdEdge.issue(fmt.Sprintf("flip%d", tick))
			}
			f.adopt(q)
		}
	}()
	buf := make([]byte, flipBlock)
	for i := 0; i < 512; i++ {
		if i%32 == 16 {
			// The goroutine races adoptions against reads; this inline flip makes
			// sure both accounts serve some blocks even when the scheduler never
			// runs that goroutine mid-loop (slow CI runners).
			q := base
			q.Expires = time.Now().Add(2 * time.Hour)
			q.Provider, q.URL = "torbox-0", torHostEdge.issue(fmt.Sprintf("inline%d", i))
			f.adopt(q)
		}
		if _, err := source.(readAtContexter).ReadAtContext(context.Background(), buf, int64(i)*flipBlock); err != nil {
			close(stop)
			t.Fatalf("flipped read %d: %v", i, err)
		}
	}
	close(stop)
	<-done

	torHost, rdHost := urlHost(torHostEdge.URL), urlHost(rdEdge.URL)
	e.recent.mu.Lock()
	defer e.recent.mu.Unlock()
	if len(e.recent.at["torbox-0"]) == 0 || len(e.recent.at["realdebrid-1"]) == 0 {
		t.Fatalf("the flip taught only one account: %+v", e.recent.at)
	}
	for provider, want := range map[string]string{"torbox-0": torHost, "realdebrid-1": rdHost} {
		for host := range e.recent.at[provider] {
			if host != want {
				t.Fatalf("%s's ledger learned %s, a host whose blocks belong to the other account: URL and provider came from different snapshots", provider, host)
			}
		}
	}
}

// An ordinary, unmarked read can see a 3xx too: Go hands a 304 (or a 300,
// or a hop without its Location) back to callers even on a client that
// follows redirects. That answer must stay on the ordinary lane — retry
// the same URL, classify the failure as transient — instead of spending
// the dead-link budget on CDN oddities and escalating into a missing
// article.
func TestRangeGetUnmarkedRedirectAnswerKeepsTransientClass(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox")
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)

	// A stubborn edge: it never answers with bytes, always a 304.
	stubborn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer stubborn.Close()
	// And every fresh link the provider mints lands on the same one.
	var links atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		links.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": stubborn.URL + "/m/token"})
	}))
	defer api.Close()

	raw, err := h.svc.Acquire(context.Background(), torrentRelease("ordinary-3xx"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.entryByID("torbox-0").p = fixtureTorBox(api.URL)
	asked := pl
	asked.URL = stubborn.URL + "/m/token"
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()
	source, err := e.Open(context.Background(), "ordinary-3xx", asked.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	_, err = source.(readAtContexter).ReadAtContext(context.Background(), make([]byte, 4096), 0)
	if !errors.Is(err, core.ErrSourceUnavailable) {
		t.Fatalf("ordinary 3xx answer error = %v, want %v: a CDN oddity must not escalate into a missing article", err, core.ErrSourceUnavailable)
	}
	if got := links.Load(); got != 0 {
		t.Fatalf("the ordinary 3xx answer minted %d fresh link(s), want 0: retries stay on the snapshot that has not been marked a replay", got)
	}
}

// The wave must notice an account switch even when the in-flight probe
// never answers with bytes: a failed probe that continues on the stale
// snapshot mints the next candidate from the old token and probes it on
// the old account's ledger, exactly what a successful stale probe forbids.
func TestResumeOnAnotherEdgeFailedProbeYieldsToAccountSwitch(t *testing.T) {
	h := newHarness(t, 4<<20, "torbox", "realdebrid")
	edgeA := newMediaServer(t, 4<<20) // second candidate: must not be asked
	edgeB := newMediaServer(t, 4<<20) // freshest: carries the pinned token, stalls the probe
	tor := h.provs["torbox-0"]
	tor.mu.Lock()
	tor.cached[testHash] = true
	tor.mu.Unlock()
	cfg := h.svc.Config()
	cfg.ReadAheadChunks = -1
	h.svc.UpdateConfig(cfg)
	raw, err := h.svc.Acquire(context.Background(), torrentRelease("failed-probe-switch"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	tok1 := pl.URL[strings.LastIndex(pl.URL, "/")+1:]
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()

	e.recent.record("torbox-0", urlHost(edgeA.URL), e.now())
	e.recent.record("torbox-0", urlHost(edgeB.URL), e.now().Add(time.Minute))

	h.svc.entryByID("torbox-0").p = fixtureTorBox("http://api.invalid")
	source, err := e.Open(context.Background(), "failed-probe-switch", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	f := source.(*httpSource).f

	// edgeB never issues tok1: when the gate lifts, the probe wakes to a
	// refused token and answers false. edgeA issues nothing either — a
	// probe that reached it would fail too; the point is that it must not
	// arrive: minted from a snapshot the file already left, it would
	// probe with the account the switch replaced.
	const off = int64(1 << 20)
	probeRange := fmt.Sprintf("bytes=%d-%d", off, off+probeSample-1)
	hold := make(chan struct{})
	defer func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}()
	edgeB.gate[tok1+"|"+probeRange] = hold // hold the first probe until the account has switched
	failed := pl.URL
	failedAddr := dialAddr(failed)
	usable := func(u string) bool {
		return u != "" && dialAddr(u) != failedAddr && !e.backoff.down(urlHost(u))
	}
	seenB := edgeB.requests.Load()
	res := make(chan [2]bool, 1)
	go func() {
		moved, replayed := f.resumeOnAnotherEdge(context.Background(), failed, usable, off)
		res <- [2]bool{moved, replayed}
	}()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if edgeB.requests.Load() > seenB {
			break // the probe is held in flight
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never reached the freshest edge")
		}
	}
	q := pl
	q.Provider = "realdebrid-1" // an account that never opted in
	q.URL = h.media.URL + "/m/rd-token"
	q.Expires = q.Expires.Add(time.Hour)
	f.adopt(q)
	close(hold) // the probe may answer now: it wakes to a refused token
	if got := <-res; got != [2]bool{false, false} {
		t.Fatalf("moved/replayed=%v, want false/false: the new account is not opted in", got)
	}
	if got := edgeA.requests.Load(); got != 0 {
		t.Fatalf("the second edge took %d request(s), none expected: a failed probe continued the wave on the stale snapshot", got)
	}
}
