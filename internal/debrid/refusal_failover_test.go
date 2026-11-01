package debrid

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// refusingRD is a Real-Debrid API that refuses addMagnet with
// infringing_file (35) for the listed hashes and serves every other torrent
// as cached.
type refusingRD struct {
	mu       sync.Mutex
	refuse   map[string]bool
	adds     map[string]int
	selected map[string]bool
}

func newRefusingRD(t *testing.T, refuse ...string) (*refusingRD, *realDebrid) {
	f := &refusingRD{refuse: map[string]bool{}, adds: map[string]int{}, selected: map[string]bool{}}
	for _, h := range refuse {
		f.refuse[h] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /torrents/addMagnet", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		m := r.PostForm.Get("magnet")
		hash := strings.ToLower(m[strings.Index(m, "btih:")+5:][:40])
		f.mu.Lock()
		f.adds[hash]++
		refused := f.refuse[hash]
		f.mu.Unlock()
		if refused {
			w.WriteHeader(451)
			fmt.Fprint(w, `{"error":"infringing_file","error_code":35}`)
			return
		}
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":"T%s","uri":"x"}`, hash)
	})
	mux.HandleFunc("GET /torrents", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) })
	mux.HandleFunc("GET /torrents/info/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		sel := f.selected[id]
		f.mu.Unlock()
		if !sel {
			fmt.Fprintf(w, `{"id":%q,"status":"waiting_files_selection","files":[{"id":1,"path":"/Movie.2024.1080p.mkv","bytes":4000000000,"selected":0}],"links":[]}`, id)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"status":"downloaded","files":[{"id":1,"path":"/Movie.2024.1080p.mkv","bytes":4000000000,"selected":1}],"links":["https://rd.example/d/%s"]}`, id, id)
	})
	mux.HandleFunc("POST /torrents/selectFiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.selected[r.PathValue("id")] = true
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /torrents/delete/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("POST /unrestrict/link", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"download":"https://cdn.example/Movie.mkv","filesize":4000000000}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, newRealDebrid(testKey, fastOpts(srv.URL))
}

func (f *refusingRD) addCount(hash string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adds[hash]
}

func hashN(n int) string { return strings.Repeat(fmt.Sprintf("%02x", n), 20) }

// refusalService wires Real-Debrid first and, when tb is set, a TorBox
// account second.
func refusalService(t *testing.T, rd *realDebrid, tb *fakeProvider, cachedOnly bool, now *time.Time) *Service {
	cfg := config.Debrid{CachedOnly: cachedOnly, RemoveUncached: true, Providers: []config.DebridProvider{
		{ID: "rd", Name: "Real-Debrid", Kind: "realdebrid", APIKey: testKey, Priority: 0, Enabled: true}}}
	if tb != nil {
		cfg.Providers = append(cfg.Providers, config.DebridProvider{ID: "tb", Name: "TorBox", Kind: "torbox", APIKey: "k", Priority: 1, Enabled: true})
	}
	return NewService(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *now },
		NewProvider: func(c config.DebridProvider, _ ProviderOptions) (Provider, error) {
			if c.ID == "tb" {
				return tb, nil
			}
			return rd, nil
		}})
}

// Real-Debrid refusing a torrent (infringing_file) hands the same release
// to the next debrid account in the owner's order, remembers the refusal so
// the next Play does not ask Real-Debrid again, and opens a release whose
// name Real-Debrid filters on the other account without asking it at all.
func TestRealDebridRefusalFailsOverToNextAccount(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h1, h2 := hashN(1), hashN(2)
	f, rd := newRefusingRD(t, h1)
	tb := newFakeProvider("torbox", newMediaServer(t, 1<<10), movieFiles(1<<30))
	tb.cached[h1], tb.cached[h2] = true, true
	svc := refusalService(t, rd, tb, true, &now)

	rel := core.Release{ID: "r1", Name: "Movie.2024.1080p.WEB-DL-GRP", Protocol: core.ProtocolTorrent, InfoHash: h1}
	b, err := svc.Acquire(t.Context(), rel)
	if err != nil {
		t.Fatalf("refused on Real-Debrid but not served by TorBox: %v", err)
	}
	if p, _ := DecodePayload(b); p.Provider != "tb" {
		t.Fatalf("served by %q", p.Provider)
	}
	if f.addCount(h1) != 1 {
		t.Fatalf("Real-Debrid adds %d", f.addCount(h1))
	}

	// Another release of the same torrent (no stored payload) skips the
	// account that refused it.
	now = now.Add(time.Minute)
	rel2 := rel
	rel2.ID = "r2"
	if _, err := svc.Acquire(t.Context(), rel2); err != nil {
		t.Fatal(err)
	}
	if f.addCount(h1) != 1 {
		t.Fatalf("refused torrent asked again: %d adds", f.addCount(h1))
	}

	if st := svc.Stats()[0]; st.State != "ok" || st.Failures != 0 || st.Blocked != 1 {
		t.Fatalf("Real-Debrid stats %+v", st)
	}
}

// With cached-only off, a torrent Real-Debrid refused is not reported as
// sent to download, and it is never queued on Real-Debrid again: the
// download goes to an account that accepted it.
func TestRefusedTorrentIsNotQueuedForDownload(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h1 := hashN(1)
	f, rd := newRefusingRD(t, h1)
	rel := core.Release{ID: "r1", Name: "Movie.2024.1080p.WEB-DL-GRP", Protocol: core.ProtocolTorrent, InfoHash: h1}

	solo := refusalService(t, rd, nil, false, &now)
	if _, err := solo.Acquire(t.Context(), rel); !errors.Is(err, core.ErrNotCached) || errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("refused torrent reported as %v", err)
	}
	if _, err := solo.Acquire(t.Context(), rel); !errors.Is(err, core.ErrNotCached) || errors.Is(err, core.ErrDownloadQueued) {
		t.Fatalf("second play: %v", err)
	}
	if f.addCount(h1) != 1 {
		t.Fatalf("refused torrent queued on Real-Debrid: %d adds", f.addCount(h1))
	}

	f2, rd2 := newRefusingRD(t, h1)
	tb := newFakeProvider("torbox", newMediaServer(t, 1<<10), movieFiles(1<<30))
	svc := refusalService(t, rd2, tb, false, &now)
	for i := range 2 {
		if _, err := svc.Acquire(t.Context(), rel); !errors.Is(err, core.ErrDownloadQueued) {
			t.Fatalf("play %d: %v", i, err)
		}
	}
	if f2.addCount(h1) != 1 || tb.callCount("acquire") < 2 {
		t.Fatalf("Real-Debrid adds %d, TorBox acquires %d", f2.addCount(h1), tb.callCount("acquire"))
	}
}
